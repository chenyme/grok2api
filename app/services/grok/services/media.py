"""
Grok 视频生成服务
"""

import asyncio
from typing import AsyncGenerator, Optional

import orjson
from curl_cffi.requests import AsyncSession

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
from app.services.grok.processors.processor import VideoStreamProcessor, VideoCollectProcessor
from app.services.grok.utils.headers import apply_statsig, build_sso_cookie

# API 端点
CREATE_POST_API = "https://grok.com/rest/media/post/create"
CHAT_API = "https://grok.com/rest/app-chat/conversations/new"

# 常量
BROWSER = "chrome136"
TIMEOUT = 300
DEFAULT_MAX_CONCURRENT = 50
_MEDIA_SEMAPHORE = asyncio.Semaphore(DEFAULT_MAX_CONCURRENT)
_MEDIA_SEM_VALUE = DEFAULT_MAX_CONCURRENT


def _get_media_semaphore() -> asyncio.Semaphore:
    global _MEDIA_SEMAPHORE, _MEDIA_SEM_VALUE
    value = get_config("performance.media_max_concurrent", DEFAULT_MAX_CONCURRENT)
    try:
        value = int(value)
    except Exception:
        value = DEFAULT_MAX_CONCURRENT
    value = max(1, value)
    if value != _MEDIA_SEM_VALUE:
        _MEDIA_SEM_VALUE = value
        _MEDIA_SEMAPHORE = asyncio.Semaphore(value)
    return _MEDIA_SEMAPHORE


