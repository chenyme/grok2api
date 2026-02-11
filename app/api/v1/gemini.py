"""
Gemini 原生格式兼容路由

提供 Gemini `generateContent` / `streamGenerateContent` 兼容接口，
并将内部 OpenAI 兼容响应转换为 Gemini 响应格式。
"""

import orjson
from typing import Any, AsyncGenerator, Dict, List, Optional

from fastapi import APIRouter, Query
from fastapi.responses import StreamingResponse
from pydantic import BaseModel, Field

from app.core.exceptions import ValidationException
from app.core.json_response import StrictJSONResponse
from app.services.grok.models.model import ModelService
from app.services.grok.services.chat import ChatService
from app.api.v1.image import ImageGenerationRequest, create_image


router = APIRouter(tags=["Gemini"])


class GeminiInlineData(BaseModel):
    """Gemini part.inlineData 结构。"""

    mimeType: Optional[str] = None
    data: Optional[str] = None


class GeminiFileData(BaseModel):
    """Gemini part.fileData 结构。"""

    mimeType: Optional[str] = None
    fileUri: Optional[str] = None


class GeminiPart(BaseModel):
    """Gemini content part。"""

    text: Optional[str] = None
    inlineData: Optional[GeminiInlineData] = None
    fileData: Optional[GeminiFileData] = None


class GeminiContent(BaseModel):
    """Gemini content。"""

    role: Optional[str] = "user"
    parts: List[GeminiPart] = Field(default_factory=list)


class GeminiGenerationConfig(BaseModel):
    """Gemini generationConfig（当前仅使用部分字段）。"""

    candidateCount: Optional[int] = 1
    responseModalities: Optional[List[str]] = None


class GeminiGenerateRequest(BaseModel):
    """Gemini generateContent 请求体。"""

    contents: List[GeminiContent] = Field(default_factory=list)
    generationConfig: Optional[GeminiGenerationConfig] = None

    model_config = {"extra": "ignore"}


# Gemini 模型别名映射：将常见 Gemini 模型名映射到内部模型
GEMINI_MODEL_ALIASES = {
    "gemini-2.5-pro": "grok-4.1-thinking",
    "gemini-2.5-flash": "grok-4.1-fast",
    "gemini-2.0-flash": "grok-4-mini",
    "gemini-1.5-pro": "grok-4",
    "gemini-1.5-flash": "grok-4-mini",
    "gemini-2.0-flash-exp-image-generation": "grok-imagine-1.0",
    "gemini-2.0-flash-preview-image-generation": "grok-imagine-1.0",
    "imagen-3.0-generate-002": "grok-imagine-1.0",
    "imagen-3.0-fast-generate-001": "grok-imagine-1.0",
}


def _normalize_model_name(model_name: str) -> str:
    """标准化 Gemini path 中的模型名称。"""
    name = (model_name or "").strip()
    if name.startswith("models/"):
        name = name.split("/", 1)[1]
    return name


def _resolve_internal_model(model_name: str) -> str:
    """将 Gemini 模型名解析为内部模型名。"""
    normalized = _normalize_model_name(model_name).lower()

    if normalized in GEMINI_MODEL_ALIASES:
        return GEMINI_MODEL_ALIASES[normalized]

    # 如果调用方直接传了内部模型名，则原样使用
    raw = _normalize_model_name(model_name)
    if ModelService.valid(raw):
        return raw

    # 对包含 image/imagen/imagine 的未知 Gemini 模型做生图兜底
    if any(keyword in normalized for keyword in ("image", "imagen", "imagine")):
        return "grok-imagine-1.0"

    # 文本兜底模型
    return "grok-4-mini"


def _is_image_generation_request(
    request: GeminiGenerateRequest, internal_model: str
) -> bool:
    """判断是否为生图请求。"""
    model_info = ModelService.get(internal_model)
    if model_info and model_info.is_image:
        return True

    modalities = (
        request.generationConfig.responseModalities
        if request.generationConfig and request.generationConfig.responseModalities
        else []
    )
    upper_modalities = {str(m).upper() for m in modalities}
    return "IMAGE" in upper_modalities


