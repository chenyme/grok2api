"""
API 认证模块
"""

import hashlib
from typing import Optional
from fastapi import HTTPException, status, Security
from fastapi.security import HTTPBearer, HTTPAuthorizationCredentials

from app.core.config import get_config

DEFAULT_API_KEY = ""
DEFAULT_APP_KEY = "grok2api"
DEFAULT_PUBLIC_KEY = ""
DEFAULT_PUBLIC_ENABLED = False

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

def get_app_key() -> str:
    """
    获取 App Key（后台管理密码）。
    """
    app_key = get_config("app.app_key", DEFAULT_APP_KEY)
    return app_key or ""

def get_public_api_key() -> str:
    """
    获取 Public API Key。

    为空时表示不启用 public 接口认证。
    """
    public_key = get_config("app.public_key", DEFAULT_PUBLIC_KEY)
    return public_key or ""

def is_public_enabled() -> bool:
    """
    是否开启 public 功能入口。
    """
    return bool(get_config("app.public_enabled", DEFAULT_PUBLIC_ENABLED))


def _hash_public_key(key: str) -> str:
    """计算 public_key 的 SHA-256 哈希，与前端 hashPublicKey 保持一致。"""
    return hashlib.sha256(f"grok2api-public:{key}".encode()).hexdigest()


def _match_public_key(credentials: str, public_key: str) -> bool:
    """检查凭证是否匹配 public_key（支持原始值和 public-<sha256> 哈希格式）。"""
    if not public_key:
        return False
    if credentials == public_key:
        return True
    if credentials.startswith("public-"):
        expected_hash = _hash_public_key(public_key)
        if credentials == f"public-{expected_hash}":
            return True
    return False


async def verify_api_key(
    auth: Optional[HTTPAuthorizationCredentials] = Security(security),
) -> Optional[str]:
    """
    验证 Bearer Token

    如果 config.toml 中未配置 api_key，则不启用认证。
    同时支持面板通过 public_key 认证（原始值或 public-<sha256> 哈希格式）。
    若 public_enabled 且未配置 public_key，允许无认证访问。
    """
    api_key = get_admin_api_key()
    public_key = get_public_api_key()
    public_enabled = is_public_enabled()

    if not api_key:
        return None

    # 无认证头：仅在 public_enabled 且无 public_key 时放行
    if not auth:
        if public_enabled and not public_key:
            return None
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Missing authentication token",
            headers={"WWW-Authenticate": "Bearer"},
        )

    # 标准 api_key 验证
    if auth.credentials == api_key:
        return auth.credentials

    # public_key 验证（原始值或哈希）
    if _match_public_key(auth.credentials, public_key):
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

    if auth.credentials != app_key:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Invalid authentication token",
            headers={"WWW-Authenticate": "Bearer"},
        )

    return auth.credentials


async def verify_public_key(
    auth: Optional[HTTPAuthorizationCredentials] = Security(security),
) -> Optional[str]:
    """
    验证 Public Key（public 接口使用）。

    默认不公开，需配置 public_key 才能访问；若开启 public_enabled 且未配置 public_key，则放开访问。
    """
    public_key = get_public_api_key()
    public_enabled = is_public_enabled()

    if not public_key:
        if public_enabled:
            return None
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Public access is disabled",
            headers={"WWW-Authenticate": "Bearer"},
        )

    if not auth:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Missing authentication token",
            headers={"WWW-Authenticate": "Bearer"},
        )

    if _match_public_key(auth.credentials, public_key):
        return auth.credentials

    raise HTTPException(
        status_code=status.HTTP_401_UNAUTHORIZED,
        detail="Invalid authentication token",
        headers={"WWW-Authenticate": "Bearer"},
    )
