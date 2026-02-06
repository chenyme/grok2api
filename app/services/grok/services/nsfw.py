"""
NSFW (Unhinged) 模式服务

使用 gRPC-Web 协议开启账号的 NSFW 功能。
"""

from dataclasses import dataclass
from typing import Optional

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
BROWSER = "chrome136"
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

    def _build_headers(self, token: str) -> dict:
        """构造 gRPC-Web 请求头"""
        cookie = build_sso_cookie(token, include_rw=True)
        return {
            "accept": "*/*",
            "content-type": "application/grpc-web+proto",
            "origin": "https://grok.com",
            "referer": "https://grok.com/",
            "user-agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36",
            "x-grpc-web": "1",
            "x-user-agent": "connect-es/2.1.1",
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
            async with AsyncSession(impersonate=BROWSER) as session:
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
