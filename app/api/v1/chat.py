"""
Chat Completions API 路由
"""

from typing import Any, Dict, List, Optional, Union
import base64
import binascii
import time

from fastapi import APIRouter
from fastapi.responses import StreamingResponse, JSONResponse
from pydantic import BaseModel, Field

from app.services.grok.services.chat import ChatService
from app.services.grok.services.image import ImageGenerationService
from app.services.grok.services.image_edit import ImageEditService
from app.services.grok.services.model import ModelService
from app.services.grok.services.video import VideoService
from app.services.token import get_token_manager
from app.core.config import get_config
from app.core.exceptions import ValidationException, AppException, ErrorType


class MessageItem(BaseModel):
    """消息项"""

    role: str
    content: Union[str, List[Dict[str, Any]]]


class VideoConfig(BaseModel):
    """视频生成配置"""

    aspect_ratio: Optional[str] = Field("3:2", description="视频比例: 1280x720(16:9), 720x1280(9:16), 1792x1024(3:2), 1024x1792(2:3), 1024x1024(1:1)")
    video_length: Optional[int] = Field(6, description="视频时长(秒): 6 / 10 / 15")
    resolution_name: Optional[str] = Field("480p", description="视频分辨率: 480p, 720p")
    preset: Optional[str] = Field("custom", description="风格预设: fun, normal, spicy")

class ImageConfig(BaseModel):
    """图片生成配置"""

    n: Optional[int] = Field(1, ge=1, le=10, description="生成数量 (1-10)")
    size: Optional[str] = Field("1024x1024", description="图片尺寸")
    response_format: Optional[str] = Field(None, description="响应格式")


class ChatCompletionRequest(BaseModel):
    """Chat Completions 请求"""

    model: str = Field(..., description="模型名称")
    messages: List[MessageItem] = Field(..., description="消息数组")
    stream: Optional[bool] = Field(None, description="是否流式输出")
    reasoning_effort: Optional[str] = Field(None, description="推理强度: none/minimal/low/medium/high/xhigh")
    temperature: Optional[float] = Field(0.8, description="采样温度: 0-2")
    top_p: Optional[float] = Field(0.95, description="nucleus 采样: 0-1")
    # 视频生成配置
    video_config: Optional[VideoConfig] = Field(None, description="视频生成参数")
    # 图片生成配置
    image_config: Optional[ImageConfig] = Field(None, description="图片生成参数")


VALID_ROLES = {"developer", "system", "user", "assistant"}
USER_CONTENT_TYPES = {"text", "image_url", "input_audio", "file"}
ALLOWED_IMAGE_SIZES = {
    "1280x720",
    "720x1280",
    "1792x1024",
    "1024x1792",
    "1024x1024",
}


def _validate_media_input(value: str, field_name: str, param: str):
    """Verify media input is a valid URL or data URI"""
    if not isinstance(value, str) or not value.strip():
        raise ValidationException(
            message=f"{field_name} cannot be empty",
            param=param,
            code="empty_media",
        )
    value = value.strip()
    if value.startswith("data:"):
        return
    if value.startswith("http://") or value.startswith("https://"):
        return
    candidate = "".join(value.split())
    if len(candidate) >= 32 and len(candidate) % 4 == 0:
        try:
            base64.b64decode(candidate, validate=True)
            raise ValidationException(
                message=f"{field_name} base64 must be provided as a data URI (data:<mime>;base64,...)",
                param=param,
                code="invalid_media",
            )
        except binascii.Error:
            pass
    raise ValidationException(
        message=f"{field_name} must be a URL or data URI",
        param=param,
        code="invalid_media",
    )


def _extract_prompt_images(messages: List[MessageItem]) -> tuple[str, List[str]]:
    """Extract prompt text and image URLs from messages"""
    last_text = ""
    image_urls: List[str] = []

    for msg in messages:
        role = msg.role or "user"
        content = msg.content
        if isinstance(content, str):
            text = content.strip()
            if text:
                last_text = text
            continue
        if not isinstance(content, list):
            continue
        for block in content:
            if not isinstance(block, dict):
                continue
            block_type = block.get("type")
            if block_type == "text":
                text = block.get("text", "")
                if isinstance(text, str) and text.strip():
                    last_text = text.strip()
            elif block_type == "image_url" and role == "user":
                image = block.get("image_url") or {}
                url = image.get("url", "")
                if isinstance(url, str) and url.strip():
                    image_urls.append(url.strip())

    return last_text, image_urls


