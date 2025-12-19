"""认证模块 - API令牌验证"""

from typing import Optional
from fastapi import Depends, HTTPException
from fastapi.security import HTTPBearer, HTTPAuthorizationCredentials

from app.core.config import setting
from app.core.logger import logger


# Bearer安全方案
security = HTTPBearer(auto_error=False)


def _build_error(message: str, code: str = "invalid_token") -> dict:
    """构建认证错误"""
    return {
        "error": {
            "message": message,
            "type": "authentication_error",
            "code": code
        }
    }


class AuthManager:
    """认证管理器 - 验证API令牌"""

    @staticmethod
    def verify(credentials: Optional[HTTPAuthorizationCredentials] = Depends(security)) -> Optional[str]:
        """验证令牌"""
        api_key = setting.grok_config.get("api_key")

        # 未设置时跳过
        if not api_key:
            logger.debug("[Auth] 未设置API_KEY，跳过验证")
            return credentials.credentials if credentials else None

        # 检查令牌
        if not credentials:
            raise HTTPException(
                status_code=401,
                detail=_build_error("缺少认证令牌", "missing_token")
            )

        # 验证令牌
        if credentials.credentials != api_key:
            raise HTTPException(
                status_code=401,
                detail=_build_error(f"令牌无效，长度: {len(credentials.credentials)}", "invalid_token")
            )

        logger.debug("[Auth] 令牌认证成功")
        return credentials.credentials


# 全局实例
auth_manager = AuthManager()