def _gemini_contents_to_openai_messages(contents: List[GeminiContent]) -> List[Dict[str, Any]]:
    """将 Gemini contents 转为内部 OpenAI 兼容 messages。"""
    messages: List[Dict[str, Any]] = []

    for content in contents:
        role = (content.role or "user").lower()
        if role == "model":
            # Gemini model -> OpenAI assistant
            mapped_role = "assistant"
        elif role in {"user", "assistant", "system", "tool", "developer"}:
            mapped_role = role
        else:
            mapped_role = "user"

        blocks: List[Dict[str, Any]] = []
        for part in content.parts:
            # 文本 part
            if part.text and part.text.strip():
                blocks.append({"type": "text", "text": part.text})

            # inlineData 支持图片输入
            if part.inlineData and part.inlineData.data:
                mime_type = part.inlineData.mimeType or "application/octet-stream"
                data_url = f"data:{mime_type};base64,{part.inlineData.data}"
                if mime_type.startswith("image/"):
                    blocks.append({"type": "image_url", "image_url": {"url": data_url}})
                else:
                    blocks.append({"type": "file", "file": {"data": data_url}})

            # fileData 支持文件 URL
            if part.fileData and part.fileData.fileUri:
                file_url = part.fileData.fileUri
                mime_type = part.fileData.mimeType or ""
                if mime_type.startswith("image/"):
                    blocks.append({"type": "image_url", "image_url": {"url": file_url}})
                else:
                    blocks.append({"type": "file", "file": {"url": file_url}})

        if not blocks:
            continue

        messages.append({"role": mapped_role, "content": blocks})

    return messages


def _extract_image_prompt(contents: List[GeminiContent]) -> str:
    """从 Gemini contents 提取生图 prompt。"""
    texts: List[str] = []
    for content in contents:
        if (content.role or "user").lower() not in {"user", "system"}:
            continue
        for part in content.parts:
            if part.text and part.text.strip():
                texts.append(part.text.strip())

    prompt = "\n".join(texts).strip()
    if not prompt:
        raise ValidationException(
            message="Gemini image generation requires non-empty text prompt in contents.parts.text",
            param="contents",
            code="empty_prompt",
        )
    return prompt


def _build_gemini_text_response(text: str, model_name: str) -> Dict[str, Any]:
    """构造 Gemini 文本非流式响应。"""
    return {
        "candidates": [
            {
                "content": {"role": "model", "parts": [{"text": text or ""}]},
                "finishReason": "STOP",
                "index": 0,
            }
        ],
        "modelVersion": _normalize_model_name(model_name),
        "usageMetadata": {
            "promptTokenCount": 0,
            "candidatesTokenCount": 0,
            "totalTokenCount": 0,
        },
    }


def _build_gemini_image_response(images_b64: List[str], model_name: str) -> Dict[str, Any]:
    """构造 Gemini 生图非流式响应（base64 inlineData）。"""
    candidates = []
    for idx, data in enumerate(images_b64):
        candidates.append(
            {
                "content": {
                    "role": "model",
                    "parts": [
                        {
                            "inlineData": {
                                # Grok 输出大多为 jpg，使用通用 image/jpeg 兼容主流 Gemini 客户端
                                "mimeType": "image/jpeg",
                                "data": data,
                            }
                        }
                    ],
                },
                "finishReason": "STOP",
                "index": idx,
            }
        )

    return {
        "candidates": candidates,
        "modelVersion": _normalize_model_name(model_name),
        "usageMetadata": {
            "promptTokenCount": 0,
            "candidatesTokenCount": 0,
            "totalTokenCount": 0,
        },
    }


def _parse_openai_sse_chunk(chunk: str) -> Optional[Dict[str, Any]]:
    """解析 OpenAI SSE chunk，提取 JSON。"""
    text = (chunk or "").strip()
    if not text or not text.startswith("data:"):
        return None
    payload = text[5:].strip()
    if not payload or payload == "[DONE]":
        return None
    try:
        return orjson.loads(payload)
    except orjson.JSONDecodeError:
        return None


async def _stream_text_as_gemini(
    stream: AsyncGenerator[str, None], model_name: str
) -> AsyncGenerator[str, None]:
    """将内部 OpenAI SSE 流转换为 Gemini SSE 流。"""
    async for chunk in stream:
        payload = _parse_openai_sse_chunk(chunk)
        if not payload:
            continue

        choices = payload.get("choices") or []
        if not choices:
            continue

        choice = choices[0]
        delta = choice.get("delta") or {}
        text_piece = delta.get("content")
        finish_reason = choice.get("finish_reason")

        # 中间增量：输出文本分片
        if text_piece:
            gemini_chunk = {
                "candidates": [
                    {
                        "content": {"role": "model", "parts": [{"text": text_piece}]},
                        "index": 0,
                    }
                ],
                "modelVersion": _normalize_model_name(model_name),
            }
            yield f"data: {orjson.dumps(gemini_chunk).decode()}\n\n"

        # 结束标记：输出 Gemini finishReason
        if finish_reason:
            final_chunk = {
                "candidates": [
                    {
                        "content": {"role": "model", "parts": []},
                        "finishReason": "STOP",
                        "index": 0,
                    }
                ],
                "modelVersion": _normalize_model_name(model_name),
            }
            yield f"data: {orjson.dumps(final_chunk).decode()}\n\n"


