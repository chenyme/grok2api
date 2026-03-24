"""
Reverse interface: list assets.
"""

from typing import Any, Dict
from curl_cffi.requests import AsyncSession

from app.core.logger import logger
from app.core.config import get_config
from app.core.proxy_pool import (
    build_http_proxies,
    get_current_proxy_from,
    rotate_proxy,
    should_rotate_proxy,
)
from app.core.exceptions import UpstreamException
from app.services.token.service import TokenService
from app.services.reverse.utils.headers import build_headers
from app.services.reverse.utils.http_fallback import request_with_aiohttp_fallback
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
            active_proxy_key = None

            async def _do_request():
                nonlocal active_proxy_key
                active_proxy_key, proxy_url = get_current_proxy_from(
                    "proxy.asset_proxy_url",
                    "proxy.base_proxy_url",
                )
                proxies = build_http_proxies(proxy_url)
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

            async def _on_retry(attempt: int, status_code: int, error: Exception, delay: float):
                if active_proxy_key and should_rotate_proxy(status_code):
                    rotate_proxy(active_proxy_key)

            return await retry_on_status(_do_request, on_retry=_on_retry)

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
            if not get_config("proxy.asset_proxy_url") and not get_config("proxy.base_proxy_url"):
                try:
                    logger.warning(
                        "AssetsListReverse: curl-cffi failed; retrying with aiohttp fallback"
                    )
                    return await request_with_aiohttp_fallback(
                        method="GET",
                        url=LIST_API,
                        headers=headers,
                        timeout=float(get_config("asset.list_timeout") or 60),
                        logger_prefix="AssetsListReverse: List failed",
                        expected_statuses=(200,),
                        response_kind="json",
                        params=params,
                    )
                except UpstreamException:
                    raise
                except Exception as fallback_exc:
                    logger.warning(
                        f"AssetsListReverse: aiohttp fallback failed, {fallback_exc}"
                    )

            logger.error(
                f"AssetsListReverse: List failed, {str(e)}",
                extra={"error_type": type(e).__name__},
            )
            raise UpstreamException(
                message=f"AssetsListReverse: List failed, {str(e)}",
                details={"status": 502, "error": str(e)},
            )


__all__ = ["AssetsListReverse"]
