"""
Grok Voice Mode Service
"""

import uuid
import orjson
from typing import Dict, Any, Optional

from curl_cffi.requests import AsyncSession

from app.core.logger import logger
from app.core.config import get_config
from app.core.exceptions import UpstreamException
from app.services.grok.statsig import StatsigService


LIVEKIT_TOKEN_API = "https://grok.com/rest/livekit/tokens"
TIMEOUT = 30
BROWSER = "chrome136"


class VoiceService:
    """Voice Mode Service (LiveKit)"""

    def __init__(self, proxy: str = None):
        self.proxy = proxy or get_config("grok.base_proxy_url", "")

    async def get_token(self, token: str, model: str = "grok-4.1") -> Dict[str, Any]:
        """
        Get LiveKit token
        
        Args:
            token: Auth token
            model: Model name (default:grok-4.1, though voice uses specific settings)
            
        Returns:
            Dict containing token and livekitUrl
        """
        headers = self._build_headers(token)
        payload = self._build_payload(model)
        
        proxies = {"http": self.proxy, "https": self.proxy} if self.proxy else None
        
        try:
            async with AsyncSession(impersonate=BROWSER) as session:
                response = await session.post(
                    LIVEKIT_TOKEN_API,
                    headers=headers,
                    data=orjson.dumps(payload),
                    timeout=TIMEOUT,
                    proxies=proxies,
                )

                if response.status_code != 200:
                    logger.error(
                        f"Voice token failed: {response.status_code}",
                        extra={"response": response.text[:200]}
                    )
                    raise UpstreamException(
                        message=f"Failed to get voice token: {response.status_code}",
                        details={"status": response.status_code, "body": response.text}
                    )
                
                return response.json()

        except Exception as e:
            logger.error(f"Voice service error: {e}")
            if isinstance(e, UpstreamException):
                raise
            raise UpstreamException(f"Voice service error: {str(e)}")

    def _build_headers(self, token: str) -> Dict[str, str]:
        headers = {
            "Accept": "*/*",
            "Content-Type": "application/json",
            "Origin": "https://grok.com",
            "Referer": "https://grok.com/",
            # Statsig ID is crucial
            "x-statsig-id": StatsigService.gen_id(),
            "x-xai-request-id": str(uuid.uuid4()),
        }
        
        token = token[4:] if token.startswith("sso=") else token
        cf = get_config("grok.cf_clearance", "")
        headers["Cookie"] = f"sso={token};cf_clearance={cf}" if cf else f"sso={token}"
        
        return headers

    def _build_payload(self, model: str) -> Dict[str, Any]:
        # Based on reverse engineered payload
        # voice: "Ara" (default), personality: "assistant"
        return {
            "sessionPayload": orjson.dumps({
                "voice": "Ara",
                "personality": "assistant",
                "playback_speed": 1,
                "enable_vision": False,
                "turn_detection": {"type": "server_vad"}
            }).decode(),
            "requestAgentDispatch": False,
            "livekitUrl": "wss://livekit.grok.com",
            "params": {"enable_markdown_transcript": "true"}
        }
