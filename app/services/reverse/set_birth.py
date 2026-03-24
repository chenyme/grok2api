"""
Reverse interface: set birth date.
"""

import datetime
import random
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
from app.services.reverse.utils.headers import build_headers
from app.services.reverse.utils.http_fallback import request_with_aiohttp_fallback
from app.services.reverse.utils.retry import retry_on_status

SET_BIRTH_API = "https://grok.com/rest/auth/set-birth-date"


class SetBirthReverse:
    """/rest/auth/set-birth-date reverse interface."""

    @staticmethod
    async def request(session: AsyncSession, token: str) -> Any:
        """Set birth date in Grok.

        Args:
            session: AsyncSession, the session to use for the request.
            token: str, the SSO token.

        Returns:
            Any: The response from the request.
        """
        try:
            # Build headers
            headers = build_headers(
                cookie_token=token,
                content_type="application/json",
                origin="https://grok.com",
                referer="https://grok.com/?_s=home",
            )

            # Build payload
            today = datetime.date.today()
            birth_year = today.year - random.randint(20, 48)
            birth_month = random.randint(1, 12)
            birth_day = random.randint(1, 28)
            hour = random.randint(0, 23)
            minute = random.randint(0, 59)
            second = random.randint(0, 59)
            microsecond = random.randint(0, 999)
            payload = {
                "birthDate": f"{birth_year:04d}-{birth_month:02d}-{birth_day:02d}"
                f"T{hour:02d}:{minute:02d}:{second:02d}.{microsecond:03d}Z"
            }

            # Curl Config
            timeout = get_config("nsfw.timeout")
            browser = get_config("proxy.browser")
            active_proxy_key = None

            async def _do_request():
                nonlocal active_proxy_key
                active_proxy_key, proxy_url = get_current_proxy_from("proxy.base_proxy_url")
                proxies = build_http_proxies(proxy_url)
                response = await session.post(
                    SET_BIRTH_API,
                    headers=headers,
                    json=payload,
                    timeout=timeout,
                    proxies=proxies,
                    impersonate=browser,
                )

                if response.status_code not in (200, 204):
                    logger.error(
                        f"SetBirthReverse: Request failed, {response.status_code}",
                        extra={"error_type": "UpstreamException"},
                    )
                    raise UpstreamException(
                        message=f"SetBirthReverse: Request failed, {response.status_code}",
                        details={"status": response.status_code},
                    )

                logger.debug(f"SetBirthReverse: Request successful, {response.status_code}")

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
                raise

            # Handle other non-upstream exceptions
            if not get_config("proxy.base_proxy_url"):
                try:
                    logger.warning(
                        "SetBirthReverse: curl-cffi failed; retrying with aiohttp fallback"
                    )
                    return await request_with_aiohttp_fallback(
                        method="POST",
                        url=SET_BIRTH_API,
                        headers=headers,
                        timeout=float(get_config("nsfw.timeout") or 60),
                        logger_prefix="SetBirthReverse: Request failed",
                        expected_statuses=(200, 204),
                        response_kind="json",
                        json_payload=payload,
                    )
                except UpstreamException:
                    raise
                except Exception as fallback_exc:
                    logger.warning(
                        f"SetBirthReverse: aiohttp fallback failed, {fallback_exc}"
                    )

            logger.error(
                f"SetBirthReverse: Request failed, {str(e)}",
                extra={"error_type": type(e).__name__},
            )
            raise UpstreamException(
                message=f"SetBirthReverse: Request failed, {str(e)}",
                details={"status": 502, "error": str(e)},
            )


__all__ = ["SetBirthReverse"]
