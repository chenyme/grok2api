"""
Chat Completions API 路由
"""

from typing import Any, Dict, List, Optional, Union

from fastapi import APIRouter
from fastapi.responses import StreamingResponse, JSONResponse
from pydantic import BaseModel, Field, field_validator

from app.services.grok.chat import ChatService
from app.services.grok.model import ModelService
from app.core.exceptions import ValidationException


router = APIRouter(tags=["Chat"])


VALID_ROLES = ["developer", "system", "user", "assistant"]
USER_CONTENT_TYPES = ["text", "image_url", "input_audio", "file"]


class MessageItem(BaseModel):
    """消息项"""
    role: str
    content: Union[str, List[Dict[str, Any]]]
    
    @field_validator("role")
    @classmethod
    def validate_role(cls, v):
        if v not in VALID_ROLES:
            raise ValueError(f"role must be one of {VALID_ROLES}")
        return v


class VideoConfig(BaseModel):
    """视频生成配置"""
    aspect_ratio: Optional[str] = Field("3:2", description="视频比例: 3:2, 16:9, 1:1 等")
    video_length: Optional[int] = Field(6, description="视频时长(秒): 5-15")
    resolution: Optional[str] = Field("SD", description="视频分辨率: SD, HD")
    preset: Optional[str] = Field("custom", description="风格预设: fun, normal, spicy")
    
    @field_validator("aspect_ratio")
    @classmethod
    def validate_aspect_ratio(cls, v):
        allowed = ["2:3", "3:2", "1:1", "9:16", "16:9"]
        if v and v not in allowed:
            raise ValidationException(
                message=f"aspect_ratio must be one of {allowed}",
                param="video_config.aspect_ratio",
                code="invalid_aspect_ratio"
            )
        return v
    
    @field_validator("video_length")
    @classmethod
    def validate_video_length(cls, v):
        if v is not None:
            if v < 5 or v > 15:
                raise ValidationException(
                    message="video_length must be between 5 and 15 seconds",
                    param="video_config.video_length",
                    code="invalid_video_length"
                )
        return v

    @field_validator("resolution")
    @classmethod
    def validate_resolution(cls, v):
        allowed = ["SD", "HD"]
        if v and v not in allowed:
            raise ValidationException(
                message=f"resolution must be one of {allowed}",
                param="video_config.resolution",
                code="invalid_resolution"
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
                code="invalid_preset"
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
    
    model_config = {
        "extra": "ignore"
    }


def validate_request(request: ChatCompletionRequest):
    """验证请求参数"""
    # 验证模型
    if not ModelService.valid(request.model):
        raise ValidationException(
            message=f"The model `{request.model}` does not exist or you do not have access to it.",
            param="model",
            code="model_not_found"
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
                    code="empty_content"
                )
        
        # 列表内容
        elif isinstance(content, list):
            if not content:
                raise ValidationException(
                    message="Message content cannot be an empty array",
                    param=f"messages.{idx}.content",
                    code="empty_content"
                )
            
            for block_idx, block in enumerate(content):
                # 检查空对象
                if not block:
                    raise ValidationException(
                        message="Content block cannot be empty",
                        param=f"messages.{idx}.content.{block_idx}",
                        code="empty_block"
                    )
                
                # 检查 type 字段
                if "type" not in block:
                    raise ValidationException(
                        message="Content block must have a 'type' field",
                        param=f"messages.{idx}.content.{block_idx}",
                        code="missing_type"
                    )
                
                block_type = block.get("type")
                
                # 检查 type 空值
                if not block_type or not isinstance(block_type, str) or not block_type.strip():
                    raise ValidationException(
                        message="Content block 'type' cannot be empty",
                        param=f"messages.{idx}.content.{block_idx}.type",
                        code="empty_type"
                    )
                
                # 验证 type 有效性
                if msg.role == "user":
                    if block_type not in USER_CONTENT_TYPES:
                        raise ValidationException(
                            message=f"Invalid content block type: '{block_type}'",
                            param=f"messages.{idx}.content.{block_idx}.type",
                            code="invalid_type"
                        )
                elif block_type != "text":
                    raise ValidationException(
                        message=f"The `{msg.role}` role only supports 'text' type, got '{block_type}'",
                        param=f"messages.{idx}.content.{block_idx}.type",
                        code="invalid_type"
                    )
                
                # 验证字段是否存在 & 非空
                if block_type == "text":
                    text = block.get("text", "")
                    if not isinstance(text, str) or not text.strip():
                        raise ValidationException(
                            message="Text content cannot be empty",
                            param=f"messages.{idx}.content.{block_idx}.text",
                            code="empty_text"
                        )
                elif block_type == "image_url":
                    image_url = block.get("image_url")
                    if not image_url or not (isinstance(image_url, dict) and image_url.get("url")):
                        raise ValidationException(
                            message="image_url must have a 'url' field",
                            param=f"messages.{idx}.content.{block_idx}.image_url",
                            code="missing_url"
                        )


@router.post("/chat/completions")
async def chat_completions(request: ChatCompletionRequest):
    """Chat Completions API - 兼容 OpenAI"""
    
    # 参数验证
    validate_request(request)
    
    # 检测视频模型
    model_info = ModelService.get(request.model)
    if model_info and model_info.is_video:
        from app.services.grok.media import VideoService
        
        # 提取视频配置 (默认值在 Pydantic 模型中处理)
        v_conf = request.video_config or VideoConfig()
        
        result = await VideoService.completions(
            model=request.model,
            messages=[msg.model_dump() for msg in request.messages],
            stream=request.stream,
            thinking=request.thinking,
            aspect_ratio=v_conf.aspect_ratio,
            video_length=v_conf.video_length,
            resolution=v_conf.resolution,
            preset=v_conf.preset
        )
    else:
        result = await ChatService.completions(
            model=request.model,
            messages=[msg.model_dump() for msg in request.messages],
            stream=request.stream,
            thinking=request.thinking
        )
    
    if isinstance(result, dict):
        return JSONResponse(content=result)
    else:
        return StreamingResponse(
            result,
            media_type="text/event-stream",
            headers={"Cache-Control": "no-cache", "Connection": "keep-alive"}
        )


__all__ = ["router"]
