"""
批量执行工具

提供分批并发、单项失败隔离的通用批量处理能力。
"""

import asyncio
from typing import Any, Awaitable, Callable, Dict, List, Optional, TypeVar

from app.core.logger import logger

T = TypeVar("T")


async def run_in_batches(
    items: List[str],
    worker: Callable[[str], Awaitable[T]],
    *,
    max_concurrent: int = 10,
    batch_size: int = 50,
    on_item: Optional[Callable[[str, Dict[str, Any]], Awaitable[None]]] = None,
    should_cancel: Optional[Callable[[], bool]] = None,
) -> Dict[str, Dict[str, Any]]:
    """
    分批并发执行，单项失败不影响整体

    Args:
        items: 待处理项列表
        worker: 异步处理函数
        max_concurrent: 最大并发数
        batch_size: 每批大小

    Returns:
        {item: {"ok": bool, "data": ..., "error": ...}}
    """
    try:
        max_concurrent = int(max_concurrent)
    except Exception:
        max_concurrent = 10
    try:
        batch_size = int(batch_size)
    except Exception:
        batch_size = 50

    max_concurrent = max(1, max_concurrent)
    batch_size = max(1, batch_size)

    sem = asyncio.Semaphore(max_concurrent)

    async def _one(item: str) -> tuple[str, dict]:
        if should_cancel and should_cancel():
            return item, {"ok": False, "error": "cancelled", "cancelled": True}
        async with sem:
            try:
                data = await worker(item)
                result = {"ok": True, "data": data}
                if on_item:
                    try:
                        await on_item(item, result)
                    except Exception:
                        pass
                return item, result
            except Exception as e:
                logger.warning(f"Batch item failed: {item[:16]}... - {e}")
                result = {"ok": False, "error": str(e)}
                if on_item:
                    try:
                        await on_item(item, result)
                    except Exception:
                        pass
                return item, result

    results: Dict[str, dict] = {}

    # 分批执行，避免一次性创建所有 task
    for i in range(0, len(items), batch_size):
        if should_cancel and should_cancel():
            break
        chunk = items[i : i + batch_size]
        pairs = await asyncio.gather(*(_one(x) for x in chunk))
        results.update(dict(pairs))

    return results


__all__ = ["run_in_batches"]
