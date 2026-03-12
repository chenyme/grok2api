"""
Reverse interface: rate limits.
"""

import orjson
from typing import Any
from curl_cffi.requests import AsyncSession

from app.core.logger import logger
from app.core.config import get_config
from app.core.exceptions import UpstreamException
from app.services.reverse.utils.headers import build_headers
from app.services.reverse.utils.retry import retry_on_status

RATE_LIMITS_API = "https://grok.com/rest/rate-limits"


class RateLimitsReverse:
    """/rest/rate-limits reverse interface."""

    @staticmethod
    async def request(session: AsyncSession, token: str) -> Any:
        """Fetch rate limits from Grok.

        Args:
            session: AsyncSession, the session to use for the request.
            token: str, the SSO token.

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
                referer="https://grok.com/",
            )

            # Build payload
            payload = {
                "requestKind": "DEFAULT",
                "modelName": "grok-4-1-thinking-1129",
            }

            # Curl Config
            timeout = get_config("usage.timeout")
            browser = get_config("proxy.browser")

            async def _do_request():
                response = await session.post(
                    RATE_LIMITS_API,
                    headers=headers,
                    data=orjson.dumps(payload),
                    timeout=timeout,
                    proxies=proxies,
                    impersonate=browser,
                )

                if response.status_code != 200:
                    try:
                        resp_text = response.text
                    except Exception:
                        resp_text = "N/A"
                    
                    # --- 识别逻辑开始 ---
                    # 区分是真正的 Token 过期还是 Cloudflare 拦截
                    is_token_expired = False
                    server_header = response.headers.get("Server", "").lower()
                    content_type = response.headers.get("Content-Type", "").lower()
                    
                    # 1. 只有当返回不是 JSON 且包含 cloudflare 关键字，或者包含特定的 challenge 标志时，才认为是网络拦截
                    is_cloudflare = "challenge-platform" in resp_text
                    if "cloudflare" in server_header and "application/json" not in content_type:
                        is_cloudflare = True
                    
                    # 2. 如果是 401 且返回 JSON 内容包含认证失败关键字，则确认为 Token 过期
                    if response.status_code == 401 and "application/json" in content_type:
                        # 增加 unauthenticated 和 bad-credentials 等更精确的关键字
                        body_lower = resp_text.lower()
                        auth_error_keywords = ["unauthorized", "not logged in", "unauthenticated", "bad-credentials"]
                        if any(k in body_lower for k in auth_error_keywords):
                            is_token_expired = True
                    # --- 识别逻辑结束 ---

                    logger.error(
                        "RateLimitsReverse: Request failed, status={}, is_token_expired={}, is_cloudflare={}, Body: {}",
                        response.status_code,
                        is_token_expired,
                        is_cloudflare,
                        resp_text[:300],
                        extra={"error_type": "UpstreamException"},
                    )
                    
                    raise UpstreamException(
                        message=f"RateLimitsReverse: Request failed, {response.status_code}",
                        details={
                            "status": response.status_code, 
                            "body": resp_text,
                            "is_token_expired": is_token_expired,
                            "is_cloudflare": is_cloudflare
                        },
                    )

                return response

            return await retry_on_status(_do_request)

        except Exception as e:
            # Handle upstream exception
            if isinstance(e, UpstreamException):
                status = None
                if e.details and isinstance(e.details, dict):
                    status = e.details.get("status")
                
                if status is None:
                    status = getattr(e, "status_code", None)
                
                logger.debug(f"RateLimitsReverse: Upstream error caught: {str(e)}, status={status}")
                raise

            # Handle other non-upstream exceptions
            import traceback
            error_details = traceback.format_exc()
            logger.error(
                f"RateLimitsReverse: Unexpected error, {type(e).__name__}: {str(e)}\n{error_details}"
            )
            raise UpstreamException(
                message=f"RateLimitsReverse: Request failed, {str(e)}",
                details={"status": 502, "error": str(e), "traceback": error_details},
            )


__all__ = ["RateLimitsReverse"]
