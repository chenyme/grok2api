"""
Grok 视频生成服务
"""

import asyncio
import time
from typing import AsyncGenerator, Optional

import orjson
from app.services.grok.services.session_pool import get_shared_session

from app.core.logger import logger
from app.core.config import get_config
from app.core.exceptions import (
    UpstreamException,
    AppException,
    ValidationException,
    ErrorType,
)
from app.services.grok.models.model import ModelService
from app.services.token import get_token_manager, EffortType
from app.services.grok.processors import VideoStreamProcessor, VideoCollectProcessor
from app.services.grok.utils.headers import apply_statsig, build_sso_cookie
from app.core.streaming import with_keepalive

CREATE_POST_API = "https://grok.com/rest/media/post/create"
CHAT_API = "https://grok.com/rest/app-chat/conversations/new"

_MEDIA_SEMAPHORE = None
_MEDIA_SEM_VALUE = 0


def _get_semaphore() -> asyncio.Semaphore:
    """获取或更新信号量"""
    global _MEDIA_SEMAPHORE, _MEDIA_SEM_VALUE
    value = max(1, int(get_config("performance.media_max_concurrent")))
    if value != _MEDIA_SEM_VALUE:
        _MEDIA_SEM_VALUE = value
        _MEDIA_SEMAPHORE = asyncio.Semaphore(value)
    return _MEDIA_SEMAPHORE


