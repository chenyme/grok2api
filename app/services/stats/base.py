"""统计持久化基类 — 消除 RequestStats/RequestLogger 重复代码"""

import asyncio
from abc import ABC, abstractmethod
from pathlib import Path
from typing import Any, Awaitable, Optional

import orjson

from app.core.logger import logger

# 统一数据目录
DATA_DIR = Path(__file__).parents[2] / "data"


class AsyncJsonStore(ABC):
    """异步 JSON 持久化基类

    子类需实现:
        _serialize() -> Any      内部状态 → JSON 可序列化对象
        _deserialize(data: Any)  JSON 对象 → 恢复内部状态
    """

    def __init__(self, filename: str, flush_interval: float = 2.0):
        self.file_path = DATA_DIR / filename
        self._lock = asyncio.Lock()
        self._loaded = False
        self._save_task: asyncio.Task | None = None
        self._dirty = False
        self._flush_interval = flush_interval

    # ── 子类必须实现 ──

    @abstractmethod
    def _serialize(self) -> Any:
        """将内部数据转为可 JSON 序列化的对象"""

    @abstractmethod
    def _deserialize(self, data: Any) -> None:
        """从 JSON 数据恢复内部状态"""

    # ── 持久化 ──

    async def _load_data(self):
        """从磁盘加载数据"""
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
                    self._deserialize(data)
                    self._loaded = True
                    logger.debug(f"[{type(self).__name__}] 加载数据成功")
        except Exception as e:
            logger.error(f"[{type(self).__name__}] 加载数据失败: {e}")
            self._loaded = True

    async def _save_data(self):
        """保存数据到磁盘"""
        if not self._loaded:
            return

        try:
            self.file_path.parent.mkdir(parents=True, exist_ok=True)
            async with self._lock:
                content = orjson.dumps(self._serialize())
                await asyncio.to_thread(self.file_path.write_bytes, content)
        except Exception as e:
            logger.error(f"[{type(self).__name__}] 保存数据失败: {e}")

    # ── 节流保存 ──

    def _spawn_task(self, coro: Awaitable) -> Optional[asyncio.Task]:
        """安全创建后台任务"""
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


__all__ = ["AsyncJsonStore", "DATA_DIR"]
