"""
Batch task manager for admin batch operations (SSE progress).
"""

from __future__ import annotations

import asyncio
import time
import uuid
from typing import Any, Dict, List, Optional


class BatchTask:
    def __init__(self, total: int):
        self.id = uuid.uuid4().hex
        self.total = int(total)
        self.processed = 0
        self.ok = 0
        self.fail = 0
        self.status = "running"
        self.warning: Optional[str] = None
        self.result: Optional[Dict[str, Any]] = None
        self.error: Optional[str] = None
        self.created_at = time.time()
        self._queues: List[asyncio.Queue] = []
        self._final_event: Optional[Dict[str, Any]] = None
        self.cancelled = False

    def snapshot(self) -> Dict[str, Any]:
        return {
            "task_id": self.id,
            "status": self.status,
            "total": self.total,
            "processed": self.processed,
            "ok": self.ok,
            "fail": self.fail,
            "warning": self.warning,
        }

    def attach(self) -> asyncio.Queue:
        q: asyncio.Queue = asyncio.Queue(maxsize=200)
        self._queues.append(q)
        return q

    def detach(self, q: asyncio.Queue) -> None:
        if q in self._queues:
            self._queues.remove(q)

    def _publish(self, event: Dict[str, Any]) -> None:
        for q in list(self._queues):
            try:
                q.put_nowait(event)
            except Exception:
                # Drop if queue is full or closed
                pass

    def record(
        self, ok: bool, *, item: Any = None, detail: Any = None, error: str = ""
    ) -> None:
        self.processed += 1
        if ok:
            self.ok += 1
        else:
            self.fail += 1
        event: Dict[str, Any] = {
            "type": "progress",
            "task_id": self.id,
            "total": self.total,
            "processed": self.processed,
            "ok": self.ok,
            "fail": self.fail,
        }
        if item is not None:
            event["item"] = item
        if detail is not None:
            event["detail"] = detail
        if error:
            event["error"] = error
        self._publish(event)

    def finish(self, result: Dict[str, Any], *, warning: Optional[str] = None) -> None:
        self.status = "done"
        self.result = result
        self.warning = warning
        event = {
            "type": "done",
            "task_id": self.id,
            "total": self.total,
            "processed": self.processed,
            "ok": self.ok,
            "fail": self.fail,
            "warning": self.warning,
            "result": result,
        }
        self._final_event = event
        self._publish(event)

    def fail_task(self, error: str) -> None:
        self.status = "error"
        self.error = error
        event = {
            "type": "error",
            "task_id": self.id,
            "total": self.total,
            "processed": self.processed,
            "ok": self.ok,
            "fail": self.fail,
            "error": error,
        }
        self._final_event = event
        self._publish(event)

    def cancel(self) -> None:
        self.cancelled = True

    def finish_cancelled(self) -> None:
        self.status = "cancelled"
        event = {
            "type": "cancelled",
            "task_id": self.id,
            "total": self.total,
            "processed": self.processed,
            "ok": self.ok,
            "fail": self.fail,
        }
        self._final_event = event
        self._publish(event)

    def final_event(self) -> Optional[Dict[str, Any]]:
        return self._final_event


_TASKS: Dict[str, BatchTask] = {}


def create_task(total: int) -> BatchTask:
    task = BatchTask(total)
    _TASKS[task.id] = task
    return task


def get_task(task_id: str) -> Optional[BatchTask]:
    return _TASKS.get(task_id)


def delete_task(task_id: str) -> None:
    _TASKS.pop(task_id, None)


async def expire_task(task_id: str, delay: int = 300) -> None:
    await asyncio.sleep(delay)
    delete_task(task_id)
