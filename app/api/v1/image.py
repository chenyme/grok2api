"""
Image Generation API 路由
"""

import asyncio
import base64
import random
import time
import uuid
from pathlib import Path
from typing import List, Optional

from fastapi import APIRouter, Request
from fastapi.responses import StreamingResponse, JSONResponse
from pydantic import BaseModel, Field

from app.services.grok.chat import GrokChatService, ChatService
from app.services.grok.model import ModelService
from app.services.grok.processor import ImageStreamProcessor, ImageCollectProcessor
from app.services.token import get_token_manager, EffortType
from app.core.exceptions import ValidationException, AppException, ErrorType
from app.core.logger import logger
from app.core.config import get_config
from app.core.streaming import with_keepalive

router = APIRouter(tags=["Images"])

_IMAGE_CACHE_DIR = Path(__file__).parent.parent.parent.parent / "data" / "tmp" / "image"


def _guess_image_ext(image_b64: str) -> str:
    raw = (image_b64 or "").strip()
    if raw.startswith("iVBOR"):
        return "png"
    if raw.startswith("/9j/"):
        return "jpg"
    if raw.startswith("UklGR"):
        return "webp"
    return "jpg"


async def _persist_image_base64(image_b64: str) -> str:
    raw = (image_b64 or "").strip()
    if "," in raw and "base64" in raw.split(",", 1)[0].lower():
        raw = raw.split(",", 1)[1]
    image_bytes = base64.b64decode(raw)

    ext = _guess_image_ext(raw)
    filename = f"gen-{uuid.uuid4().hex}.{ext}"
    _IMAGE_CACHE_DIR.mkdir(parents=True, exist_ok=True)
    output_path = _IMAGE_CACHE_DIR / filename
    await asyncio.to_thread(output_path.write_bytes, image_bytes)
    return filename


class ImageGenerationRequest(BaseModel):
    """图片生成请求 - OpenAI 兼容"""

    prompt: str = Field(..., description="图片描述")
    model: Optional[str] = Field("grok-imagine-1.0", description="模型名称")
    n: Optional[int] = Field(1, ge=1, le=10, description="生成数量 (1-10)")
    size: Optional[str] = Field("1024x1024", description="图片尺寸 (暂不支持)")
    quality: Optional[str] = Field("standard", description="图片质量 (暂不支持)")
    response_format: Optional[str] = Field("b64_json", description="响应格式")
    style: Optional[str] = Field(None, description="风格 (暂不支持)")
    stream: Optional[bool] = Field(False, description="是否流式输出")
    image: Optional[str] = Field(None, description="原图 URL 或 base64 (可选，提供时执行图像编辑)")


def validate_request(request: ImageGenerationRequest):
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
        raise ValidationException(message="n must be between 1 and 10", param="n", code="invalid_n")

    # 流式只支持 n=1 或 n=2
    if request.stream and request.n not in [1, 2]:
        raise ValidationException(
            message="Streaming is only supported when n=1 or n=2",
            param="stream",
            code="invalid_stream_n",
        )

    if request.image is not None and not str(request.image).strip():
        raise ValidationException(
            message="image cannot be empty",
            param="image",
            code="empty_image",
        )

    response_format = str(request.response_format or "b64_json").lower()
    if response_format not in {"b64_json", "url"}:
        raise ValidationException(
            message="response_format must be one of ['b64_json', 'url']",
            param="response_format",
            code="invalid_response_format",
        )


async def _upload_image_attachment(token: str, image_data: str) -> str:
    from app.services.grok.assets import UploadService

    upload_service = UploadService()
    try:
        file_id, _ = await upload_service.upload(image_data, token)
        return file_id
    finally:
        await upload_service.close()


