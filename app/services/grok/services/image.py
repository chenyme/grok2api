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
from app.services.grok.utils.stream import wrap_stream_with_usage
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
    ) -> ImageGenerationResult:
        max_token_retries = int(get_config("retry.max_retry"))
        tried_tokens: set[str] = set()
        last_error: Optional[Exception] = None

        if stream:
            async def _stream_retry() -> AsyncGenerator[str, None]:
                nonlocal last_error
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
                    yielded = False
                    try:
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
                return await self._collect_ws(
                    token_mgr=token_mgr,
                    token=current_token,
                    model_info=model_info,
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
    ) -> ImageGenerationResult:
        if enable_nsfw is None:
            enable_nsfw = bool(get_config("image.nsfw"))
        upstream = image_service.stream(
            token=token,
            prompt=prompt,
            aspect_ratio=aspect_ratio,
            n=n,
            enable_nsfw=enable_nsfw,
        )
        processor = ImageWSStreamProcessor(
            model_info.model_id,
            token,
            n=n,
            response_format=response_format,
            size=size,
        )
        stream = wrap_stream_with_usage(
            processor.process(upstream),
            token_mgr,
            token,
            model_info.model_id,
        )
        return ImageGenerationResult(stream=True, data=stream)

    async def _collect_ws(
        self,
        *,
        token_mgr: Any,
        token: str,
        model_info: Any,
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

        async def _fetch_batch(call_target: int):
            upstream = image_service.stream(
                token=token,
                prompt=prompt,
                aspect_ratio=aspect_ratio,
                n=call_target,
                enable_nsfw=enable_nsfw,
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
            tasks.append(_fetch_batch(call_target))

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
    ):
        super().__init__(model, token, response_format)
        self.n = n
        self.size = size
        self._target_id: Optional[str] = None
        self._index_map: Dict[str, int] = {}
        self._partial_map: Dict[str, int] = {}
        self._initial_sent: set[str] = set()

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
                if not partial_out:
                    continue
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
            if self._target_id and self._target_id in images:
                selected = [(self._target_id, images[self._target_id])]
            else:
                selected = (
                    [
                        max(
                            images.items(),
                            key=lambda x: (
                                x[1].get("is_final", False),
                                x[1].get("blob_size", 0),
                            ),
                        )
                    ]
                    if images
                    else []
                )
        else:
            selected = [
                (image_id, images[image_id])
                for image_id in self._index_map
                if image_id in images
            ]

        for image_id, item in selected:
            if self.response_format == "url":
                output = await self._save_blob(
                    f"{image_id}-final",
                    item.get("blob", ""),
                    item.get("is_final", False),
                    ext=item.get("ext"),
                )
            else:
                output = await self._to_output(image_id, item)
            if not output:
                continue

            if self.n == 1:
                index = 0
            else:
                index = self._index_map.get(image_id, 0)
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
            images.values(),
            key=lambda x: (x.get("is_final", False), x.get("blob_size", 0)),
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
