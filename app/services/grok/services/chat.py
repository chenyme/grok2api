"""
Grok Chat 服务
"""

import orjson
from typing import Dict, List, Any, AsyncGenerator
from dataclasses import dataclass

from curl_cffi.requests import AsyncSession

from app.core.logger import logger
from app.core.config import get_config
from app.core.exceptions import (
    AppException,
    UpstreamException,
    ValidationException,
    ErrorType,
)
from app.services.grok.models.model import ModelService
from app.services.grok.services.assets import UploadService
from app.services.grok.processors.processor import StreamProcessor, CollectProcessor
from app.services.grok.utils.retry import retry_on_status
from app.services.grok.utils.headers import apply_statsig, build_sso_cookie
from app.services.token import get_token_manager, EffortType


CHAT_API = "https://grok.com/rest/app-chat/conversations/new"
TIMEOUT = 120
DEFAULT_BROWSER = "chrome136"
DEFAULT_USER_AGENT = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"


@dataclass
class ChatRequest:
    """聊天请求数据"""

    model: str
    messages: List[Dict[str, Any]]
    stream: bool = None
    think: bool = None


class MessageExtractor:
    """消息内容提取器"""

    # 需要上传的类型
    UPLOAD_TYPES = {"image_url", "input_audio", "file"}
    # 视频模式不支持的类型
    VIDEO_UNSUPPORTED = {"input_audio", "file"}

    @staticmethod
    def extract(
        messages: List[Dict[str, Any]], is_video: bool = False
    ) -> tuple[str, List[tuple[str, str]]]:
        """
        从 OpenAI 消息格式提取内容

        Args:
            messages: OpenAI 格式消息列表
            is_video: 是否为视频模型

        Returns:
            (text, attachments): 拼接后的文本和需要上传的附件列表

        Raises:
            ValueError: 视频模型遇到不支持的内容类型
        """
        texts = []
        attachments = []  # 需要上传的附件 (URL 或 base64)

        # 先抽取每条消息的文本，保留角色信息用于合并
        extracted: List[Dict[str, str]] = []

        for msg in messages:
            role = msg.get("role", "")
            content = msg.get("content", "")
            parts = []

            # 简单字符串内容
            if isinstance(content, str):
                if content.strip():
                    parts.append(content)

            # 列表格式内容
            elif isinstance(content, list):
                for item in content:
                    item_type = item.get("type", "")

                    # 文本类型
                    if item_type == "text":
                        text = item.get("text", "")
                        if text.strip():
                            parts.append(text)

                    # 图片类型
                    elif item_type == "image_url":
                        image_data = item.get("image_url", {})
                        url = (
                            image_data.get("url", "")
                            if isinstance(image_data, dict)
                            else str(image_data)
                        )
                        if url:
                            attachments.append(("image", url))

                    # 音频类型
                    elif item_type == "input_audio":
                        if is_video:
                            raise ValueError("视频模型不支持 input_audio 类型")
                        audio_data = item.get("input_audio", {})
                        data = (
                            audio_data.get("data", "")
                            if isinstance(audio_data, dict)
                            else str(audio_data)
                        )
                        if data:
                            attachments.append(("audio", data))

                    # 文件类型
                    elif item_type == "file":
                        if is_video:
                            raise ValueError("视频模型不支持 file 类型")
                        file_data = item.get("file", {})
                        # file 可能是 URL 或 base64
                        url = file_data.get("url", "") or file_data.get("data", "")
                        if isinstance(file_data, str):
                            url = file_data
                        if url:
                            attachments.append(("file", url))

            if parts:
                extracted.append({"role": role, "text": "\n".join(parts)})

        # 合并文本
        last_user_index = None
        for i in range(len(extracted) - 1, -1, -1):
            if extracted[i]["role"] == "user":
                last_user_index = i
                break

        for i, item in enumerate(extracted):
            role = item["role"] or "user"
            text = item["text"]
            if i == last_user_index:
                texts.append(text)
            else:
                texts.append(f"{role}: {text}")

        # 换行拼接文本
        message = "\n\n".join(texts)
        return message, attachments

    @staticmethod
    def extract_text_only(messages: List[Dict[str, Any]]) -> str:
        """仅提取文本内容"""
        text, _ = MessageExtractor.extract(messages, is_video=True)
        return text


