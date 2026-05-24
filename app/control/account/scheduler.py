"""Background scheduler for periodic account quota refresh.

Runs one independent loop per pool type (basic / super / heavy), each with
its own configurable interval read from:

    account.refresh.basic_interval_sec  (default 86400 — 24 h)
    account.refresh.super_interval_sec  (default  7200 —  2 h)
    account.refresh.heavy_interval_sec  (default  7200 —  2 h)
"""

import asyncio

from app.platform.config.snapshot import get_config
from app.platform.logging.logger import logger
from .refresh import AccountRefreshService

# Pool → (config key, built-in default seconds)
_POOL_CONFIG: dict[str, tuple[str, int]] = {
    "basic": ("account.refresh.basic_interval_sec", 86_400),
    "lite": ("account.refresh.super_interval_sec",  7_200),
    "super": ("account.refresh.super_interval_sec",  7_200),
    "heavy": ("account.refresh.heavy_interval_sec",  7_200),
}


def _interval(pool: str) -> int:
    key, default = _POOL_CONFIG[pool]
    v = get_config(key, None)
    return int(v) if v is not None else default


class AccountRefreshScheduler:
    """Runs one refresh loop per pool type at pool-specific intervals.

    Lifecycle:  ``start()`` → loops run in background → ``stop()`` to cancel.
    """

    def __init__(self, refresh_service: AccountRefreshService) -> None:
        self._service = refresh_service
        self._tasks:  list[asyncio.Task] = []
        self._stop    = asyncio.Event()

    def bind_service(self, refresh_service: AccountRefreshService) -> None:
        """Update the refresh service used by the singleton scheduler."""
        self._service = refresh_service

    def is_running(self) -> bool:
        """Return True while any pool refresh loop is still active."""
        return any(not task.done() for task in self._tasks)

    def start(self) -> None:
        if self.is_running():
            return
        self._stop.clear()
        self._tasks = [
            asyncio.create_task(self._loop(pool), name=f"account-refresh-{pool}")
            for pool in _POOL_CONFIG
        ]
        intervals = {p: _interval(p) for p in _POOL_CONFIG}
        logger.info(
            "account refresh scheduler started: basic_interval_s={} super_interval_s={} heavy_interval_s={}",
            intervals["basic"], intervals["super"], intervals["heavy"],
        )

    def stop(self) -> None:
        was_running = self.is_running()
        self._stop.set()
        for t in self._tasks:
            if not t.done():
                t.cancel()
        self._tasks = []
        if was_running:
            logger.info("account refresh scheduler stopped")

    async def _loop(self, pool: str) -> None:
        while not self._stop.is_set():
            interval = _interval(pool)
            try:
                await asyncio.wait_for(self._stop.wait(), timeout=float(interval))
                break  # stop event fired
            except asyncio.TimeoutError:
                pass

            if self._stop.is_set():
                break

            try:
                result = await self._service.refresh_scheduled(pool=pool)
                logger.info(
                    "account refresh cycle completed: pool={} checked={} refreshed={} recovered={} failed={}",
                    pool, result.checked, result.refreshed, result.recovered, result.failed,
                )
            except asyncio.CancelledError:
                raise
            except Exception as exc:
                logger.error(
                    "account refresh cycle failed: pool={} error_type={} error={}",
                    pool,
                    type(exc).__name__,
                    exc,
                )


# ---------------------------------------------------------------------------
# Module-level singleton
# ---------------------------------------------------------------------------

_scheduler: AccountRefreshScheduler | None = None


def get_account_refresh_scheduler(
    refresh_service: AccountRefreshService,
) -> AccountRefreshScheduler:
    global _scheduler
    if _scheduler is None:
        _scheduler = AccountRefreshScheduler(refresh_service)
    else:
        _scheduler.bind_service(refresh_service)
    return _scheduler


__all__ = ["AccountRefreshScheduler", "get_account_refresh_scheduler"]
