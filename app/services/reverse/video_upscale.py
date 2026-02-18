"""
Reverse interface: video upscale.
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

VIDEO_UPSCALE_API = "https://grok.com/rest/media/video/upscale"


class VideoUpscaleReverse:
    """/rest/media/video/upscale reverse interface."""

    @staticmethod
    async def request(session: AsyncSession, token: str, video_id: str) -> Any:
        """Upscale video (image upscaling endpoint) in Grok.

        Args:
            session: AsyncSession, the session to use for the request.
            token: str, the SSO token.
            video_id: str, the video id.

        Returns:
            Any: The response from the request.
        """
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
            payload = {"videoId": video_id}

            # Curl Config
            timeout = get_config("video.timeout")
            browser = get_config("proxy.browser")

            async def _do_request():
                response = await session.post(
                    VIDEO_UPSCALE_API,
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
                        f"VideoUpscaleReverse: Upscale failed, {response.status_code}",
                        extra={"error_type": "UpstreamException"},
                    )
                    raise UpstreamException(
                        message=f"VideoUpscaleReverse: Upscale failed, {response.status_code}",
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
                        await TokenService.record_fail(token, status, "video_upscale_auth_failed")
                    except Exception:
                        pass
                raise

            # Handle other non-upstream exceptions
            logger.error(
                f"VideoUpscaleReverse: Upscale failed, {str(e)}",
                extra={"error_type": type(e).__name__},
            )
            raise UpstreamException(
                message=f"VideoUpscaleReverse: Upscale failed, {str(e)}",
                details={"status": 502, "error": str(e)},
            )


__all__ = ["VideoUpscaleReverse"]