class ChatRequestBuilder:
    """请求构造器"""

    @staticmethod
    def build_headers(token: str) -> Dict[str, str]:
        """构造请求头"""
        user_agent = get_config("grok.user_agent", DEFAULT_USER_AGENT)
        headers = {
            "Accept": "*/*",
            "Accept-Encoding": "gzip, deflate, br, zstd",
            "Accept-Language": "zh-CN,zh;q=0.9",
            "Baggage": "sentry-environment=production,sentry-release=d6add6fb0460641fd482d767a335ef72b9b6abb8,sentry-public_key=b311e0f2690c81f25e2c4cf6d4f7ce1c",
            "Cache-Control": "no-cache",
            "Content-Type": "application/json",
            "Origin": "https://grok.com",
            "Pragma": "no-cache",
            "Priority": "u=1, i",
            "Referer": "https://grok.com/",
            "Sec-Ch-Ua": '"Google Chrome";v="136", "Chromium";v="136", "Not(A:Brand";v="24"',
            "Sec-Ch-Ua-Arch": "arm",
            "Sec-Ch-Ua-Bitness": "64",
            "Sec-Ch-Ua-Mobile": "?0",
            "Sec-Ch-Ua-Model": "",
            "Sec-Ch-Ua-Platform": '"macOS"',
            "Sec-Fetch-Dest": "empty",
            "Sec-Fetch-Mode": "cors",
            "Sec-Fetch-Site": "same-origin",
            "User-Agent": user_agent,
        }

        apply_statsig(headers)
        headers["Cookie"] = build_sso_cookie(token)

        return headers

    @staticmethod
    def build_payload(
        message: str,
        model: str,
        mode: str = None,
        file_attachments: List[str] = None,
        image_attachments: List[str] = None,
    ) -> Dict[str, Any]:
        """
        构造请求体

        Args:
            message: 消息文本
            model: 模型名称
            mode: 模型模式
            file_attachments: 文件附件 ID 列表
            image_attachments: 图片附件 ID 列表（合并到 fileAttachments）
        """
        temporary = get_config("grok.temporary", True)

        merged_attachments: List[str] = []
        if file_attachments:
            merged_attachments.extend(file_attachments)
        if image_attachments:
            merged_attachments.extend(image_attachments)

        payload: Dict[str, Any] = {
            "temporary": temporary,
            "modelName": model,
            "message": message,
            "fileAttachments": merged_attachments,
            "imageAttachments": [],
            "disableSearch": False,
            "enableImageGeneration": True,
            "returnImageBytes": False,
            "returnRawGrokInXaiRequest": False,
            "enableImageStreaming": True,
            "imageGenerationCount": 2,
            "forceConcise": False,
            "toolOverrides": {},
            "enableSideBySide": True,
            "sendFinalMetadata": True,
            "isReasoning": False,
            "disableTextFollowUps": False,
            "responseMetadata": {
                "modelConfigOverride": {"modelMap": {}},
                "requestModelDetails": {"modelId": model},
            },
            "disableMemory": get_config("grok.disable_memory", True),
            "forceSideBySide": False,
            "isAsyncChat": False,
            "disableSelfHarmShortCircuit": False,
            "deviceEnvInfo": {
                "darkModeEnabled": False,
                "devicePixelRatio": 2,
                "screenWidth": 2056,
                "screenHeight": 1329,
                "viewportWidth": 2056,
                "viewportHeight": 1083,
            },
        }

        if mode:
            payload["modelMode"] = mode

        return payload


# ==================== Grok 服务 ====================


