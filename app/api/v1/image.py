"""
Image Generation API 路由
"""

import asyncio
import base64
import random
from pathlib import Path
from typing import List, Optional, Union

from fastapi import APIRouter, File, Form, UploadFile
from fastapi.responses import StreamingResponse, JSONResponse
from pydantic import BaseModel, Field, ValidationError

from app.services.grok.services.chat import GrokChatService
from app.services.grok.services.assets import UploadService
from app.services.grok.models.model import ModelService
from app.services.grok.processors.processor import ImageStreamProcessor, ImageCollectProcessor
from app.services.token import get_token_manager, EffortType
from app.core.exceptions import ValidationException, AppException, ErrorType
from app.core.config import get_config
from app.core.logger import logger


router = APIRouter(tags=["Images"])


class ImageGenerationRequest(BaseModel):
    """图片生成请求 - OpenAI 兼容"""

    prompt: str = Field(..., description="图片描述")
    model: Optional[str] = Field("grok-imagine-1.0", description="模型名称")
    n: Optional[int] = Field(1, ge=1, le=10, description="生成数量 (1-10)")
    size: Optional[str] = Field("1024x1024", description="图片尺寸 (暂不支持)")
    quality: Optional[str] = Field("standard", description="图片质量 (暂不支持)")
    response_format: Optional[str] = Field(None, description="响应格式")
    style: Optional[str] = Field(None, description="风格 (暂不支持)")
    stream: Optional[bool] = Field(False, description="是否流式输出")



class ImageEditRequest(BaseModel):
    """图片编辑请求 - OpenAI 兼容"""

    prompt: str = Field(..., description="编辑描述")
    model: Optional[str] = Field("grok-imagine-1.0", description="模型名称")
    image: Optional[Union[str, List[str]]] = Field(None, description="待编辑图片文件")
    n: Optional[int] = Field(1, ge=1, le=10, description="生成数量 (1-10)")
    size: Optional[str] = Field("1024x1024", description="图片尺寸 (暂不支持)")
    quality: Optional[str] = Field("standard", description="图片质量 (暂不支持)")
    response_format: Optional[str] = Field(None, description="响应格式")
    style: Optional[str] = Field(None, description="风格 (暂不支持)")
    stream: Optional[bool] = Field(False, description="是否流式输出")



def validate_generation_request(request: ImageGenerationRequest):
    """验证请求参数"""
    # 验证模型 - 通过 is_image 检查
    model_info = ModelService.get(request.model)
    if not model_info or not model_info.is_image:
        # 获取支持的图片模型列表
        image_models = [m.model_id for m in ModelService.MODELS if m.is_image]
        raise ValidationException(
            message=f"The model `{request.model}` is not supported for image generation. Supported: {image_models}",
            param="model",
            code="model_not_supported",
        )

    # 验证 prompt
    if not request.prompt or not request.prompt.strip():
        raise ValidationException(
            message="Prompt cannot be empty", param="prompt", code="empty_prompt"
        )

    # 验证 n 参数范围
    if request.n < 1 or request.n > 10:
        raise ValidationException(
            message="n must be between 1 and 10", param="n", code="invalid_n"
        )

    # 流式只支持 n=1 或 n=2
    if request.stream and request.n not in [1, 2]:
        raise ValidationException(
            message="Streaming is only supported when n=1 or n=2",
            param="stream",
            code="invalid_stream_n",
        )

    if request.response_format:
        allowed_formats = {"b64_json", "base64", "url"}
        if request.response_format not in allowed_formats:
            raise ValidationException(
                message=f"response_format must be one of {sorted(allowed_formats)}",
                param="response_format",
                code="invalid_response_format",
            )


def resolve_response_format(response_format: Optional[str]) -> str:
    fmt = response_format or get_config("app.image_format", "url")
    if isinstance(fmt, str):
        fmt = fmt.lower()
    if fmt in ("b64_json", "base64", "url"):
        return fmt
    allowed_formats = {"b64_json", "base64", "url"}
    raise ValidationException(
        message=f"response_format must be one of {sorted(allowed_formats)}",
        param="response_format",
        code="invalid_response_format",
    )


def response_field_name(response_format: str) -> str:
    if response_format == "url":
        return "url"
    if response_format == "base64":
        return "base64"
    return "b64_json"


def validate_edit_request(request: ImageEditRequest, images: List[UploadFile]):
    """验证图片编辑请求参数"""
    validate_generation_request(request)
    if not images:
        raise ValidationException(
            message="Image is required",
            param="image",
            code="missing_image",
        )
    if len(images) > 16:
        raise ValidationException(
            message="Too many images. Maximum is 16.",
            param="image",
            code="invalid_image_count",
        )


