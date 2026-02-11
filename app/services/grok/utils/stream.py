"""
流式响应通用工具
"""

import time

from typing import AsyncGenerator

from app.core.logger import logger
from app.services.grok.models.model import ModelService
from app.services.token import EffortType


async def wrap_stream_with_usage(
    stream: AsyncGenerator, token_mgr, token: str, model: str,
    *, start_time: float = 0, client_ip: str = "",
    pool_name: str = "", log_type: str = "chat",
) -> AsyncGenerator:
    """
    包装流式响应，在完成时记录使用

    Args:
        stream: 原始 AsyncGenerator
        token_mgr: TokenManager 实例
        token: Token 字符串
        model: 模型名称
        start_time: 请求开始时间
        client_ip: 客户端 IP
        pool_name: Token 池名
        log_type: 日志类型 (chat/video)
    """
    success = False
    try:
        async for chunk in stream:
            yield chunk
        success = True
    finally:
        effort_str = "low"
        if success:
            try:
                model_info = ModelService.get(model)
                effort = (
                    EffortType.HIGH
                    if (model_info and model_info.cost.value == "high")
                    else EffortType.LOW
                )
                effort_str = effort.value
                await token_mgr.consume(token, effort)
                logger.debug(
                    f"Stream completed, recorded usage for token {token[:10]}... (effort={effort.value})"
                )
            except Exception as e:
                logger.warning(f"Failed to record stream usage: {e}")

        # 记录使用日志
        try:
            from app.services.usage_log import UsageLogService
            use_time = int((time.time() - start_time) * 1000) if start_time else 0
            await UsageLogService.record(
                type=log_type, model=model, is_stream=True,
                use_time=use_time,
                status="success" if success else "error",
                error_message="" if success else "stream interrupted",
                token=token, pool_name=pool_name,
                effort=effort_str, ip=client_ip,
            )
        except Exception as e:
            logger.warning(f"Failed to record usage log: {e}")


__all__ = ["wrap_stream_with_usage"]
