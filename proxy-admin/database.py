"""管理后台数据库 — API Key 管理 + 用量统计
优化：连接池化 + 配额缓存 + 日志归档"""
import sqlite3
import secrets
import json
import time
import threading
from datetime import datetime, timedelta
from typing import Optional, List, Dict, Any
from collections import defaultdict


DB_PATH = "admin.db"
LOG_RETENTION_DAYS = 90  # 日志保留天数

# ==================== 连接池（单例 + Lock）====================

_db_lock = threading.Lock()
_db_conn: Optional[sqlite3.Connection] = None


def get_db() -> sqlite3.Connection:
    """获取共享数据库连接（线程安全单例）"""
    global _db_conn
    if _db_conn is None:
        with _db_lock:
            if _db_conn is None:
                _db_conn = sqlite3.connect(DB_PATH, check_same_thread=False)
                _db_conn.row_factory = sqlite3.Row
                _db_conn.execute("PRAGMA journal_mode=WAL")
                _db_conn.execute("PRAGMA busy_timeout=5000")
    return _db_conn


def _execute(sql: str, params=(), commit=False):
    """线程安全执行 SQL"""
    conn = get_db()
    with _db_lock:
        cursor = conn.execute(sql, params)
        if commit:
            conn.commit()
        return cursor


def _executescript(sql: str):
    """线程安全批量执行"""
    conn = get_db()
    with _db_lock:
        conn.executescript(sql)


def _fetchone(sql: str, params=()):
    conn = get_db()
    with _db_lock:
        return conn.execute(sql, params).fetchone()


def _fetchall(sql: str, params=()):
    conn = get_db()
    with _db_lock:
        return conn.execute(sql, params).fetchall()


def init_db():
    """初始化数据库表 + 清理过期日志"""
    _executescript("""
    CREATE TABLE IF NOT EXISTS api_keys (
        id              INTEGER PRIMARY KEY AUTOINCREMENT,
        key             TEXT UNIQUE NOT NULL,
        name            TEXT NOT NULL,
        quota_daily     INTEGER DEFAULT 0,
        quota_monthly   INTEGER DEFAULT 0,
        allowed_countries TEXT DEFAULT '',
        allowed_protocols TEXT DEFAULT '',
        is_active       INTEGER DEFAULT 1,
        created_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
        expires_at      DATETIME
    );

    CREATE TABLE IF NOT EXISTS api_logs (
        id              INTEGER PRIMARY KEY AUTOINCREMENT,
        key_id          INTEGER NOT NULL,
        api_key         TEXT NOT NULL,
        endpoint        TEXT NOT NULL,
        client_ip       TEXT,
        user_agent      TEXT,
        query_params    TEXT,
        proxy_count     INTEGER DEFAULT 0,
        response_ms     INTEGER DEFAULT 0,
        status_code     INTEGER DEFAULT 200,
        created_at      DATETIME DEFAULT CURRENT_TIMESTAMP
    );

    CREATE INDEX IF NOT EXISTS idx_logs_key ON api_logs(key_id, created_at);
    CREATE INDEX IF NOT EXISTS idx_logs_time ON api_logs(created_at);
    CREATE INDEX IF NOT EXISTS idx_keys_key ON api_keys(key);
    """)
    # #13 日志归档：启动时清理过期日志
    _cleanup_old_logs()


def _cleanup_old_logs():
    """清理超过 LOG_RETENTION_DAYS 天的日志"""
    cutoff = (datetime.now() - timedelta(days=LOG_RETENTION_DAYS)).strftime("%Y-%m-%d")
    _execute("DELETE FROM api_logs WHERE created_at < ?", (cutoff,), commit=True)
    row = _fetchone("SELECT changes() as cnt")
    cnt = row["cnt"] if row else 0
    if cnt > 0:
        print(f"[DB] 已清理 {cnt} 条过期日志（>{LOG_RETENTION_DAYS} 天）")


# ==================== #5 配额缓存 ====================

