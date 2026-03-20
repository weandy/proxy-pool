"""Proxy Pool 统一网关 — FastAPI 入口（聚合启动 Go 引擎 + Python 网关）"""
import os
import sys
import signal
import subprocess
import time
import socket
import asyncio
import secrets
import json as json_mod
import csv as csv_mod
from io import StringIO
import uvicorn
from pathlib import Path
from fastapi import FastAPI, Request, Form
from fastapi.responses import HTMLResponse, RedirectResponse, PlainTextResponse, JSONResponse
from fastapi.staticfiles import StaticFiles
from fastapi.templating import Jinja2Templates
from starlette.responses import StreamingResponse, Response
from contextlib import asynccontextmanager

from config import ADMIN_PORT, ADMIN_HOST, ENGINE_DIR, ENGINE_BIN
from auth import check_login, get_current_user, require_auth
from engine_client import engine
from database import init_db, ApiKeyManager, ApiLogger
from api_routes import router as api_router

# CSRF 密钥（每次进程启动时生成）
_CSRF_SECRET = secrets.token_hex(32)

# #3 SSE 并发连接计数
_sse_connections = 0
_engine_proc = None


def _start_engine():
    """启动 Go 引擎子进程"""
    global _engine_proc
    engine_path = Path(ENGINE_DIR) / ENGINE_BIN
    if not engine_path.exists():
        print(f"  ⚠ Go 引擎不存在: {engine_path}，跳过自动启动")
        print(f"    请手动启动或设置 ENGINE_DIR/ENGINE_BIN 环境变量")
        return

    print(f"  🚀 自动启动 Go 引擎: {engine_path}")
    _engine_proc = subprocess.Popen(
        [str(engine_path)],
        cwd=str(ENGINE_DIR),
        stdout=sys.stdout,
        stderr=sys.stderr,
    )
    # 等待引擎 HTTP 端口就绪
    for i in range(30):
        try:
            s = socket.create_connection(("127.0.0.1", 18080), timeout=1)
            s.close()
            print(f"  ✅ Go 引擎已就绪 (等待 {i+1}s)")
            return
        except (ConnectionRefusedError, OSError, socket.timeout):
            time.sleep(1)
    print(f"  ⚠ Go 引擎启动超时（30s），继续启动网关...")


def _stop_engine():
    """停止 Go 引擎子进程"""
    global _engine_proc
    if _engine_proc and _engine_proc.poll() is None:
        print("  🛑 关闭 Go 引擎...")
        _engine_proc.terminate()
        try:
            _engine_proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            _engine_proc.kill()
        _engine_proc = None


@asynccontextmanager
async def lifespan(app: FastAPI):
    init_db()
    _start_engine()
    print("================================")
    print("  Proxy Pool 统一网关（聚合启动）")
    print(f"  管理后台: http://{ADMIN_HOST}:{ADMIN_PORT}")
    print(f"  对外 API: http://{ADMIN_HOST}:{ADMIN_PORT}/api/*")
    print("  Go 引擎:  仅通过 localhost 内部通信")
    print("  多用户:   API Key + 配额 + 速率限制(10r/s)")
    print("================================")
    yield
    await engine.close()
    _stop_engine()

app = FastAPI(title="Proxy Pool Gateway", docs_url=None, redoc_url=None, lifespan=lifespan)
templates = Jinja2Templates(directory="templates")
# 全局注入 csrf_token（所有模板可通过 {{ csrf_token }} 使用）
templates.env.globals["csrf_token"] = ""  # 默认空值，中间件 cookie 会设置真实值

# 挂载静态文件（本地字体等）
static_dir = Path(__file__).parent / "static"
if static_dir.is_dir():
    app.mount("/static", StaticFiles(directory=str(static_dir)), name="static")


# ===== CSRF 中间件 =====
def _generate_csrf_token(request: Request) -> str:
    """Session-based CSRF token（基于 cookie）"""
    token = request.cookies.get("csrf_token") or secrets.token_hex(32)
    return token


