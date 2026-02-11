"""Proxy helpers for upstream requests."""

from __future__ import annotations

from typing import Optional
from urllib.parse import urlparse

from app.core.logger import logger


def normalize_proxy_url(proxy_url: Optional[str]) -> Optional[str]:
    """Normalize proxy URL for curl-cffi/requests compatibility.

    - Trim whitespace
    - Convert socks5:// and socks4:// to host-resolving variants socks5h:// / socks4a://
      so DNS happens on proxy side (avoids local DNS / timeout issues)
    """
    if not proxy_url:
        return None

    value = str(proxy_url).strip()
    if not value:
        return None

    parsed = urlparse(value)
    scheme = (parsed.scheme or "").lower()

    if scheme == "socks5":
        normalized = f"socks5h://{value[len('socks5://') :]}"
        logger.info(f"Normalized proxy scheme socks5 -> socks5h: {normalized}")
        return normalized

    if scheme == "socks4":
        normalized = f"socks4a://{value[len('socks4://') :]}"
        logger.info(f"Normalized proxy scheme socks4 -> socks4a: {normalized}")
        return normalized

    return value


def build_request_proxies(proxy_url: Optional[str]) -> Optional[dict]:
    """Build requests-style proxy mapping."""
    normalized = normalize_proxy_url(proxy_url)
    if not normalized:
        return None
    return {"http": normalized, "https": normalized}


__all__ = ["normalize_proxy_url", "build_request_proxies"]