class _QuotaCache:
    """内存配额计数器 — 避免每次请求查表"""

    def __init__(self):
        self._lock = threading.Lock()
        self._daily: Dict[int, int] = defaultdict(int)    # key_id → 当日请求数
        self._monthly: Dict[int, int] = defaultdict(int)  # key_id → 当月请求数
        self._current_day = ""
        self._current_month = ""

    def _check_reset(self):
        """日/月切换时重置计数器"""
        today = datetime.now().strftime("%Y-%m-%d")
        month = datetime.now().strftime("%Y-%m")
        if today != self._current_day:
            self._daily.clear()
            self._current_day = today
        if month != self._current_month:
            self._monthly.clear()
            self._current_month = month

    def increment(self, key_id: int):
        """请求成功后 +1"""
        with self._lock:
            self._check_reset()
            self._daily[key_id] += 1
            self._monthly[key_id] += 1

    def check_quota(self, key_id: int, quota_daily: int, quota_monthly: int) -> bool:
        """检查是否超配额，True=通过"""
        with self._lock:
            self._check_reset()
            if quota_daily > 0 and self._daily[key_id] >= quota_daily:
                return False
            if quota_monthly > 0 and self._monthly[key_id] >= quota_monthly:
                return False
            return True

    def warm_up(self, key_id: int):
        """启动时从数据库预热计数"""
        with self._lock:
            self._check_reset()
            today = datetime.now().strftime("%Y-%m-%d")
            month_start = datetime.now().strftime("%Y-%m-01")
            row = _fetchone(
                "SELECT COUNT(*) as cnt FROM api_logs WHERE key_id = ? AND created_at >= ?",
                (key_id, today)
            )
            self._daily[key_id] = row["cnt"] if row else 0
            row = _fetchone(
                "SELECT COUNT(*) as cnt FROM api_logs WHERE key_id = ? AND created_at >= ?",
                (key_id, month_start)
            )
            self._monthly[key_id] = row["cnt"] if row else 0


_quota_cache = _QuotaCache()


# ==================== API Key 管理 ====================

class ApiKeyManager:
    """API Key CRUD + 鉴权 + 配额"""

    @staticmethod
    def create_key(name: str, quota_daily: int = 0, quota_monthly: int = 0,
                   allowed_countries: str = "", allowed_protocols: str = "",
                   expires_at: Optional[str] = None) -> Dict[str, Any]:
        key = "pk_" + secrets.token_hex(16)
        _execute(
            """INSERT INTO api_keys (key, name, quota_daily, quota_monthly,
               allowed_countries, allowed_protocols, expires_at)
               VALUES (?, ?, ?, ?, ?, ?, ?)""",
            (key, name, quota_daily, quota_monthly, allowed_countries, allowed_protocols, expires_at),
            commit=True
        )
        return {"key": key, "name": name, "quota_daily": quota_daily, "quota_monthly": quota_monthly}

    @staticmethod
    def list_keys() -> List[Dict[str, Any]]:
        rows = _fetchall("SELECT * FROM api_keys ORDER BY created_at DESC")
        return [dict(r) for r in rows]

    @staticmethod
    def get_key_by_value(key_value: str) -> Optional[Dict[str, Any]]:
        row = _fetchone("SELECT * FROM api_keys WHERE key = ?", (key_value,))
        return dict(row) if row else None

    @staticmethod
    def update_key(key_id: int, **kwargs) -> bool:
        allowed = {"name", "quota_daily", "quota_monthly", "allowed_countries",
                    "allowed_protocols", "is_active", "expires_at"}
        updates = {k: v for k, v in kwargs.items() if k in allowed}
        if not updates:
            return False
        # 安全：列名来自白名单 allowed，不存在注入风险
        set_clause = ", ".join(f"{k} = ?" for k in updates)
        values = list(updates.values()) + [key_id]
        _execute(f"UPDATE api_keys SET {set_clause} WHERE id = ?", values, commit=True)
        return True

    @staticmethod
    def delete_key(key_id: int) -> bool:
        _execute("DELETE FROM api_keys WHERE id = ?", (key_id,), commit=True)
        _execute("DELETE FROM api_logs WHERE key_id = ?", (key_id,), commit=True)
        return True

    @staticmethod
    def validate_key(key_value: str) -> Optional[Dict[str, Any]]:
        """验证 Key（活跃 + 未过期 + 未超配额）— 使用内存缓存"""
        key_info = ApiKeyManager.get_key_by_value(key_value)
        if not key_info:
            return None
        if not key_info["is_active"]:
            return None
        if key_info["expires_at"]:
            try:
                exp = datetime.fromisoformat(key_info["expires_at"])
                if exp < datetime.now():
                    return None
            except (ValueError, TypeError):
                pass

        # #5 配额检查：内存缓存，不查数据库
        if not _quota_cache.check_quota(key_info["id"], key_info["quota_daily"], key_info["quota_monthly"]):
            return None

        return key_info

    @staticmethod
    def get_key_usage(key_id: int) -> Dict[str, Any]:
        today = datetime.now().strftime("%Y-%m-%d")
        month_start = datetime.now().strftime("%Y-%m-01")
        today_cnt = _fetchone(
            "SELECT COUNT(*) as cnt FROM api_logs WHERE key_id = ? AND created_at >= ?",
            (key_id, today)
        )["cnt"]
        month_cnt = _fetchone(
            "SELECT COUNT(*) as cnt FROM api_logs WHERE key_id = ? AND created_at >= ?",
            (key_id, month_start)
        )["cnt"]
        total_cnt = _fetchone(
            "SELECT COUNT(*) as cnt FROM api_logs WHERE key_id = ?",
            (key_id,)
        )["cnt"]
        return {"today": today_cnt, "month": month_cnt, "total": total_cnt}


