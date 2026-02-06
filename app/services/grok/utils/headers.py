"""
Common header helpers for Grok services.
"""

from __future__ import annotations

import uuid
from typing import Dict

from app.core.config import get_config
from app.services.grok.utils.statsig import StatsigService


def _normalize_token(token: str) -> str:
    return token[4:] if token.startswith("sso=") else token


def build_sso_cookie(token: str, include_rw: bool = False) -> str:
    token = _normalize_token(token)
    cf = get_config("grok.cf_clearance", "")
    cookie = f"sso={token}"
    if include_rw:
        cookie = f"{cookie}; sso-rw={token}"
    if cf:
        cookie = f"{cookie};cf_clearance={cf}"
    return cookie


def apply_statsig(headers: Dict[str, str]) -> None:
    headers["x-statsig-id"] = StatsigService.gen_id()
    headers["x-xai-request-id"] = str(uuid.uuid4())
