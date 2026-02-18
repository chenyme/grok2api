import asyncio
import time
import uuid
from typing import Optional, List, Dict, Any

import orjson
from fastapi import APIRouter, Depends, HTTPException, Query, Request
from fastapi.responses import StreamingResponse
from pydantic import BaseModel

from app.core.auth import verify_public_key
from app.core.logger import logger
from app.services.grok.services.video import VideoService
from app.services.grok.services.model import ModelService

router = APIRouter()

VIDEO_SESSION_TTL = 600
_VIDEO_SESSIONS: dict[str, dict] = {}
_VIDEO_SESSIONS_LOCK = asyncio.Lock()

_VIDEO_RATIO_MAP = {
    "1280x720": "16:9",
    "720x1280": "9:16",
    "1792x1024": "3:2",
    "1024x1792": "2:3",
    "1024x1024": "1:1",
    "16:9": "16:9",
    "9:16": "9:16",
    "3:2": "3:2",
    "2:3": "2:3",
    "1:1": "1:1",
}


async def _clean_sessions(now: float) -> None:
    expired = [
        key
        for key, info in _VIDEO_SESSIONS.items()
        if now - float(info.get("created_at") or 0) > VIDEO_SESSION_TTL
    ]
    for key in expired:
        _VIDEO_SESSIONS.pop(key, None)


async def _new_session(
    prompt: str,
    aspect_ratio: str,
    video_length: int,
    resolution_name: str,
    preset: str,
    image_url: Optional[str],
    reasoning_effort: Optional[str],
) -> str:
    task_id = uuid.uuid4().hex
    now = time.time()
    async with _VIDEO_SESSIONS_LOCK:
        await _clean_sessions(now)
        _VIDEO_SESSIONS[task_id] = {
            "prompt": prompt,
            "aspect_ratio": aspect_ratio,
            "video_length": video_length,
            "resolution_name": resolution_name,
            "preset": preset,
            "image_url": image_url,
            "reasoning_effort": reasoning_effort,
            "created_at": now,
        }
    return task_id


async def _get_session(task_id: str) -> Optional[dict]:
    if not task_id:
        return None
    now = time.time()
    async with _VIDEO_SESSIONS_LOCK:
        await _clean_sessions(now)
        info = _VIDEO_SESSIONS.get(task_id)
        if not info:
            return None
        created_at = float(info.get("created_at") or 0)
        if now - created_at > VIDEO_SESSION_TTL:
            _VIDEO_SESSIONS.pop(task_id, None)
            return None
        return dict(info)


async def _drop_session(task_id: str) -> None:
    if not task_id:
        return
    async with _VIDEO_SESSIONS_LOCK:
        _VIDEO_SESSIONS.pop(task_id, None)


async def _drop_sessions(task_ids: List[str]) -> int:
    if not task_ids:
        return 0
    removed = 0
    async with _VIDEO_SESSIONS_LOCK:
        for task_id in task_ids:
            if task_id and task_id in _VIDEO_SESSIONS:
                _VIDEO_SESSIONS.pop(task_id, None)
                removed += 1
    return removed


def _normalize_ratio(value: Optional[str]) -> str:
    raw = (value or "").strip()
    return _VIDEO_RATIO_MAP.get(raw, "")


def _validate_image_url(image_url: str) -> None:
    value = (image_url or "").strip()
    if not value:
        return
    if value.startswith("data:"):
        return
    if value.startswith("http://") or value.startswith("https://"):
        return
    raise HTTPException(
        status_code=400,
        detail="image_url must be a URL or data URI (data:<mime>;base64,...)",
    )


class VideoStartRequest(BaseModel):
    prompt: str
    aspect_ratio: Optional[str] = "3:2"
    video_length: Optional[int] = 6
    resolution_name: Optional[str] = "480p"
    preset: Optional[str] = "normal"
    image_url: Optional[str] = None
    reasoning_effort: Optional[str] = None