@app.middleware("http")
async def csrf_middleware(request: Request, call_next):
    # 仅对管理接口的状态变更请求验证 CSRF
    if request.method in ("POST", "PUT", "DELETE") and request.url.path.startswith("/admin"):
        cookie_token = request.cookies.get("csrf_token", "")
        header_token = request.headers.get("x-csrf-token", "")
        if not cookie_token or cookie_token != header_token:
            return JSONResponse({"error": "CSRF 验证失败"}, status_code=403)
    response = await call_next(request)
    # 确保每个响应都设置 csrf_token cookie
    if "csrf_token" not in request.cookies:
        response.set_cookie("csrf_token", secrets.token_hex(32), httponly=False, samesite="strict")
    return response


# 挂载对外 API 路由（多 Key 鉴权）
app.include_router(api_router)


# ==================== 登录失败限制 ====================
_login_attempts: dict = {}  # ip -> {count, locked_until}

def _check_login_limit(ip: str) -> bool:
    """检查 IP 是否被锁定，True=允许登录"""
    info = _login_attempts.get(ip)
    if not info:
        return True
    import time as _time
    if info.get("locked_until", 0) > _time.time():
        return False
    return True

def _record_login_failure(ip: str):
    import time as _time
    info = _login_attempts.setdefault(ip, {"count": 0, "locked_until": 0})
    info["count"] += 1
    if info["count"] >= 5:
        info["locked_until"] = _time.time() + 300  # 锁定 5 分钟
        info["count"] = 0

def _reset_login_attempts(ip: str):
    _login_attempts.pop(ip, None)


# ==================== 健康检查 ====================

@app.get("/healthz")
async def healthz():
    return {"status": "ok", "gateway": "running"}


# ==================== 管理页面路由（JWT 登录）======================================

@app.get("/", response_class=HTMLResponse)
async def index(request: Request):
    user = get_current_user(request)
    if not user:
        return RedirectResponse("/login", status_code=302)
    return RedirectResponse("/dashboard", status_code=302)


@app.get("/login", response_class=HTMLResponse)
async def login_page(request: Request):
    return templates.TemplateResponse("login.html", {"request": request, "error": ""})


@app.post("/login")
async def login_submit(request: Request, username: str = Form(...), password: str = Form(...)):
    client_ip = request.client.host if request.client else "unknown"
    if not _check_login_limit(client_ip):
        return templates.TemplateResponse("login.html", {"request": request, "error": "登录失败次数过多，请 5 分钟后重试"})
    token = check_login(username, password)
    if not token:
        _record_login_failure(client_ip)
        return templates.TemplateResponse("login.html", {"request": request, "error": "用户名或密码错误"})
    _reset_login_attempts(client_ip)
    resp = RedirectResponse("/dashboard", status_code=302)
    resp.set_cookie("token", token, httponly=True, max_age=86400)
    return resp


@app.get("/logout")
async def logout():
    resp = RedirectResponse("/login", status_code=302)
    resp.delete_cookie("token")
    return resp


@app.get("/dashboard", response_class=HTMLResponse)
async def dashboard(request: Request):
    user = require_auth(request)
    return templates.TemplateResponse("dashboard.html", {"request": request, "user": user})


@app.get("/admin/api/sse/stats")
async def sse_stats(request: Request):
    """SSE 实时统计推送（每 5 秒）— 限制最大并发连接数"""
    require_auth(request)

    # #3 SSE 连接数限制
    MAX_SSE = 5
    if _sse_connections >= MAX_SSE:
        return JSONResponse({"error": "Too many SSE connections"}, status_code=429)

    async def event_stream():
        global _sse_connections
        _sse_connections += 1
        try:
            while True:
                if await request.is_disconnected():
                    break
                try:
                    stats = await engine.get_stats()
                    keys = ApiKeyManager.list_keys()
                    stats["total_keys"] = len(keys)
                    stats["active_keys"] = sum(1 for k in keys if k["is_active"])
                    yield f"data: {json_mod.dumps(stats, ensure_ascii=False, default=str)}\n\n"
                    # 任务运行时 1 秒更新，空闲时 5 秒
                    task = stats.get("task", {})
                    interval = 1 if task.get("running") else 5
                except Exception as e:
                    yield f"data: {{\"error\": \"{str(e)}\"}}\n\n"
                    interval = 5
                await asyncio.sleep(interval)
        finally:
            _sse_connections -= 1

    return StreamingResponse(event_stream(), media_type="text/event-stream",
                             headers={"Cache-Control": "no-cache", "X-Accel-Buffering": "no"})


