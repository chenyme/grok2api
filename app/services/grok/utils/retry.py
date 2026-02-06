"""
Grok API 重试工具

提供可配置的重试机制，支持:
- 指数退避 + decorrelated jitter
- Retry-After header 支持
- 429 专用退避策略
- 重试预算控制
"""

import asyncio
import random
from typing import Callable, Any, Optional, List
from functools import wraps

from app.core.logger import logger
from app.core.config import get_config
from app.core.exceptions import UpstreamException


class RetryConfig:
    """重试配置"""

    @staticmethod
    def get_max_retry() -> int:
        """获取最大重试次数"""
        return get_config("grok.max_retry", 3)

    @staticmethod
    def get_retry_codes() -> List[int]:
        """获取可重试的状态码"""
        return get_config("grok.retry_status_codes", [401, 429, 403])

    @staticmethod
    def get_backoff_base() -> float:
        """获取退避基础时间(秒)"""
        return get_config("grok.retry_backoff_base", 0.5)

    @staticmethod
    def get_backoff_factor() -> float:
        """获取退避因子"""
        return get_config("grok.retry_backoff_factor", 2.0)

    @staticmethod
    def get_backoff_max() -> float:
        """获取最大退避时间(秒)"""
        return get_config("grok.retry_backoff_max", 30.0)

    @staticmethod
    def get_retry_budget() -> float:
        """获取重试总预算时间(秒)"""
        return get_config("grok.retry_budget", 90.0)


class RetryContext:
    """重试上下文"""

    def __init__(self):
        self.attempt = 0
        self.max_retry = RetryConfig.get_max_retry()
        self.retry_codes = RetryConfig.get_retry_codes()
        self.last_error = None
        self.last_status = None
        self.total_delay = 0.0
        self.retry_budget = RetryConfig.get_retry_budget()

        # 退避参数
        self.backoff_base = RetryConfig.get_backoff_base()
        self.backoff_factor = RetryConfig.get_backoff_factor()
        self.backoff_max = RetryConfig.get_backoff_max()

        # decorrelated jitter 状态
        self._last_delay = self.backoff_base

    def should_retry(self, status_code: int) -> bool:
        """判断是否重试"""
        if self.attempt >= self.max_retry:
            return False
        if status_code not in self.retry_codes:
            return False
        if self.total_delay >= self.retry_budget:
            return False
        return True

    def record_error(self, status_code: int, error: Exception):
        """记录错误信息"""
        self.last_status = status_code
        self.last_error = error
        self.attempt += 1

    def calculate_delay(
        self, status_code: int, retry_after: Optional[float] = None
    ) -> float:
        """
        计算退避延迟时间

        Args:
            status_code: HTTP 状态码
            retry_after: Retry-After header 值(秒)

        Returns:
            延迟时间(秒)
        """
        # 优先使用 Retry-After
        if retry_after is not None and retry_after > 0:
            delay = min(retry_after, self.backoff_max)
            self._last_delay = delay
            return delay

        # 429 使用 decorrelated jitter
        if status_code == 429:
            # decorrelated jitter: delay = random(base, last_delay * 3)
            delay = random.uniform(self.backoff_base, self._last_delay * 3)
            delay = min(delay, self.backoff_max)
            self._last_delay = delay
            return delay

        # 其他状态码使用指数退避 + full jitter
        exp_delay = self.backoff_base * (self.backoff_factor**self.attempt)
        delay = random.uniform(0, min(exp_delay, self.backoff_max))
        return delay

    def record_delay(self, delay: float):
        """记录延迟时间"""
        self.total_delay += delay