def _resolve_image_format(value: Optional[str]) -> str:
    fmt = value or get_config("app.image_format") or "url"
    if isinstance(fmt, str):
        fmt = fmt.lower()
    if fmt == "base64":
        return "b64_json"
    if fmt in ("b64_json", "url"):
        return fmt
    raise ValidationException(
        message="image_format must be one of url, base64, b64_json",
        param="image_format",
        code="invalid_image_format",
    )


def _image_field(response_format: str) -> str:
    if response_format == "url":
        return "url"
    return "b64_json"

def _validate_image_config(image_conf: ImageConfig, *, stream: bool):
    n = image_conf.n or 1
    if n < 1 or n > 10:
        raise ValidationException(
            message="n must be between 1 and 10",
            param="image_config.n",
            code="invalid_n",
        )
    if stream and n not in (1, 2):
        raise ValidationException(
            message="Streaming is only supported when n=1 or n=2",
            param="image_config.n",
            code="invalid_stream_n",
        )
    if image_conf.response_format:
        allowed_formats = {"b64_json", "base64", "url"}
        if image_conf.response_format not in allowed_formats:
            raise ValidationException(
                message="response_format must be one of b64_json, base64, url",
                param="image_config.response_format",
                code="invalid_response_format",
            )
    if image_conf.size and image_conf.size not in ALLOWED_IMAGE_SIZES:
        raise ValidationException(
            message=f"size must be one of {sorted(ALLOWED_IMAGE_SIZES)}",
            param="image_config.size",
            code="invalid_size",
        )
