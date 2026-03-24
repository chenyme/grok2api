"""
Reverse interface: media post create link.
"""

import orjson
from typing import Any
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

MEDIA_POST_LINK_API = "https://grok.com/rest/media/post/create-link"


class MediaPostLinkReverse:
    """/rest/media/post/create-link reverse interface."""

    @staticmethod
    async def request(
        session: AsyncSession,
        token: str,
        post_id: str,
    ) -> Any:
        try:
            # Build headers
            headers = build_headers(
                cookie_token=token,
                content_type="application/json",
                origin="https://grok.com",
                referer="https://grok.com",
            )

            # Build payload
            payload = {
                "postId": post_id,
                "source": "post-page",
                "platform": "web"
            }

            # Curl Config
            timeout = get_config("video.timeout")
            browser = get_config("proxy.browser")
            active_proxy_key = None

            async def _do_request():
                nonlocal active_proxy_key
                active_proxy_key, proxy_url = get_current_proxy_from("proxy.base_proxy_url")
                proxies = build_http_proxies(proxy_url)
                response = await session.post(
                    MEDIA_POST_LINK_API,
                    headers=headers,
                    data=orjson.dumps(payload),
                    timeout=timeout,
                    proxies=proxies,
                    impersonate=browser,
                )

                if response.status_code != 200:
                    content = ""
                    try:
                        content = await response.text()
                    except Exception:
                        pass
                    logger.error(
                        f"MediaPostLinkReverse: Media post create link failed, {response.status_code}",
                        extra={"error_type": "UpstreamException"},
                    )
                    raise UpstreamException(
                        message=f"MediaPostLinkReverse: Media post create link failed, {response.status_code}",
                        details={"status": response.status_code, "body": content},
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
                        await TokenService.record_fail(token, status, "media_post_link_auth_failed")
                    except Exception:
                        pass
                raise

            # Handle other non-upstream exceptions
            if not get_config("proxy.base_proxy_url"):
                try:
                    logger.warning(
                        "MediaPostLinkReverse: curl-cffi failed; retrying with aiohttp fallback"
                    )
                    return await request_with_aiohttp_fallback(
                        method="POST",
                        url=MEDIA_POST_LINK_API,
                        headers=headers,
                        timeout=float(get_config("video.timeout") or 60),
                        logger_prefix="MediaPostLinkReverse: Media post create link failed",
                        expected_statuses=(200,),
                        response_kind="json",
                        data=orjson.dumps(payload),
                    )
                except UpstreamException:
                    raise
                except Exception as fallback_exc:
                    logger.warning(
                        f"MediaPostLinkReverse: aiohttp fallback failed, {fallback_exc}"
                    )

            logger.error(
                f"MediaPostLinkReverse: Media post create link failed, {str(e)}",
                extra={"error_type": type(e).__name__},
            )
            raise UpstreamException(
                message=f"MediaPostLinkReverse: Media post create link failed, {str(e)}",
                details={"status": 502, "error": str(e)},
            )


__all__ = ["MediaPostLinkReverse"]