async def call_grok(
    token_mgr,
    token: str,
    prompt: str,
    model_info,
    file_attachments: Optional[List[str]] = None,
    response_format: str = "b64_json",
) -> List[str]:
    """调用 Grok 获取图片，返回 base64 列表"""
    chat_service = GrokChatService()
    success = False

    try:
        response = await chat_service.chat(
            token=token,
            message=prompt,
            model=model_info.grok_model,
            mode=model_info.model_mode,
            stream=True,
            file_attachments=file_attachments,
        )

        # 收集图片
        processor = ImageCollectProcessor(
            model_info.model_id, token, response_format=response_format
        )
        images = await processor.process(response)
        success = True
        return images

    except Exception as e:
        logger.error(f"Grok image call failed: {e}")
        return []
    finally:
        # 只在成功时记录使用，失败时不扣费（避免清零 fail_count）
        if success:
            try:
                effort = (
                    EffortType.HIGH
                    if (model_info and model_info.cost.value == "high")
                    else EffortType.LOW
                )
                await token_mgr.consume(token, effort)
            except Exception as e:
                logger.warning(f"Failed to consume token: {e}")


@router.post("/images/generations")
async def create_image(request: ImageGenerationRequest):
    """
    Image Generation API

    流式响应格式:
    - event: image_generation.partial_image
    - event: image_generation.completed

    非流式响应格式:
    - {"created": ..., "data": [{"b64_json": "..."}], "usage": {...}}
    """
    # stream 默认为 false
    if request.stream is None:
        request.stream = False

    if request.response_format is None:
        request.response_format = resolve_response_format(None)

    # 参数验证
    validate_generation_request(request)

    response_format = resolve_response_format(request.response_format)
    response_field = response_field_name(response_format)

    # 获取 token
    try:
        token_mgr = await get_token_manager()
        await token_mgr.reload_if_stale()
        token = None
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

    # 获取模型信息
    model_info = ModelService.get(request.model)

    # 流式模式
    if request.stream:
        chat_service = GrokChatService()
        response = await chat_service.chat(
            token=token,
            message=f"Image Generation: {request.prompt}",
            model=model_info.grok_model,
            mode=model_info.model_mode,
            stream=True,
        )

        processor = ImageStreamProcessor(
            model_info.model_id, token, n=request.n, response_format=response_format
        )

        # 包装流式响应，在成功完成时记录使用
        async def _wrap_stream(stream):
            success = False
            try:
                async for chunk in stream:
                    yield chunk
                success = True
            finally:
                # 只在成功完成时扣费
                if success:
                    try:
                        effort = (
                            EffortType.HIGH
                            if (model_info and model_info.cost.value == "high")
                            else EffortType.LOW
                        )
                        await token_mgr.consume(token, effort)
                    except Exception as e:
                        logger.warning(f"Failed to consume token: {e}")

        return StreamingResponse(
            _wrap_stream(processor.process(response)),
            media_type="text/event-stream",
            headers={"Cache-Control": "no-cache", "Connection": "keep-alive"},
        )

    # 非流式模式
    n = request.n

    calls_needed = (n + 1) // 2

    if calls_needed == 1:
        # 单次调用
        all_images = await call_grok(
            token_mgr,
            token,
            f"Image Generation: {request.prompt}",
            model_info,
            response_format=response_format,
        )
    else:
        # 并发调用
        tasks = [
            call_grok(
                token_mgr,
                token,
                f"Image Generation: {request.prompt}",
                model_info,
                response_format=response_format,
            )
            for _ in range(calls_needed)
        ]
        results = await asyncio.gather(*tasks, return_exceptions=True)

        # 收集成功的图片
        all_images = []
        for result in results:
            if isinstance(result, Exception):
                logger.error(f"Concurrent call failed: {result}")
            elif isinstance(result, list):
                all_images.extend(result)

    # 随机选取 n 张图片
    if len(all_images) >= n:
        selected_images = random.sample(all_images, n)
    else:
        # 全部返回，error 填充缺失
        selected_images = all_images.copy()
        while len(selected_images) < n:
            selected_images.append("error")

    # 构建响应
    import time

    data = [{response_field: img} for img in selected_images]

    return JSONResponse(
        content={
            "created": int(time.time()),
            "data": data,
            "usage": {
                "total_tokens": 0
                * len([img for img in selected_images if img != "error"]),
                "input_tokens": 0,
                "output_tokens": 0
                * len([img for img in selected_images if img != "error"]),
                "input_tokens_details": {"text_tokens": 0, "image_tokens": 0},
            },
        }
    )