async def call_grok(
    token_mgr,
    token: str,
    prompt: str,
    model_info,
    file_attachment_id: Optional[str] = None,
) -> tuple[List[str], str]:
    """
    调用 Grok 获取图片

    Returns:
        tuple[List[str], str]: (base64 列表, 错误信息)
        成功时错误信息为空字符串，失败时返回错误描述
    """
    chat_service = GrokChatService()
    success = False
    error_msg = ""

    try:
        message_prefix = "Image Edit" if file_attachment_id else "Image Generation"
        response = await chat_service.chat(
            token=token,
            message=f"{message_prefix}:{prompt}",
            model=model_info.grok_model,
            mode=model_info.model_mode,
            think=False,
            stream=True,
            file_attachments=[file_attachment_id] if file_attachment_id else None,
        )

        # 收集图片
        processor = ImageCollectProcessor(model_info.model_id, token)
        images = await processor.process(response)

        if not images:
            error_msg = "No images generated from upstream"
            return [], error_msg

        success = True
        return images, ""

    except AppException as e:
        error_msg = str(e.message)
        logger.error(f"Grok image call failed: {error_msg}")
        return [], error_msg
    except Exception as e:
        error_msg = f"Image generation failed: {str(e)}"
        logger.error(f"Grok image call failed: {e}")
        return [], error_msg
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
async def create_image(request: ImageGenerationRequest, raw_request: Request):
    """
    Image Generation API

    流式响应格式:
    - event: image_generation.partial_image
    - event: image_generation.completed

    非流式响应格式:
    - {"created": ..., "data": [{"b64_json": "..."}], "usage": {...}}
    """
    from app.core.auth import get_client_ip, get_request_key_name

    # stream 默认为 false
    if request.stream is None:
        request.stream = False

    client_ip = get_client_ip(raw_request)
    key_name = await get_request_key_name(raw_request)
    start_time = time.time()

    def _record_request(success: bool, token_value: str = "", error: str = ""):
        duration = max(0.0, time.time() - start_time)
        asyncio.create_task(
            ChatService._record_stats(
                request.model,
                success,
                duration,
                token_value,
                error,
                client_ip=client_ip,
                key_name=key_name,
            )
        )

    # 参数验证
    validate_request(request)

    # 获取 token
    try:
        token_mgr = await get_token_manager()
        await token_mgr.reload_if_stale()
        token = None
        selected_pool = ""
        pool_candidates = ModelService.pool_candidates_for_model(request.model)
        for pool_name in pool_candidates:
            token = token_mgr.get_token(pool_name)
            if token:
                selected_pool = pool_name
                break
    except Exception as e:
        logger.error(f"Failed to get token: {e}")
        _record_request(False, "", str(e))
        raise AppException(
            message="Internal service error obtaining token",
            error_type=ErrorType.SERVER.value,
            code="internal_error",
        )

    if not token:
        pool_hint = (
            f" Model `{request.model}` requires Super tier tokens (ssoSuper pool)."
            if pool_candidates == ["ssoSuper"]
            else ""
        )
        message = f"No available tokens.{pool_hint} Please try again later."
        _record_request(False, "", message)
        raise AppException(
            message=message,
            error_type=ErrorType.RATE_LIMIT.value,
            code="rate_limit_exceeded",
            status_code=429,
        )

    logger.info(
        "Image API token selected: "
        f"model={request.model}, pool={selected_pool or 'unknown'}, "
        f"key_name={key_name}, suffix={token[-6:] if len(token) >= 6 else token}"
    )

    # 获取模型信息
    model_info = ModelService.get(request.model)
    file_attachment_id = None
    if request.image:
        try:
            file_attachment_id = await _upload_image_attachment(token, request.image)
        except Exception as e:
            logger.error(f"Image upload failed for edit: {e}")
            _record_request(False, token, f"Failed to upload edit image: {str(e)}")
            raise AppException(
                message=f"Failed to upload edit image: {str(e)}",
                error_type=ErrorType.SERVER.value,
                code="image_upload_failed",
            )

    # 流式模式
    if request.stream:
        chat_service = GrokChatService()
        try:
            message_prefix = "Image Edit" if file_attachment_id else "Image Generation"
            response = await chat_service.chat(
                token=token,
                message=f"{message_prefix}:{request.prompt}",
                model=model_info.grok_model,
                mode=model_info.model_mode,
                think=False,
                stream=True,
                file_attachments=[file_attachment_id] if file_attachment_id else None,
            )
        except AppException:
            raise
        except Exception as e:
            logger.error(f"Image stream connection failed: {e}")
            _record_request(
                False, token, f"Failed to connect to image generation service: {str(e)}"
            )
            raise AppException(
                message=f"Failed to connect to image generation service: {str(e)}",
                error_type=ErrorType.SERVER.value,
                code="image_generation_failed",
            )

        processor = ImageStreamProcessor(model_info.model_id, token, n=request.n)

        # 包装流式响应，在成功完成时记录使用
        async def _wrap_stream(stream):
            success = False
            error_msg = ""
            try:
                keepalive = get_config("performance.sse_keepalive_sec", 15)
                try:
                    keepalive = float(keepalive)
                except Exception:
                    keepalive = 15.0
                wrapped = with_keepalive(stream, keepalive, ping_message=": ping\n\n")
                async for chunk in wrapped:
                    yield chunk
                success = True
            except Exception as e:
                error_msg = str(e)
                raise
            finally:
                _record_request(success, token, error_msg)
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
            headers={
                "Cache-Control": "no-cache",
                "Connection": "keep-alive",
                "X-Accel-Buffering": "no",
            },
        )

    # 非流式模式
    n = request.n

    calls_needed = (n + 1) // 2

    all_images = []
    last_error = ""

    if calls_needed == 1:
        # 单次调用
        images, error = await call_grok(
            token_mgr,
            token,
            request.prompt,
            model_info,
            file_attachment_id=file_attachment_id,
        )
        all_images = images
        last_error = error
    else:
        # 并发调用
        tasks = [
            call_grok(
                token_mgr,
                token,
                request.prompt,
                model_info,
                file_attachment_id=file_attachment_id,
            )
            for _ in range(calls_needed)
        ]
        results = await asyncio.gather(*tasks, return_exceptions=True)

        # 收集成功的图片
        for result in results:
            if isinstance(result, Exception):
                logger.error(f"Concurrent call failed: {result}")
                last_error = str(result)
            elif isinstance(result, tuple):
                images, error = result
                all_images.extend(images)
                if error:
                    last_error = error

    # 如果没有任何图片生成，返回错误
    if not all_images:
        error_text = last_error or "Failed to generate images. Please try again."
        _record_request(False, token, error_text)
        raise AppException(
            message=error_text,
            error_type=ErrorType.SERVER.value,
            code="image_generation_failed",
        )

    # 随机选取 n 张图片
    if len(all_images) >= n:
        selected_images = random.sample(all_images, n)
    else:
        # 返回所有可用图片，并警告数量不足
        selected_images = all_images
        logger.warning(f"Requested {n} images but only {len(all_images)} generated")

    # 构建响应
    response_format = str(request.response_format or "b64_json").lower()
    app_url = str(get_config("app.app_url") or "").rstrip("/")
    data = []

    if response_format == "url":
        for img in selected_images:
            try:
                filename = await _persist_image_base64(img)
                path = f"/v1/files/image/{filename}"
                url = f"{app_url}{path}" if app_url else path
                data.append({"url": url})
            except Exception as e:
                logger.warning(f"Failed to persist generated image, fallback to b64: {e}")
                data.append({"b64_json": img})
    else:
        data = [{"b64_json": img} for img in selected_images]

    _record_request(True, token)

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


@router.post("/images/edits")
async def edit_image(request: ImageGenerationRequest, raw_request: Request):
    """Image Edit API（兼容 JSON 请求体）"""
    if not request.image:
        raise ValidationException(
            message="image is required for image editing",
            param="image",
            code="missing_image",
        )
    return await create_image(request, raw_request)


__all__ = ["router"]
