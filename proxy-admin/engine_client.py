"""封装与 Go 引擎 /internal/* 的 HTTP 通信
优化：全局 AsyncClient 复用 TCP 连接"""
from typing import List
import httpx
from config import ENGINE_URL, INTERNAL_KEY


class EngineClient:
    def __init__(self, base_url: str = ENGINE_URL, key: str = INTERNAL_KEY):
        self.base = base_url.rstrip("/")
        self.headers = {"X-Internal-Key": key}
        # #6 全局复用 AsyncClient（HTTP keep-alive + 连接池）
        self._client: httpx.AsyncClient | None = None

    def _get_client(self) -> httpx.AsyncClient:
        if self._client is None or self._client.is_closed:
            self._client = httpx.AsyncClient(
                timeout=15,
                limits=httpx.Limits(max_keepalive_connections=5, max_connections=10),
            )
        return self._client

    async def _get(self, path: str, params: dict = None) -> dict:
        r = await self._get_client().get(
            f"{self.base}{path}", headers=self.headers, params=params
        )
        r.raise_for_status()
        return r.json()

    async def _post(self, path: str, json_data: dict = None) -> dict:
        r = await self._get_client().post(
            f"{self.base}{path}", headers=self.headers, json=json_data
        )
        r.raise_for_status()
        return r.json()

    async def _put(self, path: str, json_data: dict = None) -> dict:
        r = await self._get_client().put(
            f"{self.base}{path}", headers=self.headers, json=json_data
        )
        r.raise_for_status()
        return r.json()

    async def close(self):
        if self._client and not self._client.is_closed:
            await self._client.aclose()

    # ===== 配置管理 =====
    async def get_config(self) -> dict:
        return await self._get("/internal/config")

    async def update_config(self, data: dict) -> dict:
        return await self._put("/internal/config", data)

    # ===== 引擎状态 =====
    async def get_stats(self) -> dict:
        return await self._get("/internal/stats")

    # ===== 任务控制 =====
    async def trigger_task(self) -> dict:
        return await self._post("/internal/task/trigger")

    async def cancel_task(self) -> dict:
        return await self._post("/internal/task/cancel")

    async def get_task_status(self) -> dict:
        return await self._get("/internal/task/status")

    # ===== 代理管理 =====
    async def get_proxies(self, **kwargs) -> dict:
        return await self._get("/internal/proxies", params={**kwargs, "format": "json"})

    async def blacklist_addrs(self, addrs: List[str]) -> dict:
        return await self._post("/internal/proxies/blacklist", {"addrs": addrs, "action": "blacklist"})

    async def revive_blacklist(self) -> dict:
        return await self._post("/internal/proxies/blacklist", {"action": "revive"})

    # ===== URL 源管理 =====
    async def get_urls(self) -> dict:
        return await self._get("/internal/urls")

    async def update_urls(self, urls: List[str]) -> dict:
        return await self._put("/internal/urls", {"urls": urls})

    # ===== 代理源质量 =====
    async def get_source_stats(self) -> dict:
        return await self._get("/internal/sources")

    # ===== 历史趋势 =====
    async def get_daily_stats(self, days: int = 30) -> dict:
        return await self._get("/internal/stats/daily", params={"days": days})

    # ===== Telegram 测试 =====
    async def test_telegram(self) -> dict:
        return await self._get("/internal/tg/test")

    # ===== 直通查询（带自定义参数） =====
    async def get_proxies_raw(self, params: dict) -> dict:
        """直接传递查询参数到 /internal/proxies（供 api_routes 使用）"""
        r = await self._get_client().get(
            f"{self.base}/internal/proxies",
            headers=self.headers, params=params, timeout=30,
        )
        r.raise_for_status()
        return r.json()

    async def add_urls(self, urls: list) -> dict:
        """批量添加源 URL"""
        return await self._post("/internal/urls", {"urls": urls})

    async def delete_urls(self, urls: list) -> dict:
        """批量删除源 URL"""
        r = await self._get_client().request(
            "DELETE", f"{self.base}/internal/urls",
            headers=self.headers, json={"urls": urls}, timeout=10,
        )
        r.raise_for_status()
        return r.json()


# 全局实例
engine = EngineClient()