@router.post("/images/edits")
async def edit_image(
    prompt: str = Form(...),
    image: List[UploadFile] = File(...),
    model: Optional[str] = Form("grok-imagine-1.0"),
    n: int = Form(1),
    size: str = Form("1024x1024"),
    quality: str = Form("standard"),
    response_format: Optional[str] = Form(None),
    style: Optional[str] = Form(None),
    stream: Optional[bool] = Form(False),
):
    """
    Image Edits API

    同官方 API 格式，仅支持 multipart/form-data 文件上传
    """
    if response_format is None:
        response_format = resolve_response_format(None)

    try:
        edit_request = ImageEditRequest(
            prompt=prompt,
            model=model,
            n=n,
            size=size,
            quality=quality,
            response_format=response_format,
            style=style,
            stream=stream,
        )
    except ValidationError as exc:
        errors = exc.errors()
        if errors:
            first = errors[0]
            loc = first.get("loc", [])
            msg = first.get("msg", "Invalid request")
            code = first.get("type", "invalid_value")
            param_parts = [
                str(x) for x in loc if not (isinstance(x, int) or str(x).isdigit())
            ]
            param = ".".join(param_parts) if param_parts else None
            raise ValidationException(message=msg, param=param, code=code)
        raise ValidationException(message="Invalid request", code="invalid_value")

    if edit_request.stream is None:
        edit_request.stream = False

    response_format = resolve_response_format(edit_request.response_format)
    edit_request.response_format = response_format
    response_field = response_field_name(response_format)

    # 参数验证
    validate_edit_request(edit_request, image)

    max_image_bytes = 50 * 1024 * 1024
    allowed_types = {"image/png", "image/jpeg", "image/webp", "image/jpg"}

    images: List[str] = []
    for item in image:
        content = await item.read()
        await item.close()
        if not content:
            raise ValidationException(
                message="File content is empty",
                param="image",
                code="empty_file",
            )
        if len(content) > max_image_bytes:
            raise ValidationException(
                message="Image file too large. Maximum is 50MB.",
                param="image",
                code="file_too_large",
            )
        mime = (item.content_type or "").lower()
        if mime == "image/jpg":
            mime = "image/jpeg"
        ext = Path(item.filename or "").suffix.lower()
        if mime not in allowed_types:
            if ext in (".jpg", ".jpeg"):
                mime = "image/jpeg"
            elif ext == ".png":
                mime = "image/png"
            elif ext == ".webp":
                mime = "image/webp"
            else:
                raise ValidationException(
                    message="Unsupported image type. Supported: png, jpg, webp.",
                    param="image",
                    code="invalid_image_type",
                )
        b64 = base64.b64encode(content).decode()
        images.append(f"data:{mime};base64,{b64}")

    # 获取 token
    try:
        token_mgr = await get_token_manager()
        await token_mgr.reload_if_stale()
        token = None
        for pool_name in ModelService.pool_candidates_for_model(edit_request.model):
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

    # 获取模型信息
    model_info = ModelService.get(edit_request.model)

    # 上传图片
    file_ids: List[str] = []
    upload_service = UploadService()
    try:
        for image in images:
            file_id, _ = await upload_service.upload(image, token)
            file_ids.append(file_id)
    finally:
        await upload_service.close()

    # 流式模式
    if edit_request.stream:
        chat_service = GrokChatService()
        response = await chat_service.chat(
            token=token,
            message=f"Image Edit: {edit_request.prompt}",
            model=model_info.grok_model,
            mode=model_info.model_mode,
            stream=True,
            file_attachments=file_ids,
        )

        processor = ImageStreamProcessor(
            model_info.model_id, token, n=edit_request.n, response_format=response_format
        )

        async def _wrap_stream(stream):
            success = False
            try:
                async for chunk in stream:
                    yield chunk
                success = True
            finally:
                if success:
                    try:
                        effort = (
                            EffortType.HIGH
                            if (model_info and model_info.cost.value == "high")
                            else EffortType.LOW
                        )
                        await token_mgr.consume(token, effort)
                    except Exception as e:
                        logger.warning(f"Failed to consume token: {e}")

        return StreamingResponse(
            _wrap_stream(processor.process(response)),
            media_type="text/event-stream",
            headers={"Cache-Control": "no-cache", "Connection": "keep-alive"},
        )

    # 非流式模式
    n = edit_request.n
    calls_needed = (n + 1) // 2

    if calls_needed == 1:
        all_images = await call_grok(
            token_mgr,
            token,
            f"Image Edit: {edit_request.prompt}",
            model_info,
            file_attachments=file_ids,
            response_format=response_format,
        )
    else:
        tasks = [
            call_grok(
                token_mgr,
                token,
                f"Image Edit: {edit_request.prompt}",
                model_info,
                file_attachments=file_ids,
                response_format=response_format,
            )
            for _ in range(calls_needed)
        ]
        results = await asyncio.gather(*tasks, return_exceptions=True)

        all_images = []
        for result in results:
            if isinstance(result, Exception):
                logger.error(f"Concurrent call failed: {result}")
            elif isinstance(result, list):
                all_images.extend(result)

    if len(all_images) >= n:
        selected_images = random.sample(all_images, n)
    else:
        selected_images = all_images.copy()
        while len(selected_images) < n:
            selected_images.append("error")

    import time

    data = [{response_field: img} for img in selected_images]

    return JSONResponse(
        content={
            "created": int(time.time()),
            "data": data,
            "usage": {
                "total_tokens": 0
                * len([img for img in selected_images if img != "error"]),
                "input_tokens": 0,
                "output_tokens": 0
                * len([img for img in selected_images if img != "error"]),
                "input_tokens_details": {"text_tokens": 0, "image_tokens": 0},
            },
        }
    )


__all__ = ["router"]
