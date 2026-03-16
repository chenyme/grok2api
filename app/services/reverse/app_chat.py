"""
Reverse interface: app chat conversations.
"""

import orjson
from typing import Any, Dict, List, Optional
from urllib.parse import urlparse
from curl_cffi.requests import AsyncSession

from app.core.logger import logger
from app.core.config import get_config
from app.core.exceptions import UpstreamException
from app.services.token.service import TokenService
from app.services.reverse.utils.headers import build_headers
from app.services.reverse.utils.retry import retry_on_status

CHAT_API = "https://grok.com/rest/app-chat/conversations/new"
_LAST_PROXY_LOG_STATE: tuple[str, str] | None = None


def _normalize_chat_proxy(proxy_url: str) -> str:
    """Normalize proxy URL for curl-cffi app-chat requests."""
    if not proxy_url:
        return proxy_url
    parsed = urlparse(proxy_url)
    scheme = parsed.scheme.lower()
    if scheme == "socks5":
        return proxy_url.replace("socks5://", "socks5h://", 1)
    if scheme == "socks4":
        return proxy_url.replace("socks4://", "socks4a://", 1)
    return proxy_url


def _log_proxy_state_once(base_proxy: str, normalized_proxy: str = "", scheme: str = ""):
    """仅在代理状态变化时记录一次代理配置日志。"""
    global _LAST_PROXY_LOG_STATE

    state = ("enabled", normalized_proxy) if base_proxy else ("direct", "")
    if state == _LAST_PROXY_LOG_STATE:
        return

    _LAST_PROXY_LOG_STATE = state
    if base_proxy:
        logger.info(
            f"AppChatReverse proxy enabled: scheme={scheme}, target={normalized_proxy}"
        )
    else:
        logger.info("AppChatReverse proxy is empty, requests will use direct network")


