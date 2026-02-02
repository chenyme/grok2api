"""Token 刷新调度器"""

import asyncio
from typing import Optional

from app.core.logger import logger
from app.core.storage import get_storage, StorageError, RedisStorage
from app.services.token.manager import get_token_manager


class TokenRefreshScheduler:
    """Token 自动刷新调度器"""
    
    def __init__(self, interval_hours: int = 8):
        self.interval_hours = interval_hours
        self.interval_seconds = interval_hours * 3600
        self._task: Optional[asyncio.Task] = None
        self._running = False
    
    async def _refresh_loop(self):
        """刷新循环"""
        logger.info(f"Scheduler: started (interval: {self.interval_hours}h)")
        
        while self._running:
            try:
                await asyncio.sleep(self.interval_seconds)
                storage = get_storage()
                lock_acquired = False
                lock = None

                if isinstance(storage, RedisStorage):
                    # Redis: non-blocking lock to avoid multi-worker duplication
                    lock_key = "grok2api:lock:token_refresh"
                    lock = storage.redis.lock(lock_key, timeout=self.interval_seconds + 60, blocking_timeout=0)
                    lock_acquired = await lock.acquire(blocking=False)
                else:
                    try:
                        async with storage.acquire_lock("token_refresh", timeout=0):
                            lock_acquired = True
                    except StorageError:
                        lock_acquired = False

                if not lock_acquired:
                    logger.info("Scheduler: skipped (lock not acquired)")
                    continue

                try:
                    logger.info("Scheduler: starting token refresh...")
                    manager = await get_token_manager()
                    result = await manager.refresh_cooling_tokens()
                    
                    logger.info(
                        f"Scheduler: refresh completed - "
                        f"checked={result['checked']}, "
                        f"refreshed={result['refreshed']}, "
                        f"recovered={result['recovered']}, "
                        f"expired={result['expired']}"
                    )
                finally:
                    if lock is not None and lock_acquired:
                        try:
                            await lock.release()
                        except Exception:
                            pass
                
            except asyncio.CancelledError:
                break
            except Exception as e:
                logger.error(f"Scheduler: refresh error - {e}")
    
    def start(self):
        """启动调度器"""
        if self._running:
            logger.warning("Scheduler: already running")
            return
        
        self._running = True
        self._task = asyncio.create_task(self._refresh_loop())
        logger.info("Scheduler: enabled")
    
    def stop(self):
        """停止调度器"""
        if not self._running:
            return
        
        self._running = False
        if self._task:
            self._task.cancel()
        logger.info("Scheduler: stopped")


# 全局单例
_scheduler: Optional[TokenRefreshScheduler] = None


def get_scheduler(interval_hours: int = 8) -> TokenRefreshScheduler:
    """获取调度器单例"""
    global _scheduler
    if _scheduler is None:
        _scheduler = TokenRefreshScheduler(interval_hours)
    return _scheduler


__all__ = ["TokenRefreshScheduler", "get_scheduler"]
