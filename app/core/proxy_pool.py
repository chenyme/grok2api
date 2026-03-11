"""
Proxy round-robin pool.

Supports comma-separated proxy URLs in config, returns the next proxy
in round-robin order on each call.
"""

import threading
from typing import Optional

from app.core.logger import logger

# ---- internal state ----
_lock = threading.Lock()
_pools: dict[str, list[str]] = {}   # key -> parsed list
_indexes: dict[str, int] = {}       # key -> current index
_raw_cache: dict[str, str] = {}     # key -> last raw config value


def _parse_proxies(raw: str) -> list[str]:
    """Parse comma-separated proxy URLs, stripping whitespace and empties."""
    if not raw:
        return []
    return [p.strip() for p in raw.split(",") if p.strip()]


def get_next_proxy(config_key: str) -> Optional[str]:
    """Return the next proxy URL for *config_key* in round-robin order.

    If the underlying config value has changed since the last call the pool
    is silently rebuilt.

    Returns ``""`` (empty string) when no proxy is configured, which keeps
    the same falsy semantics existing callers rely on.
    """
    from app.core.config import config  # avoid circular at module level

    # Read raw value from config store (bypass our own interceptor)
    raw = config.get(config_key, "") or ""

    with _lock:
        # Rebuild pool when the raw value changes
        if raw != _raw_cache.get(config_key):
            proxies = _parse_proxies(raw)
            _pools[config_key] = proxies
            _indexes[config_key] = 0
            _raw_cache[config_key] = raw
            if len(proxies) > 1:
                logger.info(
                    f"ProxyPool: {config_key} loaded {len(proxies)} proxies for round-robin"
                )

        pool = _pools.get(config_key, [])
        if not pool:
            return ""

        idx = _indexes.get(config_key, 0) % len(pool)
        proxy = pool[idx]
        _indexes[config_key] = idx + 1

    return proxy


__all__ = ["get_next_proxy"]
