"""
API 认证模块
"""

import secrets
from typing import Optional, Dict, Any
from fastapi import HTTPException, status, Security, Request
from fastapi.security import HTTPBearer, HTTPAuthorizationCredentials

from app.core.config import get_config
from app.core.logger import logger
from app.core.network import trusted_proxy_ips

DEFAULT_API_KEY = ""
DEFAULT_APP_PASSWORD = ""

# 定义 Bearer Scheme
security = HTTPBearer(
    auto_error=False,
    scheme_name="API Key",
    description="Enter your API Key in the format: Bearer <key>",
)


def get_admin_api_key() -> str:
    """
    获取后台 API Key。

    为空时表示不启用后台接口认证。
    """
    api_key = get_config("app.api_key", DEFAULT_API_KEY)
    return api_key or ""


def _allow_anonymous_api() -> bool:
    return bool(get_config("security.allow_anonymous_api", False))


def _allow_anonymous_admin() -> bool:
    return bool(get_config("security.allow_anonymous_admin", False))


async def validate_admin_token(token: str) -> bool:
    """
    验证后台访问凭据（兼容 app_password 与 API Key）。

    Returns:
        True 表示通过验证；False 表示验证失败。
    """
    from app.services.api_keys import api_key_manager

    if not api_key_manager._loaded:
        await api_key_manager.init()

    app_password = get_config("app.app_password", "")
    api_key = get_admin_api_key()
    has_custom_keys = len(api_key_manager.get_all_keys()) > 0

    # 未配置任何 key 时按配置决定是否允许匿名
    if not (app_password or api_key or has_custom_keys):
        return _allow_anonymous_admin()

    if app_password and secrets.compare_digest(token, app_password):
        return True

    key_info = api_key_manager.validate_key(token)
    if not key_info:
        return False
    return bool(key_info.get("is_admin", False))


def _is_from_trusted_proxy(request: Request) -> bool:
    if not request.client:
        return False
    trusted = trusted_proxy_ips()
    if "*" in trusted:
        logger.warning(
            "Wildcard trusted_proxy_ips detected - all proxies trusted. Restrict in production."
        )
        return True
    return str(request.client.host) in trusted


def get_client_ip(request: Request) -> str:
    """获取客户端真实 IP（仅信任受信代理转发头）"""
    if _is_from_trusted_proxy(request):
        forwarded = request.headers.get("X-Forwarded-For")
        if forwarded:
            return forwarded.split(",")[0].strip()

        real_ip = request.headers.get("X-Real-IP")
        if real_ip:
            return real_ip.strip()

    if request.client:
        return request.client.host

    return "unknown"


async def verify_api_key(
    auth: Optional[HTTPAuthorizationCredentials] = Security(security),
) -> Optional[str]:
    """
    验证 Bearer Token

    支持两种 Key：
    1. 配置文件中的 api_key (管理员)
    2. Key 管理页面生成的 keys (多用户)

    如果 config.toml 中未配置 api_key 且没有自定义 keys，则不启用认证。
    """
    from app.services.api_keys import api_key_manager

    # 初始化 key manager
    if not api_key_manager._loaded:
        await api_key_manager.init()

    api_key = get_admin_api_key()
    has_custom_keys = len(api_key_manager.get_all_keys()) > 0

    # 没有配置 key 时默认拒绝（可通过配置显式放开匿名）
    if not api_key and not has_custom_keys:
        if _allow_anonymous_api():
            return None
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="API authentication is not configured",
            headers={"WWW-Authenticate": "Bearer"},
        )

    if not auth:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Missing authentication token",
            headers={"WWW-Authenticate": "Bearer"},
        )

    # 使用 api_key_manager 验证（它会同时检查配置 key 和自定义 keys）
    key_info = api_key_manager.validate_key(auth.credentials)
    if key_info:
        return auth.credentials

    raise HTTPException(
        status_code=status.HTTP_401_UNAUTHORIZED,
        detail="Invalid authentication token",
        headers={"WWW-Authenticate": "Bearer"},
    )


