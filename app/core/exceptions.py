"""
全局异常处理 - OpenAI 兼容错误格式
"""

from typing import Any, Optional
from enum import Enum
from fastapi import Request, HTTPException
from fastapi.responses import JSONResponse
from fastapi.exceptions import RequestValidationError

from app.core.logger import logger


# ============= 错误类型 =============

class ErrorType(str, Enum):
    """OpenAI 错误类型"""
    INVALID_REQUEST = "invalid_request_error"
    AUTHENTICATION = "authentication_error"
    PERMISSION = "permission_error"
    NOT_FOUND = "not_found_error"
    RATE_LIMIT = "rate_limit_error"
    SERVER = "server_error"
    SERVICE_UNAVAILABLE = "service_unavailable_error"


# ============= 辅助函数 =============

def error_response(
    message: str,
    error_type: str = ErrorType.INVALID_REQUEST.value,
    param: str = None,
    code: str = None
) -> dict:
    """构建 OpenAI 错误响应"""
    return {
        "error": {
            "message": message,
            "type": error_type,
            "param": param,
            "code": code
        }
    }


# ============= 异常类 =============

class AppException(Exception):
    """应用基础异常"""
    
    def __init__(
        self,
        message: str,
        error_type: str = ErrorType.SERVER.value,
        code: str = None,
        param: str = None,
        status_code: int = 500
    ):
        self.message = message
        self.error_type = error_type
        self.code = code
        self.param = param
        self.status_code = status_code
        super().__init__(message)


class ValidationException(AppException):
    """验证错误"""
    
    def __init__(self, message: str, param: str = None, code: str = None):
        super().__init__(
            message=message,
            error_type=ErrorType.INVALID_REQUEST.value,
            code=code or "invalid_value",
            param=param,
            status_code=400
        )


class AuthenticationException(AppException):
    """认证错误"""
    
    def __init__(self, message: str = "Invalid API key"):
        super().__init__(
            message=message,
            error_type=ErrorType.AUTHENTICATION.value,
            code="invalid_api_key",
            status_code=401
        )


class UpstreamException(AppException):
    """上游服务错误"""
    
    def __init__(self, message: str, details: Any = None):
        super().__init__(
            message=message,
            error_type=ErrorType.SERVER.value,
            code="upstream_error",
            status_code=502
        )
        self.details = details


# ============= 异常处理器 =============

async def app_exception_handler(request: Request, exc: AppException) -> JSONResponse:
    """处理应用异常"""
    logger.warning(f"AppException: {exc.error_type} - {exc.message}")
    
    return JSONResponse(
        status_code=exc.status_code,
        content=error_response(
            message=exc.message,
            error_type=exc.error_type,
            param=exc.param,
            code=exc.code
        )
    )


async def http_exception_handler(request: Request, exc: HTTPException) -> JSONResponse:
    """处理 HTTP 异常"""
    type_map = {
        400: ErrorType.INVALID_REQUEST.value,
        401: ErrorType.AUTHENTICATION.value,
        403: ErrorType.PERMISSION.value,
        404: ErrorType.NOT_FOUND.value,
        429: ErrorType.RATE_LIMIT.value,
    }
    error_type = type_map.get(exc.status_code, ErrorType.SERVER.value)
    
    # 默认 code 映射
    code_map = {
        401: "invalid_api_key",
        403: "insufficient_quota",
        404: "model_not_found",
        429: "rate_limit_exceeded",
    }
    code = code_map.get(exc.status_code, None)
    
    logger.warning(f"HTTPException: {exc.status_code} - {exc.detail}")
    
    return JSONResponse(
        status_code=exc.status_code,
        content=error_response(
            message=str(exc.detail),
            error_type=error_type,
            code=code
        )
    )


async def validation_exception_handler(request: Request, exc: RequestValidationError) -> JSONResponse:
    """处理验证错误"""
    errors = exc.errors()
    
    if errors:
        first = errors[0]
        loc = first.get("loc", [])
        msg = first.get("msg", "Invalid request")
        code = first.get("type", "invalid_value")
        
        # JSON 解析错误
        if code == "json_invalid" or "JSON" in msg:
            message = "Invalid JSON in request body. Please check for trailing commas or syntax errors."
            param = "body"
        else:
            param_parts = [str(x) for x in loc if not (isinstance(x, int) or str(x).isdigit())]
            param = ".".join(param_parts) if param_parts else None
            message = msg
    else:
        param, message, code = None, "Invalid request", "invalid_value"
    
    logger.warning(f"ValidationError: {param} - {message}")
    
    return JSONResponse(
        status_code=400,
        content=error_response(
            message=message,
            error_type=ErrorType.INVALID_REQUEST.value,
            param=param,
            code=code
        )
    )


async def generic_exception_handler(request: Request, exc: Exception) -> JSONResponse:
    """处理未捕获异常"""
    logger.exception(f"Unhandled: {type(exc).__name__}: {str(exc)}")
    
    return JSONResponse(
        status_code=500,
        content=error_response(
            message="Internal server error",
            error_type=ErrorType.SERVER.value,
            code="internal_error"
        )
    )


# ============= 注册 =============

def register_exception_handlers(app):
    """注册异常处理器"""
    app.add_exception_handler(AppException, app_exception_handler)
    app.add_exception_handler(HTTPException, http_exception_handler)
    app.add_exception_handler(RequestValidationError, validation_exception_handler)
    app.add_exception_handler(Exception, generic_exception_handler)
    app.add_exception_handler(Exception, generic_exception_handler)


__all__ = [
    "ErrorType",
    "AppException",
    "ValidationException",
    "AuthenticationException",
    "UpstreamException",
    "error_response",
    "register_exception_handlers",
]
