"""XAI LiveKit protocol — token endpoint and WebSocket URL.

The token fetch sends a JSON payload to /rest/livekit/tokens and receives
a short-lived LiveKit access token.  The WebSocket connection uses that
access token as a query parameter on wss://livekit.grok.com/rtc.
"""

from typing import Any, Dict
from urllib.parse import urlencode

import orjson

LIVEKIT_TOKEN_URL = "https://grok.com/rest/livekit/tokens"
LIVEKIT_WS_BASE = "wss://livekit.grok.com"

# SDK version parameters sent with every WebSocket connection.
_WS_PARAMS: Dict[str, str] = {
    "auto_subscribe": "1",
    "sdk": "js",
    "version": "2.11.4",
    "protocol": "15",
}


def build_token_request_payload(
    voice: str = "ara",
    personality: str = "assistant",
    speed: float = 1.0,
    custom_instruction: str = "",
) -> bytes:
    """Return the JSON body for POST /rest/livekit/tokens."""
    payload_dict = {
        "voice": voice,
        "personality": None,
        "playback_speed": speed,
        "enable_vision": False,
        "turn_detection": {"type": "server_vad"},
    }
    if custom_instruction:
        payload_dict["instructions"] = custom_instruction
        payload_dict["is_raw_instructions"] = True
    else:
        payload_dict["personality"] = personality

    session_payload = orjson.dumps(payload_dict).decode()

    return orjson.dumps(
        {
            "sessionPayload": session_payload,
            "requestAgentDispatch": False,
            "livekitUrl": LIVEKIT_WS_BASE,
            "params": {"enable_markdown_transcript": "true"},
        }
    )


def build_ws_url(access_token: str) -> str:
    """Build the wss:// URL for the LiveKit WebSocket connection."""
    base = LIVEKIT_WS_BASE.rstrip("/")
    rtc = f"{base}/rtc" if not base.endswith("/rtc") else base
    params = dict(_WS_PARAMS)
    params["access_token"] = access_token
    return f"{rtc}?{urlencode(params)}"


__all__ = [
    "LIVEKIT_TOKEN_URL",
    "LIVEKIT_WS_BASE",
    "build_token_request_payload",
    "build_ws_url",
]