# ==================== 用量日志（异步批量写入） ====================

class _LogBuffer:
    """异步日志缓冲：内存队列 + 后台线程批量写入"""

    def __init__(self, flush_interval: float = 3.0):
        self._buffer: list = []
        self._lock = threading.Lock()
        self._flush_interval = flush_interval
        self._running = True
        self._thread = threading.Thread(target=self._loop, daemon=True)
        self._thread.start()

    def append(self, record: tuple):
        with self._lock:
            self._buffer.append(record)

    def flush(self):
        """强制刷新缓冲区"""
        with self._lock:
            batch = self._buffer[:]
            self._buffer.clear()
        if not batch:
            return
        conn = get_db()
        with _db_lock:
            conn.executemany(
                """INSERT INTO api_logs
                   (key_id, api_key, endpoint, client_ip, user_agent, query_params,
                    proxy_count, response_ms, status_code)
                   VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)""",
                batch,
            )
            conn.commit()

    def _loop(self):
        while self._running:
            time.sleep(self._flush_interval)
            try:
                self.flush()
            except Exception:
                pass

    def stop(self):
        self._running = False
        self.flush()


_log_buffer = _LogBuffer(flush_interval=3.0)


class ApiLogger:

    @staticmethod
    def log(key_id: int, api_key: str, endpoint: str, client_ip: str,
            user_agent: str, query_params: dict, proxy_count: int = 0,
            response_ms: int = 0, status_code: int = 200):
        # P1-8 异步批量写入：先进内存队列
        _log_buffer.append((
            key_id, api_key, endpoint, client_ip, user_agent,
            json.dumps(query_params, ensure_ascii=False),
            proxy_count, response_ms, status_code,
        ))
        # #5 配额缓存 +1（保持同步，确保限速实时生效）
        _quota_cache.increment(key_id)

    @staticmethod
    def get_recent_logs(key_id: int = 0, limit: int = 100) -> List[Dict[str, Any]]:
        if key_id > 0:
            rows = _fetchall(
                "SELECT * FROM api_logs WHERE key_id = ? ORDER BY created_at DESC LIMIT ?",
                (key_id, limit)
            )
        else:
            rows = _fetchall(
                "SELECT * FROM api_logs ORDER BY created_at DESC LIMIT ?",
                (limit,)
            )
        return [dict(r) for r in rows]

    @staticmethod
    def get_daily_stats(key_id: int = 0, days: int = 30) -> List[Dict[str, Any]]:
        start = (datetime.now() - timedelta(days=days)).strftime("%Y-%m-%d")
        if key_id > 0:
            rows = _fetchall(
                """SELECT DATE(created_at) as date, COUNT(*) as requests,
                   SUM(proxy_count) as proxies, AVG(response_ms) as avg_ms
                   FROM api_logs WHERE key_id = ? AND created_at >= ?
                   GROUP BY DATE(created_at) ORDER BY date""",
                (key_id, start)
            )
        else:
            rows = _fetchall(
                """SELECT DATE(created_at) as date, COUNT(*) as requests,
                   SUM(proxy_count) as proxies, AVG(response_ms) as avg_ms
                   FROM api_logs WHERE created_at >= ?
                   GROUP BY DATE(created_at) ORDER BY date""",
                (start,)
            )
        return [dict(r) for r in rows]

    @staticmethod
    def get_csv_data(key_id: int = 0, days: int = 30) -> str:
        logs = ApiLogger.get_recent_logs(key_id, limit=10000)
        lines = ["id,api_key,endpoint,client_ip,user_agent,proxy_count,response_ms,status_code,created_at"]
        for l in logs:
            lines.append(f"{l['id']},{l['api_key']},{l['endpoint']},{l['client_ip']},"
                         f"\"{l['user_agent']}\",{l['proxy_count']},{l['response_ms']},"
                         f"{l['status_code']},{l['created_at']}")
        return "\n".join(lines)
