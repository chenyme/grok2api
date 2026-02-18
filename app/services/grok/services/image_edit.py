"""
Grok image edit service.
"""

import asyncio
import random
import re
from dataclasses import dataclass
from typing import AsyncGenerator, AsyncIterable, List, Union, Any

import orjson
from curl_cffi.requests.errors import RequestsError

from app.core.config import get_config
from app.core.exceptions import (
    AppException,
    ErrorType,
    UpstreamException,
    StreamIdleTimeoutError,
)
from app.core.logger import logger
from app.services.grok.utils.process import (
    BaseProcessor,
    _with_idle_timeout,
    _normalize_line,
    _collect_images,
    _is_http2_error,
)
from app.services.grok.utils.upload import UploadService
from app.services.grok.utils.retry import pick_token, rate_limited
from app.services.grok.services.chat import GrokChatService
from app.services.grok.services.video import VideoService
from app.services.grok.utils.stream import wrap_stream_with_usage
from app.services.token import EffortType


@dataclass
class ImageEditResult:
    stream: bool
    data: Union[AsyncGenerator[str, None], List[str]]


class ImageEditService:
    """Image edit orchestration service."""

    async def edit(
        self,
        *,
        token_mgr: Any,
        token: str,
        model_info: Any,
        prompt: str,
        images: List[str],
        n: int,
        response_format: str,
        stream: bool,
    ) -> ImageEditResult:
        max_token_retries = int(get_config("retry.max_retry"))
        tried_tokens: set[str] = set()
        last_error: Exception | None = None

        for attempt in range(max_token_retries):
            preferred = token if attempt == 0 else None
            current_token = await pick_token(
                token_mgr, model_info.model_id, tried_tokens, preferred=preferred
            )
            if not current_token:
                if last_error:
                    raise last_error
                raise AppException(
                    message="No available tokens. Please try again later.",
                    error_type=ErrorType.RATE_LIMIT.value,
                    code="rate_limit_exceeded",
                    status_code=429,
                )

            tried_tokens.add(current_token)
            try:
                image_urls = await self._upload_images(images, current_token)
                parent_post_id = await self._get_parent_post_id(
                    current_token, image_urls
                )

                model_config_override = {
                    "modelMap": {
                        "imageEditModel": "imagine",
                        "imageEditModelConfig": {
                            "imageReferences": image_urls,
                        },
                    }
                }
                if parent_post_id:
                    model_config_override["modelMap"]["imageEditModelConfig"][
                        "parentPostId"
                    ] = parent_post_id

                tool_overrides = {"imageGen": True}

                if stream:
                    response = await GrokChatService().chat(
                        token=current_token,
                        message=prompt,
                        model=model_info.grok_model,
                        mode=None,
                        stream=True,
                        tool_overrides=tool_overrides,
                        model_config_override=model_config_override,
                    )
                    processor = ImageStreamProcessor(
                        model_info.model_id,
                        current_token,
                        n=n,
                        response_format=response_format,
                    )
                    return ImageEditResult(
                        stream=True,
                        data=wrap_stream_with_usage(
                            processor.process(response),
                            token_mgr,
                            current_token,
                            model_info.model_id,
                        ),
                    )

                images_out = await self._collect_images(
                    token=current_token,
                    prompt=prompt,
                    model_info=model_info,
                    n=n,
                    response_format=response_format,
                    tool_overrides=tool_overrides,
                    model_config_override=model_config_override,
                )
                try:
                    effort = (
                        EffortType.HIGH
                        if (model_info and model_info.cost.value == "high")
                        else EffortType.LOW
                    )
                    await token_mgr.consume(current_token, effort)
                    logger.debug(
                        f"Image edit completed, recorded usage (effort={effort.value})"
                    )
                except Exception as e:
                    logger.warning(f"Failed to record image edit usage: {e}")
                return ImageEditResult(stream=False, data=images_out)

            except UpstreamException as e:
                last_error = e
                if rate_limited(e):
                    await token_mgr.mark_rate_limited(current_token)
                    logger.warning(
                        f"Token {current_token[:10]}... rate limited (429), "
                        f"trying next token (attempt {attempt + 1}/{max_token_retries})"
                    )
                    continue
                raise

        if last_error:
            raise last_error
        raise AppException(
            message="No available tokens. Please try again later.",
            error_type=ErrorType.RATE_LIMIT.value,
            code="rate_limit_exceeded",
            status_code=429,
        )

    async def _upload_images(self, images: List[str], token: str) -> List[str]:
        image_urls: List[str] = []
        upload_service = UploadService()
        try:
            for image in images:
                _, file_uri = await upload_service.upload_file(image, token)
                if file_uri:
                    if file_uri.startswith("http"):
                        image_urls.append(file_uri)
                    else:
                        image_urls.append(
                            f"https://assets.grok.com/{file_uri.lstrip('/')}"
                        )
        finally:
            await upload_service.close()

        if not image_urls:
            raise AppException(
                message="Image upload failed",
                error_type=ErrorType.SERVER.value,
                code="upload_failed",
            )

        return image_urls

    async def _get_parent_post_id(self, token: str, image_urls: List[str]) -> str:
        parent_post_id = None
        try:
            media_service = VideoService()
            parent_post_id = await media_service.create_image_post(token, image_urls[0])
            logger.debug(f"Parent post ID: {parent_post_id}")
        except Exception as e:
            logger.warning(f"Create image post failed: {e}")

        if parent_post_id:
            return parent_post_id

        for url in image_urls:
            match = re.search(r"/generated/([a-f0-9-]+)/", url)
            if match:
                parent_post_id = match.group(1)
                logger.debug(f"Parent post ID: {parent_post_id}")
                break
            match = re.search(r"/users/[^/]+/([a-f0-9-]+)/content", url)
            if match:
                parent_post_id = match.group(1)
                logger.debug(f"Parent post ID: {parent_post_id}")
                break

        return parent_post_id or ""

    async def _collect_images(
        self,
        *,
        token: str,
        prompt: str,
        model_info: Any,
        n: int,
        response_format: str,
        tool_overrides: dict,
        model_config_override: dict,
    ) -> List[str]:
        calls_needed = (n + 1) // 2

        async def _call_edit():
            response = await GrokChatService().chat(
                token=token,
                message=prompt,
                model=model_info.grok_model,
                mode=None,
                stream=True,
                tool_overrides=tool_overrides,
                model_config_override=model_config_override,
            )
            processor = ImageCollectProcessor(
                model_info.model_id, token, response_format=response_format
            )
            return await processor.process(response)

        last_error: Exception | None = None
        rate_limit_error: Exception | None = None

        if calls_needed == 1:
            all_images = await _call_edit()
        else:
            tasks = [_call_edit() for _ in range(calls_needed)]
            results = await asyncio.gather(*tasks, return_exceptions=True)

            all_images: List[str] = []
            for result in results:
                if isinstance(result, Exception):
                    logger.error(f"Concurrent call failed: {result}")
                    last_error = result
                    if rate_limited(result):
                        rate_limit_error = result
                elif isinstance(result, list):
                    all_images.extend(result)

        if not all_images:
            if rate_limit_error:
                raise rate_limit_error
            if last_error:
                raise last_error
            raise UpstreamException(
                "Image edit returned no results", details={"error": "empty_result"}
            )

        if len(all_images) >= n:
            return all_images[:n]

        selected_images = all_images.copy()
        while len(selected_images) < n:
            selected_images.append("error")
        return selected_images


