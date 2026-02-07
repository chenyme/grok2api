"""
NSFW (Unhinged) 模式服务

使用 gRPC-Web 协议开启账号的 NSFW 功能。
"""

from dataclasses import dataclass
from typing import Optional
import datetime
import random

from curl_cffi.requests import AsyncSession

from app.core.config import get_config
from app.core.logger import logger
from app.services.grok.protocols.grpc_web import (
    encode_grpc_web_payload,
    parse_grpc_web_response,
    get_grpc_status,
)
from app.services.grok.utils.headers import build_sso_cookie


NSFW_API = "https://grok.com/auth_mgmt.AuthManagement/UpdateUserFeatureControls"
BIRTH_DATE_API = "https://grok.com/rest/auth/set-birth-date"
DEFAULT_BROWSER = "chrome136"
DEFAULT_USER_AGENT = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36"
TIMEOUT = 30


@dataclass
class NSFWResult:
    """NSFW 操作结果"""

    success: bool
    http_status: int
    grpc_status: Optional[int] = None
    grpc_message: Optional[str] = None
    error: Optional[str] = None


class NSFWService:
    """NSFW 模式服务"""

    def __init__(self, proxy: str = None):
        self.proxy = proxy or get_config("grok.base_proxy_url", "")

    @staticmethod
    def _random_birth_date() -> str:
        """生成随机出生日期"""
        today = datetime.date.today()
        age = random.randint(20, 40)
        birth_year = today.year - age
        birth_month = random.randint(1, 12)
        birth_day = random.randint(1, 28)
        hour = random.randint(0, 23)
        minute = random.randint(0, 59)
        second = random.randint(0, 59)
        microsecond = random.randint(0, 999)
        return f"{birth_year:04d}-{birth_month:02d}-{birth_day:02d}T{hour:02d}:{minute:02d}:{second:02d}.{microsecond:03d}Z"

    def _build_headers(self, token: str) -> dict:
        """构造 gRPC-Web 请求头"""
        cookie = build_sso_cookie(token, include_rw=True)
        user_agent = get_config("grok.user_agent", DEFAULT_USER_AGENT)
        return {
            "accept": "*/*",
            "content-type": "application/grpc-web+proto",
            "origin": "https://grok.com",
            "referer": "https://grok.com/",
            "user-agent": user_agent,
            "x-grpc-web": "1",
            "x-user-agent": "connect-es/2.1.1",
            "cookie": cookie,
        }

    def _build_birth_headers(self, token: str) -> dict:
        """构造设置出生日期请求头"""
        cookie = build_sso_cookie(token, include_rw=True)
        user_agent = get_config("grok.user_agent", DEFAULT_USER_AGENT)
        return {
            "accept": "*/*",
            "content-type": "application/json",
            "origin": "https://grok.com",
            "referer": "https://grok.com/?_s=account",
            "user-agent": user_agent,
            "cookie": cookie,
        }

    @staticmethod
    def _build_payload() -> bytes:
        """构造请求 payload"""
        # protobuf (match captured HAR):
        # 0a 02 10 01                   -> field 1 (len=2) with inner bool=true
        # 12 1a                         -> field 2, length 26
        #   0a 18 <name>                -> nested message with name string
        name = b"always_show_nsfw_content"
        inner = b"\x0a" + bytes([len(name)]) + name
        protobuf = b"\x0a\x02\x10\x01\x12" + bytes([len(inner)]) + inner
        return encode_grpc_web_payload(protobuf)

    async def _set_birth_date(self, session: AsyncSession, token: str) -> tuple[bool, int, Optional[str]]:
        """设置出生日期"""
        headers = self._build_birth_headers(token)
        payload = {"birthDate": self._random_birth_date()}
        proxies = {"http": self.proxy, "https": self.proxy} if self.proxy else None
        try:
            response = await session.post(
                BIRTH_DATE_API,
                json=payload,
                headers=headers,
                timeout=TIMEOUT,
                proxies=proxies,
            )
            if response.status_code in (200, 204):
                return True, response.status_code, None
            return False, response.status_code, f"HTTP {response.status_code}"
        except Exception as e:
            return False, 0, str(e)[:100]

    async def enable(self, token: str) -> NSFWResult:
        """为单个 token 开启 NSFW 模式"""
        headers = self._build_headers(token)
        payload = self._build_payload()
        logger.debug(
            "NSFW payload: len={} hex={}",
            len(payload),
            payload.hex(),
        )
        proxies = {"http": self.proxy, "https": self.proxy} if self.proxy else None

        try:
            browser = get_config("grok.browser", DEFAULT_BROWSER)
            async with AsyncSession(impersonate=browser) as session:
                ok, birth_status, birth_err = await self._set_birth_date(
                    session, token
                )
                if not ok:
                    return NSFWResult(
                        success=False,
                        http_status=birth_status,
                        error=f"Set birth date failed: {birth_err}",
                    )
                response = await session.post(
                    NSFW_API,
                    data=payload,
                    headers=headers,
                    timeout=TIMEOUT,
                    proxies=proxies,
                )

                if response.status_code != 200:
                    return NSFWResult(
                        success=False,
                        http_status=response.status_code,
                        error=f"HTTP {response.status_code}",
                    )

                # 解析 gRPC-Web 响应
                content_type = response.headers.get("content-type")
                _, trailers = parse_grpc_web_response(
                    response.content, content_type=content_type
                )

                grpc_status = get_grpc_status(trailers)
                logger.debug(
                    "NSFW response: http={} grpc={} msg={} trailers={}",
                    response.status_code,
                    grpc_status.code,
                    grpc_status.message,
                    trailers,
                )

                # HTTP 200 且无 grpc-status（空响应）或 grpc-status=0 都算成功
                success = grpc_status.code == -1 or grpc_status.ok

                return NSFWResult(
                    success=success,
                    http_status=response.status_code,
                    grpc_status=grpc_status.code,
                    grpc_message=grpc_status.message or None,
                )

        except Exception as e:
            logger.error(f"NSFW enable failed: {e}")
            return NSFWResult(success=False, http_status=0, error=str(e)[:100])


__all__ = ["NSFWService", "NSFWResult"]
