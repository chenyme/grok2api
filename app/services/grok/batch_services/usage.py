"""
Batch usage service.
"""

import asyncio
from typing import Any, Awaitable, Callable, Dict, List, Optional

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
                browser = get_config("proxy.browser")
                if browser:
                    session_ctx = ResettableSession(impersonate=browser)
                else:
                    session_ctx = ResettableSession()
                async with session_ctx as session:
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

            except Exception as e:
                # 最后一次失败已经被记录
                logger.debug(f"UsageService.get failed for token {token[:10]}...: {str(e)}")
                raise


    @staticmethod
    async def batch(
        token_refs: List[tuple[Optional[str], str]],
        mgr,
        *,
        on_item: Optional[
            Callable[[tuple[Optional[str], str], Dict[str, Any]], Awaitable[None]]
        ] = None,
        should_cancel: Optional[Callable[[], bool]] = None,
    ) -> Dict[tuple[Optional[str], str], Dict[str, Any]]:
        batch_size = get_config("usage.batch_size")
        async def _refresh_one(token_ref: tuple[Optional[str], str]):
            pool_name, token = token_ref
            return await mgr.sync_usage(
                token,
                consume_on_fail=False,
                is_usage=False,
                pool_name=pool_name,
            )

        return await run_batch(
            token_refs,
            _refresh_one,
            batch_size=batch_size,
            on_item=on_item,
            should_cancel=should_cancel,
        )


__all__ = ["UsageService"]