class VideoService:
    """视频生成服务"""

    def __init__(self, proxy: str = None):
        self.proxy = proxy or get_config("grok.base_proxy_url", "")
        self.timeout = get_config("grok.timeout", TIMEOUT)

    def _build_headers(
        self, token: str, referer: str = "https://grok.com/imagine"
    ) -> dict:
        """构建请求头"""
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
            "User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36",
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
        """
        创建媒体帖子

        Args:
            token: 认证 Token
            prompt: 提示词（视频生成用）
            media_type: 媒体类型 (MEDIA_POST_TYPE_VIDEO 或 MEDIA_POST_TYPE_IMAGE)
            media_url: 媒体 URL（图片模式用）

        Returns:
            post ID
        """
        try:
            headers = self._build_headers(token)

            # 根据类型构建不同的载荷
            if media_type == "MEDIA_POST_TYPE_IMAGE" and media_url:
                payload = {"mediaType": media_type, "mediaUrl": media_url}
            else:
                payload = {"mediaType": media_type, "prompt": prompt}

            async with AsyncSession() as session:
                response = await session.post(
                    CREATE_POST_API,
                    headers=headers,
                    json=payload,
                    impersonate=BROWSER,
                    timeout=30,
                    proxies=self._build_proxies(),
                )

            if response.status_code != 200:
                logger.error(f"Create post failed: {response.status_code}")
                raise UpstreamException(
                    f"Failed to create post: {response.status_code}"
                )

            data = response.json()
            post_id = data.get("post", {}).get("id", "")

            if not post_id:
                raise UpstreamException("No post ID in response")

            logger.info(f"Media post created: {post_id} (type={media_type})")
            return post_id

        except Exception as e:
            logger.error(f"Create post error: {e}")
            if isinstance(e, AppException):
                raise e
            raise UpstreamException(f"Create post error: {str(e)}")

    async def create_image_post(self, token: str, image_url: str) -> str:
        """
        创建图片帖子

        Args:
            token: 认证 Token
            image_url: 完整的图片 URL (https://assets.grok.com/...)

        Returns:
            post ID
        """
        return await self.create_post(
            token, prompt="", media_type="MEDIA_POST_TYPE_IMAGE", media_url=image_url
        )

    def _build_payload(
        self,
        prompt: str,
        post_id: str,
        aspect_ratio: str = "3:2",
        video_length: int = 6,
        resolution_name: str = "480p",
        preset: str = "normal",
    ) -> dict:
        """构建视频生成载荷"""
        mode_flag = "--mode=custom"
        if preset == "fun":
            mode_flag = "--mode=extremely-crazy"
        elif preset == "normal":
            mode_flag = "--mode=normal"
        elif preset == "spicy":
            mode_flag = "--mode=extremely-spicy-or-crazy"

        full_prompt = f"{prompt} {mode_flag}"
        video_gen_config = {
            "parentPostId": post_id,
            "aspectRatio": aspect_ratio,
            "videoLength": video_length,
            "resolutionName": resolution_name,
        }
        return {
            "temporary": True,
            "modelName": "grok-3",
            "message": full_prompt,
            "toolOverrides": {"videoGen": True},
            "enableSideBySide": True,
            "responseMetadata": {
                "experiments": [],
                "modelConfigOverride": {
                    "modelMap": {
                        "videoGenModelConfig": video_gen_config
                    }
                },
            },
        }

    async def generate(
        self,
        token: str,
        prompt: str,
        aspect_ratio: str = "3:2",
        video_length: int = 6,
        resolution_name: str = "480p",
        preset: str = "normal",
    ) -> AsyncGenerator[bytes, None]:
        """
        生成视频

        Args:
            token: 认证 Token
            prompt: 视频描述
            aspect_ratio: 宽高比
            video_length: 视频时长
            resolution_name: 分辨率
            preset: 预设

        Returns:
            AsyncGenerator，流式传输

        Raises:
            UpstreamException: 连接失败时
        """
        async with _get_media_semaphore():
            session = None
            try:
                # Step 1: 创建帖子
                post_id = await self.create_post(token, prompt)

                # Step 2: 建立连接
                headers = self._build_headers(token)
                payload = self._build_payload(
                    prompt, post_id, aspect_ratio, video_length, resolution_name, preset
                )

                session = AsyncSession(impersonate=BROWSER)
                response = await session.post(
                    CHAT_API,
                    headers=headers,
                    data=orjson.dumps(payload),
                    timeout=self.timeout,
                    stream=True,
                    proxies=self._build_proxies(),
                )

                if response.status_code != 200:
                    logger.error(f"Video generation failed: {response.status_code}")
                    try:
                        await session.close()
                    except Exception:
                        pass
                    raise UpstreamException(
                        message=f"Video generation failed: {response.status_code}",
                        details={"status": response.status_code},
                    )

                # Step 3: 流式传输
                async def stream_response():
                    try:
                        async for line in response.aiter_lines():
                            yield line
                    finally:
                        if session:
                            await session.close()

                return stream_response()

            except Exception as e:
                if session:
                    try:
                        await session.close()
                    except Exception:
                        pass
                logger.error(f"Video generation error: {e}")
                if isinstance(e, AppException):
                    raise e
                raise UpstreamException(f"Video generation error: {str(e)}")

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
        """
        从图片生成视频

        Args:
            token: 认证 Token
            prompt: 视频描述
            image_url: 图片 URL
            aspect_ratio: 宽高比
            video_length: 视频时长
            resolution: 分辨率
            preset: 预设

        Returns:
            AsyncGenerator，流式传输
        """
        async with _get_media_semaphore():
            session = None
            try:
                # Step 1: 创建帖子
                post_id = await self.create_image_post(token, image_url)

                # Step 2: 建立连接
                headers = self._build_headers(token)
                payload = self._build_payload(
                    prompt, post_id, aspect_ratio, video_length, resolution, preset
                )

                session = AsyncSession(impersonate=BROWSER)
                response = await session.post(
                    CHAT_API,
                    headers=headers,
                    data=orjson.dumps(payload),
                    timeout=self.timeout,
                    stream=True,
                    proxies=self._build_proxies(),
                )

                if response.status_code != 200:
                    logger.error(f"Video from image failed: {response.status_code}")
                    try:
                        await session.close()
                    except Exception:
                        pass
                    raise UpstreamException(
                        message=f"Video from image failed: {response.status_code}",
                        details={"status": response.status_code},
                    )

                # Step 3: 流式传输
                async def stream_response():
                    try:
                        async for line in response.aiter_lines():
                            yield line
                    finally:
                        if session:
                            await session.close()

                return stream_response()

            except Exception as e:
                if session:
                    try:
                        await session.close()
                    except Exception:
                        pass
                logger.error(f"Video from image error: {e}")
                if isinstance(e, AppException):
                    raise e
                raise UpstreamException(f"Video from image error: {str(e)}")

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
                        f"Video stream completed, recorded usage for token {token[:10]}... (effort={effort.value})"
                    )
                except Exception as e:
                    logger.warning(f"Failed to record video stream usage: {e}")

    @staticmethod
    async def completions(
        model: str,
        messages: list,
        stream: bool = None,
        thinking: str = None,
        aspect_ratio: str = "3:2",
        video_length: int = 6,
        resolution: str = "480p",
        preset: str = "normal",
    ):
        """
        视频生成入口

        Args:
            model: 模型名称
            messages: 消息列表
            stream: 是否流式
            thinking: 思考模式
            aspect_ratio: 宽高比
            video_length: 视频时长
            resolution: 分辨率
            preset: 预设模式

        Returns:
            AsyncGenerator (流式) 或 dict (非流式)
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
                        # 上传图片
                        _, file_uri = await upload_service.upload(attach_data, token)
                        image_url = f"https://assets.grok.com/{file_uri}"
                        logger.info(f"Image uploaded for video: {image_url}")
                        break  # 视频模型只使用第一张图片
            finally:
                await upload_service.close()

        # 生成视频
        service = VideoService()

        # 图片转视频
        if image_url:
            response = await service.generate_from_image(
                token,
                prompt,
                image_url,
                aspect_ratio,
                video_length,
                resolution,
                preset,
            )
        else:
            response = await service.generate(
                token, prompt, aspect_ratio, video_length, resolution, preset
            )

        # 处理响应
        if is_stream:
            processor = VideoStreamProcessor(model, token, think)
            return VideoService._wrap_stream(
                processor.process(response), token_mgr, token, model
            )
        else:
            result = await VideoCollectProcessor(model, token).process(response)
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
                    f"Video completed, recorded usage for token {token[:10]}... (effort={effort.value})"
                )
            except Exception as e:
                logger.warning(f"Failed to record video usage: {e}")
            return result


__all__ = ["VideoService"]