def extract_retry_after(error: Exception) -> Optional[float]:
    """
    从异常中提取 Retry-After 值

    Args:
        error: 异常对象

    Returns:
        Retry-After 秒数，或 None
    """
    if not isinstance(error, UpstreamException):
        return None

    details = error.details or {}

    # 尝试从 details 中获取
    retry_after = details.get("retry_after")
    if retry_after is not None:
        try:
            return float(retry_after)
        except (ValueError, TypeError):
            pass

    # 尝试从 headers 中获取
    headers = details.get("headers", {})
    if isinstance(headers, dict):
        retry_after = headers.get("Retry-After") or headers.get("retry-after")
        if retry_after is not None:
            try:
                return float(retry_after)
            except (ValueError, TypeError):
                pass

    return None


async def retry_on_status(
    func: Callable,
    *args,
    extract_status: Callable[[Exception], Optional[int]] = None,
    on_retry: Callable[[int, int, Exception, float], None] = None,
    **kwargs,
) -> Any:
    """
    通用重试函数

    Args:
        func: 重试的异步函数
        *args: 函数参数
        extract_status: 异常提取状态码的函数
        on_retry: 重试时的回调函数 (attempt, status_code, error, delay)
        **kwargs: 函数关键字参数

    Returns:
        函数执行结果

    Raises:
        最后一次失败的异常
    """
    ctx = RetryContext()

    # 状态码提取器
    if extract_status is None:

        def extract_status(e: Exception) -> Optional[int]:
            if isinstance(e, UpstreamException):
                # 优先从 details 获取，回退到 status_code 属性
                if e.details and "status" in e.details:
                    return e.details["status"]
                return getattr(e, "status_code", None)
            return None

    while ctx.attempt <= ctx.max_retry:
        try:
            result = await func(*args, **kwargs)

            # 记录日志
            if ctx.attempt > 0:
                logger.info(
                    f"Retry succeeded after {ctx.attempt} attempts, "
                    f"total delay: {ctx.total_delay:.2f}s"
                )

            return result

        except Exception as e:
            # 提取状态码
            status_code = extract_status(e)

            if status_code is None:
                # 错误无法识别
                logger.error(f"Non-retryable error: {e}")
                raise

            # 记录错误
            ctx.record_error(status_code, e)

            # 判断是否重试
            if ctx.should_retry(status_code):
                # 提取 Retry-After
                retry_after = extract_retry_after(e)

                # 计算延迟
                delay = ctx.calculate_delay(status_code, retry_after)

                # 检查是否超出预算
                if ctx.total_delay + delay > ctx.retry_budget:
                    logger.warning(
                        f"Retry budget exhausted: {ctx.total_delay:.2f}s + {delay:.2f}s > {ctx.retry_budget}s"
                    )
                    raise

                ctx.record_delay(delay)

                logger.warning(
                    f"Retry {ctx.attempt}/{ctx.max_retry} for status {status_code}, "
                    f"waiting {delay:.2f}s (total: {ctx.total_delay:.2f}s)"
                    + (f", Retry-After: {retry_after}s" if retry_after else "")
                )

                # 回调
                if on_retry:
                    on_retry(ctx.attempt, status_code, e, delay)

                await asyncio.sleep(delay)
                continue
            else:
                # 不可重试或重试次数耗尽
                if status_code in ctx.retry_codes:
                    logger.error(
                        f"Retry exhausted after {ctx.attempt} attempts, "
                        f"last status: {status_code}, total delay: {ctx.total_delay:.2f}s"
                    )
                else:
                    logger.error(f"Non-retryable status code: {status_code}")

                # 抛出最后一次的错误
                raise


def with_retry(
    extract_status: Callable[[Exception], Optional[int]] = None,
    on_retry: Callable[[int, int, Exception, float], None] = None,
):
    """
    重试装饰器

    Usage:
        @with_retry()
        async def my_api_call():
            ...
    """

    def decorator(func: Callable):
        @wraps(func)
        async def wrapper(*args, **kwargs):
            return await retry_on_status(
                func, *args, extract_status=extract_status, on_retry=on_retry, **kwargs
            )

        return wrapper

    return decorator


__all__ = [
    "RetryConfig",
    "RetryContext",
    "retry_on_status",
    "with_retry",
    "extract_retry_after",
]