def validate_request(request: ChatCompletionRequest):
    """验证请求参数"""
    # 验证模型
    if not ModelService.valid(request.model):
        raise ValidationException(
            message=f"The model `{request.model}` does not exist or you do not have access to it.",
            param="model",
            code="model_not_found",
        )

    # 验证消息
    for idx, msg in enumerate(request.messages):
        if not isinstance(msg.role, str) or msg.role not in VALID_ROLES:
            raise ValidationException(
                message=f"role must be one of {sorted(VALID_ROLES)}",
                param=f"messages.{idx}.role",
                code="invalid_role",
            )
        content = msg.content

        # 字符串内容
        if isinstance(content, str):
            if not content.strip():
                raise ValidationException(
                    message="Message content cannot be empty",
                    param=f"messages.{idx}.content",
                    code="empty_content",
                )

        # 列表内容
        elif isinstance(content, list):
            if not content:
                raise ValidationException(
                    message="Message content cannot be an empty array",
                    param=f"messages.{idx}.content",
                    code="empty_content",
                )

            for block_idx, block in enumerate(content):
                # 检查空对象
                if not isinstance(block, dict):
                    raise ValidationException(
                        message="Content block must be an object",
                        param=f"messages.{idx}.content.{block_idx}",
                        code="invalid_block",
                    )
                if not block:
                    raise ValidationException(
                        message="Content block cannot be empty",
                        param=f"messages.{idx}.content.{block_idx}",
                        code="empty_block",
                    )

                # 检查 type 字段
                if "type" not in block:
                    raise ValidationException(
                        message="Content block must have a 'type' field",
                        param=f"messages.{idx}.content.{block_idx}",
                        code="missing_type",
                    )

                block_type = block.get("type")

                # 检查 type 空值
                if (
                    not block_type
                    or not isinstance(block_type, str)
                    or not block_type.strip()
                ):
                    raise ValidationException(
                        message="Content block 'type' cannot be empty",
                        param=f"messages.{idx}.content.{block_idx}.type",
                        code="empty_type",
                    )

                # 验证 type 有效性
                if msg.role == "user":
                    if block_type not in USER_CONTENT_TYPES:
                        raise ValidationException(
                            message=f"Invalid content block type: '{block_type}'",
                            param=f"messages.{idx}.content.{block_idx}.type",
                            code="invalid_type",
                        )
                else:
                    if block_type != "text":
                        raise ValidationException(
                            message=f"The `{msg.role}` role only supports 'text' type, got '{block_type}'",
                            param=f"messages.{idx}.content.{block_idx}.type",
                            code="invalid_type",
                        )

                # 验证字段是否存在 & 非空
                if block_type == "text":
                    text = block.get("text", "")
                    if not isinstance(text, str) or not text.strip():
                        raise ValidationException(
                            message="Text content cannot be empty",
                            param=f"messages.{idx}.content.{block_idx}.text",
                            code="empty_text",
                        )
                elif block_type == "image_url":
                    image_url = block.get("image_url")
                    if not image_url or not isinstance(image_url, dict):
                        raise ValidationException(
                            message="image_url must have a 'url' field",
                            param=f"messages.{idx}.content.{block_idx}.image_url",
                            code="missing_url",
                        )
                    _validate_media_input(
                        image_url.get("url", ""),
                        "image_url.url",
                        f"messages.{idx}.content.{block_idx}.image_url.url",
                    )
                elif block_type == "input_audio":
                    audio = block.get("input_audio")
                    if not audio or not isinstance(audio, dict):
                        raise ValidationException(
                            message="input_audio must have a 'data' field",
                            param=f"messages.{idx}.content.{block_idx}.input_audio",
                            code="missing_audio",
                        )
                    _validate_media_input(
                        audio.get("data", ""),
                        "input_audio.data",
                        f"messages.{idx}.content.{block_idx}.input_audio.data",
                    )
                elif block_type == "file":
                    file_data = block.get("file")
                    if not file_data or not isinstance(file_data, dict):
                        raise ValidationException(
                            message="file must have a 'file_data' field",
                            param=f"messages.{idx}.content.{block_idx}.file",
                            code="missing_file",
                        )
                    _validate_media_input(
                        file_data.get("file_data", ""),
                        "file.file_data",
                        f"messages.{idx}.content.{block_idx}.file.file_data",
                    )
        else:
            raise ValidationException(
                message="Message content must be a string or array",
                param=f"messages.{idx}.content",
                code="invalid_content",
            )

    # 默认验证
    if request.stream is not None:
        if isinstance(request.stream, bool):
            pass
        elif isinstance(request.stream, str):
            if request.stream.lower() in ("true", "1", "yes"):
                request.stream = True
            elif request.stream.lower() in ("false", "0", "no"):
                request.stream = False
            else:
                raise ValidationException(
                    message="stream must be a boolean",
                    param="stream",
                    code="invalid_stream",
                )
        else:
            raise ValidationException(
                message="stream must be a boolean",
                param="stream",
                code="invalid_stream",
            )

    allowed_efforts = {"none", "minimal", "low", "medium", "high", "xhigh"}
    if request.reasoning_effort is not None:
        if not isinstance(request.reasoning_effort, str) or (
            request.reasoning_effort not in allowed_efforts
        ):
            raise ValidationException(
                message=f"reasoning_effort must be one of {sorted(allowed_efforts)}",
                param="reasoning_effort",
                code="invalid_reasoning_effort",
            )

    if request.temperature is None:
        request.temperature = 0.8
    else:
        try:
            request.temperature = float(request.temperature)
        except Exception:
            raise ValidationException(
                message="temperature must be a float",
                param="temperature",
                code="invalid_temperature",
            )
        if not (0 <= request.temperature <= 2):
            raise ValidationException(
                message="temperature must be between 0 and 2",
                param="temperature",
                code="invalid_temperature",
            )

    if request.top_p is None:
        request.top_p = 0.95
    else:
        try:
            request.top_p = float(request.top_p)
        except Exception:
            raise ValidationException(
                message="top_p must be a float",
                param="top_p",
                code="invalid_top_p",
            )
        if not (0 <= request.top_p <= 1):
            raise ValidationException(
                message="top_p must be between 0 and 1",
                param="top_p",
                code="invalid_top_p",
            )

    model_info = ModelService.get(request.model)
    # image 验证
    if model_info and (model_info.is_image or model_info.is_image_edit):
        prompt, image_urls = _extract_prompt_images(request.messages)
        if not prompt:
            raise ValidationException(
                message="Prompt cannot be empty",
                param="messages",
                code="empty_prompt",
            )
        image_conf = request.image_config or ImageConfig()
        n = image_conf.n or 1
        if not (1 <= n <= 10):
            raise ValidationException(
                message="n must be between 1 and 10",
                param="image_config.n",
                code="invalid_n",
            )
        if request.stream and n not in (1, 2):
            raise ValidationException(
                message="Streaming is only supported when n=1 or n=2",
                param="stream",
                code="invalid_stream_n",
            )

        response_format = _resolve_image_format(image_conf.response_format)
        image_conf.n = n
        image_conf.response_format = response_format
        if not image_conf.size:
            image_conf.size = "1024x1024"
        allowed_sizes = {
            "1280x720",
            "720x1280",
            "1792x1024",
            "1024x1792",
            "1024x1024",
        }
        if image_conf.size not in allowed_sizes:
            raise ValidationException(
                message=f"size must be one of {sorted(allowed_sizes)}",
                param="image_config.size",
                code="invalid_size",
            )
        request.image_config = image_conf

    # image edit 验证
    if model_info and model_info.is_image_edit:
        _, image_urls = _extract_prompt_images(request.messages)
        if not image_urls:
            raise ValidationException(
                message="image_url is required for image edits",
                param="messages",
                code="missing_image",
            )

    # video 验证
    if model_info and model_info.is_video:
        config = request.video_config or VideoConfig()
        ratio_map = {
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
        if config.aspect_ratio is None:
            config.aspect_ratio = "3:2"
        if config.aspect_ratio not in ratio_map:
            raise ValidationException(
                message=f"aspect_ratio must be one of {list(ratio_map.keys())}",
                param="video_config.aspect_ratio",
                code="invalid_aspect_ratio",
            )
        config.aspect_ratio = ratio_map[config.aspect_ratio]

        if config.video_length not in (6, 10, 15):
            raise ValidationException(
                message="video_length must be 6, 10, or 15 seconds",
                param="video_config.video_length",
                code="invalid_video_length",
            )
        if config.resolution_name not in ("480p", "720p"):
            raise ValidationException(
                message="resolution_name must be one of ['480p', '720p']",
                param="video_config.resolution_name",
                code="invalid_resolution",
            )
        if config.preset not in ("fun", "normal", "spicy", "custom"):
            raise ValidationException(
                message="preset must be one of ['fun', 'normal', 'spicy', 'custom']",
                param="video_config.preset",
                code="invalid_preset",
            )
        request.video_config = config


router = APIRouter(tags=["Chat"])


@router.post("/chat/completions")
async def chat_completions(request: ChatCompletionRequest):
    """Chat Completions API - 兼容 OpenAI"""
    from app.core.logger import logger

    # 参数验证
    validate_request(request)

    logger.debug(f"Chat request: model={request.model}, stream={request.stream}")

    # 检测模型类型
    model_info = ModelService.get(request.model)
    if model_info and model_info.is_image_edit:
        prompt, image_urls = _extract_prompt_images(request.messages)
        if not image_urls:
            raise ValidationException(
                message="Image is required",
                param="image",
                code="missing_image",
            )
        image_url = image_urls[-1]

        is_stream = (
            request.stream if request.stream is not None else get_config("app.stream")
        )
        image_conf = request.image_config or ImageConfig()
        _validate_image_config(image_conf, stream=bool(is_stream))
        response_format = _resolve_image_format(image_conf.response_format)
        response_field = _image_field(response_format)
        n = image_conf.n or 1

        token_mgr = await get_token_manager()
        await token_mgr.reload_if_stale()

        token = None
        for pool_name in ModelService.pool_candidates_for_model(request.model):
            token = token_mgr.get_token(pool_name)
            if token:
                break

        if not token:
            raise AppException(
                message="No available tokens. Please try again later.",
                error_type=ErrorType.RATE_LIMIT.value,
                code="rate_limit_exceeded",
                status_code=429,
            )

        result = await ImageEditService().edit(
            token_mgr=token_mgr,
            token=token,
            model_info=model_info,
            prompt=prompt,
            images=[image_url],
            n=n,
            response_format=response_format,
            stream=bool(is_stream),
        )

        if result.stream:
            return StreamingResponse(
                result.data,
                media_type="text/event-stream",
                headers={"Cache-Control": "no-cache", "Connection": "keep-alive"},
            )

        data = [{response_field: img} for img in result.data]
        return JSONResponse(
            content={
                "created": int(time.time()),
                "data": data,
                "usage": {
                    "total_tokens": 0,
                    "input_tokens": 0,
                    "output_tokens": 0,
                    "input_tokens_details": {"text_tokens": 0, "image_tokens": 0},
                },
            }
        )

    if model_info and model_info.is_image:
        prompt, _ = _extract_prompt_images(request.messages)

        is_stream = (
            request.stream if request.stream is not None else get_config("app.stream")
        )
        image_conf = request.image_config or ImageConfig()
        _validate_image_config(image_conf, stream=bool(is_stream))
        response_format = _resolve_image_format(image_conf.response_format)
        response_field = _image_field(response_format)
        n = image_conf.n or 1
        size = image_conf.size or "1024x1024"
        aspect_ratio_map = {
            "1280x720": "16:9",
            "720x1280": "9:16",
            "1792x1024": "3:2",
            "1024x1792": "2:3",
            "1024x1024": "1:1",
        }
        aspect_ratio = aspect_ratio_map.get(size, "2:3")

        token_mgr = await get_token_manager()
        await token_mgr.reload_if_stale()

        token = None
        for pool_name in ModelService.pool_candidates_for_model(request.model):
            token = token_mgr.get_token(pool_name)
            if token:
                break

        if not token:
            raise AppException(
                message="No available tokens. Please try again later.",
                error_type=ErrorType.RATE_LIMIT.value,
                code="rate_limit_exceeded",
                status_code=429,
            )

        result = await ImageGenerationService().generate(
            token_mgr=token_mgr,
            token=token,
            model_info=model_info,
            prompt=prompt,
            n=n,
            response_format=response_format,
            size=size,
            aspect_ratio=aspect_ratio,
            stream=bool(is_stream),
        )

        if result.stream:
            return StreamingResponse(
                result.data,
                media_type="text/event-stream",
                headers={"Cache-Control": "no-cache", "Connection": "keep-alive"},
            )

        data = [{response_field: img} for img in result.data]
        usage = result.usage_override or {
            "total_tokens": 0,
            "input_tokens": 0,
            "output_tokens": 0,
            "input_tokens_details": {"text_tokens": 0, "image_tokens": 0},
        }
        return JSONResponse(
            content={
                "created": int(time.time()),
                "data": data,
                "usage": usage,
            }
        )

    if model_info and model_info.is_video:
        # 提取视频配置 (默认值在 Pydantic 模型中处理)
        v_conf = request.video_config or VideoConfig()

        result = await VideoService.completions(
            model=request.model,
            messages=[msg.model_dump() for msg in request.messages],
            stream=request.stream,
            reasoning_effort=request.reasoning_effort,
            aspect_ratio=v_conf.aspect_ratio,
            video_length=v_conf.video_length,
            resolution=v_conf.resolution_name,
            preset=v_conf.preset,
        )
    else:
        result = await ChatService.completions(
            model=request.model,
            messages=[msg.model_dump() for msg in request.messages],
            stream=request.stream,
            reasoning_effort=request.reasoning_effort,
            temperature=request.temperature,
            top_p=request.top_p,
        )

    if isinstance(result, dict):
        return JSONResponse(content=result)
    else:
        return StreamingResponse(
            result,
            media_type="text/event-stream",
            headers={"Cache-Control": "no-cache", "Connection": "keep-alive"},
        )


__all__ = ["router"]
