"""
Image Generation API 路由
"""

import asyncio
import random
from typing import List, Optional

from fastapi import APIRouter
from fastapi.responses import StreamingResponse, JSONResponse
from pydantic import BaseModel, Field

from app.services.grok.chat import GrokChatService
from app.services.grok.model import ModelService
from app.services.grok.processor import ImageStreamProcessor, ImageCollectProcessor
from app.services.token import get_token_manager, EffortType
from app.core.exceptions import ValidationException, AppException, ErrorType
from app.core.logger import logger


router = APIRouter(tags=["Images"])


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


async def call_grok(token_mgr, token: str, prompt: str, model_info) -> List[str]:
    """
    调用 Grok 获取图片，返回 base64 列表
    """
    chat_service = GrokChatService()
    success = False

    try:
        response = await chat_service.chat(
            token=token,
            message=f"Image Generation:{prompt}",
            model=model_info.grok_model,
            mode=model_info.model_mode,
            think=False,
            stream=True,
        )

        # 收集图片
        processor = ImageCollectProcessor(model_info.model_id, token)
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

    # 参数验证
    validate_request(request)

    # 获取 token
    try:
        token_mgr = await get_token_manager()
        await token_mgr.reload_if_stale()
        pool_name = ModelService.pool_for_model(request.model)
        token = token_mgr.get_token(pool_name)
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
            message=f"Image Generation:{request.prompt}",
            model=model_info.grok_model,
            mode=model_info.model_mode,
            think=False,
            stream=True,
        )

        processor = ImageStreamProcessor(model_info.model_id, token, n=request.n)

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
        all_images = await call_grok(token_mgr, token, request.prompt, model_info)
    else:
        # 并发调用
        tasks = [
            call_grok(token_mgr, token, request.prompt, model_info)
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

    data = [{"b64_json": img} for img in selected_images]

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
