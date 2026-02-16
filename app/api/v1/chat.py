"""
Chat Completions API 路由
"""

import time
from typing import Any, Dict, List, Optional, Union

from fastapi import APIRouter
from fastapi.responses import StreamingResponse, JSONResponse, Response
from pydantic import BaseModel, Field, field_validator

from app.services.grok.services.chat import ChatService
from app.services.grok.models.model import ModelService
from app.core.exceptions import ValidationException
from app.api.v1.image import ImageGenerationRequest, create_image


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
    video_length: Optional[int] = Field(6, description="视频时长(秒): 6 / 10 / 15")
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
            if v not in (6, 10, 15):
                raise ValidationException(
                    message="video_length must be 6, 10, or 15 seconds",
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

    # 图片生成兼容参数（用于将误发到 /chat/completions 的图片请求自动转发到 /images/generations）
    n: Optional[int] = Field(1, ge=1, le=10, description="图片数量")
    size: Optional[str] = Field("1024x1024", description="图片尺寸")
    quality: Optional[str] = Field("standard", description="图片质量")
    response_format: Optional[str] = Field(None, description="图片响应格式")
    style: Optional[str] = Field(None, description="图片风格")

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




def _is_rephrase_template_text(text: str) -> bool:
    """识别外部客户端注入的 websearch/rephrase 模板提示词。"""
    if not text:
        return False
    lower = text.lower()
    markers = [
        "use user's language to rephrase the question",
        "your role is to rephrase follow-up queries",
        "<websearch>",
        "follow up question:",
        "there are several examples attached",
        "use websearch to rephrase",
    ]
    hit = sum(1 for m in markers if m in lower)
    if hit >= 2 or ("<examples>" in lower and "follow up question" in lower):
        return True
    return (len(text) > 500) and ("guidelines:" in lower or "1." in lower and "2." in lower)


def _extract_image_prompt(messages: List[MessageItem]) -> str:
    """从 chat messages 提取图片 prompt（仅保留客户端原始用户输入）。"""

    for msg in reversed(messages):
        if msg.role != "user":
            continue

        content = msg.content
        text = ""
        if isinstance(content, str):
            text = content.strip()
        elif isinstance(content, list):
            parts = []
            for item in content:
                if item.get("type") == "text":
                    txt = str(item.get("text", "")).strip()
                    if txt:
                        parts.append(txt)
            text = "\n".join(parts).strip()

        if not text:
            continue

        # 严格跳过注入模板：不再尝试从模板中提取 follow-up question，
        # 以保证上游仅收到用户原始请求文本。
        if _is_rephrase_template_text(text):
            continue

        # 非模板文本，直接作为原始 prompt
        return text

    raise ValidationException(
        message="Image prompt cannot be extracted from messages (only template text found)",
        param="messages",
        code="empty_prompt",
    )


def _format_image_markdown_lines(data: List[Dict[str, Any]]) -> str:
    """将图片 URL 列表格式化为 Markdown 图片语法，便于客户端直接预览。"""
    lines = []
    for idx, item in enumerate(data, start=1):
        if not isinstance(item, dict):
            continue
        u = item.get("url")
        if not u:
            continue
        alt = str(item.get("id") or item.get("image_id") or f"image-{idx}")
        lines.append(f"![{alt}]({u})")
    return "\n".join(lines)


def _to_chat_completion_from_image_response(image_resp: Response, model: str) -> JSONResponse:
    """将 /images/generations 响应转为 chat.completions 非流式格式，便于外部客户端兼容。"""
    payload = {}
    try:
        import json

        body = getattr(image_resp, "body", b"") or b""
        if isinstance(body, (bytes, bytearray)):
            payload = json.loads(body.decode("utf-8") or "{}")
    except Exception:
        payload = {}

    data = payload.get("data") or []
    content = _format_image_markdown_lines(data)
    if not content:
        urls = []
        for item in data:
            if isinstance(item, dict):
                u = item.get("url") or item.get("b64_json") or item.get("base64")
                if u:
                    urls.append(str(u))
        content = "\n".join(urls)
    return JSONResponse(
        content={
            "id": f"chatcmpl-{int(time.time() * 1000)}",
            "object": "chat.completion",
            "created": int(time.time()),
            "model": model,
            "choices": [
                {
                    "index": 0,
                    "message": {"role": "assistant", "content": content},
                    "finish_reason": "stop",
                }
            ],
            "usage": payload.get("usage")
            or {
                "prompt_tokens": 0,
                "completion_tokens": 0,
                "total_tokens": 0,
                "prompt_tokens_details": {
                    "cached_tokens": 0,
                    "text_tokens": 0,
                    "audio_tokens": 0,
                    "image_tokens": 0,
                },
                "completion_tokens_details": {
                    "text_tokens": 0,
                    "audio_tokens": 0,
                    "reasoning_tokens": 0,
                },
            },
            "images": data,
        }
    )


def _to_chat_stream_from_image_response(image_resp: Response, model: str) -> StreamingResponse:
    """将 /images/generations 响应转为 chat.completions 流式格式。"""
    payload = {}
    try:
        import json

        body = getattr(image_resp, "body", b"") or b""
        if isinstance(body, (bytes, bytearray)):
            payload = json.loads(body.decode("utf-8") or "{}")
    except Exception:
        payload = {}

    data = payload.get("data") or []
    content = _format_image_markdown_lines(data)
    if not content:
        urls = []
        for item in data:
            if isinstance(item, dict):
                u = item.get("url")
                if u:
                    urls.append(str(u))
        content = "\n".join(urls)
    chunk_id = f"chatcmpl-{int(time.time() * 1000)}"
    created = int(time.time())

    async def gen():
        import json

        first_chunk = {
            "id": chunk_id,
            "object": "chat.completion.chunk",
            "created": created,
            "model": model,
            "choices": [
                {
                    "index": 0,
                    "delta": {"role": "assistant", "content": content},
                    "finish_reason": None,
                }
            ],
        }
        done_chunk = {
            "id": chunk_id,
            "object": "chat.completion.chunk",
            "created": created,
            "model": model,
            "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
            "usage": payload.get("usage"),
        }

        yield f"data: {json.dumps(first_chunk, ensure_ascii=False)}\n\n"
        yield f"data: {json.dumps(done_chunk, ensure_ascii=False)}\n\n"
        yield "data: [DONE]\n\n"

    return StreamingResponse(
        gen(),
        media_type="text/event-stream",
        headers={"Cache-Control": "no-cache", "Connection": "keep-alive"},
    )

@router.post("/chat/completions")
async def chat_completions(request: ChatCompletionRequest):
    """Chat Completions API - 兼容 OpenAI"""
    from app.core.logger import logger

    # 参数验证
    validate_request(request)

    logger.debug(f"Chat request: model={request.model}, stream={request.stream}")

    # 模型路由：图片模型自动兼容转发到 /v1/images/generations
    model_info = ModelService.get(request.model)
    if model_info and request.model == "grok-superimage-1.0":
        prompt = _extract_image_prompt(request.messages)
        logger.info(f"Compat route superimage -> images/generations, prompt={prompt[:120]}...")

        image_request = ImageGenerationRequest(
            prompt=prompt,
            model=request.model,
            n=request.n or 1,
            size=request.size or "1024x1024",
            quality=request.quality or "standard",
            response_format="url",
            style=request.style,
            stream=False,
        )
        image_resp = await create_image(image_request)
        if request.stream:
            return _to_chat_stream_from_image_response(image_resp, request.model)
        return _to_chat_completion_from_image_response(image_resp, request.model)

    # 检测视频模型
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


__all__ = ["router"]