class ImageStreamProcessor(BaseProcessor):
    """HTTP image stream processor."""

    def __init__(
        self, model: str, token: str = "", n: int = 1, response_format: str = "b64_json"
    ):
        super().__init__(model, token)
        self.partial_index = 0
        self.n = n
        self.target_index = 0 if n == 1 else None
        self.response_format = response_format
        if response_format == "url":
            self.response_field = "url"
        elif response_format == "base64":
            self.response_field = "base64"
        else:
            self.response_field = "b64_json"

    def _sse(self, event: str, data: dict) -> str:
        """Build SSE response."""
        return f"event: {event}\ndata: {orjson.dumps(data).decode()}\n\n"

    async def process(
        self, response: AsyncIterable[bytes]
    ) -> AsyncGenerator[str, None]:
        """Process stream response."""
        final_images = []
        idle_timeout = get_config("image.stream_timeout")

        try:
            async for line in _with_idle_timeout(response, idle_timeout, self.model):
                line = _normalize_line(line)
                if not line:
                    continue
                try:
                    data = orjson.loads(line)
                except orjson.JSONDecodeError:
                    continue

                resp = data.get("result", {}).get("response", {})

                # Image generation progress
                if img := resp.get("streamingImageGenerationResponse"):
                    image_index = img.get("imageIndex", 0)
                    progress = img.get("progress", 0)

                    if self.n == 1 and image_index != self.target_index:
                        continue

                    out_index = 0 if self.n == 1 else image_index

                    yield self._sse(
                        "image_generation.partial_image",
                        {
                            "type": "image_generation.partial_image",
                            self.response_field: "",
                            "index": out_index,
                            "progress": progress,
                        },
                    )
                    continue

                # modelResponse
                if mr := resp.get("modelResponse"):
                    if urls := _collect_images(mr):
                        for url in urls:
                            if self.response_format == "url":
                                processed = await self.process_url(url, "image")
                                if processed:
                                    final_images.append(processed)
                                continue
                            try:
                                dl_service = self._get_dl()
                                base64_data = await dl_service.parse_b64(
                                    url, self.token, "image"
                                )
                                if base64_data:
                                    if "," in base64_data:
                                        b64 = base64_data.split(",", 1)[1]
                                    else:
                                        b64 = base64_data
                                    final_images.append(b64)
                            except Exception as e:
                                logger.warning(
                                    f"Failed to convert image to base64, falling back to URL: {e}"
                                )
                                processed = await self.process_url(url, "image")
                                if processed:
                                    final_images.append(processed)
                    continue

            for index, b64 in enumerate(final_images):
                if self.n == 1:
                    if index != self.target_index:
                        continue
                    out_index = 0
                else:
                    out_index = index

                yield self._sse(
                    "image_generation.completed",
                    {
                        "type": "image_generation.completed",
                        self.response_field: b64,
                        "index": out_index,
                        "usage": {
                            "total_tokens": 0,
                            "input_tokens": 0,
                            "output_tokens": 0,
                            "input_tokens_details": {
                                "text_tokens": 0,
                                "image_tokens": 0,
                            },
                        },
                    },
                )
        except asyncio.CancelledError:
            logger.debug("Image stream cancelled by client")
        except StreamIdleTimeoutError as e:
            raise UpstreamException(
                message=f"Image stream idle timeout after {e.idle_seconds}s",
                status_code=504,
                details={
                    "error": str(e),
                    "type": "stream_idle_timeout",
                    "idle_seconds": e.idle_seconds,
                },
            )
        except RequestsError as e:
            if _is_http2_error(e):
                logger.warning(f"HTTP/2 stream error in image: {e}")
                raise UpstreamException(
                    message="Upstream connection closed unexpectedly",
                    status_code=502,
                    details={"error": str(e), "type": "http2_stream_error"},
                )
            logger.error(f"Image stream request error: {e}")
            raise UpstreamException(
                message=f"Upstream request failed: {e}",
                status_code=502,
                details={"error": str(e)},
            )
        except Exception as e:
            logger.error(
                f"Image stream processing error: {e}",
                extra={"error_type": type(e).__name__},
            )
            raise
        finally:
            await self.close()