class AppChatReverse:
    """/rest/app-chat/conversations/new reverse interface."""

    @staticmethod
    def _resolve_custom_personality() -> Optional[str]:
        """Resolve optional custom personality from app config."""
        value = get_config("app.custom_instruction", "")
        if value is None:
            return None
        if not isinstance(value, str):
            value = str(value)
        if not value.strip():
            return None
        return value

    @staticmethod
    def build_payload(
        message: str,
        model: str,
        mode: str = None,
        file_attachments: List[str] = None,
        tool_overrides: Dict[str, Any] = None,
        model_config_override: Dict[str, Any] = None,
    ) -> Dict[str, Any]:
        """Build chat payload for Grok app-chat API."""

        attachments = file_attachments or []

        payload = {
            "deviceEnvInfo": {
                "darkModeEnabled": False,
                "devicePixelRatio": 2,
                "screenHeight": 1329,
                "screenWidth": 2056,
                "viewportHeight": 1083,
                "viewportWidth": 2056,
            },
            "disableMemory": get_config("app.disable_memory"),
            "disableSearch": False,
            "disableSelfHarmShortCircuit": False,
            "disableTextFollowUps": False,
            "enableImageGeneration": True,
            "enableImageStreaming": True,
            "enableSideBySide": True,
            "fileAttachments": attachments,
            "forceConcise": False,
            "forceSideBySide": False,
            "imageAttachments": [],
            "imageGenerationCount": 2,
            "isAsyncChat": False,
            "isReasoning": False,
            "message": message,
            "modelMode": mode,
            "modelName": model,
            "responseMetadata": {
                "requestModelDetails": {"modelId": model},
            },
            "returnImageBytes": False,
            "returnRawGrokInXaiRequest": False,
            "sendFinalMetadata": True,
            "temporary": get_config("app.temporary"),
            "toolOverrides": tool_overrides or {},
        }

        if model == "grok-420":
            payload["enable420"] = True

        custom_personality = AppChatReverse._resolve_custom_personality()
        if custom_personality is not None:
            payload["customPersonality"] = custom_personality

        if model_config_override:
            payload["responseMetadata"]["modelConfigOverride"] = model_config_override

        import json
        logger.debug(f"AppChatReverse payload: {json.dumps(payload, indent=4, ensure_ascii=False)}")

        return payload

    @staticmethod
    async def request(
        session: AsyncSession,
        token: str,
        message: str,
        model: str,
        mode: str = None,
        file_attachments: List[str] = None,
        tool_overrides: Dict[str, Any] = None,
        model_config_override: Dict[str, Any] = None,
    ) -> Any:
        """Send app chat request to Grok.
        
        Args:
            session: AsyncSession, the session to use for the request.
            token: str, the SSO token.
            message: str, the message to send.
            model: str, the model to use.
            mode: str, the mode to use.
            file_attachments: List[str], the file attachments to send.
            tool_overrides: Dict[str, Any], the tool overrides to use.
            model_config_override: Dict[str, Any], the model config override to use.

        Returns:
            Any: The response from the request.
        """
        try:
            # Get proxies
            base_proxy = get_config("proxy.base_proxy_url")
            proxy = None
            proxies = None
            if base_proxy:
                normalized_proxy = _normalize_chat_proxy(base_proxy)
                scheme = urlparse(normalized_proxy).scheme.lower()
                if scheme.startswith("socks"):
                    # curl_cffi 对 SOCKS 代理优先使用 proxy 参数，避免被按 HTTP CONNECT 处理
                    proxy = normalized_proxy
                else:
                    proxies = {"http": normalized_proxy, "https": normalized_proxy}
                _log_proxy_state_once(base_proxy, normalized_proxy, scheme)
            else:
                _log_proxy_state_once("")

            # Build headers
            headers = build_headers(
                cookie_token=token,
                content_type="application/json",
                origin="https://grok.com",
                referer="https://grok.com/",
            )

            # Build payload
            payload = AppChatReverse.build_payload(
                message=message,
                model=model,
                mode=mode,
                file_attachments=file_attachments,
                tool_overrides=tool_overrides,
                model_config_override=model_config_override,
            )
            payload_summary = {
                "model": payload.get("modelName"),
                "mode": payload.get("modelMode"),
                "message_len": payload.get("message") or "",
                "file_attachments": len(payload.get("fileAttachments") or []),
                "custom_personality_len": len(payload.get("customPersonality") or ""),
            }
            logger.debug(
                "AppChatReverse final Grok params (redacted)",
                extra={"grok_payload": payload_summary},
            )

            # Curl Config
            timeout = float(get_config("chat.timeout") or 0)
            if timeout <= 0:
                timeout = max(
                    float(get_config("video.timeout") or 0),
                    float(get_config("image.timeout") or 0),
                )
            browser = get_config("proxy.browser")

            async def _do_request():
                response = await session.post(
                    CHAT_API,
                    headers=headers,
                    data=orjson.dumps(payload),
                    timeout=timeout,
                    stream=True,
                    proxy=proxy,
                    proxies=proxies,
                    impersonate=browser,
                )

                if response.status_code != 200:

                    # Get response content
                    content = ""
                    try:
                        content = await response.text()
                    except Exception:
                        pass

                    logger.debug(
                        "AppChatReverse: Chat failed response body: %s",
                        content,
                    )
                    logger.error(
                        f"AppChatReverse: Chat failed, {response.status_code}",
                        extra={"error_type": "UpstreamException"},
                    )
                    raise UpstreamException(
                        message=f"AppChatReverse: Chat failed, {response.status_code}",
                        details={"status": response.status_code, "body": content},
                    )

                return response

            def extract_status(e: Exception) -> Optional[int]:
                if isinstance(e, UpstreamException):
                    if e.details and "status" in e.details:
                        status = e.details["status"]
                    else:
                        status = getattr(e, "status_code", None)
                    if status == 429:
                        return None
                    return status
                return None

            response = await retry_on_status(_do_request, extract_status=extract_status)

            # Stream response
            async def stream_response():
                try:
                    async for line in response.aiter_lines():
                        yield line
                finally:
                    await session.close()

            return stream_response()

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
                        await TokenService.record_fail(
                            token, status, "app_chat_auth_failed"
                        )
                    except Exception:
                        pass
                raise

            # Handle other non-upstream exceptions
            logger.error(
                f"AppChatReverse: Chat failed, {str(e)}",
                extra={"error_type": type(e).__name__},
            )
            raise UpstreamException(
                message=f"AppChatReverse: Chat failed, {str(e)}",
                details={"status": 502, "error": str(e)},
            )


__all__ = ["AppChatReverse"]
