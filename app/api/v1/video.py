"""
Video Generation API 路由
"""

from typing import Optional

from fastapi import APIRouter, Request
from fastapi.responses import StreamingResponse, JSONResponse
from pydantic import BaseModel, Field, field_validator

from app.core.exceptions import ValidationException
from app.services.grok.media import VideoService
from app.services.grok.model import ModelService

router = APIRouter(tags=["Videos"])


class VideoGenerationRequest(BaseModel):
    """视频生成请求 - OpenAI 兼容"""

    prompt: str = Field(..., description="视频描述")
    model: Optional[str] = Field("grok-imagine-1.0-video", description="视频模型名称")
    image: Optional[str] = Field(None, description="图片 URL 或 base64 (可选，用于图生视频)")
    stream: Optional[bool] = Field(False, description="是否流式输出")
    aspect_ratio: Optional[str] = Field("3:2", description="视频比例: 3:2, 16:9, 1:1 等")
    video_length: Optional[int] = Field(
        None,
        description="视频时长(秒): 6 或 10；留空时自动按池选择（basic=6, super=10）",
    )
    resolution: Optional[str] = Field("480p", description="视频分辨率: 480p, 720p")
    preset: Optional[str] = Field("normal", description="风格预设: fun, normal, spicy, custom")

    @field_validator("aspect_ratio")
    @classmethod
    def validate_aspect_ratio(cls, v):
        allowed = ["2:3", "3:2", "1:1", "9:16", "16:9"]
        if v and v not in allowed:
            raise ValidationException(
                message=f"aspect_ratio must be one of {allowed}",
                param="aspect_ratio",
                code="invalid_aspect_ratio",
            )
        return v

    @field_validator("video_length")
    @classmethod
    def validate_video_length(cls, v):
        if v is not None and v not in (6, 10):
            raise ValidationException(
                message="video_length must be 6 or 10 seconds",
                param="video_length",
                code="invalid_video_length",
            )
        return v

    @field_validator("resolution")
    @classmethod
    def validate_resolution(cls, v):
        allowed = ["480p", "720p"]
        if v and v not in allowed:
            raise ValidationException(
                message=f"resolution must be one of {allowed}",
                param="resolution",
                code="invalid_resolution",
            )
        return v

    @field_validator("preset")
    @classmethod
    def validate_preset(cls, v):
        if not v:
            return "normal"
        allowed = ["fun", "normal", "spicy", "custom"]
        if v not in allowed:
            raise ValidationException(
                message=f"preset must be one of {allowed}",
                param="preset",
                code="invalid_preset",
            )
        return v


def _validate_request(request: VideoGenerationRequest):
    model_info = ModelService.get(request.model)
    if not model_info or not model_info.is_video:
        video_models = [m.model_id for m in ModelService.MODELS if m.is_video]
        raise ValidationException(
            message=(
                f"The model `{request.model}` is not supported for video generation. "
                f"Supported: {video_models}"
            ),
            param="model",
            code="model_not_supported",
        )

    if not request.prompt or not request.prompt.strip():
        raise ValidationException(
            message="Prompt cannot be empty", param="prompt", code="empty_prompt"
        )


@router.post("/videos/generations")
async def create_video(request: VideoGenerationRequest, raw_request: Request):
    """
    Video Generation API

    - 当提供 image 时执行图生视频
    - 当未提供 image 时执行文生视频
    """
    _validate_request(request)

    from app.core.auth import get_client_ip, get_request_key_name

    client_ip = get_client_ip(raw_request)
    key_name = await get_request_key_name(raw_request)

    if request.image:
        content = [
            {"type": "text", "text": request.prompt},
            {"type": "image_url", "image_url": {"url": request.image}},
        ]
        messages = [{"role": "user", "content": content}]
    else:
        messages = [{"role": "user", "content": request.prompt}]

    result = await VideoService.completions(
        model=request.model,
        messages=messages,
        stream=request.stream,
        thinking=None,
        aspect_ratio=request.aspect_ratio,
        video_length=request.video_length,
        resolution=request.resolution,
        preset=request.preset,
        client_ip=client_ip,
        key_name=key_name,
    )

    if isinstance(result, dict):
        return JSONResponse(content=result)

    return StreamingResponse(
        result,
        media_type="text/event-stream",
        headers={
            "Cache-Control": "no-cache",
            "Connection": "keep-alive",
            "X-Accel-Buffering": "no",
        },
    )


__all__ = ["router"]
