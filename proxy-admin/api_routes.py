"""对外 API 路由 — 多用户 Key 鉴权 + 速率限制 + 用量统计"""
import time
import threading
from typing import Dict
from collections import defaultdict
from fastapi import APIRouter, Request, HTTPException
from fastapi.responses import PlainTextResponse
from engine_client import engine
from database import ApiKeyManager, ApiLogger

router = APIRouter(prefix="/api", tags=["Public API"])


# ==================== #2 速率限制（令牌桶） ====================

class _RateLimiter:
    """按 Key 的令牌桶速率限制器"""

    def __init__(self, rate: float = 10.0, burst: int = 20):
        """rate: 每秒允许请求数, burst: 突发上限"""
        self._rate = rate
        self._burst = burst
        self._lock = threading.Lock()
        self._tokens: Dict[int, float] = defaultdict(lambda: float(burst))
        self._last_time: Dict[int, float] = {}

    def allow(self, key_id: int) -> bool:
        now = time.time()
        with self._lock:
            if key_id not in self._last_time:
                self._last_time[key_id] = now
                self._tokens[key_id] = self._burst - 1
                return True

            elapsed = now - self._last_time[key_id]
            self._last_time[key_id] = now

            # 补充令牌
            self._tokens[key_id] = min(
                self._burst,
                self._tokens[key_id] + elapsed * self._rate
            )

            if self._tokens[key_id] >= 1:
                self._tokens[key_id] -= 1
                return True
            return False


_rate_limiter = _RateLimiter(rate=10.0, burst=20)  # 10 req/s, 突发 20


# ==================== 辅助函数 ====================

def _extract_key(request: Request) -> str:
    key = request.query_params.get("key", "") or request.query_params.get("api_key", "")
    if not key:
        auth = request.headers.get("Authorization", "")
        if auth.startswith("Bearer "):
            key = auth[7:]
    return key


def _validate_key(request: Request):
    key_value = _extract_key(request)
    if not key_value:
        raise HTTPException(status_code=401, detail="Missing api_key parameter or Authorization header")
    key_info = ApiKeyManager.validate_key(key_value)
    if not key_info:
        raise HTTPException(status_code=401, detail="Invalid, expired, or quota exceeded api_key")

    # #2 速率限制
    if not _rate_limiter.allow(key_info["id"]):
        raise HTTPException(status_code=429, detail="Rate limit exceeded (10 req/s)")

    return key_info


def _get_client_ip(request: Request) -> str:
    forwarded = request.headers.get("X-Forwarded-For", "")
    if forwarded:
        return forwarded.split(",")[0].strip()
    real_ip = request.headers.get("X-Real-IP", "")
    if real_ip:
        return real_ip
    if request.client:
        return request.client.host
    return "unknown"


def _log_request(key_info: dict, request: Request, endpoint: str,
                 proxy_count: int = 0, response_ms: int = 0, status_code: int = 200):
    try:
        ApiLogger.log(
            key_id=key_info["id"],
            api_key=key_info["key"],
            endpoint=endpoint,
            client_ip=_get_client_ip(request),
            user_agent=request.headers.get("User-Agent", ""),
            query_params=dict(request.query_params),
            proxy_count=proxy_count,
            response_ms=response_ms,
            status_code=status_code,
        )
    except Exception:
        pass


def _apply_permissions(key_info: dict, params: dict) -> dict:
    allowed_countries = key_info.get("allowed_countries", "")
    if allowed_countries:
        req_country = params.get("country", "")
        if req_country:
            allowed = set(c.strip().upper() for c in allowed_countries.split(","))
            requested = set(c.strip().upper() for c in req_country.split(","))
            filtered = requested & allowed
            if not filtered:
                raise HTTPException(status_code=403, detail="Requested country not in your allowed list")
            params["country"] = ",".join(filtered)
        else:
            params["country"] = allowed_countries

    allowed_protocols = key_info.get("allowed_protocols", "")
    if allowed_protocols:
        params["protocol"] = allowed_protocols

    return params


# ==================== API 路由 ====================

@router.get("/proxies")
async def get_proxies(request: Request):
    """获取代理列表
    - number: 数量，可为数字或 "all"（默认 100）
    - format: "json"(默认) / "txt"(纯文本)
    - country / protocol / sort: 过滤排序
    """
    start = time.time()
    key_info = _validate_key(request)

    params = dict(request.query_params)
    params.pop("api_key", None)
    params.pop("key", None)
    output_format = params.pop("format", "txt")
    # type -> ip_type 简写映射
    if "type" in params and "ip_type" not in params:
        params["ip_type"] = params.pop("type")
    else:
        params.pop("type", None)
    params = _apply_permissions(key_info, params)

    number = params.pop("number", "")
    if number:
        params["number"] = number
    if "size" not in params and not number:
        params["number"] = "100"

    params["format"] = "json"

    # P2-9 使用 EngineClient 公共方法
    result = await engine.get_proxies_raw(params)

    elapsed = int((time.time() - start) * 1000)
    proxies = result.get("proxies", [])
    proxy_count = len(proxies)
    _log_request(key_info, request, "/api/proxies", proxy_count=proxy_count, response_ms=elapsed)

    if output_format == "json":
        return result

    # 默认 txt：纯 ip:port
    text = "\n".join(p["addr"] for p in proxies)
    return PlainTextResponse(text, media_type="text/plain; charset=utf-8")


@router.get("/proxy/random")
async def get_random_proxy(request: Request):
    """加权随机一个代理 — 支持 country/protocol 过滤"""
    start = time.time()
    key_info = _validate_key(request)

    params = {}
    country = request.query_params.get("country", "")
    protocol = request.query_params.get("protocol", "")
    if country:
        params["country"] = country
    if protocol:
        params["protocol"] = protocol
    params = _apply_permissions(key_info, params)

    # P2-9 使用 EngineClient 公共方法
    data = await engine.get_proxies_raw({
        "format": "json", "sort": "score", "size": "1",
        "country": params.get("country", ""),
        "protocol": params.get("protocol", ""),
    })
    proxies = data.get("proxies", [])
    if not proxies:
        elapsed = int((time.time() - start) * 1000)
        _log_request(key_info, request, "/api/proxy/random", status_code=503, response_ms=elapsed)
        raise HTTPException(status_code=503, detail="no proxy available")
    p = proxies[0]
    elapsed = int((time.time() - start) * 1000)
    _log_request(key_info, request, "/api/proxy/random", proxy_count=1, response_ms=elapsed)
    return {
        "proxy": p["addr"],
        "country": p.get("country", "XX"),
        "score": p.get("score", 0),
        "protocol": p.get("protocol", "http"),
        "first_seen": p.get("first_seen", ""),
        "last_success": p.get("last_success", ""),
    }


@router.get("/stats")
async def get_stats(request: Request):
    """代理池统计信息"""
    start = time.time()
    key_info = _validate_key(request)
    result = await engine.get_stats()
    elapsed = int((time.time() - start) * 1000)
    _log_request(key_info, request, "/api/stats", response_ms=elapsed)
    return result