class GrokChatService:
    """Grok API 调用服务"""

    def __init__(self, proxy: str = None):
        self.proxy = proxy or get_config("grok.base_proxy_url", "")

    async def chat(
        self,
        token: str,
        message: str,
        model: str = "grok-3",
        mode: str = None,
        stream: bool = None,
        file_attachments: List[str] = None,
        image_attachments: List[str] = None,
        raw_payload: Dict[str, Any] = None,
    ):
        """
        发送聊天请求

        Args:
            token: 认证 Token
            message: 消息文本
            model: Grok 模型名称
            mode: 模型模式
            stream: 是否流式
            file_attachments: 文件附件 ID 列表
            image_attachments: 图片附件 URL 列表

        Raises:
            UpstreamException: 当 Grok API 返回错误且重试耗尽时
        """
        if stream is None:
            stream = get_config("grok.stream", True)

        headers = ChatRequestBuilder.build_headers(token)
        if raw_payload is not None:
            payload = raw_payload
        else:
            payload = ChatRequestBuilder.build_payload(
                message,
                model,
                mode,
                file_attachments,
                image_attachments,
            )
        proxies = {"http": self.proxy, "https": self.proxy} if self.proxy else None
        timeout = get_config("grok.timeout", TIMEOUT)

        # 状态码提取器
        def extract_status(e: Exception) -> int | None:
            if isinstance(e, UpstreamException) and e.details:
                return e.details.get("status")
            return None

        logger.debug(f"Payload: {payload}")
        
        # 建立连接函数
        async def establish_connection():
            """建立连接并返回 response 对象"""
            browser = get_config("grok.browser", DEFAULT_BROWSER)
            session = AsyncSession(impersonate=browser)
            try:
                response = await session.post(
                    CHAT_API,
                    headers=headers,
                    data=orjson.dumps(payload),
                    timeout=timeout,
                    stream=True,
                    proxies=proxies,
                )

                if response.status_code != 200:
                    try:
                        content = await response.text()
                    except Exception:
                        content = ""

                    logger.error(
                        f"Chat failed: {response.status_code}",
                        extra={
                            "status": response.status_code,
                            "token": token[:10] + "...",
                        },
                    )
                    body_for_log = content if content else "<empty>"
                    logger.debug(
                        "Grok API error response "
                        f"(status={response.status_code}): {body_for_log}"
                    )
                    # 关闭 session 并抛出异常
                    try:
                        await session.close()
                    except Exception:
                        pass
                    raise UpstreamException(
                        message=f"Grok API request failed: {response.status_code}",
                        details={
                            "status": response.status_code,
                            "body": content,
                        },
                    )

                # 返回 session 和 response
                return session, response

            except UpstreamException:
                # 已经处理过的异常，直接抛出
                raise
            except Exception as e:
                # 其他异常，关闭 session 并包装
                logger.error(f"Chat request error: {e}")
                try:
                    await session.close()
                except Exception:
                    pass
                raise UpstreamException(
                    message=f"Chat connection failed: {str(e)}",
                    details={"error": str(e)},
                )

        # 建立连接
        session = None
        response = None
        try:
            session, response = await retry_on_status(
                establish_connection, extract_status=extract_status
            )
        except Exception as e:
            # 记录失败
            status_code = extract_status(e)
            if status_code:
                token_mgr = await get_token_manager()
                reason = str(e)
                if isinstance(e, UpstreamException) and e.details:
                    body = e.details.get("body")
                    if body:
                        reason = f"{reason} | body: {body}"
                await token_mgr.record_fail(token, status_code, reason)
            raise

        # 流式传输
        async def stream_response():
            try:
                async for line in response.aiter_lines():
                    yield line
            finally:
                if session:
                    await session.close()

        return stream_response()

    async def chat_openai(self, token: str, request: ChatRequest):
        """OpenAI 兼容接口"""
        model_info = ModelService.get(request.model)
        if not model_info:
            raise ValidationException(f"Unknown model: {request.model}")

        grok_model = model_info.grok_model
        mode = model_info.model_mode
        is_video = model_info.is_video

        # 提取消息和附件
        try:
            message, attachments = MessageExtractor.extract(
                request.messages, is_video=is_video
            )
        except ValueError as e:
            raise ValidationException(str(e))

        # 处理附件上传
        file_ids = []

        if attachments:
            upload_service = UploadService()
            try:
                for attach_type, attach_data in attachments:
                    # 获取 ID
                    file_id, _ = await upload_service.upload(attach_data, token)

                    if attach_type == "image":
                        file_ids.append(file_id)
                        logger.debug(f"Image uploaded: {file_id}")
                    else:
                        file_ids.append(file_id)
                        logger.debug(f"File uploaded: {file_id}")
            finally:
                await upload_service.close()

        stream = (
            request.stream
            if request.stream is not None
            else get_config("grok.stream", True)
        )

        response = await self.chat(
            token,
            message,
            grok_model,
            mode,
            stream,
            file_attachments=file_ids,
            image_attachments=[],
        )

        return response, stream, request.model


