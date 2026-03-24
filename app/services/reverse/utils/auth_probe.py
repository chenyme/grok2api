"""
Lightweight fallback probes for auth diagnosis when curl-cffi resets the connection.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

import aiohttp
import orjson

from app.core.config import get_config
from app.core.logger import logger
from app.core.ssl_certs import create_ssl_context
from app.services.reverse.utils.headers import build_headers

RATE_LIMITS_API = "https://grok.com/rest/rate-limits"
RATE_LIMITS_PAYLOAD = {
    "requestKind": "DEFAULT",
    "modelName": "grok-4-1-thinking-1129",
}


@dataclass
class ProbeResult:
    status: int
    body: str
    content_type: str
    server_header: str
    is_token_expired: bool
    is_cloudflare: bool


class ProbeJSONResponse:
    """Small response adapter used when the fallback probe succeeds."""

    def __init__(self, result: ProbeResult):
        self.status_code = result.status
        self.headers = {
            "content-type": result.content_type,
            "server": result.server_header,
        }
        self.text = result.body
        self._result = result

    def json(self) -> Any:
        return orjson.loads(self.text) if self.text else {}


def classify_probe(status: int, content_type: str, server_header: str, body: str) -> tuple[bool, bool]:
    content_type_lower = (content_type or "").lower()
    server_lower = (server_header or "").lower()
    body_lower = (body or "").lower()

    is_cloudflare = "challenge-platform" in body_lower
    if "cloudflare" in server_lower and "application/json" not in content_type_lower:
        is_cloudflare = True

    # A direct 401 from Grok's authenticated rate-limit endpoint is a strong signal
    # that the SSO session is no longer valid, even if the body is empty.
    is_token_expired = status == 401
    if is_token_expired:
        is_cloudflare = False
    if not is_token_expired and status == 401 and "application/json" in content_type_lower:
        auth_error_keywords = [
            "unauthorized",
            "not logged in",
            "unauthenticated",
            "bad-credentials",
        ]
        is_token_expired = any(keyword in body_lower for keyword in auth_error_keywords)

    return is_token_expired, is_cloudflare


async def probe_rate_limits_status(token: str) -> ProbeResult | None:
    """Probe the rate-limits endpoint with aiohttp to recover a usable status/body."""
    if get_config("proxy.base_proxy_url"):
        return None

    headers = build_headers(
        cookie_token=token,
        content_type="application/json",
        origin="https://grok.com",
        referer="https://grok.com/",
    )

    timeout = float(get_config("usage.timeout") or 20)
    client_timeout = aiohttp.ClientTimeout(total=timeout)

    try:
        async with aiohttp.ClientSession(timeout=client_timeout, trust_env=True) as session:
            async with session.post(
                RATE_LIMITS_API,
                headers=headers,
                data=orjson.dumps(RATE_LIMITS_PAYLOAD),
                ssl=create_ssl_context(),
            ) as response:
                body = await response.text()
                content_type = response.headers.get("Content-Type", "")
                server_header = response.headers.get("Server", "")
                is_token_expired, is_cloudflare = classify_probe(
                    response.status, content_type, server_header, body
                )
                return ProbeResult(
                    status=response.status,
                    body=body,
                    content_type=content_type,
                    server_header=server_header,
                    is_token_expired=is_token_expired,
                    is_cloudflare=is_cloudflare,
                )
    except Exception as exc:
        logger.debug("probe_rate_limits_status failed: {}", exc)
        return None


__all__ = [
    "ProbeJSONResponse",
    "ProbeResult",
    "classify_probe",
    "probe_rate_limits_status",
]