@router.post("/video/start", dependencies=[Depends(verify_public_key)])
async def public_video_start(data: VideoStartRequest):
    prompt = (data.prompt or "").strip()
    if not prompt:
        raise HTTPException(status_code=400, detail="Prompt cannot be empty")

    aspect_ratio = _normalize_ratio(data.aspect_ratio)
    if not aspect_ratio:
        raise HTTPException(
            status_code=400,
            detail="aspect_ratio must be one of ['16:9','9:16','3:2','2:3','1:1']",
        )

    video_length = int(data.video_length or 6)
    if video_length not in (6, 10, 15):
        raise HTTPException(
            status_code=400, detail="video_length must be 6, 10, or 15 seconds"
        )

    resolution_name = str(data.resolution_name or "480p")
    if resolution_name not in ("480p", "720p"):
        raise HTTPException(
            status_code=400,
            detail="resolution_name must be one of ['480p','720p']",
        )

    preset = str(data.preset or "normal")
    if preset not in ("fun", "normal", "spicy", "custom"):
        raise HTTPException(
            status_code=400,
            detail="preset must be one of ['fun','normal','spicy','custom']",
        )

    image_url = (data.image_url or "").strip() or None
    if image_url:
        _validate_image_url(image_url)

    reasoning_effort = (data.reasoning_effort or "").strip() or None
    if reasoning_effort:
        allowed = {"none", "minimal", "low", "medium", "high", "xhigh"}
        if reasoning_effort not in allowed:
            raise HTTPException(
                status_code=400,
                detail=f"reasoning_effort must be one of {sorted(allowed)}",
            )

    task_id = await _new_session(
        prompt,
        aspect_ratio,
        video_length,
        resolution_name,
        preset,
        image_url,
        reasoning_effort,
    )
    return {"task_id": task_id, "aspect_ratio": aspect_ratio}


@router.get("/video/sse")
async def public_video_sse(request: Request, task_id: str = Query("")):
    session = await _get_session(task_id)
    if not session:
        raise HTTPException(status_code=404, detail="Task not found")

    prompt = str(session.get("prompt") or "").strip()
    aspect_ratio = str(session.get("aspect_ratio") or "3:2")
    video_length = int(session.get("video_length") or 6)
    resolution_name = str(session.get("resolution_name") or "480p")
    preset = str(session.get("preset") or "normal")
    image_url = session.get("image_url")
    reasoning_effort = session.get("reasoning_effort")

    async def event_stream():
        try:
            model_id = "grok-imagine-1.0-video"
            model_info = ModelService.get(model_id)
            if not model_info or not model_info.is_video:
                payload = {
                    "error": "Video model is not available.",
                    "code": "model_not_supported",
                }
                yield f"data: {orjson.dumps(payload).decode()}\n\n"
                yield "data: [DONE]\n\n"
                return

            if image_url:
                messages: List[Dict[str, Any]] = [
                    {
                        "role": "user",
                        "content": [
                            {"type": "text", "text": prompt},
                            {"type": "image_url", "image_url": {"url": image_url}},
                        ],
                    }
                ]
            else:
                messages = [{"role": "user", "content": prompt}]

            stream = await VideoService.completions(
                model_id,
                messages,
                stream=True,
                reasoning_effort=reasoning_effort,
                aspect_ratio=aspect_ratio,
                video_length=video_length,
                resolution=resolution_name,
                preset=preset,
            )

            async for chunk in stream:
                if await request.is_disconnected():
                    break
                yield chunk
        except Exception as e:
            logger.warning(f"Public video SSE error: {e}")
            payload = {"error": str(e), "code": "internal_error"}
            yield f"data: {orjson.dumps(payload).decode()}\n\n"
            yield "data: [DONE]\n\n"
        finally:
            await _drop_session(task_id)

    return StreamingResponse(
        event_stream(),
        media_type="text/event-stream",
        headers={"Cache-Control": "no-cache", "Connection": "keep-alive"},
    )


class VideoStopRequest(BaseModel):
    task_ids: List[str]


@router.post("/video/stop", dependencies=[Depends(verify_public_key)])
async def public_video_stop(data: VideoStopRequest):
    removed = await _drop_sessions(data.task_ids or [])
    return {"status": "success", "removed": removed}


__all__ = ["router"]
