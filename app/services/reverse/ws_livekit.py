"""
Reverse interface: LiveKit token + WebSocket.
"""

import orjson
from typing import Any, Dict
from urllib.parse import urlencode
from curl_cffi.requests import AsyncSession

from app.core.logger import logger
from app.core.config import get_config
from app.core.exceptions import UpstreamException
from app.services.token.service import TokenService
from app.services.reverse.utils.headers import build_headers, build_ws_headers
from app.services.reverse.utils.retry import retry_on_status
from app.services.reverse.utils.websocket import WebSocketClient, WebSocketConnection

LIVEKIT_TOKEN_API = "https://grok.com/rest/livekit/tokens"
LIVEKIT_WS_URL = "wss://livekit.grok.com"


class LivekitTokenReverse:
    """/rest/livekit/tokens reverse interface."""

    @staticmethod
    async def request(
        session: AsyncSession,
        token: str,
        voice: str = "ara",
        personality: str = "assistant",
        speed: float = 1.0,
    ) -> Dict[str, Any]:
        """Fetch LiveKit token.
        
        Args:
            session: AsyncSession, the session to use for the request.
            token: str, the SSO token.
            voice: str, the voice to use for the request.
            personality: str, the personality to use for the request.
            speed: float, the speed to use for the request.

        Returns:
            Dict[str, Any]: The LiveKit token.
        """
        try:
            # Get proxies
            base_proxy = get_config("proxy.base_proxy_url")
            proxies = {"http": base_proxy, "https": base_proxy} if base_proxy else None

            # Build headers
            headers = build_headers(
                cookie_token=token,
                content_type="application/json",
                origin="https://grok.com",
                referer="https://grok.com/",
            )

            # Build payload
            payload = {
                "sessionPayload": orjson.dumps(
                    {
                        "voice": voice,
                        "personality": personality,
                        "playback_speed": speed,
                        "enable_vision": False,
                        "turn_detection": {"type": "server_vad"},
                    }
                ).decode(),
                "requestAgentDispatch": False,
                "livekitUrl": LIVEKIT_WS_URL,
                "params": {"enable_markdown_transcript": "true"},
            }

            # Curl Config
            timeout = get_config("voice.timeout")
            browser = get_config("proxy.browser")

            async def _do_request():
                response = await session.post(
                    LIVEKIT_TOKEN_API,
                    headers=headers,
                    data=orjson.dumps(payload),
                    timeout=timeout,
                    proxies=proxies,
                    impersonate=browser,
                )

                if response.status_code != 200:
                    body = response.text[:200]
                    logger.error(
                        f"LivekitTokenReverse: Request failed, {response.status_code}, body={body}"
                    )
                    raise UpstreamException(
                        message=f"LivekitTokenReverse: Request failed, {response.status_code}",
                        details={"status": response.status_code, "body": response.text},
                    )

                return response

            response = await retry_on_status(_do_request)
            return response

        except Exception as e:
            # Handle upstream exception
            if isinstance(e, UpstreamException):
                status = None
                if e.details and "status" in e.details:
                    status = e.details["status"]
                else:
                    status = getattr(e, "status_code", None)
                if status == 401:
                    try:
                        await TokenService.record_fail(
                            token, status, "livekit_token_auth_failed"
                        )
                    except Exception:
                        pass
                raise

            # Handle other non-upstream exceptions
            logger.error(
                f"LivekitTokenReverse: Request failed, {str(e)}",
                extra={"error_type": type(e).__name__},
            )
            raise UpstreamException(
                message=f"LivekitTokenReverse: Request failed, {str(e)}",
                details={"status": 502, "error": str(e)},
            )


class LivekitWebSocketReverse:
    """LiveKit WebSocket reverse interface."""

    def __init__(self) -> None:
        self._client = WebSocketClient()

    async def connect(self, token: str) -> WebSocketConnection:
        """Connect to the LiveKit WebSocket.
        
        Args:
            token: str, the SSO token.

        Returns:
            WebSocketConnection: The LiveKit WebSocket connection.
        """
        # Format URL
        base = LIVEKIT_WS_URL.rstrip("/")
        if not base.endswith("/rtc"):
            base = f"{base}/rtc"

        # Build parameters
        params = {
            "access_token": token,
            "auto_subscribe": "1",
            "sdk": "js",
            "version": "2.11.4",
            "protocol": "15",
        }

        # Build URL
        url = f"{base}?{urlencode(params)}"

        # Build WebSocket headers
        ws_headers = build_ws_headers()

        try:
            return await self._client.connect(
                url, headers=ws_headers, timeout=get_config("voice.timeout")
            )
        except Exception as e:
            logger.error(f"LivekitWebSocketReverse: Connect failed, {e}")
            raise UpstreamException(
                f"LivekitWebSocketReverse: Connect failed, {str(e)}"
            )


__all__ = [
    "LivekitTokenReverse",
    "LivekitWebSocketReverse",
    "LIVEKIT_TOKEN_API",
    "LIVEKIT_WS_URL",
]