# ==================== Chat 业务服务 ====================


class ChatService:
    """Chat 业务服务"""

    @staticmethod
    async def _wrap_stream(stream: AsyncGenerator, token_mgr, token: str, model: str):
        """
        包装流式响应，在完成时记录使用

        Args:
            stream: 原始 AsyncGenerator
            token_mgr: TokenManager 实例
            token: Token 字符串
            model: 模型名称
        """
        success = False
        try:
            async for chunk in stream:
                yield chunk
            success = True
        finally:
            # 只在成功完成时记录使用，失败/异常时不扣费
            if success:
                try:
                    model_info = ModelService.get(model)
                    effort = (
                        EffortType.HIGH
                        if (model_info and model_info.cost.value == "high")
                        else EffortType.LOW
                    )
                    await token_mgr.consume(token, effort)
                    logger.debug(
                        f"Stream completed, recorded usage for token {token[:10]}... (effort={effort.value})"
                    )
                except Exception as e:
                    logger.warning(f"Failed to record stream usage: {e}")

    @staticmethod
    async def completions(
        model: str,
        messages: List[Dict[str, Any]],
        stream: bool = None,
        thinking: str = None,
    ):
        """
        Chat Completions 入口

        Args:
            model: 模型名称
            messages: 消息列表
            stream: 是否流式
            thinking: 思考模式

        Returns:
            AsyncGenerator 或 dict
        """
        # 获取 token
        try:
            token_mgr = await get_token_manager()
            await token_mgr.reload_if_stale()
            token = None
            for pool_name in ModelService.pool_candidates_for_model(model):
                token = token_mgr.get_token(pool_name)
                if token:
                    break
        except Exception as e:
            logger.error(f"Failed to get token: {e}")
            raise AppException(
                message="Internal service error obtaining token",
                error_type=ErrorType.SERVER.value,
                code="internal_error",
            )

        if not token:
            raise AppException(
                message="No available tokens. Please try again later.",
                error_type=ErrorType.RATE_LIMIT.value,
                code="rate_limit_exceeded",
                status_code=429,
            )

        # 解析参数
        think = None
        if thinking == "enabled":
            think = True
        elif thinking == "disabled":
            think = False

        is_stream = stream if stream is not None else get_config("grok.stream", True)

        # 构造请求
        chat_request = ChatRequest(
            model=model, messages=messages, stream=is_stream, think=think
        )

        # 请求 Grok
        service = GrokChatService()
        try:
            response, _, model_name = await service.chat_openai(token, chat_request)
        except AppException:
            raise
        except Exception as e:
            logger.error(f"Chat service error: {e}")
            raise UpstreamException(
                message=f"Service processing failed: {str(e)}",
                details={"error": str(e)},
            )

        # 处理响应
        if is_stream:
            processor = StreamProcessor(model_name, token, think)
            return ChatService._wrap_stream(
                processor.process(response), token_mgr, token, model
            )
        else:
            result = await CollectProcessor(model_name, token).process(response)
            # 非流式：处理完成后立即记录使用
            try:
                model_info = ModelService.get(model)
                effort = (
                    EffortType.HIGH
                    if (model_info and model_info.cost.value == "high")
                    else EffortType.LOW
                )
                await token_mgr.consume(token, effort)
                logger.debug(
                    f"Collect completed, recorded usage for token {token[:10]}... (effort={effort.value})"
                )
            except Exception as e:
                logger.warning(f"Failed to record collect usage: {e}")
            return result


__all__ = [
    "GrokChatService",
    "ChatRequest",
    "ChatRequestBuilder",
    "MessageExtractor",
    "ChatService",
]
