"""Voice token endpoint — LiveKit token acquisition."""

from fastapi import APIRouter, Depends
from pydantic import BaseModel

from app.platform.errors import AppError, RateLimitError, UpstreamError
from app.platform.logging.logger import logger
from app.platform.runtime.clock import now_s
from app.platform.auth.middleware import verify_webui_key

router = APIRouter(
    prefix="/webui/api",
    dependencies=[Depends(verify_webui_key)],
    tags=["WebUI - Voice"],
)


class VoiceTokenResponse(BaseModel):
    token: str
    url: str
    participant_name: str = ""
    room_name: str = ""


@router.get("/voice/token", response_model=VoiceTokenResponse)
async def voice_token(
    voice: str = "ara",
    personality: str = "assistant",
    speed: float = 1.0,
    instruction: str = "",
):
    """Acquire a LiveKit voice session token."""
    from app.dataplane.account import _directory as _acct_dir

    if _acct_dir is None:
        raise RateLimitError("Account directory not initialised")

    # Voice uses super/basic pools → try super first, then basic, then heavy.
    from app.control.model.enums import ModeId

    ts = now_s()
    acct = await _acct_dir.reserve(
        pool_candidates=(1, 0, 2), mode_id=int(ModeId.AUTO), now_s_override=ts
    )
    if acct is None:
        raise RateLimitError("No available tokens for voice mode")

    token = acct.token
    try:
        from app.dataplane.reverse.transport.livekit import fetch_livekit_token

        data = await fetch_livekit_token(
            token,
            voice=voice,
            personality=personality,
            speed=speed,
            custom_instruction=instruction,
        )
        lk_token = data.get("token")
        if not lk_token:
            raise UpstreamError("Upstream returned no voice token")
        return VoiceTokenResponse(
            token=lk_token,
            url=str(data.get("livekitUrl") or "wss://livekit.grok.com"),
            participant_name=str(
                data.get("participantName")
                or data.get("participant_name")
                or data.get("identity")
                or ""
            ),
            room_name=str(
                data.get("roomName") or data.get("room_name") or data.get("room") or ""
            ),
        )
    except AppError:
        raise
    except Exception as e:
        raise UpstreamError(f"Voice token error: {e}")
    finally:
        await _acct_dir.release(acct)