async def _stream_image_as_gemini(
    images_b64: List[str], model_name: str
) -> AsyncGenerator[str, None]:
    """将非流式生图结果按 Gemini SSE 分片输出。"""
    for idx, data in enumerate(images_b64):
        chunk = {
            "candidates": [
                {
                    "content": {
                        "role": "model",
                        "parts": [
                            {
                                "inlineData": {
                                    "mimeType": "image/jpeg",
                                    "data": data,
                                }
                            }
                        ],
                    },
                    "index": idx,
                }
            ],
            "modelVersion": _normalize_model_name(model_name),
        }
        yield f"data: {orjson.dumps(chunk).decode()}\n\n"

    # 最终收尾 chunk
    final_chunk = {
        "candidates": [
            {"content": {"role": "model", "parts": []}, "finishReason": "STOP", "index": 0}
        ],
        "modelVersion": _normalize_model_name(model_name),
    }
    yield f"data: {orjson.dumps(final_chunk).decode()}\n\n"


async def _generate_image_b64(request: GeminiGenerateRequest, n: int) -> List[str]:
    """调用现有图片接口，返回 base64 图片数组。"""
    prompt = _extract_image_prompt(request.contents)

    image_resp = await create_image(
        ImageGenerationRequest(
            prompt=prompt,
            model="grok-imagine-1.0",
            n=max(1, min(n, 10)),
            response_format="b64_json",
            stream=False,
        )
    )

    raw = orjson.loads(image_resp.body)
    data_items = raw.get("data") or []

    images_b64: List[str] = []
    for item in data_items:
        b64 = item.get("b64_json") or item.get("base64")
        if b64 and b64 != "error":
            images_b64.append(b64)

    return images_b64


async def _handle_generate(
    model_name: str,
    request: GeminiGenerateRequest,
    stream: bool,
):
    """Gemini generate/stream 统一处理入口。"""
    internal_model = _resolve_internal_model(model_name)

    # Gemini image generation：强制 base64 返回（inlineData.data）
    if _is_image_generation_request(request, internal_model):
        n = 1
        if request.generationConfig and request.generationConfig.candidateCount:
            n = request.generationConfig.candidateCount

        images_b64 = await _generate_image_b64(request, n=n)

        if stream:
            return StreamingResponse(
                _stream_image_as_gemini(images_b64, model_name),
                media_type="text/event-stream",
                headers={"Cache-Control": "no-cache", "Connection": "keep-alive"},
            )

        return StrictJSONResponse(content=_build_gemini_image_response(images_b64, model_name))

    # Gemini 文本/多模态理解 -> 复用 chat completions
    messages = _gemini_contents_to_openai_messages(request.contents)
    if not messages:
        raise ValidationException(
            message="Gemini request requires non-empty contents.parts",
            param="contents",
            code="empty_content",
        )

    result = await ChatService.completions(
        model=internal_model,
        messages=messages,
        stream=stream,
        thinking=None,
    )

    if stream:
        return StreamingResponse(
            _stream_text_as_gemini(result, model_name),
            media_type="text/event-stream",
            headers={"Cache-Control": "no-cache", "Connection": "keep-alive"},
        )

    text = (
        (result.get("choices") or [{}])[0]
        .get("message", {})
        .get("content", "")
    )
    return StrictJSONResponse(content=_build_gemini_text_response(text, model_name))


@router.post("/v1beta/models/{model_name}:generateContent")
async def gemini_generate_content(model_name: str, request: GeminiGenerateRequest):
    """Gemini 非流式接口：generateContent。"""
    return await _handle_generate(model_name=model_name, request=request, stream=False)


@router.post("/v1/models/{model_name}:generateContent")
async def gemini_generate_content_v1(model_name: str, request: GeminiGenerateRequest):
    """Gemini 非流式接口（v1 别名）。"""
    return await _handle_generate(model_name=model_name, request=request, stream=False)


@router.post("/v1beta/models/{model_name}:streamGenerateContent")
async def gemini_stream_generate_content(
    model_name: str,
    request: GeminiGenerateRequest,
    alt: Optional[str] = Query(default=None),
):
    """Gemini 流式接口：streamGenerateContent。"""
    # 兼容 Gemini SDK 常见参数 `?alt=sse`，当前无需额外处理。
    _ = alt
    return await _handle_generate(model_name=model_name, request=request, stream=True)


@router.post("/v1/models/{model_name}:streamGenerateContent")
async def gemini_stream_generate_content_v1(
    model_name: str,
    request: GeminiGenerateRequest,
    alt: Optional[str] = Query(default=None),
):
    """Gemini 流式接口（v1 别名）。"""
    _ = alt
    return await _handle_generate(model_name=model_name, request=request, stream=True)


__all__ = ["router"]
