from fastapi import APIRouter, Depends
from pydantic import BaseModel

from app.core.auth import verify_public_key
from app.core.exceptions import AppException
from app.services.grok.services.voice import VoiceService
from app.services.token.manager import get_token_manager

router = APIRouter()


class VoiceTokenResponse(BaseModel):
    token: str
    url: str
    participant_name: str = ""
    room_name: str = ""


@router.get(
    "/voice/token",
    dependencies=[Depends(verify_public_key)],
    response_model=VoiceTokenResponse,
)
async def public_voice_token(
    voice: str = "ara",
    personality: str = "assistant",
    speed: float = 1.0,
):
    """获取 Grok Voice Mode (LiveKit) Token"""
    token_mgr = await get_token_manager()
    sso_token = None
    for pool_name in ("ssoBasic", "ssoSuper"):
        sso_token = token_mgr.get_token(pool_name)
        if sso_token:
            break

    if not sso_token:
        raise AppException(
            "No available tokens for voice mode",
            code="no_token",
            status_code=503,
        )

    service = VoiceService()
    try:
        data = await service.get_token(
            token=sso_token,
            voice=voice,
            personality=personality,
            speed=speed,
        )
        token = data.get("token")
        if not token:
            raise AppException(
                "Upstream returned no voice token",
                code="upstream_error",
                status_code=502,
            )

        return VoiceTokenResponse(
            token=token,
            url="wss://livekit.grok.com",
            participant_name="",
            room_name="",
        )

    except Exception as e:
        if isinstance(e, AppException):
            raise
        raise AppException(
            f"Voice token error: {str(e)}",
            code="voice_error",
            status_code=500,
        )


@router.get("/verify", dependencies=[Depends(verify_public_key)])
async def public_verify_api():
    """验证 Public Key"""
    return {"status": "success"}