async def verify_api_key_with_info(
    request: Request,
    auth: Optional[HTTPAuthorizationCredentials] = Security(security),
) -> Dict[str, Any]:
    """
    验证 Bearer Token 并返回详细信息（IP、key_name）

    返回: {"key": str, "name": str, "ip": str}
    """
    from app.services.api_keys import api_key_manager

    # 初始化 key manager
    if not api_key_manager._loaded:
        await api_key_manager.init()

    client_ip = get_client_ip(request)

    api_key = get_admin_api_key()
    has_custom_keys = len(api_key_manager.get_all_keys()) > 0
    if not api_key and not has_custom_keys:
        if _allow_anonymous_api():
            return {"key": "", "name": "default", "ip": client_ip}
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="API authentication is not configured",
            headers={"WWW-Authenticate": "Bearer"},
        )

    if not auth:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Missing authentication token",
            headers={"WWW-Authenticate": "Bearer"},
        )

    # 验证并获取 key 信息
    key_info = api_key_manager.validate_key(auth.credentials)
    if not key_info:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Invalid authentication token",
            headers={"WWW-Authenticate": "Bearer"},
        )

    return {
        "key": auth.credentials,
        "name": key_info.get("name", "default"),
        "ip": client_ip,
    }


async def get_request_key_name(request: Request, default: str = "default") -> str:
    """从请求头中解析并返回 key 名称（仅用于日志标识）。"""
    auth_header = request.headers.get("Authorization", "")
    if not auth_header.startswith("Bearer "):
        return default

    token = auth_header[7:].strip()
    if not token:
        return default

    from app.services.api_keys import api_key_manager

    if not api_key_manager._loaded:
        await api_key_manager.init()

    key_info = api_key_manager.validate_key(token)
    if not key_info:
        return default
    return key_info.get("name", default)


async def verify_app_password(
    auth: Optional[HTTPAuthorizationCredentials] = Security(security),
) -> Optional[str]:
    """
    验证后台登录密码（app_password）。

    app_password 必须配置，否则拒绝登录。
    """
    app_password = get_config("app.app_password", DEFAULT_APP_PASSWORD)

    if not app_password:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="App password is not configured",
            headers={"WWW-Authenticate": "Bearer"},
        )

    if not auth:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Missing authentication token",
            headers={"WWW-Authenticate": "Bearer"},
        )

    if not secrets.compare_digest(auth.credentials, app_password):
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Invalid authentication token",
            headers={"WWW-Authenticate": "Bearer"},
        )

    return auth.credentials


async def verify_admin_access(
    auth: Optional[HTTPAuthorizationCredentials] = Security(security),
) -> Optional[str]:
    """
    后台接口认证（兼容 app_password 与 API Key）。

    规则:
    - app_password / api_key / 自定义 keys 任一配置存在时，必须认证
    - 当未配置任何 key 时，视为不启用认证
    """
    from app.services.api_keys import api_key_manager

    if not api_key_manager._loaded:
        await api_key_manager.init()

    app_password = get_config("app.app_password", "")
    api_key = get_admin_api_key()
    has_custom_keys = len(api_key_manager.get_all_keys()) > 0

    auth_required = bool(app_password or api_key or has_custom_keys)
    if not auth_required:
        if _allow_anonymous_admin():
            return None
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Admin authentication is not configured",
            headers={"WWW-Authenticate": "Bearer"},
        )

    if not auth:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Missing authentication token",
            headers={"WWW-Authenticate": "Bearer"},
        )

    if await validate_admin_token(auth.credentials):
        return auth.credentials

    raise HTTPException(
        status_code=status.HTTP_401_UNAUTHORIZED,
        detail="Invalid authentication token",
        headers={"WWW-Authenticate": "Bearer"},
    )
