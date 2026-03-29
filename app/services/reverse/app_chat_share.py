"""
Reverse interface: app chat share link creation.
"""

import orjson
from typing import Any

from curl_cffi.requests import AsyncSession

from app.core.config import get_config
from app.core.exceptions import UpstreamException
from app.core.logger import logger
from app.core.proxy_pool import (
    build_http_proxies,
    get_current_proxy_from,
    rotate_proxy,
    should_rotate_proxy,
)
from app.services.reverse.utils.headers import build_headers
from app.services.reverse.utils.retry import retry_on_status
from app.services.token.service import TokenService


class AppChatShareReverse:
    """/rest/app-chat/conversations/{conversationId}/share reverse interface."""

    @staticmethod
    async def request(
        session: AsyncSession,
        token: str,
        conversation_id: str,
        response_id: str,
        allow_indexing: bool = True,
    ) -> Any:
        api = f"https://grok.com/rest/app-chat/conversations/{conversation_id}/share"

        try:
            referer = f"https://grok.com/c/{conversation_id}"
            if response_id:
                referer = f"{referer}?rid={response_id}"

            headers = build_headers(
                cookie_token=token,
                content_type="application/json",
                origin="https://grok.com",
                referer=referer,
            )

            payload = {
                "responseId": response_id,
                "allowIndexing": bool(allow_indexing),
            }

            timeout = get_config("image.timeout") or get_config("chat.timeout")
            browser = get_config("proxy.browser")
            active_proxy_key = None

            async def _do_request():
                nonlocal active_proxy_key
                active_proxy_key, proxy_url = get_current_proxy_from(
                    "proxy.base_proxy_url"
                )
                proxies = build_http_proxies(proxy_url)
                response = await session.post(
                    api,
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
                        "AppChatShareReverse: Share create failed, %s",
                        response.status_code,
                        extra={"error_type": "UpstreamException"},
                    )
                    raise UpstreamException(
                        message=(
                            "AppChatShareReverse: Share create failed, "
                            f"{response.status_code}"
                        ),
                        details={"status": response.status_code, "body": content},
                    )

                return response

            async def _on_retry(
                attempt: int,
                status_code: int,
                error: Exception,
                delay: float,
            ):
                if active_proxy_key and should_rotate_proxy(status_code):
                    rotate_proxy(active_proxy_key)

            return await retry_on_status(_do_request, on_retry=_on_retry)

        except Exception as e:
            if isinstance(e, UpstreamException):
                status = None
                if e.details and "status" in e.details:
                    status = e.details["status"]
                else:
                    status = getattr(e, "status_code", None)
                if status == 401:
                    try:
                        await TokenService.record_fail(
                            token, status, "app_chat_share_auth_failed"
                        )
                    except Exception:
                        pass
                raise

            logger.error(
                f"AppChatShareReverse: Share create failed, {str(e)}",
                extra={"error_type": type(e).__name__},
            )
            raise UpstreamException(
                message=f"AppChatShareReverse: Share create failed, {str(e)}",
                details={"status": 502, "error": str(e)},
            )


__all__ = ["AppChatShareReverse"]
