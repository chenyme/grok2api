"""请求日志审计 - 记录近期请求"""

import time
from typing import List, Dict, Any
from collections import deque

from app.core.config import get_config
from .base import AsyncJsonStore


class RequestLogger(AsyncJsonStore):
    """请求日志记录器（单例）"""

    _instance = None

    def __new__(cls):
        if cls._instance is None:
            cls._instance = super().__new__(cls)
        return cls._instance

    def __init__(self, max_len: int = 1000):
        if hasattr(self, "_initialized"):
            return

        super().__init__("logs.json")

        self._logs: deque[Dict] = deque(maxlen=max_len)
        self._initialized = True

    # ── AsyncJsonStore 接口 ──

    def _serialize(self) -> Any:
        return list(self._logs)

    def _deserialize(self, data: Any) -> None:
        if isinstance(data, list):
            self._logs.clear()
            self._logs.extend(data)

    # ── 配置 ──

    def _apply_config(self) -> None:
        max_len = get_config("stats.log_max_entries", 1000)
        flush_interval = get_config("stats.flush_interval_sec", 2)
        try:
            max_len = int(max_len)
        except Exception:
            max_len = 1000
        try:
            flush_interval = float(flush_interval)
        except Exception:
            flush_interval = 2.0
        max_len = max(1, max_len)
        if self._logs.maxlen != max_len:
            self._logs = deque(list(self._logs), maxlen=max_len)
        self._flush_interval = max(0.0, flush_interval)

    async def init(self):
        self._apply_config()
        if not self._loaded:
            await self._load_data()

    # ── 业务逻辑 ──

    async def add_log(
        self,
        ip: str,
        model: str,
        duration: float,
        status: int,
        key_name: str,
        token_suffix: str = "",
        error: str = "",
    ):
        if not self._loaded:
            await self.init()
        else:
            self._apply_config()

        try:
            now = time.time()
            time_str = time.strftime("%Y-%m-%d %H:%M:%S", time.localtime(now))

            log = {
                "id": str(int(now * 1000)),
                "time": time_str,
                "timestamp": now,
                "ip": ip,
                "model": model,
                "duration": round(duration, 2),
                "status": status,
                "key_name": key_name,
                "token_suffix": token_suffix,
                "error": error,
            }

            async with self._lock:
                self._logs.appendleft(log)

            self._schedule_save()

        except Exception as e:
            from app.core.logger import logger

            logger.error(f"[Logger] 记录日志失败: {e}")

    async def get_logs(self, limit: int = 1000) -> List[Dict]:
        async with self._lock:
            return list(self._logs)[:limit]

    async def clear_logs(self):
        async with self._lock:
            self._logs.clear()
        await self._save_data()


# 全局实例
request_logger = RequestLogger()
