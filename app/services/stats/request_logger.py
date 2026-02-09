"""请求日志审计 - 记录近期请求"""

import time
import asyncio
import orjson
from typing import List, Dict, Deque, Awaitable, Optional
from collections import deque
from pathlib import Path

from app.core.logger import logger
from app.core.config import get_config


class RequestLogger:
    """请求日志记录器"""

    _instance = None

    def __new__(cls):
        if cls._instance is None:
            cls._instance = super().__new__(cls)
        return cls._instance

    def __init__(self, max_len: int = 1000):
        if hasattr(self, "_initialized"):
            return

        # 数据文件路径: app/data/logs.json
        self.file_path = Path(__file__).parents[2] / "data" / "logs.json"
        self._logs: Deque[Dict] = deque(maxlen=max_len)
        self._lock = asyncio.Lock()
        self._loaded = False
        self._save_task: asyncio.Task | None = None
        self._dirty = False
        self._flush_interval = 2.0

        self._initialized = True

    async def init(self):
        """初始化加载数据"""
        self._apply_config()
        if not self._loaded:
            await self._load_data()

    async def _load_data(self):
        """从磁盘加载日志数据"""
        if self._loaded:
            return

        if not self.file_path.exists():
            self._loaded = True
            return

        try:
            async with self._lock:
                content = await asyncio.to_thread(self.file_path.read_bytes)
                if content:
                    data = orjson.loads(content)
                    if isinstance(data, list):
                        self._logs.clear()
                        self._logs.extend(data)
                    self._loaded = True
                    logger.debug(f"[Logger] 加载日志成功: {len(self._logs)} 条")
        except Exception as e:
            logger.error(f"[Logger] 加载日志失败: {e}")
            self._loaded = True

    def _apply_config(self) -> None:
        """应用配置参数"""
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

    async def _save_data(self):
        """保存日志数据到磁盘"""
        if not self._loaded:
            return

        try:
            # 确保目录存在
            self.file_path.parent.mkdir(parents=True, exist_ok=True)

            async with self._lock:
                # 转换为列表保存
                content = orjson.dumps(list(self._logs))
                await asyncio.to_thread(self.file_path.write_bytes, content)
        except Exception as e:
            logger.error(f"[Logger] 保存日志失败: {e}")

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
        """添加日志"""
        if not self._loaded:
            await self.init()
        else:
            self._apply_config()

        try:
            now = time.time()
            # 格式化时间
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
                self._logs.appendleft(log)  # 最新的在前

            # 异步保存（节流）
            self._schedule_save()

        except Exception as e:
            logger.error(f"[Logger] 记录日志失败: {e}")

    async def get_logs(self, limit: int = 1000) -> List[Dict]:
        """获取日志"""
        async with self._lock:
            return list(self._logs)[:limit]

    async def clear_logs(self):
        """清空日志"""
        async with self._lock:
            self._logs.clear()
        await self._save_data()

    def _spawn_task(self, coro: Awaitable) -> Optional[asyncio.Task]:
        """安全创建后台任务；无运行中事件循环时静默跳过。"""
        try:
            loop = asyncio.get_running_loop()
        except RuntimeError:
            try:
                coro.close()
            except Exception:
                pass
            return None

        try:
            return loop.create_task(coro)
        except RuntimeError:
            try:
                coro.close()
            except Exception:
                pass
            return None

    def _schedule_save(self) -> None:
        if not self._loaded:
            return
        if self._flush_interval <= 0:
            self._spawn_task(self._save_data())
            return
        self._dirty = True
        if self._save_task and not self._save_task.done():
            return
        self._save_task = self._spawn_task(self._flush_loop())

    async def _flush_loop(self) -> None:
        try:
            while True:
                await asyncio.sleep(self._flush_interval)
                if not self._dirty:
                    break
                self._dirty = False
                await self._save_data()
        finally:
            self._save_task = None
            if self._dirty:
                self._schedule_save()


# 全局实例
request_logger = RequestLogger()
