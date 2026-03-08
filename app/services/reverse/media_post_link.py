"""
Reverse interface: media post create link.
"""

import orjson
from typing import Any
from curl_cffi.requests import AsyncSession

from app.core.logger import logger
from app.core.config import get_config
from app.core.exceptions import UpstreamException
from app.services.token.service import TokenService
from app.services.reverse.utils.headers import build_headers
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
            # Get proxies
            base_proxy = get_config("proxy.base_proxy_url")
            proxies = {"http": base_proxy, "https": base_proxy} if base_proxy else None

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

            async def _do_request():
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
                        await TokenService.record_fail(token, status, "media_post_link_auth_failed")
                    except Exception:
                        pass
                raise

            # Handle other non-upstream exceptions
            logger.error(
                f"MediaPostLinkReverse: Media post create link failed, {str(e)}",
                extra={"error_type": type(e).__name__},
            )
            raise UpstreamException(
                message=f"MediaPostLinkReverse: Media post create link failed, {str(e)}",
                details={"status": 502, "error": str(e)},
            )


__all__ = ["MediaPostLinkReverse"]
