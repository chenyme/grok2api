"""
Voice Mode API
"""

from fastapi import APIRouter, Depends
from pydantic import BaseModel

from app.core.auth import verify_api_key
from app.services.grok.voice import VoiceService
from app.services.token import get_token_manager
from app.core.exceptions import AppException

router = APIRouter(tags=["Voice"])


class VoiceTokenResponse(BaseModel):
    token: str
    url: str
    participant_name: str = ""
    room_name: str = ""


@router.get("/voice/token", response_model=VoiceTokenResponse)
async def get_voice_token(
    voice: str = "ara",
    personality: str = "assistant",
    speed: float = 1.0,
    api_key: str = Depends(verify_api_key)
):
    """
    Get Grok Voice Mode (LiveKit) Token
    
    Returns:
        token: Access Token
        url: LiveKit Server URL (wss://livekit.grok.com)
    """
    token_mgr = await get_token_manager()
    # Use 'grok-3' or similar model to get a valid pool token
    sso_token = token_mgr.get_token("ssoBasic") 

    if not sso_token:
        raise AppException("No available tokens for voice mode", code="no_token", status_code=503)

    service = VoiceService()
    try:
        data = await service.get_token(
            token=sso_token,
            voice=voice,
            personality=personality,
            speed=speed
        )
        
        # Check for errors in data structure
        token = data.get("token")
        if not token:
             raise AppException("Upstream returned no voice token", code="upstream_error", status_code=502)
             
        # Parse payload if needed, but for now return raw
        return VoiceTokenResponse(
            token=token,
            url="wss://livekit.grok.com",
            participant_name="",
            room_name=""
        )
        
    except Exception as e:
        if isinstance(e, AppException):
            raise
        raise AppException(f"Voice token error: {str(e)}", code="voice_error", status_code=500)
