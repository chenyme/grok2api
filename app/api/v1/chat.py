# -*- coding: utf-8 -*-
"""
聊天API路由模块

提供OpenAI兼容的聊天API接口，支持与Grok模型的交互。
"""

from fastapi import APIRouter, Depends, HTTPException
from typing import Optional
from fastapi.responses import StreamingResponse

from app.core.auth import auth_manager
from app.core.exception import GrokApiException
from app.core.logger import logger
from app.services.grok.client import GrokClient
from app.models.openai_schema import OpenAIChatRequest

# 聊天路由
router = APIRouter(prefix="/chat", tags=["聊天"])


@router.post("/completions", response_model=None)
async def chat_completions(
    request: OpenAIChatRequest,
    _: Optional[str] = Depends(auth_manager.verify)
):
    """
    创建聊天补全

    兼容OpenAI聊天API的端点，支持流式和非流式响应。

    Args:
        request: OpenAI格式的聊天请求
        _: 认证依赖（自动验证）

    Returns:
        OpenAIChatCompletionResponse: 非流式响应
        StreamingResponse: 流式响应

    Raises:
        HTTPException: 当请求处理失败时
    """
    try:
        logger.info(f"[Chat] 收到聊天请求")

        # 调用Grok客户端处理请求
        result = await GrokClient.openai_to_grok(request.model_dump())
        
        # 如果是流式响应，GrokClient已经返回了Iterator，直接包装为StreamingResponse
        if request.stream:
            return StreamingResponse(
                content=result,
                media_type="text/event-stream",
                headers={
                    "Cache-Control": "no-cache",
                    "Connection": "keep-alive",
                    "X-Accel-Buffering": "no"
                }
            )
        
        # 非流式响应直接返回
        return result
        
    except GrokApiException as e:
        logger.error(f"[Chat] Grok API错误: {str(e)} - 详情: {e.details}")
        raise HTTPException(
            status_code=500,
            detail={
                "error": {
                    "message": str(e),
                    "type": e.error_code or "grok_api_error",
                    "code": e.error_code or "unknown"
                }
            }
        )
    except Exception as e:
        logger.error(f"[Chat] 聊天请求处理失败: {str(e)}")
        raise HTTPException(
            status_code=500,
            detail={
                "error": {
                    "message": "服务器内部错误",
                    "type": "internal_error",
                    "code": "internal_server_error"
                }
            }
        )