@app.get("/proxies", response_class=HTMLResponse)
async def proxies_page(request: Request):
    user = require_auth(request)
    page = int(request.query_params.get("page", 1))
    country = request.query_params.get("country", "")
    ip_type = request.query_params.get("ip_type", "")
    protocol = request.query_params.get("protocol", "")
    min_score = request.query_params.get("min_score", "")
    max_latency = request.query_params.get("max_latency", "")
    params = {"page": page, "size": 20, "sort": "score"}
    if country:
        params["country"] = country
    if ip_type:
        params["ip_type"] = ip_type
    if protocol:
        params["protocol"] = protocol
    if min_score:
        params["min_score"] = min_score
    if max_latency:
        params["max_latency"] = max_latency
    try:
        data = await engine.get_proxies(**params)
    except Exception as e:
        data = {"proxies": [], "total": 0, "count": 0, "error": str(e)}
    filters = {"country": country, "ip_type": ip_type, "protocol": protocol,
               "min_score": min_score, "max_latency": max_latency}
    return templates.TemplateResponse("proxies.html", {
        "request": request, "user": user, "data": data, "page": page,
        "country": country, "filters": filters
    })


@app.get("/settings", response_class=HTMLResponse)
async def settings_page(request: Request):
    user = require_auth(request)
    try:
        cfg = await engine.get_config()
    except Exception as e:
        cfg = {"error": str(e)}
    return templates.TemplateResponse("settings.html", {"request": request, "user": user, "config": cfg})


@app.get("/urls", response_class=HTMLResponse)
async def urls_page(request: Request):
    user = require_auth(request)
    try:
        url_data = await engine.get_urls()
    except Exception as e:
        url_data = {"urls": [], "error": str(e)}
    try:
        source_data = await engine.get_source_stats()
    except Exception:
        source_data = {"sources": []}

    # 合并：以 proxy_sources 表为权威源（已删除的不显示）
    active_urls = set(url_data.get("urls") or [])
    stats_map = {s["url"]: s for s in (source_data.get("sources") or []) if s["url"] in active_urls}
    empty = {"total": 0, "alive_http": 0, "alive_https": 0, "avg_score": 0,
             "avg_latency": 0, "total_fetched": 0, "total_new": 0, "dup_rate": 0, 
             "last_fetch_at": "", "fetch_success_rate": 0}
    merged = []
    # 有统计数据的排前面（按存活排序）
    for url in active_urls:
        if url in stats_map:
            merged.append(stats_map[url])
        else:
            merged.append({**empty, "url": url})
    # 按存活数降序排列
    merged.sort(key=lambda s: (s.get("alive_http", 0) + s.get("alive_https", 0)), reverse=True)
    source_data["sources"] = merged

    return templates.TemplateResponse("urls.html", {
        "request": request, "user": user,
        "url_data": url_data, "source_data": source_data
    })


# ==================== Key 管理页面 ====================

@app.get("/keys", response_class=HTMLResponse)
async def keys_page(request: Request):
    user = require_auth(request)
    keys = ApiKeyManager.list_keys()
    # 附加用量
    for k in keys:
        k["usage"] = ApiKeyManager.get_key_usage(k["id"])
    return templates.TemplateResponse("keys.html", {"request": request, "user": user, "keys": keys})


# ==================== 用量统计页面 ====================

@app.get("/usage", response_class=HTMLResponse)
async def usage_page(request: Request):
    user = require_auth(request)
    key_id = int(request.query_params.get("key_id", 0))
    keys = ApiKeyManager.list_keys()
    logs = ApiLogger.get_recent_logs(key_id, limit=200)
    daily = ApiLogger.get_daily_stats(key_id, days=30)
    return templates.TemplateResponse("usage.html", {
        "request": request, "user": user, "keys": keys,
        "logs": logs, "daily": daily, "selected_key": key_id
    })





# ==================== 管理后台 AJAX（JWT 登录保护）====================

@app.post("/admin/api/config")
async def admin_update_config(request: Request):
    require_auth(request)
    body = await request.json()
    return await engine.update_config(body)


@app.post("/admin/api/task/trigger")
async def admin_trigger(request: Request):
    require_auth(request)
    return await engine.trigger_task()


