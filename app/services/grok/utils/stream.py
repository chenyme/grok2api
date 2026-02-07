"""
流式响应通用工具
"""

from typing import AsyncGenerator

from app.core.logger import logger
from app.services.grok.models.model import ModelService
from app.services.token import EffortType


async def wrap_stream_with_usage(
    stream: AsyncGenerator, token_mgr, token: str, model: str
) -> AsyncGenerator:
    """
    包装流式响应，在完成时记录使用

    Args:
        stream: 原始 AsyncGenerator
        token_mgr: TokenManager 实例
        token: Token 字符串
        model: 模型名称
    """
    success = False
    try:
        async for chunk in stream:
            yield chunk
        success = True
    finally:
        if success:
            try:
                model_info = ModelService.get(model)
                effort = (
                    EffortType.HIGH
                    if (model_info and model_info.cost.value == "high")
                    else EffortType.LOW
                )
                await token_mgr.consume(token, effort)
                logger.debug(
                    f"Stream completed, recorded usage for token {token[:10]}... (effort={effort.value})"
                )
            except Exception as e:
                logger.warning(f"Failed to record stream usage: {e}")


__all__ = ["wrap_stream_with_usage"]
