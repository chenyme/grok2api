"""LiveKit transport — fetch session token and open the WebSocket.

``fetch_livekit_token`` POSTs to /rest/livekit/tokens with proxy support.
``connect_livekit_ws`` opens the wss:// connection and returns a
``WebSocketConnection`` that the products layer can read/write.
"""

from typing import Any, Dict, Optional

import orjson

from app.platform.logging.logger import logger
from app.platform.config.snapshot import get_config
from app.platform.errors import UpstreamError
from app.control.proxy.models import (
    ProxyFeedback,
    ProxyFeedbackKind,
    ProxyScope,
    RequestKind,
)
from app.dataplane.proxy import get_proxy_runtime
from app.dataplane.proxy.adapters.headers import build_http_headers, build_ws_headers
from app.dataplane.reverse.protocol.xai_livekit import (
    LIVEKIT_TOKEN_URL,
    build_token_request_payload,
    build_ws_url,
)
from app.dataplane.reverse.transport.http import post_json
from app.dataplane.reverse.transport.websocket import (
    WebSocketClient,
    WebSocketConnection,
)
from app.dataplane.reverse.transport._proxy_feedback import upstream_feedback


# ------------------------------------------------------------------
# Token fetch
# ------------------------------------------------------------------


async def fetch_livekit_token(
    token: str,
    *,
    voice: str = "ara",
    personality: str = "assistant",
    speed: float = 1.0,
    custom_instruction: str = "",
) -> Dict[str, Any]:
    """Fetch a LiveKit session token for *token*.

    Returns the parsed JSON body from /rest/livekit/tokens.
    Raises ``UpstreamError`` on failure.
    """
    cfg = get_config()
    timeout_s = cfg.get_float("voice.timeout", 60.0)

    proxy = await get_proxy_runtime()
    lease = await proxy.acquire(scope=ProxyScope.APP, kind=RequestKind.HTTP)

    payload = build_token_request_payload(
        voice=voice,
        personality=personality,
        speed=speed,
        custom_instruction=custom_instruction,
    )

    try:
        result = await post_json(
            LIVEKIT_TOKEN_URL,
            token,
            payload,
            lease=lease,
            timeout_s=timeout_s,
            origin="https://grok.com",
            referer="https://grok.com/",
        )
    except UpstreamError as exc:
        await proxy.feedback(lease, upstream_feedback(exc))
        raise
    except Exception as exc:
        await proxy.feedback(
            lease, ProxyFeedback(kind=ProxyFeedbackKind.TRANSPORT_ERROR)
        )
        raise UpstreamError(f"fetch_livekit_token: transport error: {exc}") from exc

    await proxy.feedback(
        lease,
        ProxyFeedback(kind=ProxyFeedbackKind.SUCCESS, status_code=200),
    )
    logger.debug("livekit session token fetched")
    return result


# ------------------------------------------------------------------
# WebSocket connect
# ------------------------------------------------------------------


async def connect_livekit_ws(
    token: str,
    access_token: str,
    *,
    timeout_s: Optional[float] = None,
) -> WebSocketConnection:
    """Open a WebSocket connection to the LiveKit RTC endpoint.

    Args:
        token:        SSO session cookie — used for WebSocket headers.
        access_token: Short-lived LiveKit token from ``fetch_livekit_token``.
        timeout_s:    Connection + read timeout; defaults to ``voice.timeout`` config.

    Returns:
        ``WebSocketConnection`` wrapping the live aiohttp WS connection.

    Raises:
        ``UpstreamError`` on connection failure.
    """
    cfg = get_config()
    timeout = (
        timeout_s if timeout_s is not None else cfg.get_float("voice.timeout", 120.0)
    )

    proxy = await get_proxy_runtime()
    lease = await proxy.acquire(scope=ProxyScope.APP, kind=RequestKind.WEBSOCKET)

    url = build_ws_url(access_token)
    headers = build_ws_headers(token=token, lease=lease)
    client = WebSocketClient()

    async def _on_close() -> None:
        try:
            await proxy.feedback(
                lease,
                ProxyFeedback(kind=ProxyFeedbackKind.SUCCESS, status_code=200),
            )
        except Exception:
            pass

    try:
        connection = await client.connect(
            url,
            headers=headers,
            timeout=timeout,
            lease=lease,
            on_close=_on_close,
        )
    except Exception as exc:
        await proxy.feedback(
            lease,
            ProxyFeedback(kind=ProxyFeedbackKind.TRANSPORT_ERROR),
        )
        logger.error("livekit websocket connect failed: error={}", exc)
        raise UpstreamError(f"connect_livekit_ws: {exc}") from exc

    logger.debug("livekit websocket connected: url={}", url)
    return connection


__all__ = ["fetch_livekit_token", "connect_livekit_ws"]
