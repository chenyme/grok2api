"""聊天API路由 - OpenAI兼容的聊天接口"""

from fastapi import APIRouter, Depends, HTTPException
from typing import Optional
from fastapi.responses import StreamingResponse

from app.core.auth import auth_manager
from app.core.exception import GrokApiException
from app.core.logger import logger
from app.services.grok.client import GrokClient
from app.models.openai_schema import OpenAIChatRequest


router = APIRouter(prefix="/chat", tags=["聊天"])


@router.post("/completions", response_model=None)
async def chat_completions(request: OpenAIChatRequest, _: Optional[str] = Depends(auth_manager.verify)):
    """创建聊天补全（支持流式和非流式）"""
    try:
        logger.info("[Chat] 收到聊天请求")

        # 调用Grok客户端
        result = await GrokClient.openai_to_grok(request.model_dump())
        
        # 流式响应
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
        
        # 非流式响应
        return result
        
    except GrokApiException as e:
        logger.error(f"[Chat] Grok API错误: {e} - 详情: {e.details}")
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
        logger.error(f"[Chat] 处理失败: {e}")
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
