"""
Reverse interface: list assets.
"""

from typing import Any, Dict
from curl_cffi.requests import AsyncSession

from app.core.logger import logger
from app.core.config import get_config
from app.core.exceptions import UpstreamException
from app.services.token.service import TokenService
from app.services.reverse.utils.headers import build_headers
from app.services.reverse.utils.retry import retry_on_status

LIST_API = "https://grok.com/rest/assets"


class AssetsListReverse:
    """/rest/assets reverse interface."""

    @staticmethod
    async def request(session: AsyncSession, token: str, params: Dict[str, Any]) -> Any:
        """List assets from Grok.

        Args:
            session: AsyncSession, the session to use for the request.
            token: str, the SSO token.
            params: Dict[str, Any], the parameters for the request.

        Returns:
            Any: The response from the request.
        """
        try:
            # Get proxies
            base_proxy = get_config("proxy.base_proxy_url")
            assert_proxy = get_config("proxy.asset_proxy_url")
            if assert_proxy:
                proxies = {"http": assert_proxy, "https": assert_proxy}
            else:
                proxies = {"http": base_proxy, "https": base_proxy}

            # Build headers
            headers = build_headers(
                cookie_token=token,
                content_type="application/json",
                origin="https://grok.com",
                referer="https://grok.com/files",
            )

            # Curl Config
            timeout = get_config("asset.list_timeout")
            browser = get_config("proxy.browser")

            async def _do_request():
                response = await session.get(
                    LIST_API,
                    headers=headers,
                    params=params,
                    proxies=proxies,
                    timeout=timeout,
                    impersonate=browser,
                )

                if response.status_code != 200:
                    logger.error(
                        f"AssetsListReverse: List failed, {response.status_code}",
                        extra={"error_type": "UpstreamException"},
                    )
                    raise UpstreamException(
                        message=f"AssetsListReverse: List failed, {response.status_code}",
                        details={"status": response.status_code},
                    )

                return response

            return await retry_on_status(_do_request)

        except Exception as e:
            # Handle upstream exception
            if isinstance(e, UpstreamException):
                status = None
                if e.details and "status" in e.details:
                    status = e.details["status"]
                else:
                    status = getattr(e, "status_code", None)
                if status == 401:
                    try:
                        await TokenService.record_fail(token, status, "assets_list_auth_failed")
                    except Exception:
                        pass
                raise

            # Handle other non-upstream exceptions
            logger.error(
                f"AssetsListReverse: List failed, {str(e)}",
                extra={"error_type": type(e).__name__},
            )
            raise UpstreamException(
                message=f"AssetsListReverse: List failed, {str(e)}",
                details={"status": 502, "error": str(e)},
            )


__all__ = ["AssetsListReverse"]