class VideoService:
    """视频生成服务"""

    def __init__(self, proxy: str = None):
        self.proxy = proxy or get_config("network.base_proxy_url")
        self.timeout = get_config("network.timeout")
        self.last_post_id: str = ""

    def _build_headers(self, token: str, referer: str = "https://grok.com/imagine") -> dict:
        """构建请求头"""
        user_agent = get_config("security.user_agent")
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
            "Referer": referer,
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

    def _build_proxies(self) -> Optional[dict]:
        """构建代理"""
        return {"http": self.proxy, "https": self.proxy} if self.proxy else None

    async def create_post(
        self,
        token: str,
        prompt: str,
        media_type: str = "MEDIA_POST_TYPE_VIDEO",
        media_url: str = None,
    ) -> str:
        """创建媒体帖子，返回 post ID"""
        try:
            headers = self._build_headers(token)

            # 根据类型构建不同的载荷
            if media_type == "MEDIA_POST_TYPE_IMAGE" and media_url:
                payload = {"mediaType": media_type, "mediaUrl": media_url}
            else:
                payload = {"mediaType": media_type, "prompt": prompt}

            session = get_shared_session()
            response = await session.post(
                CREATE_POST_API,
                headers=headers,
                json=payload,
                impersonate=get_config("security.browser"),
                timeout=30,
                proxies=self._build_proxies(),
            )

            if response.status_code != 200:
                logger.error(f"Create post failed: {response.status_code}")
                raise UpstreamException(f"Failed to create post: {response.status_code}")

            post_id = response.json().get("post", {}).get("id", "")
            if not post_id:
                raise UpstreamException("No post ID in response")

            logger.info(f"Media post created: {post_id} (type={media_type})")
            return post_id

        except AppException:
            raise
        except Exception as e:
            logger.error(f"Create post error: {e}")
            raise UpstreamException(f"Create post error: {str(e)}")

    async def create_image_post(self, token: str, image_url: str) -> str:
        """创建图片帖子，返回 post ID"""
        return await self.create_post(
            token, prompt="", media_type="MEDIA_POST_TYPE_IMAGE", media_url=image_url
        )

    def _build_payload(
        self,
        prompt: str,
        post_id: str = "",
        aspect_ratio: str = "3:2",
        video_length: int = 6,
        resolution_name: str = "480p",
        preset: str = "normal",
        image_url: str = "",
    ) -> dict:
        """构建视频生成载荷

        Args:
            prompt: 用户文本提示
            post_id: 图生视频时由 create_image_post 返回的帖子 ID；文生视频传空
            image_url: 图生视频时已上传的图片 URL（嵌入 message 供上游识别）
        """
        mode_map = {
            "fun": "--mode=extremely-crazy",
            "normal": "--mode=normal",
            "spicy": "--mode=extremely-spicy-or-crazy",
        }
        mode_flag = mode_map.get(preset, "--mode=custom")

        # 图生视频：message 包含图片引用 + 用户提示 + 模式标记
        # 文生视频：message 仅包含用户提示 + 模式标记
        if image_url and post_id:
            message = f"{image_url} {prompt} {mode_flag}".strip()
        else:
            message = f"{prompt} {mode_flag}"

        disable_memory = get_config("chat.disable_memory", True)

        payload = {
            "temporary": True,
            "modelName": "grok-3",
            "message": message,
            "fileAttachments": [],
            "imageAttachments": [],
            "disableSearch": False,
            "disableArtifact": True,
            "disableArtifactDiff": True,
            "enableImageGeneration": True,
            "returnImageBytes": False,
            "returnRawGrokInXaiRequest": False,
            "enableImageStreaming": True,
            "imageGenerationCount": 2,
            "forceConcise": False,
            "toolOverrides": {"videoGen": True},
            "enableSideBySide": True,
            "sendFinalMetadata": True,
            "isReasoning": False,
            "isRegenRequest": False,
            "isFromGrokFiles": False,
            "disableTextFollowUps": False,
            "disableMemory": disable_memory,
            "forceSideBySide": False,
            "isAsyncChat": False,
            "disableSelfHarmShortCircuit": False,
            "skipCancelCurrentInflightRequests": True,
            "workspaceIds": [],
            "deviceEnvInfo": {
                "darkModeEnabled": False,
                "devicePixelRatio": 2,
                "screenWidth": 2056,
                "screenHeight": 1329,
                "viewportWidth": 2056,
                "viewportHeight": 1083,
            },
            "metadata": {
                "is_quick_answer": False,
                "is_think_harder": False,
                "requestModelDetails": {"modelId": "grok-3"},
            },
            "responseMetadata": {
                "experiments": [],
                "requestModelDetails": {"modelId": "grok-3"},
                "modelConfigOverride": {
                    "modelMap": {
                        "videoGenModelConfig": {
                            "parentPostId": post_id,
                            "aspectRatio": aspect_ratio,
                            "videoLength": video_length,
                            "resolutionName": resolution_name,
                            "isVideoEdit": False,
                        }
                    }
                },
            },
        }

        logger.debug(f"Video generation payload: {payload}")

        return payload

    async def _generate_internal(
        self,
        token: str,
        post_id: str,
        prompt: str,
        aspect_ratio: str,
        video_length: int,
        resolution_name: str,
        preset: str,
        image_url: str = "",
    ) -> AsyncGenerator[bytes, None]:
        """内部生成逻辑"""
        try:
            # 有 post_id 时 Referer 带上具体 ID，与浏览器行为一致
            referer = (
                f"https://grok.com/imagine/{post_id}" if post_id else "https://grok.com/imagine"
            )
            headers = self._build_headers(token, referer=referer)
            payload = self._build_payload(
                prompt,
                post_id,
                aspect_ratio,
                video_length,
                resolution_name,
                preset,
                image_url=image_url,
            )

            session = get_shared_session()
            response = await session.post(
                CHAT_API,
                headers=headers,
                data=orjson.dumps(payload),
                timeout=self.timeout,
                stream=True,
                proxies=self._build_proxies(),
            )

            if response.status_code != 200:
                logger.error(
                    f"Video generation failed: status={response.status_code}, post_id={post_id}"
                )
                raise UpstreamException(
                    message=f"Video generation failed: {response.status_code}",
                    details={"status": response.status_code},
                )

            logger.info(f"Video generation started: post_id={post_id}")

            async def stream_response():
                async for line in response.aiter_lines():
                    yield line

            return stream_response()

        except Exception as e:
            logger.error(f"Video generation error: {e}")
            if isinstance(e, AppException):
                raise
            raise UpstreamException(f"Video generation error: {str(e)}")

    async def generate(
        self,
        token: str,
        prompt: str,
        aspect_ratio: str = "3:2",
        video_length: int = 6,
        resolution_name: str = "480p",
        preset: str = "normal",
    ) -> AsyncGenerator[bytes, None]:
        """文生视频：不创建 post，parentPostId 留空"""
        logger.info(
            f"Video generation: prompt='{prompt[:50]}...', ratio={aspect_ratio}, length={video_length}s, preset={preset}"
        )
        async with _get_semaphore():
            self.last_post_id = ""
            return await self._generate_internal(
                token,
                "",
                prompt,
                aspect_ratio,
                video_length,
                resolution_name,
                preset,
            )

    async def generate_from_image(
        self,
        token: str,
        prompt: str,
        image_url: str,
        aspect_ratio: str = "3:2",
        video_length: int = 6,
        resolution: str = "480p",
        preset: str = "normal",
    ) -> AsyncGenerator[bytes, None]:
        """图生视频：创建 MEDIA_POST_TYPE_IMAGE 帖子，message 携带图片 URL"""
        logger.info(f"Image to video: prompt='{prompt[:50]}...', image={image_url[:80]}")
        async with _get_semaphore():
            post_id = await self.create_image_post(token, image_url)
            self.last_post_id = post_id
            return await self._generate_internal(
                token,
                post_id,
                prompt,
                aspect_ratio,
                video_length,
                resolution,
                preset,
                image_url=image_url,
            )

    @staticmethod
    async def _record_stats(
        model: str,
        success: bool,
        duration: float,
        token: str,
        error: str,
        client_ip: str,
        key_name: str,
    ) -> None:
        from app.services.grok.services.chat import ChatService

        await ChatService._record_stats(
            model,
            success,
            duration,
            token,
            error,
            client_ip=client_ip,
            key_name=key_name,
        )

    @staticmethod
    async def completions(
        model: str,
        messages: list,
        stream: bool = None,
        thinking: str = None,
        aspect_ratio: str = "3:2",
        video_length: Optional[int] = None,
        resolution: str = "480p",
        preset: str = "normal",
        client_ip: str = "unknown",
        key_name: str = "default",
    ):
        """视频生成入口"""
        start_time = time.time()

        # 获取 token
        token_mgr = await get_token_manager()
        await token_mgr.reload_if_stale()

        token = ""
        selected_pool = ""

        try:
            requested_video_length = video_length if video_length in (6, 10) else None

            pool_candidates = ModelService.pool_candidates_for_model(model)
            if requested_video_length == 10 and "ssoSuper" in pool_candidates:
                pool_candidates = ["ssoSuper"] + [p for p in pool_candidates if p != "ssoSuper"]

            for pool_name in pool_candidates:
                token = token_mgr.get_token(pool_name)
                if token:
                    selected_pool = pool_name
                    break

            if not token:
                raise AppException(
                    message="No available tokens. Please try again later.",
                    error_type=ErrorType.RATE_LIMIT.value,
                    code="rate_limit_exceeded",
                    status_code=429,
                )

            logger.info(
                "Video token selected: "
                f"model={model}, pool={selected_pool or 'unknown'}, "
                f"key_name={key_name}, suffix={token[-6:] if len(token) >= 6 else token}"
            )

            if requested_video_length is None:
                effective_video_length = 10 if selected_pool == "ssoSuper" else 6
            elif requested_video_length == 10 and selected_pool == "ssoBasic":
                effective_video_length = 6
                logger.warning(
                    "Requested 10s video but ssoSuper token unavailable; downgraded to 6s on ssoBasic"
                )
            else:
                effective_video_length = requested_video_length

            logger.info(
                "Video length resolved: "
                f"requested={requested_video_length if requested_video_length else 'auto'}, "
                f"effective={effective_video_length}, pool={selected_pool or 'unknown'}"
            )

            think = {"enabled": True, "disabled": False}.get(thinking)
            is_stream = stream if stream is not None else get_config("chat.stream")

            # 提取内容
            from app.services.grok.services.chat import MessageExtractor
            from app.services.grok.services.assets import UploadService

            try:
                prompt, attachments = MessageExtractor.extract(messages, is_video=True)
            except ValueError as e:
                raise ValidationException(str(e))

            # 处理图片附件
            image_url = None
            if attachments:
                upload_service = UploadService()
                try:
                    for attach_type, attach_data in attachments:
                        if attach_type == "image":
                            _, file_uri = await upload_service.upload(attach_data, token)
                            image_url = f"https://assets.grok.com/{file_uri}"
                            logger.info(f"Image uploaded for video: {image_url}")
                            break
                finally:
                    await upload_service.close()

            # 生成视频
            service = VideoService()
            if image_url:
                response = await service.generate_from_image(
                    token,
                    prompt,
                    image_url,
                    aspect_ratio,
                    effective_video_length,
                    resolution,
                    preset,
                )
            else:
                response = await service.generate(
                    token,
                    prompt,
                    aspect_ratio,
                    effective_video_length,
                    resolution,
                    preset,
                )

            post_id = service.last_post_id

            # 处理响应
            if is_stream:
                processor = VideoStreamProcessor(model, token, think, post_id=post_id)

                async def _wrap_stream(stream_gen):
                    success = False
                    error_msg = ""

                    keepalive = get_config("performance.sse_keepalive_sec", 15)
                    try:
                        keepalive = float(keepalive)
                    except Exception:
                        keepalive = 15.0

                    stream_with_keepalive = with_keepalive(
                        stream_gen, keepalive, ping_message=": ping\n\n"
                    )

                    try:
                        async for chunk in stream_with_keepalive:
                            yield chunk
                        success = True
                    except Exception as e:
                        error_msg = str(e)
                        raise
                    finally:
                        duration = max(0.0, time.time() - start_time)
                        asyncio.create_task(
                            VideoService._record_stats(
                                model,
                                success,
                                duration,
                                token,
                                error_msg,
                                client_ip,
                                key_name,
                            )
                        )

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
                                    f"Video stream completed, recorded usage (effort={effort.value})"
                                )
                            except Exception as e:
                                logger.warning(f"Failed to record video stream usage: {e}")

                return _wrap_stream(processor.process(response))

            result = await VideoCollectProcessor(model, token, post_id=post_id).process(response)
            try:
                model_info = ModelService.get(model)
                effort = (
                    EffortType.HIGH
                    if (model_info and model_info.cost.value == "high")
                    else EffortType.LOW
                )
                await token_mgr.consume(token, effort)
                logger.debug(f"Video completed, recorded usage (effort={effort.value})")
            except Exception as e:
                logger.warning(f"Failed to record video usage: {e}")

            duration = max(0.0, time.time() - start_time)
            asyncio.create_task(
                VideoService._record_stats(model, True, duration, token, "", client_ip, key_name)
            )
            return result
        except Exception as e:
            duration = max(0.0, time.time() - start_time)
            error_msg = str(getattr(e, "message", e))
            asyncio.create_task(
                VideoService._record_stats(
                    model, False, duration, token, error_msg, client_ip, key_name
                )
            )
            raise


__all__ = ["VideoService"]
