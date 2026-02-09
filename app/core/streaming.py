"""
Streaming helpers for SSE keepalive.
"""

from __future__ import annotations

import asyncio
from typing import AsyncGenerator, TypeVar

T = TypeVar("T")


async def with_keepalive(
    stream: AsyncGenerator[T, None],
    interval: float,
    *,
    ping_message: T,
) -> AsyncGenerator[T, None]:
    """
    Wrap an async generator and emit keepalive messages if idle.

    Args:
        stream: original async generator.
        interval: keepalive interval in seconds (<=0 disables).
        ping_message: message to emit on idle.
    """
    if interval <= 0:
        async for item in stream:
            yield item
        return

    iterator = stream.__aiter__()
    while True:
        try:
            item = await asyncio.wait_for(iterator.__anext__(), timeout=interval)
            yield item
        except asyncio.TimeoutError:
            yield ping_message
        except StopAsyncIteration:
            break
