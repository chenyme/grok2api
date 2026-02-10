"""
Common header helpers for Grok services.
"""

from __future__ import annotations

import uuid
from typing import Dict

from app.core.config import get_config
from app.services.grok.utils.statsig import StatsigService

# Grok Chat API（chat / media 共用）
GROK_CHAT_API = "https://grok.com/rest/app-chat/conversations/new"

# 不含动态字段的静态请求头
GROK_STATIC_HEADERS: Dict[str, str] = {
    "Accept": "*/*",
    "Accept-Encoding": "gzip, deflate, br, zstd",
    "Accept-Language": "zh-CN,zh;q=0.9",
    "Baggage": (
        "sentry-environment=production,"
        "sentry-release=d6add6fb0460641fd482d767a335ef72b9b6abb8,"
        "sentry-public_key=b311e0f2690c81f25e2c4cf6d4f7ce1c"
    ),
    "Cache-Control": "no-cache",
    "Content-Type": "application/json",
    "Origin": "https://grok.com",
    "Pragma": "no-cache",
    "Priority": "u=1, i",
    "Sec-Ch-Ua": '"Google Chrome";v="136", "Chromium";v="136", "Not(A:Brand";v="24"',
    "Sec-Ch-Ua-Arch": "arm",
    "Sec-Ch-Ua-Bitness": "64",
    "Sec-Ch-Ua-Mobile": "?0",
    "Sec-Ch-Ua-Model": "",
    "Sec-Ch-Ua-Platform": '"macOS"',
    "Sec-Fetch-Dest": "empty",
    "Sec-Fetch-Mode": "cors",
    "Sec-Fetch-Site": "same-origin",
}


def _normalize_token(token: str) -> str:
    return token[4:] if token.startswith("sso=") else token


def build_sso_cookie(token: str, include_rw: bool = False) -> str:
    token = _normalize_token(token)
    cf = get_config("security.cf_clearance")
    cookie = f"sso={token}"
    if include_rw:
        cookie = f"{cookie}; sso-rw={token}"
    if cf:
        cookie = f"{cookie};cf_clearance={cf}"
    return cookie


def apply_statsig(headers: Dict[str, str]) -> None:
    headers["x-statsig-id"] = StatsigService.gen_id()
    headers["x-xai-request-id"] = str(uuid.uuid4())


def build_grok_headers(
    token: str, referer: str = "https://grok.com/"
) -> Dict[str, str]:
    """构建完整 Grok 请求头（静态 + 动态字段）。"""
    headers = GROK_STATIC_HEADERS.copy()
    headers["User-Agent"] = get_config(
        "security.user_agent",
        "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "
        "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36",
    )
    headers["Referer"] = referer
    apply_statsig(headers)
    headers["Cookie"] = build_sso_cookie(token)
    return headers
