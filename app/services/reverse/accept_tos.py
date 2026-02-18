"""
Reverse interface: accept ToS (gRPC-Web).
"""

from curl_cffi.requests import AsyncSession

from app.core.logger import logger
from app.core.config import get_config
from app.core.exceptions import UpstreamException
from app.services.reverse.utils.headers import build_headers
from app.services.reverse.utils.retry import retry_on_status
from app.services.reverse.utils.grpc import GrpcClient, GrpcStatus

ACCEPT_TOS_API = "https://accounts.x.ai/auth_mgmt.AuthManagement/SetTosAcceptedVersion"


class AcceptTosReverse:
    """/auth_mgmt.AuthManagement/SetTosAcceptedVersion reverse interface."""

    @staticmethod
    async def request(session: AsyncSession, token: str) -> GrpcStatus:
        """Accept ToS via gRPC-Web.

        Args:
            session: AsyncSession, the session to use for the request.
            token: str, the SSO token.

        Returns:
            GrpcStatus: Parsed gRPC status.
        """
        try:
            # Get proxies
            base_proxy = get_config("proxy.base_proxy_url")
            proxies = {"http": base_proxy, "https": base_proxy} if base_proxy else None

            # Build headers
            headers = build_headers(
                cookie_token=token,
                origin="https://accounts.x.ai",
                referer="https://accounts.x.ai/accept-tos",
            )
            headers["Content-Type"] = "application/grpc-web+proto"
            headers["Accept"] = "*/*"
            headers["Sec-Fetch-Dest"] = "empty"
            headers["x-grpc-web"] = "1"
            headers["x-user-agent"] = "connect-es/2.1.1"
            headers["Cache-Control"] = "no-cache"
            headers["Pragma"] = "no-cache"

            # Build payload
            payload = GrpcClient.encode_payload(b"\x10\x01")

            # Curl Config
            timeout = get_config("nsfw.timeout")
            browser = get_config("proxy.browser")

            async def _do_request():
                response = await session.post(
                    ACCEPT_TOS_API,
                    headers=headers,
                    data=payload,
                    timeout=timeout,
                    proxies=proxies,
                    impersonate=browser,
                )

                if response.status_code != 200:
                    logger.error(
                        f"AcceptTosReverse: Request failed, {response.status_code}",
                        extra={"error_type": "UpstreamException"},
                    )
                    raise UpstreamException(
                        message=f"AcceptTosReverse: Request failed, {response.status_code}",
                        details={"status": response.status_code},
                    )

                logger.debug(f"AcceptTosReverse: Request successful, {response.status_code}")

                return response

            response = await retry_on_status(_do_request)

            _, trailers = GrpcClient.parse_response(
                response.content,
                content_type=response.headers.get("content-type"),
                headers=response.headers,
            )
            grpc_status = GrpcClient.get_status(trailers)

            if grpc_status.code not in (-1, 0):
                raise UpstreamException(
                    message=f"AcceptTosReverse: gRPC failed, {grpc_status.code}",
                    details={
                        "status": grpc_status.http_equiv,
                        "grpc_status": grpc_status.code,
                        "grpc_message": grpc_status.message,
                    },
                )

            return grpc_status

        except Exception as e:
            # Handle upstream exception
            if isinstance(e, UpstreamException):
                raise

            # Handle other non-upstream exceptions
            logger.error(
                f"AcceptTosReverse: Request failed, {str(e)}",
                extra={"error_type": type(e).__name__},
            )
            raise UpstreamException(
                message=f"AcceptTosReverse: Request failed, {str(e)}",
                details={"status": 502, "error": str(e)},
            )


__all__ = ["AcceptTosReverse"]
