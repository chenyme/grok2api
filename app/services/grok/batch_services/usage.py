"""
Batch usage service.
"""

import asyncio
from typing import Callable, Awaitable, Dict, Any, Optional, List

from app.core.logger import logger
from app.core.config import get_config
from app.services.reverse.rate_limits import RateLimitsReverse
from app.services.reverse.utils.session import ResettableSession
from app.core.batch import run_batch

_USAGE_SEMAPHORE = None
_USAGE_SEM_VALUE = None


def _get_usage_semaphore() -> asyncio.Semaphore:
    value = max(1, int(get_config("usage.concurrent")))
    global _USAGE_SEMAPHORE, _USAGE_SEM_VALUE
    if _USAGE_SEMAPHORE is None or value != _USAGE_SEM_VALUE:
        _USAGE_SEM_VALUE = value
        _USAGE_SEMAPHORE = asyncio.Semaphore(value)
    return _USAGE_SEMAPHORE


class UsageService:
    """用量查询服务"""

    async def get(self, token: str) -> Dict:
        """
        获取速率限制信息

        Args:
            token: 认证 Token

        Returns:
            响应数据

        Raises:
            UpstreamException: 当获取失败且重试耗尽时
        """
        async with _get_usage_semaphore():
            try:
                async with ResettableSession() as session:
                    response = await RateLimitsReverse.request(session, token)
                data = response.json()
                remaining = data.get("remainingTokens")
                if remaining is None:
                    remaining = data.get("remainingQueries")
                    if remaining is not None:
                        data["remainingTokens"] = remaining
                logger.info(
                    f"Usage sync success: remaining={remaining}, token={token[:10]}..."
                )
                return data

            except Exception:
                # 最后一次失败已经被记录
                raise


    @staticmethod
    async def batch(
        tokens: List[str],
        mgr,
        *,
        on_item: Optional[Callable[[str, Dict[str, Any]], Awaitable[None]]] = None,
        should_cancel: Optional[Callable[[], bool]] = None,
    ) -> Dict[str, Dict[str, Any]]:
        batch_size = get_config("usage.batch_size")
        async def _refresh_one(t: str):
            return await mgr.sync_usage(t, consume_on_fail=False, is_usage=False)

        return await run_batch(
            tokens,
            _refresh_one,
            batch_size=batch_size,
            on_item=on_item,
            should_cancel=should_cancel,
        )


__all__ = ["UsageService"]
