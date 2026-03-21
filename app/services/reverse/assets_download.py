"""
Reverse interface: download asset.
"""

import urllib.parse
from typing import Any
from pathlib import Path
from urllib.parse import urlparse
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
from app.services.reverse.utils.retry import retry_on_status

DOWNLOAD_API = "https://assets.grok.com"

_CONTENT_TYPES = {
    ".jpg": "image/jpeg",
    ".jpeg": "image/jpeg",
    ".png": "image/png",
    ".webp": "image/webp",
    ".mp4": "video/mp4",
    ".webm": "video/webm",
}


class AssetsDownloadReverse:
    """assets.grok.com/{path} reverse interface."""

    @staticmethod
    async def request(session: AsyncSession, token: str, file_path: str) -> Any:
        """Download asset from Grok.

        Args:
            session: AsyncSession, the session to use for the request.
            token: str, the SSO token.
            file_path: str, the path of the file to download.

        Returns:
            Any: The response from the request.
        """
        try:
            parsed = urlparse(file_path)
            origin = "https://assets.grok.com"
            referer = "https://grok.com/"
            if parsed.scheme and parsed.netloc:
                url = file_path
                request_path = parsed.path or "/"
                if parsed.query:
                    request_path = f"{request_path}?{parsed.query}"
                origin = f"{parsed.scheme}://{parsed.netloc}"
                referer = f"{origin}/"
            else:
                if not file_path.startswith("/"):
                    file_path = f"/{file_path}"
                request_path = file_path
                url = f"{DOWNLOAD_API}{file_path}"

            # Guess content type by extension for Accept/Sec-Fetch-Dest
            content_type = _CONTENT_TYPES.get(Path(urllib.parse.urlparse(request_path).path).suffix.lower())

            # Build headers
            headers = build_headers(
                cookie_token=token,
                content_type=content_type,
                origin=origin,
                referer=referer,
            )
            ## Align with browser download navigation headers
            headers["Cache-Control"] = "no-cache"
            headers["Pragma"] = "no-cache"
            headers["Priority"] = "u=0, i"
            headers["Sec-Fetch-Mode"] = "navigate"
            headers["Sec-Fetch-User"] = "?1"
            headers["Upgrade-Insecure-Requests"] = "1"

            # Curl Config
            timeout = get_config("asset.download_timeout")
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
                    url,
                    headers=headers,
                    proxies=proxies,
                    timeout=timeout,
                    allow_redirects=True,
                    impersonate=browser,
                    stream=True,
                )

                if response.status_code != 200:
                    logger.error(
                        f"AssetsDownloadReverse: Download failed, {response.status_code}",
                        extra={"error_type": "UpstreamException"},
                    )
                    raise UpstreamException(
                        message=f"AssetsDownloadReverse: Download failed, {response.status_code}",
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
                        await TokenService.record_fail(token, status, "assets_download_auth_failed")
                    except Exception:
                        pass
                raise

            # Handle other non-upstream exceptions
            logger.error(
                f"AssetsDownloadReverse: Download failed, {str(e)}",
                extra={"error_type": type(e).__name__},
            )
            raise UpstreamException(
                message=f"AssetsDownloadReverse: Download failed, {str(e)}",
                details={"status": 502, "error": str(e)},
            )


__all__ = ["AssetsDownloadReverse"]
