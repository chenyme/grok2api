"""Bypass server support for Grok upstream requests."""

from __future__ import annotations

from typing import Optional, Tuple
from urllib.parse import urlsplit

from app.core.config import setting
from app.core.logger import logger


def resolve_bypass(url: str) -> Tuple[str, Optional[str]]:
    """Return (effective_url, x_hostname).

    When bypass mode is enabled, rewrites base URL but keeps the original path,
    and returns the original upstream hostname for the `x-hostname` header.
    """
    try:
        bypass_enabled = bool(setting.grok_config.get("bypass_server", False))
        bypass_baseurl = (setting.grok_config.get("bypass_baseurl", "") or "").strip()
        if not bypass_enabled or not bypass_baseurl:
            return url, None

        parsed = urlsplit(url)
        if not parsed.scheme or not parsed.netloc:
            return url, None

        base = bypass_baseurl.rstrip("/")
        path = parsed.path if parsed.path.startswith("/") else f"/{parsed.path}"
        return f"{base}{path}", parsed.hostname
    except Exception as e:
        logger.debug(f"[Bypass] resolve failed: {e}")
        return url, None

