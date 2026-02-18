"""
Reverse interface: app chat conversations.
"""

import orjson
from typing import Any, Dict, List, Optional
from curl_cffi.requests import AsyncSession

from app.core.logger import logger
from app.core.config import get_config
from app.core.exceptions import UpstreamException
from app.services.token.service import TokenService
from app.services.reverse.utils.headers import build_headers
from app.services.reverse.utils.retry import retry_on_status

CHAT_API = "https://grok.com/rest/app-chat/conversations/new"


class AppChatReverse:
    """/rest/app-chat/conversations/new reverse interface."""

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
                "screenWidth": 2056,
                "screenHeight": 1329,
                "viewportWidth": 2056,
                "viewportHeight": 1083,
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

        if model_config_override:
            payload["responseMetadata"]["modelConfigOverride"] = model_config_override

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
            proxies = {"http": base_proxy, "https": base_proxy} if base_proxy else None

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

            # Curl Config
            timeout = max(
                float(get_config("chat.timeout") or 0),
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
