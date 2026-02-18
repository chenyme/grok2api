"""
Shared locking helpers for assets operations.
"""

import asyncio
import time
from contextlib import asynccontextmanager
from pathlib import Path

from app.core.config import get_config
from app.core.storage import DATA_DIR

try:
    import fcntl
except ImportError:
    fcntl = None


LOCK_DIR = DATA_DIR / ".locks"

_UPLOAD_SEMAPHORE = None
_UPLOAD_SEM_VALUE = None
_DOWNLOAD_SEMAPHORE = None
_DOWNLOAD_SEM_VALUE = None


def _get_upload_semaphore() -> asyncio.Semaphore:
    """Return global semaphore for upload operations."""
    value = max(1, int(get_config("asset.upload_concurrent")))

    global _UPLOAD_SEMAPHORE, _UPLOAD_SEM_VALUE
    if _UPLOAD_SEMAPHORE is None or value != _UPLOAD_SEM_VALUE:
        _UPLOAD_SEM_VALUE = value
        _UPLOAD_SEMAPHORE = asyncio.Semaphore(value)
    return _UPLOAD_SEMAPHORE


def _get_download_semaphore() -> asyncio.Semaphore:
    """Return global semaphore for download operations."""
    value = max(1, int(get_config("asset.download_concurrent")))

    global _DOWNLOAD_SEMAPHORE, _DOWNLOAD_SEM_VALUE
    if _DOWNLOAD_SEMAPHORE is None or value != _DOWNLOAD_SEM_VALUE:
        _DOWNLOAD_SEM_VALUE = value
        _DOWNLOAD_SEMAPHORE = asyncio.Semaphore(value)
    return _DOWNLOAD_SEMAPHORE


@asynccontextmanager
async def _file_lock(name: str, timeout: int = 10):
    """File lock guard."""
    if fcntl is None:
        yield
        return

    LOCK_DIR.mkdir(parents=True, exist_ok=True)
    lock_path = Path(LOCK_DIR) / f"{name}.lock"
    fd = None
    locked = False
    start = time.monotonic()

    try:
        fd = open(lock_path, "a+")
        while True:
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                locked = True
                break
            except BlockingIOError:
                if time.monotonic() - start >= timeout:
                    break
                await asyncio.sleep(0.05)
        if not locked:
            raise TimeoutError(f"Failed to acquire lock: {name}")
        yield
    finally:
        if fd:
            if locked:
                try:
                    fcntl.flock(fd, fcntl.LOCK_UN)
                except Exception:
                    pass
            fd.close()


__all__ = ["_get_upload_semaphore", "_get_download_semaphore", "_file_lock"]
