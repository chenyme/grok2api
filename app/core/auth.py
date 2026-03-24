"""
API 认证模块
"""

import hmac
from typing import Optional, Iterable
from fastapi import HTTPException, status, Security
from fastapi.security import HTTPBearer, HTTPAuthorizationCredentials

from app.core.config import get_config

DEFAULT_API_KEY = ""
DEFAULT_APP_KEY = "grok2api"
DEFAULT_FUNCTION_KEY = ""
DEFAULT_FUNCTION_ENABLED = False

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


def _normalize_api_keys(value: Optional[object]) -> list[str]:
    if not value:
        return []
    if isinstance(value, str):
        raw = value.strip()
        if not raw:
            return []
        return [part.strip() for part in raw.split(",") if part.strip()]
    if isinstance(value, Iterable):
        keys: list[str] = []
        for item in value:
            if not item:
                continue
            if isinstance(item, str):
                stripped = item.strip()
                if stripped:
                    keys.append(stripped)
        return keys
    return []


def _compare_token(left: str, right: str) -> bool:
    """Use byte comparison so non-ASCII tokens do not raise TypeError."""
    return hmac.compare_digest(
        left.encode("utf-8", errors="surrogatepass"),
        right.encode("utf-8", errors="surrogatepass"),
    )


def get_app_key() -> str:
    """
    获取 App Key（后台管理密码）。
    """
    app_key = get_config("app.app_key", DEFAULT_APP_KEY)
    return app_key or ""

def get_function_api_key() -> str:
    """
    获取功能玩法 API Key。

    为空时表示不启用功能玩法接口认证。
    """
    function_key = get_config("app.function_key", DEFAULT_FUNCTION_KEY)
    return function_key or ""


def is_function_enabled() -> bool:
    """
    是否开启功能玩法入口。
    """
    return bool(get_config("app.function_enabled", DEFAULT_FUNCTION_ENABLED))


def _match_function_key(credentials: str, function_key: str) -> bool:
    """检查凭证是否匹配 function_key。"""
    if not function_key:
        return False
    normalized = function_key.strip()
    if not normalized:
        return False
    # 常量时间比较，避免基于时序的探测
    return _compare_token(credentials, normalized)


async def verify_api_key(
    auth: Optional[HTTPAuthorizationCredentials] = Security(security),
) -> Optional[str]:
    """
    验证 Bearer Token

    如果 config.toml 中未配置 api_key，则不启用认证。
    """
    api_key = get_admin_api_key()
    api_keys = _normalize_api_keys(api_key)
    if not api_keys:
        return None

    if not auth:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Missing authentication token",
            headers={"WWW-Authenticate": "Bearer"},
        )

    # 标准 api_key 验证
    for key in api_keys:
        if _compare_token(auth.credentials, key):
            return auth.credentials

    raise HTTPException(
        status_code=status.HTTP_401_UNAUTHORIZED,
        detail="Invalid authentication token",
        headers={"WWW-Authenticate": "Bearer"},
    )


async def verify_app_key(
    auth: Optional[HTTPAuthorizationCredentials] = Security(security),
) -> Optional[str]:
    """
    验证后台登录密钥（app_key）。

    app_key 必须配置，否则拒绝登录。
    """
    app_key = get_app_key()

    if not app_key:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="App key is not configured",
            headers={"WWW-Authenticate": "Bearer"},
        )

    if not auth:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Missing authentication token",
            headers={"WWW-Authenticate": "Bearer"},
        )

    if not _compare_token(auth.credentials, app_key):
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Invalid authentication token",
            headers={"WWW-Authenticate": "Bearer"},
        )

    return auth.credentials


async def verify_function_key(
    auth: Optional[HTTPAuthorizationCredentials] = Security(security),
) -> Optional[str]:
    """
    验证功能玩法 Key（function 接口使用）。

    默认不公开，需配置 function_key 才能访问；
    若开启 function_enabled 且未配置 function_key，则放开访问。
    """
    function_key = get_function_api_key()
    function_enabled = is_function_enabled()

    if not function_key:
        if function_enabled:
            return None
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Function access is disabled",
            headers={"WWW-Authenticate": "Bearer"},
        )

    if not auth:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Missing authentication token",
            headers={"WWW-Authenticate": "Bearer"},
        )

    if _match_function_key(auth.credentials, function_key):
        return auth.credentials

    raise HTTPException(
        status_code=status.HTTP_401_UNAUTHORIZED,
        detail="Invalid authentication token",
        headers={"WWW-Authenticate": "Bearer"},
    )
