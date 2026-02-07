"""
Chat Completions API 路由
"""

from typing import Any, Dict, List, Optional, Union

from fastapi import APIRouter
from fastapi.responses import StreamingResponse, JSONResponse
from pydantic import BaseModel, Field, field_validator

from app.services.grok.services.chat import ChatService
from app.services.grok.models.model import ModelService
from app.core.exceptions import ValidationException


router = APIRouter(tags=["Chat"])


VALID_ROLES = ["developer", "system", "user", "assistant", "tool"]
# 角色别名映射 (OpenAI 兼容: function -> tool)
ROLE_ALIASES = {"function": "tool"}
USER_CONTENT_TYPES = ["text", "image_url", "input_audio", "file"]


class MessageItem(BaseModel):
    """消息项"""

    role: str
    content: Union[str, List[Dict[str, Any]]]
    tool_call_id: Optional[str] = None  # tool 角色需要的字段
    name: Optional[str] = None  # function 角色的函数名

    @field_validator("role")
    @classmethod
    def validate_role(cls, v):
        # 大小写归一化
        v_lower = v.lower() if isinstance(v, str) else v
        # 别名映射
        v_normalized = ROLE_ALIASES.get(v_lower, v_lower)
        if v_normalized not in VALID_ROLES:
            raise ValueError(f"role must be one of {VALID_ROLES}")
        return v_normalized


class VideoConfig(BaseModel):
    """视频生成配置"""

    aspect_ratio: Optional[str] = Field(
        "3:2", description="视频比例: 3:2, 16:9, 1:1 等"
    )
    video_length: Optional[int] = Field(6, description="视频时长(秒): 6 或 10")
    resolution_name: Optional[str] = Field("480p", description="视频分辨率: 480p, 720p")
    preset: Optional[str] = Field("custom", description="风格预设: fun, normal, spicy")

    @field_validator("aspect_ratio")
    @classmethod
    def validate_aspect_ratio(cls, v):
        allowed = ["2:3", "3:2", "1:1", "9:16", "16:9"]
        if v and v not in allowed:
            raise ValidationException(
                message=f"aspect_ratio must be one of {allowed}",
                param="video_config.aspect_ratio",
                code="invalid_aspect_ratio",
            )
        return v

    @field_validator("video_length")
    @classmethod
    def validate_video_length(cls, v):
        if v is not None:
            if v not in (6, 10):
                raise ValidationException(
                    message="video_length must be 6 or 10 seconds",
                    param="video_config.video_length",
                    code="invalid_video_length",
                )
        return v

    @field_validator("resolution_name")
    @classmethod
    def validate_resolution(cls, v):
        allowed = ["480p", "720p"]
        if v and v not in allowed:
            raise ValidationException(
                message=f"resolution_name must be one of {allowed}",
                param="video_config.resolution_name",
                code="invalid_resolution",
            )
        return v

    @field_validator("preset")
    @classmethod
    def validate_preset(cls, v):
        # 允许为空，默认 custom
        if not v:
            return "custom"
        allowed = ["fun", "normal", "spicy", "custom"]
        if v not in allowed:
            raise ValidationException(
                message=f"preset must be one of {allowed}",
                param="video_config.preset",
                code="invalid_preset",
            )
        return v


class ChatCompletionRequest(BaseModel):
    """Chat Completions 请求"""

    model: str = Field(..., description="模型名称")
    messages: List[MessageItem] = Field(..., description="消息数组")
    stream: Optional[bool] = Field(None, description="是否流式输出")
    thinking: Optional[str] = Field(None, description="思考模式: enabled/disabled/None")

    # 视频生成配置
    video_config: Optional[VideoConfig] = Field(None, description="视频生成参数")

    @field_validator("stream", mode="before")
    @classmethod
    def validate_stream(cls, v):
        """确保 stream 参数被正确解析为布尔值"""
        if v is None:
            return None
        if isinstance(v, bool):
            return v
        if isinstance(v, str):
            if v.lower() in ("true", "1", "yes"):
                return True
            if v.lower() in ("false", "0", "no"):
                return False
            # 未识别的字符串值抛出错误
            raise ValueError(
                f"Invalid stream value '{v}'. Must be a boolean or one of: true, false, 1, 0, yes, no"
            )
        # 非布尔非字符串类型抛出错误
        raise ValueError(
            f"Invalid stream value type '{type(v).__name__}'. Must be a boolean or string."
        )

    model_config = {"extra": "ignore"}