class ImageCollectProcessor(BaseProcessor):
    """HTTP image non-stream processor."""

    def __init__(self, model: str, token: str = "", response_format: str = "b64_json"):
        if response_format == "base64":
            response_format = "b64_json"
        super().__init__(model, token)
        self.response_format = response_format

    async def process(self, response: AsyncIterable[bytes]) -> List[str]:
        """Process and collect images."""
        images = []
        idle_timeout = get_config("image.stream_timeout")

        try:
            async for line in _with_idle_timeout(response, idle_timeout, self.model):
                line = _normalize_line(line)
                if not line:
                    continue
                try:
                    data = orjson.loads(line)
                except orjson.JSONDecodeError:
                    continue

                resp = data.get("result", {}).get("response", {})

                if mr := resp.get("modelResponse"):
                    if urls := _collect_images(mr):
                        for url in urls:
                            if self.response_format == "url":
                                processed = await self.process_url(url, "image")
                                if processed:
                                    images.append(processed)
                                continue
                            try:
                                dl_service = self._get_dl()
                                base64_data = await dl_service.parse_b64(
                                    url, self.token, "image"
                                )
                                if base64_data:
                                    if "," in base64_data:
                                        b64 = base64_data.split(",", 1)[1]
                                    else:
                                        b64 = base64_data
                                    images.append(b64)
                            except Exception as e:
                                logger.warning(
                                    f"Failed to convert image to base64, falling back to URL: {e}"
                                )
                                processed = await self.process_url(url, "image")
                                if processed:
                                    images.append(processed)

        except asyncio.CancelledError:
            logger.debug("Image collect cancelled by client")
        except StreamIdleTimeoutError as e:
            logger.warning(f"Image collect idle timeout: {e}")
        except RequestsError as e:
            if _is_http2_error(e):
                logger.warning(f"HTTP/2 stream error in image collect: {e}")
            else:
                logger.error(f"Image collect request error: {e}")
        except Exception as e:
            logger.error(
                f"Image collect processing error: {e}",
                extra={"error_type": type(e).__name__},
            )
        finally:
            await self.close()

        return images


__all__ = ["ImageEditService", "ImageEditResult"]
