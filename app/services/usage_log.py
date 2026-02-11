"""
使用记录服务 (Usage Log Service)
记录每次 API 调用的关键信息，支持异步写入不阻塞请求。
"""

import asyncio
import time
import uuid
from typing import Any, Dict, List, Optional, Tuple

from pydantic import BaseModel, Field

from app.core.logger import logger
from app.core.storage import get_storage


class UsageLog(BaseModel):
    """使用记录数据模型"""

    id: str = Field(default_factory=lambda: uuid.uuid4().hex[:16])
    created_at: int = Field(default_factory=lambda: int(time.time()))
    type: str = "chat"  # chat / image / video
    model: str = ""
    is_stream: bool = False
    use_time: int = 0  # 耗时（毫秒）
    status: str = "success"  # success / error
    error_message: str = ""
    token_hash: str = ""  # 脱敏 Token
    pool_name: str = ""
    effort: str = "low"  # low / high
    ip: str = ""
    request_id: str = ""


def mask_token(token: str) -> str:
    """Token 脱敏：保留前8后8"""
    if not token or len(token) <= 20:
        return token or ""
    return f"{token[:8]}...{token[-8:]}"


class UsageLogService:
    """使用记录服务"""

    @staticmethod
    async def record(
        *,
        type: str = "chat",
        model: str = "",
        is_stream: bool = False,
        use_time: int = 0,
        status: str = "success",
        error_message: str = "",
        token: str = "",
        pool_name: str = "",
        effort: str = "low",
        ip: str = "",
        request_id: str = "",
    ):
        """
        异步记录一条使用日志。
        使用 create_task 确保不阻塞请求响应。
        """
        log = UsageLog(
            type=type,
            model=model,
            is_stream=is_stream,
            use_time=use_time,
            status=status,
            error_message=error_message,
            token_hash=mask_token(token),
            pool_name=pool_name,
            effort=effort,
            ip=ip,
            request_id=request_id,
        )
        asyncio.create_task(UsageLogService._save(log))

    @staticmethod
    async def _save(log: UsageLog):
        """实际写入存储"""
        try:
            storage = get_storage()
            await storage.save_log(log.model_dump())
        except Exception as e:
            logger.warning(f"Failed to save usage log: {e}")

    @staticmethod
    async def query(
        *,
        type: str = None,
        model: str = None,
        status: str = None,
        start_time: int = None,
        end_time: int = None,
        token_hash: str = None,
        page: int = 1,
        page_size: int = 20,
    ) -> Tuple[List[Dict[str, Any]], int]:
        """查询使用记录"""
        storage = get_storage()
        filters = {}
        if type:
            filters["type"] = type
        if model:
            filters["model"] = model
        if status:
            filters["status"] = status
        if start_time:
            filters["start_time"] = start_time
        if end_time:
            filters["end_time"] = end_time
        if token_hash:
            filters["token_hash"] = token_hash
        return await storage.query_logs(filters, page, page_size)

    @staticmethod
    async def delete(before_timestamp: int) -> int:
        """删除指定时间之前的记录"""
        storage = get_storage()
        return await storage.delete_logs(before_timestamp)

    @staticmethod
    async def stats() -> Dict[str, Any]:
        """统计摘要"""
        storage = get_storage()
        return await storage.stats_logs()
