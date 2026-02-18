"""
Resettable session wrapper for reverse requests.
"""

import asyncio
from typing import Any, Iterable, Optional

from curl_cffi.requests import AsyncSession

from app.core.config import get_config
from app.core.logger import logger


class ResettableSession:
    """AsyncSession wrapper that resets connection on specific HTTP status codes."""

    def __init__(
        self,
        *,
        reset_on_status: Optional[Iterable[int]] = None,
        **session_kwargs: Any,
    ):
        self._session_kwargs = dict(session_kwargs)
        config_codes = get_config("retry.reset_session_status_codes")
        if reset_on_status is None:
            reset_on_status = config_codes if config_codes is not None else [403]
        if isinstance(reset_on_status, int):
            reset_on_status = [reset_on_status]
        self._reset_on_status = (
            {int(code) for code in reset_on_status} if reset_on_status else set()
        )
        self._reset_requested = False
        self._reset_lock = asyncio.Lock()
        self._session = AsyncSession(**self._session_kwargs)

    async def _maybe_reset(self) -> None:
        if not self._reset_requested:
            return
        async with self._reset_lock:
            if not self._reset_requested:
                return
            self._reset_requested = False
            old_session = self._session
            self._session = AsyncSession(**self._session_kwargs)
            try:
                await old_session.close()
            except Exception:
                pass
            logger.debug("ResettableSession: session reset")

    async def _request(self, method: str, *args: Any, **kwargs: Any):
        await self._maybe_reset()
        response = await getattr(self._session, method)(*args, **kwargs)
        if self._reset_on_status and response.status_code in self._reset_on_status:
            self._reset_requested = True
        return response

    async def get(self, *args: Any, **kwargs: Any):
        return await self._request("get", *args, **kwargs)

    async def post(self, *args: Any, **kwargs: Any):
        return await self._request("post", *args, **kwargs)

    async def reset(self) -> None:
        self._reset_requested = True
        await self._maybe_reset()

    async def close(self) -> None:
        if self._session is None:
            return
        try:
            await self._session.close()
        finally:
            self._session = None
            self._reset_requested = False

    async def __aenter__(self) -> "ResettableSession":
        return self

    async def __aexit__(self, exc_type, exc, tb) -> None:
        await self.close()

    def __getattr__(self, name: str) -> Any:
        return getattr(self._session, name)


__all__ = ["ResettableSession"]
