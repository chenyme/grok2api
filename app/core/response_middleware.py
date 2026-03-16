"""
响应中间件
Response Middleware

用于记录请求日志、生成 TraceID 和计算请求耗时
"""

import time
import uuid
from starlette.middleware.base import BaseHTTPMiddleware
from starlette.requests import Request

from app.core.config import get_config
from app.core.logger import logger


class ResponseLoggerMiddleware(BaseHTTPMiddleware):
    """
    请求日志/响应追踪中间件
    Request Logging and Response Tracking Middleware
    """

    @staticmethod
    def _should_log_response(path: str, status_code: int, duration_ms: float) -> bool:
        if path == "/health" and not bool(
            get_config("log.log_health_requests", False)
        ):
            return False

        if bool(get_config("log.log_all_requests", False)):
            return True

        try:
            slow_ms = float(get_config("log.request_slow_ms", 3000))
        except (TypeError, ValueError):
            slow_ms = 3000.0

        return status_code >= 400 or duration_ms >= slow_ms

    async def dispatch(self, request: Request, call_next):
        # 生成请求 ID
        trace_id = str(uuid.uuid4())
        request.state.trace_id = trace_id

        start_time = time.time()
        path = request.url.path

        if path.startswith("/static/") or path in (
            "/",
            "/login",
            "/imagine",
            "/voice",
            "/admin",
            "/admin/login",
            "/admin/config",
            "/admin/cache",
            "/admin/token",
        ):
            return await call_next(request)

        try:
            response = await call_next(request)

            # 计算耗时
            duration = (time.time() - start_time) * 1000

            if self._should_log_response(path, response.status_code, duration):
                log_method = (
                    logger.error
                    if response.status_code >= 500
                    else logger.warning
                    if response.status_code >= 400
                    else logger.info
                )
                log_method(
                    f"Response: {request.method} {request.url.path} - {response.status_code} ({duration:.2f}ms)",
                    extra={
                        "traceID": trace_id,
                        "method": request.method,
                        "path": request.url.path,
                        "status": response.status_code,
                        "duration_ms": round(duration, 2),
                    },
                )

            return response

        except Exception as e:
            duration = (time.time() - start_time) * 1000
            logger.opt(exception=e).error(
                f"Response Error: {request.method} {request.url.path} - {str(e)} ({duration:.2f}ms)",
                extra={
                    "traceID": trace_id,
                    "method": request.method,
                    "path": request.url.path,
                    "duration_ms": round(duration, 2),
                    "error": str(e),
                },
            )
            raise
