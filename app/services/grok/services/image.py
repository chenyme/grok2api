"""
Grok image services.
"""

import asyncio
import base64
import math
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any, AsyncGenerator, AsyncIterable, Dict, List, Optional, Union

import orjson

from app.core.config import get_config
from app.core.logger import logger
from app.core.storage import DATA_DIR
from app.core.exceptions import AppException, ErrorType, UpstreamException
from app.services.grok.utils.process import BaseProcessor
from app.services.grok.utils.retry import pick_token, rate_limited
from app.services.grok.utils.response import make_response_id, make_chat_chunk, wrap_image_content
from app.services.grok.utils.stream import wrap_stream_with_usage
from app.services.grok.services.chat import GrokChatService
from app.services.grok.services.image_edit import (
    ImageStreamProcessor as AppChatImageStreamProcessor,
    ImageCollectProcessor as AppChatImageCollectProcessor,
)
from app.services.token import EffortType
from app.services.reverse.ws_imagine import ImagineWebSocketReverse


image_service = ImagineWebSocketReverse()


@dataclass
class ImageGenerationResult:
    stream: bool
    data: Union[AsyncGenerator[str, None], List[str]]
    usage_override: Optional[dict] = None


class ImageGenerationService:
    """Image generation orchestration service."""

    @staticmethod
    def _app_chat_request_overrides(
        count: int,
        enable_nsfw: Optional[bool],
    ) -> Dict[str, Any]:
        overrides: Dict[str, Any] = {
            "imageGenerationCount": max(1, int(count or 1)),
        }
        if enable_nsfw is not None:
            overrides["enableNsfw"] = bool(enable_nsfw)
        return overrides

    async def generate(
        self,
        *,
        token_mgr: Any,
        token: str,
        model_info: Any,
        prompt: str,
        n: int,
        response_format: str,
        size: str,
        aspect_ratio: str,
        stream: bool,
        enable_nsfw: Optional[bool] = None,
        chat_format: bool = False,
        prefer_ws: bool = False,
    ) -> ImageGenerationResult:
        max_token_retries = int(get_config("retry.max_retry") or 3)
        tried_tokens: set[str] = set()
        last_error: Optional[Exception] = None

        # resolve nsfw once for routing and upstream
        if enable_nsfw is None:
            enable_nsfw = bool(get_config("image.nsfw"))
        prefer_tags = {"nsfw"} if enable_nsfw else None

        if prefer_ws:
            if stream:
                return await self._stream_ws(
                    token_mgr=token_mgr,
                    token=token,
                    model_info=model_info,
                    prompt=prompt,
                    n=n,
                    response_format=response_format,
                    size=size,
                    aspect_ratio=aspect_ratio,
                    enable_nsfw=enable_nsfw,
                    chat_format=chat_format,
                )
            return await self._collect_ws(
                token_mgr=token_mgr,
                token=token,
                model_info=model_info,
                tried_tokens=tried_tokens,
                prompt=prompt,
                n=n,
                response_format=response_format,
                aspect_ratio=aspect_ratio,
                enable_nsfw=enable_nsfw,
            )

        if stream:

            async def _stream_retry() -> AsyncGenerator[str, None]:
                nonlocal last_error
                for attempt in range(max_token_retries):
                    preferred = token if (attempt == 0 and not prefer_tags) else None
                    current_token = await pick_token(
                        token_mgr,
                        model_info.model_id,
                        tried_tokens,
                        preferred=preferred,
                        prefer_tags=prefer_tags,
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
                    yielded = False
                    try:
                        try:
                            result = await self._stream_app_chat(
                                token_mgr=token_mgr,
                                token=current_token,
                                model_info=model_info,
                                prompt=prompt,
                                n=n,
                                response_format=response_format,
                                enable_nsfw=enable_nsfw,
                                chat_format=chat_format,
                            )
                        except UpstreamException as app_chat_error:
                            if rate_limited(app_chat_error):
                                raise
                            logger.warning(
                                "App-chat image stream failed, falling back to ws_imagine: %s",
                                app_chat_error,
                            )
                            result = await self._stream_ws(
                                token_mgr=token_mgr,
                                token=current_token,
                                model_info=model_info,
                                prompt=prompt,
                                n=n,
                                response_format=response_format,
                                size=size,
                                aspect_ratio=aspect_ratio,
                                enable_nsfw=enable_nsfw,
                                chat_format=chat_format,
                            )
                        async for chunk in result.data:
                            yielded = True
                            yield chunk
                        return
                    except UpstreamException as e:
                        last_error = e
                        if rate_limited(e):
                            if yielded:
                                raise
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

            return ImageGenerationResult(stream=True, data=_stream_retry())

        for attempt in range(max_token_retries):
            preferred = token if (attempt == 0 and not prefer_tags) else None
            current_token = await pick_token(
                token_mgr,
                model_info.model_id,
                tried_tokens,
                preferred=preferred,
                prefer_tags=prefer_tags,
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
                try:
                    return await self._collect_app_chat(
                        token_mgr=token_mgr,
                        token=current_token,
                        model_info=model_info,
                        prompt=prompt,
                        n=n,
                        response_format=response_format,
                        enable_nsfw=enable_nsfw,
                    )
                except UpstreamException as app_chat_error:
                    if rate_limited(app_chat_error):
                        raise
                    logger.warning(
                        "App-chat image collect failed, falling back to ws_imagine: %s",
                        app_chat_error,
                    )
                    return await self._collect_ws(
                        token_mgr=token_mgr,
                        token=current_token,
                        model_info=model_info,
                        tried_tokens=tried_tokens,
                        prompt=prompt,
                        n=n,
                        response_format=response_format,
                        aspect_ratio=aspect_ratio,
                        enable_nsfw=enable_nsfw,
                    )
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

    async def _stream_ws(
        self,
        *,
        token_mgr: Any,
        token: str,
        model_info: Any,
        prompt: str,
        n: int,
        response_format: str,
        size: str,
        aspect_ratio: str,
        enable_nsfw: Optional[bool] = None,
        chat_format: bool = False,
    ) -> ImageGenerationResult:
        if enable_nsfw is None:
            enable_nsfw = bool(get_config("image.nsfw"))
        stream_retries = int(get_config("image.blocked_parallel_attempts") or 5) + 1
        stream_retries = max(1, min(stream_retries, 10))
        upstream = image_service.stream(
            token=token,
            prompt=prompt,
            aspect_ratio=aspect_ratio,
            n=n,
            enable_nsfw=enable_nsfw,
            max_retries=stream_retries,
        )
        processor = ImageWSStreamProcessor(
            model_info.model_id,
            token,
            n=n,
            response_format=response_format,
            size=size,
            chat_format=chat_format,
        )
        stream = wrap_stream_with_usage(
            processor.process(upstream),
            token_mgr,
            token,
            model_info.model_id,
        )
        return ImageGenerationResult(stream=True, data=stream)

    async def _stream_app_chat(
        self,
        *,
        token_mgr: Any,
        token: str,
        model_info: Any,
        prompt: str,
        n: int,
        response_format: str,
        enable_nsfw: Optional[bool] = None,
        chat_format: bool = False,
    ) -> ImageGenerationResult:
        response = await GrokChatService().chat(
            token=token,
            message=prompt,
            model=model_info.grok_model,
            mode=model_info.model_mode,
            stream=True,
            tool_overrides={"imageGen": True},
            request_overrides=self._app_chat_request_overrides(n, enable_nsfw),
        )
        processor = AppChatImageStreamProcessor(
            model_info.model_id,
            token,
            n=n,
            response_format=response_format,
            chat_format=chat_format,
        )
        stream = wrap_stream_with_usage(
            processor.process(response),
            token_mgr,
            token,
            model_info.model_id,
        )
        return ImageGenerationResult(stream=True, data=stream)

    async def _collect_app_chat(
        self,
        *,
        token_mgr: Any,
        token: str,
        model_info: Any,
        prompt: str,
        n: int,
        response_format: str,
        enable_nsfw: Optional[bool] = None,
    ) -> ImageGenerationResult:
        per_call = min(max(1, n), 2)
        calls_needed = max(1, int(math.ceil(n / per_call)))

        async def _call_generate(call_target: int) -> List[str]:
            response = await GrokChatService().chat(
                token=token,
                message=prompt,
                model=model_info.grok_model,
                mode=model_info.model_mode,
                stream=True,
                tool_overrides={"imageGen": True},
                request_overrides=self._app_chat_request_overrides(
                    call_target, enable_nsfw
                ),
            )
            processor = AppChatImageCollectProcessor(
                model_info.model_id,
                token,
                response_format=response_format,
            )
            return await processor.process(response)

        if calls_needed == 1:
            all_images = await _call_generate(n)
        else:
            tasks = []
            for i in range(calls_needed):
                remaining = n - (i * per_call)
                tasks.append(_call_generate(min(per_call, remaining)))
            results = await asyncio.gather(*tasks, return_exceptions=True)
            all_images: List[str] = []
            last_error: Optional[Exception] = None
            rate_limit_error: Optional[Exception] = None
            for result in results:
                if isinstance(result, Exception):
                    logger.warning(f"Concurrent app-chat image call failed: {result}")
                    last_error = result
                    if rate_limited(result):
                        rate_limit_error = result
                    continue
                for image in result:
                    if image not in all_images:
                        all_images.append(image)

            if not all_images:
                if rate_limit_error:
                    raise rate_limit_error
                if last_error:
                    raise last_error

        if not all_images:
            raise UpstreamException(
                "Image generation returned no results",
                details={"error": "empty_result", "path": "app_chat"},
            )

        try:
            await token_mgr.consume(token, self._get_effort(model_info))
        except Exception as e:
            logger.warning(f"Failed to consume token: {e}")

        selected = self._select_images(all_images, n)
        usage_override = {
            "total_tokens": 0,
            "input_tokens": 0,
            "output_tokens": 0,
            "input_tokens_details": {"text_tokens": 0, "image_tokens": 0},
        }
        return ImageGenerationResult(
            stream=False, data=selected, usage_override=usage_override
        )

    async def _collect_ws(
        self,
        *,
        token_mgr: Any,
        token: str,
        model_info: Any,
        tried_tokens: set[str],
        prompt: str,
        n: int,
        response_format: str,
        aspect_ratio: str,
        enable_nsfw: Optional[bool] = None,
    ) -> ImageGenerationResult:
        if enable_nsfw is None:
            enable_nsfw = bool(get_config("image.nsfw"))
        all_images: List[str] = []
        seen = set()
        expected_per_call = 6
        calls_needed = max(1, int(math.ceil(n / expected_per_call)))
        calls_needed = min(calls_needed, n)

        async def _fetch_batch(call_target: int, call_token: str):
            stream_retries = int(get_config("image.blocked_parallel_attempts") or 5) + 1
            stream_retries = max(1, min(stream_retries, 10))
            upstream = image_service.stream(
                token=call_token,
                prompt=prompt,
                aspect_ratio=aspect_ratio,
                n=call_target,
                enable_nsfw=enable_nsfw,
                max_retries=stream_retries,
            )
            processor = ImageWSCollectProcessor(
                model_info.model_id,
                token,
                n=call_target,
                response_format=response_format,
            )
            return await processor.process(upstream)

        tasks = []
        for i in range(calls_needed):
            remaining = n - (i * expected_per_call)
            call_target = min(expected_per_call, remaining)
            tasks.append(_fetch_batch(call_target, token))

        results = await asyncio.gather(*tasks, return_exceptions=True)
        for batch in results:
            if isinstance(batch, Exception):
                logger.warning(f"WS batch failed: {batch}")
                continue
            for img in batch:
                if img not in seen:
                    seen.add(img)
                    all_images.append(img)
                if len(all_images) >= n:
                    break
            if len(all_images) >= n:
                break

        # If upstream likely blocked/reviewed some images, run extra parallel attempts
        # and only keep valid finals selected by ws_imagine classification.
        if len(all_images) < n:
            remaining = n - len(all_images)
            extra_attempts = int(get_config("image.blocked_parallel_attempts") or 5)
            extra_attempts = max(0, min(extra_attempts, 10))
            parallel_enabled = bool(get_config("image.blocked_parallel_enabled", True))
            if extra_attempts > 0:
                logger.warning(
                    f"Image finals insufficient ({len(all_images)}/{n}), running "
                    f"{extra_attempts} recovery attempts for remaining={remaining}, "
                    f"parallel_enabled={parallel_enabled}"
                )
                extra_tasks = []
                if parallel_enabled:
                    recovery_tried = set(tried_tokens)
                    recovery_tokens: List[str] = []
                    for _ in range(extra_attempts):
                        recovery_token = await pick_token(
                            token_mgr,
                            model_info.model_id,
                            recovery_tried,
                        )
                        if not recovery_token:
                            break
                        recovery_tried.add(recovery_token)
                        recovery_tokens.append(recovery_token)

                    if recovery_tokens:
                        logger.info(
                            f"Recovery using {len(recovery_tokens)} distinct tokens"
                        )
                    for recovery_token in recovery_tokens:
                        extra_tasks.append(
                            _fetch_batch(min(expected_per_call, remaining), recovery_token)
                        )
                else:
                    extra_tasks = [
                        _fetch_batch(min(expected_per_call, remaining), token)
                        for _ in range(extra_attempts)
                    ]

                if not extra_tasks:
                    logger.warning("No tokens available for recovery attempts")
                    extra_results = []
                else:
                    extra_results = await asyncio.gather(*extra_tasks, return_exceptions=True)
                for batch in extra_results:
                    if isinstance(batch, Exception):
                        logger.warning(f"WS recovery batch failed: {batch}")
                        continue
                    for img in batch:
                        if img not in seen:
                            seen.add(img)
                            all_images.append(img)
                        if len(all_images) >= n:
                            break
                    if len(all_images) >= n:
                        break
                logger.info(
                    f"Image recovery attempts completed: finals={len(all_images)}/{n}, "
                    f"attempts={extra_attempts}"
                )

        if len(all_images) < n:
            logger.error(
                f"Image generation failed after recovery attempts: finals={len(all_images)}/{n}, "
                f"blocked_parallel_attempts={int(get_config('image.blocked_parallel_attempts') or 5)}"
            )
            raise UpstreamException(
                "Image generation blocked or no valid final image",
                details={
                    "error_code": "blocked_no_final_image",
                    "final_images": len(all_images),
                    "requested": n,
                },
            )

        try:
            await token_mgr.consume(token, self._get_effort(model_info))
        except Exception as e:
            logger.warning(f"Failed to consume token: {e}")

        selected = self._select_images(all_images, n)
        usage_override = {
            "total_tokens": 0,
            "input_tokens": 0,
            "output_tokens": 0,
            "input_tokens_details": {"text_tokens": 0, "image_tokens": 0},
        }
        return ImageGenerationResult(
            stream=False, data=selected, usage_override=usage_override
        )

    @staticmethod
    def _get_effort(model_info: Any) -> EffortType:
        return (
            EffortType.HIGH
            if (model_info and model_info.cost.value == "high")
            else EffortType.LOW
        )

    @staticmethod
    def _select_images(images: List[str], n: int) -> List[str]:
        if len(images) >= n:
            return images[:n]
        selected = images.copy()
        while len(selected) < n:
            selected.append("error")
        return selected


class ImageWSBaseProcessor(BaseProcessor):
    """WebSocket image processor base."""

    def __init__(self, model: str, token: str = "", response_format: str = "b64_json"):
        if response_format == "base64":
            response_format = "b64_json"
        super().__init__(model, token)
        self.response_format = response_format
        if response_format == "url":
            self.response_field = "url"
        elif response_format == "base64":
            self.response_field = "base64"
        else:
            self.response_field = "b64_json"
        self._image_dir: Optional[Path] = None

    def _ensure_image_dir(self) -> Path:
        if self._image_dir is None:
            base_dir = DATA_DIR / "tmp" / "image"
            base_dir.mkdir(parents=True, exist_ok=True)
            self._image_dir = base_dir
        return self._image_dir

    def _strip_base64(self, blob: str) -> str:
        if not blob:
            return ""
        if "," in blob and "base64" in blob.split(",", 1)[0]:
            return blob.split(",", 1)[1]
        return blob

    def _guess_ext(self, blob: str) -> Optional[str]:
        if not blob:
            return None
        header = ""
        data = blob
        if "," in blob and "base64" in blob.split(",", 1)[0]:
            header, data = blob.split(",", 1)
        header = header.lower()
        if "image/png" in header:
            return "png"
        if "image/jpeg" in header or "image/jpg" in header:
            return "jpg"
        if data.startswith("iVBORw0KGgo"):
            return "png"
        if data.startswith("/9j/"):
            return "jpg"
        return None

    def _filename(self, image_id: str, is_final: bool, ext: Optional[str] = None) -> str:
        if ext:
            ext = ext.lower()
            if ext == "jpeg":
                ext = "jpg"
        if not ext:
            ext = "jpg" if is_final else "png"
        return f"{image_id}.{ext}"

    def _build_file_url(self, filename: str) -> str:
        app_url = get_config("app.app_url")
        if app_url:
            return f"{app_url.rstrip('/')}/v1/files/image/{filename}"
        return f"/v1/files/image/{filename}"

    async def _save_blob(
        self, image_id: str, blob: str, is_final: bool, ext: Optional[str] = None
    ) -> str:
        data = self._strip_base64(blob)
        if not data:
            return ""
        image_dir = self._ensure_image_dir()
        ext = ext or self._guess_ext(blob)
        filename = self._filename(image_id, is_final, ext=ext)
        filepath = image_dir / filename

        def _write_file():
            with open(filepath, "wb") as f:
                f.write(base64.b64decode(data))

        await asyncio.to_thread(_write_file)
        return self._build_file_url(filename)

    def _pick_best(self, existing: Optional[Dict], incoming: Dict) -> Dict:
        if not existing:
            return incoming
        if incoming.get("is_final") and not existing.get("is_final"):
            return incoming
        if existing.get("is_final") and not incoming.get("is_final"):
            return existing
        if incoming.get("blob_size", 0) > existing.get("blob_size", 0):
            return incoming
        return existing

    async def _to_output(self, image_id: str, item: Dict) -> str:
        try:
            if self.response_format == "url":
                return await self._save_blob(
                    image_id,
                    item.get("blob", ""),
                    item.get("is_final", False),
                    ext=item.get("ext"),
                )
            return self._strip_base64(item.get("blob", ""))
        except Exception as e:
            logger.warning(f"Image output failed: {e}")
            return ""


class ImageWSStreamProcessor(ImageWSBaseProcessor):
    """WebSocket image stream processor."""

    def __init__(
        self,
        model: str,
        token: str = "",
        n: int = 1,
        response_format: str = "b64_json",
        size: str = "1024x1024",
        chat_format: bool = False,
    ):
        super().__init__(model, token, response_format)
        self.n = n
        self.size = size
        self.chat_format = chat_format
        self._target_id: Optional[str] = None
        self._index_map: Dict[str, int] = {}
        self._partial_map: Dict[str, int] = {}
        self._initial_sent: set[str] = set()
        self._id_generated: bool = False
        self._response_id: str = ""

    def _assign_index(self, image_id: str) -> Optional[int]:
        if image_id in self._index_map:
            return self._index_map[image_id]
        if len(self._index_map) >= self.n:
            return None
        self._index_map[image_id] = len(self._index_map)
        return self._index_map[image_id]

    def _sse(self, event: str, data: dict) -> str:
        return f"event: {event}\ndata: {orjson.dumps(data).decode()}\n\n"

    async def process(self, response: AsyncIterable[dict]) -> AsyncGenerator[str, None]:
        images: Dict[str, Dict] = {}
        emitted_chat_chunk = False

        async for item in response:
            if item.get("type") == "error":
                message = item.get("error") or "Upstream error"
                code = item.get("error_code") or "upstream_error"
                status = item.get("status")
                if code == "rate_limit_exceeded" or status == 429:
                    raise UpstreamException(message, details=item)
                yield self._sse(
                    "error",
                    {
                        "error": {
                            "message": message,
                            "type": "server_error",
                            "code": code,
                        }
                    },
                )
                return
            if item.get("type") != "image":
                continue

            image_id = item.get("image_id")
            if not image_id:
                continue

            if self.n == 1:
                if self._target_id is None:
                    self._target_id = image_id
                index = 0 if image_id == self._target_id else None
            else:
                index = self._assign_index(image_id)

            images[image_id] = self._pick_best(images.get(image_id), item)

            if index is None:
                continue

            if item.get("stage") != "final":
                # Chat Completions image stream should only expose final results.
                if self.chat_format:
                    continue
                if image_id not in self._initial_sent:
                    self._initial_sent.add(image_id)
                    stage = item.get("stage") or "preview"
                    if stage == "medium":
                        partial_index = 1
                        self._partial_map[image_id] = 1
                    else:
                        partial_index = 0
                        self._partial_map[image_id] = 0
                else:
                    stage = item.get("stage") or "partial"
                    if stage == "preview":
                        continue
                    partial_index = self._partial_map.get(image_id, 0)
                    if stage == "medium":
                        partial_index = max(partial_index, 1)
                    self._partial_map[image_id] = partial_index

                if self.response_format == "url":
                    partial_id = f"{image_id}-{stage}-{partial_index}"
                    partial_out = await self._save_blob(
                        partial_id,
                        item.get("blob", ""),
                        False,
                        ext=item.get("ext"),
                    )
                else:
                    partial_out = self._strip_base64(item.get("blob", ""))

                if self.chat_format and partial_out:
                    partial_out = wrap_image_content(partial_out, self.response_format)

                if not partial_out:
                    continue

                if self.chat_format:
                    # OpenAI ChatCompletion chunk format for partial
                    if not self._id_generated:
                        self._response_id = make_response_id()
                        self._id_generated = True
                    emitted_chat_chunk = True
                    yield self._sse(
                        "chat.completion.chunk",
                        make_chat_chunk(
                            self._response_id,
                            self.model,
                            partial_out,
                            index=index,
                        ),
                    )
                else:
                    # Original image_generation format
                    yield self._sse(
                        "image_generation.partial_image",
                        {
                            "type": "image_generation.partial_image",
                            self.response_field: partial_out,
                            "created_at": int(time.time()),
                            "size": self.size,
                            "index": index,
                            "partial_image_index": partial_index,
                            "image_id": image_id,
                            "stage": stage,
                        },
                    )

        if self.n == 1:
            target_item = images.get(self._target_id) if self._target_id else None
            if target_item and target_item.get("is_final", False):
                selected = [(self._target_id, target_item)]
            elif images:
                selected = [
                    max(
                        images.items(),
                        key=lambda x: (
                            x[1].get("is_final", False),
                            x[1].get("blob_size", 0),
                        ),
                    )
                ]
            else:
                selected = []
        else:
            selected = [
                (image_id, images[image_id])
                for image_id in self._index_map
                if image_id in images and images[image_id].get("is_final", False)
            ]

        for image_id, item in selected:
            if self.response_format == "url":
                final_image_id = image_id
                # Keep original imagine image name for imagine chat stream output.
                if self.model != "grok-imagine-1.0-fast":
                    final_image_id = f"{image_id}-final"
                output = await self._save_blob(
                    final_image_id,
                    item.get("blob", ""),
                    item.get("is_final", False),
                    ext=item.get("ext"),
                )
                if self.chat_format and output:
                    output = wrap_image_content(output, self.response_format)
            else:
                output = await self._to_output(image_id, item)
                if self.chat_format and output:
                    output = wrap_image_content(output, self.response_format)

            if not output:
                continue

            if self.n == 1:
                index = 0
            else:
                index = self._index_map.get(image_id, 0)

            if not self._id_generated:
                self._response_id = make_response_id()
                self._id_generated = True

            if self.chat_format:
                # OpenAI ChatCompletion chunk format
                emitted_chat_chunk = True
                yield self._sse(
                    "chat.completion.chunk",
                    make_chat_chunk(
                        self._response_id,
                        self.model,
                        output,
                        index=index,
                        is_final=True,
                    ),
                )
            else:
                # Original image_generation format
                yield self._sse(
                    "image_generation.completed",
                    {
                        "type": "image_generation.completed",
                        self.response_field: output,
                        "created_at": int(time.time()),
                        "size": self.size,
                        "index": index,
                        "image_id": image_id,
                        "stage": "final",
                        "usage": {
                            "total_tokens": 0,
                            "input_tokens": 0,
                            "output_tokens": 0,
                            "input_tokens_details": {"text_tokens": 0, "image_tokens": 0},
                        },
                    },
                )

        if self.chat_format:
            if not self._id_generated:
                self._response_id = make_response_id()
                self._id_generated = True
            if not emitted_chat_chunk:
                yield self._sse(
                    "chat.completion.chunk",
                    make_chat_chunk(
                        self._response_id,
                        self.model,
                        "",
                        index=0,
                        is_final=True,
                    ),
                )
            yield "data: [DONE]\n\n"


class ImageWSCollectProcessor(ImageWSBaseProcessor):
    """WebSocket image non-stream processor."""

    def __init__(
        self, model: str, token: str = "", n: int = 1, response_format: str = "b64_json"
    ):
        super().__init__(model, token, response_format)
        self.n = n

    async def process(self, response: AsyncIterable[dict]) -> List[str]:
        images: Dict[str, Dict] = {}

        async for item in response:
            if item.get("type") == "error":
                message = item.get("error") or "Upstream error"
                raise UpstreamException(message, details=item)
            if item.get("type") != "image":
                continue
            image_id = item.get("image_id")
            if not image_id:
                continue
            images[image_id] = self._pick_best(images.get(image_id), item)

        selected = sorted(
            [item for item in images.values() if item.get("is_final", False)],
            key=lambda x: x.get("blob_size", 0),
            reverse=True,
        )
        if self.n:
            selected = selected[: self.n]

        results: List[str] = []
        for item in selected:
            output = await self._to_output(item.get("image_id", ""), item)
            if output:
                results.append(output)

        return results


__all__ = ["ImageGenerationService"]
