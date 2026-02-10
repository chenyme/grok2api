"""
Token 获取和 effort 计算的共享工具。

消除 chat.py / media.py 中的重复 token 选择和 effort 判断逻辑。
"""

from __future__ import annotations

from typing import List, Optional, Tuple

from app.core.exceptions import AppException, ErrorType
from app.core.logger import logger
from app.services.grok.models.model import ModelService
from app.services.token import EffortType, get_token_manager


async def acquire_token_for_model(
    model: str,
    pool_priority_override: Optional[List[str]] = None,
) -> Tuple:
    """获取指定模型可用的 token。

    Args:
        model: OpenAI 兼容模型名
        pool_priority_override: 覆盖默认的池优先级列表

    Returns:
        (token_mgr, token, pool_candidates)

    Raises:
        AppException: token 获取失败或无可用 token
    """
    try:
        token_mgr = await get_token_manager()
        await token_mgr.reload_if_stale()
    except Exception as e:
        logger.error(f"Failed to get token manager: {e}")
        raise AppException(
            message="Internal service error obtaining token",
            error_type=ErrorType.SERVER.value,
            code="internal_error",
        )

    pool_candidates = pool_priority_override or ModelService.pool_candidates_for_model(model)
    token = None
    for pool_name in pool_candidates:
        token = token_mgr.get_token(pool_name)
        if token:
            break

    if not token:
        pool_hint = (
            f" Model `{model}` requires Super tier tokens (ssoSuper pool)."
            if pool_candidates == ["ssoSuper"]
            else ""
        )
        raise AppException(
            message=f"No available tokens.{pool_hint} Please try again later.",
            error_type=ErrorType.RATE_LIMIT.value,
            code="rate_limit_exceeded",
            status_code=429,
        )

    return token_mgr, token, pool_candidates


def compute_effort(model: str) -> EffortType:
    """根据模型 cost 计算 effort 等级。"""
    model_info = ModelService.get(model)
    if model_info and model_info.cost.value == "high":
        return EffortType.HIGH
    return EffortType.LOW