@app.get("/admin/api/stats/daily")
async def admin_daily_stats(request: Request):
    require_auth(request)
    days = int(request.query_params.get("days", 30))
    return await engine.get_daily_stats(days)


@app.post("/admin/api/tg/test")
async def admin_tg_test(request: Request):
    require_auth(request)
    return await engine.test_telegram()


@app.post("/admin/api/task/cancel")
async def admin_cancel(request: Request):
    require_auth(request)
    return await engine.cancel_task()


@app.get("/admin/api/proxies/export")
async def admin_export_proxies(request: Request):
    """导出代理列表（txt 或 csv）"""
    require_auth(request)
    fmt = request.query_params.get("fmt", "txt")
    params = {"number": "all", "sort": "score"}
    for k in ["country", "ip_type", "protocol", "min_score", "max_latency"]:
        v = request.query_params.get(k, "")
        if v:
            params[k] = v
    data = await engine.get_proxies(**params)
    proxies = data.get("proxies", [])
    if fmt == "csv":
        buf = StringIO()
        writer = csv_mod.writer(buf)
        writer.writerow(["addr", "country", "protocol", "ip_type", "isp_name", "score", "avg_latency"])
        for p in proxies:
            writer.writerow([p.get("addr",""), p.get("country",""), p.get("protocol",""),
                             p.get("ip_type",""), p.get("isp_name",""),
                             f'{p.get("score",0):.0f}', f'{p.get("avg_latency",0):.0f}'])
        return Response(buf.getvalue(), media_type="text/csv",
                       headers={"Content-Disposition": "attachment; filename=proxies.csv"})
    else:
        lines = "\n".join(p.get("addr", "") for p in proxies)
        return Response(lines, media_type="text/plain",
                       headers={"Content-Disposition": "attachment; filename=proxies.txt"})


@app.post("/admin/api/proxies/blacklist")
async def admin_blacklist(request: Request):
    require_auth(request)
    body = await request.json()
    return await engine.blacklist_addrs(body.get("addrs", []))


@app.post("/admin/api/proxies/revive")
async def admin_revive(request: Request):
    require_auth(request)
    return await engine.revive_blacklist()


@app.post("/admin/api/urls")
async def admin_update_urls(request: Request):
    require_auth(request)
    body = await request.json()
    return await engine.update_urls(body.get("urls", []))


@app.post("/admin/api/urls/add")
async def admin_add_urls(request: Request):
    """批量添加源 URL"""
    require_auth(request)
    body = await request.json()
    return await engine.add_urls(body.get("urls", []))


@app.api_route("/admin/api/urls", methods=["DELETE"])
async def admin_delete_urls(request: Request):
    """批量删除源 URL"""
    require_auth(request)
    body = await request.json()
    return await engine.delete_urls(body.get("urls", []))


# ===== Key 管理 API =====

@app.post("/admin/api/keys/create")
async def admin_create_key(request: Request):
    require_auth(request)
    body = await request.json()
    result = ApiKeyManager.create_key(
        name=body.get("name", "unnamed"),
        quota_daily=body.get("quota_daily", 0),
        quota_monthly=body.get("quota_monthly", 0),
        allowed_countries=body.get("allowed_countries", ""),
        allowed_protocols=body.get("allowed_protocols", ""),
        expires_at=body.get("expires_at"),
    )
    return result


@app.put("/admin/api/keys/{key_id}")
async def admin_update_key(key_id: int, request: Request):
    require_auth(request)
    body = await request.json()
    ok = ApiKeyManager.update_key(key_id, **body)
    return {"status": "updated" if ok else "no_change"}


@app.delete("/admin/api/keys/{key_id}")
async def admin_delete_key(key_id: int, request: Request):
    require_auth(request)
    ok = ApiKeyManager.delete_key(key_id)
    return {"status": "deleted" if ok else "not_found"}


# ===== 用量 API =====

@app.get("/admin/api/usage/csv")
async def admin_usage_csv(request: Request):
    require_auth(request)
    key_id = int(request.query_params.get("key_id", 0))
    csv_data = ApiLogger.get_csv_data(key_id)
    return PlainTextResponse(csv_data, media_type="text/csv",
                             headers={"Content-Disposition": "attachment; filename=api_usage.csv"})


if __name__ == "__main__":
    uvicorn.run("main:app", host=ADMIN_HOST, port=ADMIN_PORT, reload=True)