class VideoUpscaleRequest(BaseModel):
    """视频超分请求"""
    
    video_id: str = Field(..., description="视频 ID")
    model: str = Field("grok-imagine-1.0-video", description="模型名称")
    stream: bool = Field(False, description="是否流式输出 (暂不支持)")


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
                elif msg.role in ("tool", "function"):
                    # tool/function 角色只支持 text 类型，但内容可以是 JSON 字符串
                    if block_type != "text":
                        raise ValidationException(
                            message=f"The `{msg.role}` role only supports 'text' type, got '{block_type}'",
                            param=f"messages.{idx}.content.{block_idx}.type",
                            code="invalid_type",
                        )
                elif block_type != "text":
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
                    if not image_url or not (
                        isinstance(image_url, dict) and image_url.get("url")
                    ):
                        raise ValidationException(
                            message="image_url must have a 'url' field",
                            param=f"messages.{idx}.content.{block_idx}.image_url",
                            code="missing_url",
                        )


@router.post("/chat/completions")
async def chat_completions(request: ChatCompletionRequest):
    """Chat Completions API - 兼容 OpenAI"""
    from app.core.logger import logger

    # 参数验证
    validate_request(request)

    logger.debug(f"Chat request: model={request.model}, stream={request.stream}")

    # 检测视频模型
    model_info = ModelService.get(request.model)
    if model_info and model_info.is_video:
        from app.services.grok.services.media import VideoService

        # 提取视频配置 (默认值在 Pydantic 模型中处理)
        v_conf = request.video_config or VideoConfig()

        result = await VideoService.completions(
            model=request.model,
            messages=[msg.model_dump() for msg in request.messages],
            stream=request.stream,
            thinking=request.thinking,
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
            thinking=request.thinking,
        )

    if isinstance(result, dict):
        return JSONResponse(content=result)
    else:
        return StreamingResponse(
            result,
            media_type="text/event-stream",
            headers={"Cache-Control": "no-cache", "Connection": "keep-alive"},
        )


@router.post("/video/upscale")
async def upscale_video(request: VideoUpscaleRequest):
    """
    视频超分接口

    直接调用 Grok 的 upscale 接口对指定视频 ID 进行高清放大。
    """
    from app.services.grok.services.media import VideoService
    from app.services.token import get_token_manager, EffortType
    from app.services.grok.models.model import ModelService
    from app.core.exceptions import AppException, ErrorType
    from app.core.logger import logger

    # 1. 获取 Token
    try:
        token_mgr = await get_token_manager()
        await token_mgr.reload_if_stale()
        token = None
        # 尝试获取可用 token
        for pool_name in ModelService.pool_candidates_for_model(request.model):
            token = token_mgr.get_token(pool_name)
            if token:
                break
    except Exception as e:
        logger.error(f"Failed to get token: {e}")
        raise AppException(
            message="Internal service error obtaining token",
            error_type=ErrorType.SERVER.value,
            code="internal_error",
        )

    if not token:
        raise AppException(
            message="No available tokens. Please try again later.",
            error_type=ErrorType.RATE_LIMIT.value,
            code="rate_limit_exceeded",
            status_code=429,
        )

    # 2. 调用 upscale
    service = VideoService()
    try:
        # 记录开始
        logger.info(f"Starting upscale for video {request.video_id} with token {token[:10]}...")
        result = await service.upscale(token, request.video_id)

        # 记录使用量
        try:
            model_info = ModelService.get(request.model)
            effort = (
                EffortType.HIGH
                if (model_info and model_info.cost.value == "high")
                else EffortType.LOW
            )
            await token_mgr.consume(token, effort)
        except Exception as e:
            logger.warning(f"Failed to record upscale usage: {e}")

        return JSONResponse(content=result)

    except Exception as e:
        logger.error(f"Upscale failed: {e}")
        if isinstance(e, AppException):
            raise e
        raise AppException(
            message=f"Upscale failed: {str(e)}",
            error_type=ErrorType.SERVER.value,
            code="upscale_error",
        )


__all__ = ["router"]
