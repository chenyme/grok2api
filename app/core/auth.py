"""认证模块"""

from typing import Optional
from fastapi import Depends, HTTPException
from fastapi.security import HTTPBearer, HTTPAuthorizationCredentials
from app.core.config import setting
from app.core.logger import logger


security = HTTPBearer(auto_error=False)


class AuthManager:
    """认证管理器"""

    @staticmethod
    def verify(credentials: Optional[HTTPAuthorizationCredentials] = Depends(security)) -> Optional[str]:
        """验证认证令牌"""
        api_key = setting.grok_config.get("api_key")

        if not api_key:
            logger.debug("[Auth] 未设置API_KEY，跳过验证。")
            return credentials.credentials if credentials else None

        if not credentials:
            raise HTTPException(
                status_code=401,
                detail={
                    "error": {
                        "message": "缺少认证令牌",
                        "type": "authentication_error",
                        "code": "missing_token"
                    }
                }
            )

        if credentials.credentials != api_key:
            raise HTTPException(
                status_code=401,
                detail={
                    "error": {
                        "message": f"令牌无效，长度: {len(credentials.credentials)}",
                        "type": "authentication_error",
                        "code": "invalid_token"
                    }
                }
            )

        logger.debug("[Auth] 令牌认证成功")
        return credentials.credentials


auth_manager = AuthManager()