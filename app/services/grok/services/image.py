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
from curl_cffi.requests.errors import RequestsError

from app.core.config import get_config
from app.core.logger import logger
from app.core.storage import DATA_DIR
from app.core.exceptions import AppException, ErrorType, UpstreamException
from app.services.grok.utils.process import (
    BaseProcessor,
    _collect_image_candidates,
    _is_http2_error,
    _normalize_line,
    _pick_preferred_image_candidate,
    _with_idle_timeout,
)
from app.services.grok.utils.retry import pick_token, rate_limited
from app.services.grok.utils.response import make_response_id, make_chat_chunk, wrap_image_content
from app.services.grok.utils.share_resolver import resolve_grok_share_image
from app.services.grok.utils.stream import wrap_stream_with_usage
from app.services.grok.services.chat import GrokChatService
from app.services.grok.services.image_edit import (
    ImageStreamProcessor as AppChatImageStreamProcessor,
)
from app.services.token import EffortType
from app.services.reverse.app_chat_share import AppChatShareReverse
from app.services.reverse.utils.session import ResettableSession
from app.services.reverse.ws_imagine import ImagineWebSocketReverse


image_service = ImagineWebSocketReverse()


@dataclass
class ImageGenerationResult:
    stream: bool
    data: Union[AsyncGenerator[str, None], List[str]]
    usage_override: Optional[dict] = None
    share_url: str = ""
    share_image_url: str = ""
    share_image_source: str = ""
    share_image_expires_at: str = ""


@dataclass
class AppChatImageCollectPayload:
    images: List[str]
    post_id: str = ""
    post_id_rank: int = 999
    conversation_id: str = ""
    response_id: str = ""


def _pick_str(value: Any) -> str:
    if isinstance(value, str):
        return value.strip()
    return ""


def _maybe_extract_uuid(value: Any) -> str:
    text = _pick_str(value)
    if not text:
        return ""

    if (
        len(text) in (32, 36)
        and text.replace("-", "").isalnum()
        and text.count("-") in (0, 4)
    ):
        return text

    import re

    match = re.search(r"/generated/([0-9a-fA-F-]{32,36})/", text)
    if match:
        return match.group(1)

    match = re.search(r"\b([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\b", text)
    if match:
        return match.group(1)

    return ""


def _append_post_id_candidate(
    candidates: List[tuple[int, str]],
    rank: int,
    value: Any,
):
    post_id = _maybe_extract_uuid(value)
    if post_id:
        candidates.append((rank, post_id))


def _collect_post_id_candidates(resp: Dict[str, Any]) -> List[tuple[int, str]]:
    candidates: List[tuple[int, str]] = []

    post = resp.get("post")
    if isinstance(post, dict):
        _append_post_id_candidate(candidates, 1, post.get("id"))

    for key in ("postId", "post_id"):
        _append_post_id_candidate(candidates, 2, resp.get(key))

    for key in ("parentPostId", "parent_post_id", "originalPostId", "original_post_id"):
        _append_post_id_candidate(candidates, 3, resp.get(key))

    image_resp = resp.get("streamingImageGenerationResponse")
    if isinstance(image_resp, dict):
        for key in ("postId", "parentPostId", "originalPostId"):
            _append_post_id_candidate(candidates, 4, image_resp.get(key))

    model_resp = resp.get("modelResponse")
    if isinstance(model_resp, dict):
        file_attachments = model_resp.get("fileAttachments")
        if isinstance(file_attachments, list):
            for value in file_attachments:
                _append_post_id_candidate(candidates, 5, value)
        elif file_attachments:
            _append_post_id_candidate(candidates, 5, file_attachments)

        for key in ("postId", "parentPostId", "originalPostId"):
            _append_post_id_candidate(candidates, 5, model_resp.get(key))

        metadata = model_resp.get("metadata")
        if isinstance(metadata, dict):
            for key in ("postId", "parentPostId", "originalPostId"):
                _append_post_id_candidate(candidates, 6, metadata.get(key))

        raw_cards = model_resp.get("cardAttachmentsJson") or []
        if isinstance(raw_cards, list):
            for raw in raw_cards:
                if not isinstance(raw, str) or not raw.strip():
                    continue
                try:
                    card = orjson.loads(raw)
                except orjson.JSONDecodeError:
                    continue
                if not isinstance(card, dict):
                    continue
                for key in ("postId", "parentPostId", "originalPostId"):
                    _append_post_id_candidate(candidates, 6, card.get(key))

    card_attachment = resp.get("cardAttachment")
    if isinstance(card_attachment, dict):
        raw = card_attachment.get("jsonData")
        if isinstance(raw, str) and raw.strip():
            try:
                card = orjson.loads(raw)
            except orjson.JSONDecodeError:
                card = None
            if isinstance(card, dict):
                for key in ("postId", "parentPostId", "originalPostId"):
                    _append_post_id_candidate(candidates, 6, card.get(key))

    return candidates


def _pick_best_post_id(candidates: List[tuple[int, str]]) -> tuple[str, int]:
    best_rank = 999
    best_post_id = ""
    for rank, value in candidates:
        if value and rank < best_rank:
            best_rank = rank
            best_post_id = value
    return best_post_id, best_rank


def _new_session() -> ResettableSession:
    browser = get_config("proxy.browser")
    if browser:
        return ResettableSession(impersonate=browser)
    return ResettableSession()


def _extract_app_chat_result_payload(data: Dict[str, Any]) -> Dict[str, Any]:
    if not isinstance(data, dict):
        return {}
    result = data.get("result")
    if not isinstance(result, dict):
        return {}
    response = result.get("response")
    if isinstance(response, dict):
        return response
    return result


def _extract_app_chat_share_context(data: Dict[str, Any]) -> tuple[str, str]:
    if not isinstance(data, dict):
        return "", ""

    result = data.get("result")
    if not isinstance(result, dict):
        return "", ""

    response = result.get("response")
    if not isinstance(response, dict):
        response = result

    conversation_id = ""
    for value in (
        result.get("conversationId"),
        response.get("conversationId"),
        ((result.get("conversation") or {}).get("conversationId"))
        if isinstance(result.get("conversation"), dict)
        else "",
        ((response.get("conversation") or {}).get("conversationId"))
        if isinstance(response.get("conversation"), dict)
        else "",
    ):
        conversation_id = _pick_str(value)
        if conversation_id:
            break

    response_id = ""
    model_resp = response.get("modelResponse")
    if isinstance(model_resp, dict):
        response_id = _pick_str(model_resp.get("responseId"))

    if not response_id:
        for source in (response, result):
            if not isinstance(source, dict):
                continue
            candidate = _pick_str(source.get("responseId"))
            if not candidate:
                continue
            if any(
                key in source
                for key in (
                    "modelResponse",
                    "cardAttachment",
                    "token",
                    "finalMetadata",
                    "progressReport",
                    "uiLayout",
                    "llmInfo",
                    "streamingImageGenerationResponse",
                )
            ):
                response_id = candidate
                break

    if not response_id:
        user_response = result.get("userResponse")
        if isinstance(user_response, dict):
            response_id = _pick_str(user_response.get("responseId"))

    return conversation_id, response_id


def _build_app_chat_share_url(payload: Any) -> str:
    if not isinstance(payload, dict):
        return ""

    for key in ("shareLink", "shareUrl", "shareURL"):
        value = _pick_str(payload.get(key))
        if value:
            return value

    share_link_id = _pick_str(payload.get("shareLinkId")) or _pick_str(
        payload.get("publicId")
    )
    if not share_link_id:
        return ""

    return f"https://grok.com/share/{share_link_id}"


async def _create_image_share_link(
    token: str,
    conversation_id: str,
    response_id: str,
) -> str:
    if not token or not conversation_id or not response_id:
        return ""

    try:
        async with _new_session() as session:
            response = await AppChatShareReverse.request(
                session,
                token,
                conversation_id,
                response_id,
            )
        payload = response.json() if response is not None else {}
        share_link = _build_app_chat_share_url(payload)
        if share_link:
            logger.info(f"Image share link created: {share_link}")
            return share_link
    except Exception as e:
        logger.warning(f"Image share link failed: {e}")

    return ""


async def _resolve_share_image_details(
    share_url: str,
) -> tuple[str, str, str]:
    if not share_url:
        return "", "", ""

    try:
        resolved = await resolve_grok_share_image(share_url)
        return (
            resolved.image_url,
            resolved.source,
            resolved.expires_at,
        )
    except Exception as e:
        logger.warning(f"Share image resolve failed: {e}")
        return "", "", ""


class ImageAppChatCollectProcessor(BaseProcessor):
    """App-chat image non-stream processor with share-context collection."""

    def __init__(self, model: str, token: str = "", response_format: str = "b64_json"):
        if response_format == "base64":
            response_format = "b64_json"
        super().__init__(model, token)
        self.response_format = response_format

    async def _process_image_url(self, url: str) -> str:
        if self.response_format == "url":
            return await self.process_url(url, "image")

        try:
            dl_service = self._get_dl()
            base64_data = await dl_service.parse_b64(url, self.token, "image")
            if base64_data:
                if "," in base64_data:
                    return base64_data.split(",", 1)[1]
                return base64_data
        except Exception as e:
            logger.warning(
                f"Failed to convert image to base64, falling back to URL: {e}"
            )
            return await self.process_url(url, "image")

        return ""

    async def process(self, response: AsyncIterable[bytes]) -> AppChatImageCollectPayload:
        best_candidates: dict[str, Any] = {}
        post_id = ""
        post_id_rank = 999
        conversation_id = ""
        response_id = ""
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

                resp = _extract_app_chat_result_payload(data)
                if not resp:
                    continue

                current_conversation_id, current_response_id = (
                    _extract_app_chat_share_context(data)
                )
                if current_conversation_id:
                    conversation_id = current_conversation_id
                if current_response_id:
                    response_id = current_response_id

                current_post_id, current_rank = _pick_best_post_id(
                    _collect_post_id_candidates(resp)
                )
                if current_post_id and current_rank < post_id_rank:
                    post_id = current_post_id
                    post_id_rank = current_rank

                if mr := resp.get("modelResponse"):
                    if candidates := _collect_image_candidates(mr):
                        for candidate in candidates:
                            fallback_post_id = _maybe_extract_uuid(candidate.url)
                            if fallback_post_id and post_id_rank > 7:
                                post_id = fallback_post_id
                                post_id_rank = 7
                            existing = best_candidates.get(candidate.key)
                            best_candidates[candidate.key] = (
                                _pick_preferred_image_candidate(existing, candidate)
                            )

        except asyncio.CancelledError:
            logger.debug("Image collect cancelled by client")
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

        images: List[str] = []
        for candidate in sorted(best_candidates.values(), key=lambda item: item.order):
            processed = await self._process_image_url(candidate.url)
            if processed:
                images.append(processed)

        return AppChatImageCollectPayload(
            images=images,
            post_id=post_id,
            post_id_rank=post_id_rank,
            conversation_id=conversation_id,
            response_id=response_id,
        )


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
        return_share_url: bool = False,
    ) -> ImageGenerationResult:
        max_token_retries = int(get_config("retry.max_retry") or 3)
        tried_tokens: set[str] = set()
        last_error: Optional[Exception] = None

        # resolve nsfw once for routing and upstream
        if enable_nsfw is None:
            enable_nsfw = bool(get_config("image.nsfw"))
        prefer_tags = {"nsfw"} if enable_nsfw else None

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
                        return_share_url=return_share_url,
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
        return_share_url: bool = False,
    ) -> ImageGenerationResult:
        per_call = min(max(1, n), 2)
        calls_needed = max(1, int(math.ceil(n / per_call)))

        async def _call_generate(call_target: int) -> AppChatImageCollectPayload:
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
            processor = ImageAppChatCollectProcessor(
                model_info.model_id,
                token,
                response_format=response_format,
            )
            return await processor.process(response)

        if calls_needed == 1:
            payload = await _call_generate(n)
            all_images = payload.images
            share_conversation_id = payload.conversation_id
            share_response_id = payload.response_id
        else:
            tasks = []
            for i in range(calls_needed):
                remaining = n - (i * per_call)
                tasks.append(_call_generate(min(per_call, remaining)))
            results = await asyncio.gather(*tasks, return_exceptions=True)
            all_images: List[str] = []
            share_conversation_id = ""
            share_response_id = ""
            last_error: Optional[Exception] = None
            rate_limit_error: Optional[Exception] = None
            for result in results:
                if isinstance(result, Exception):
                    logger.warning(f"Concurrent app-chat image call failed: {result}")
                    last_error = result
                    if rate_limited(result):
                        rate_limit_error = result
                    continue
                if (
                    not share_conversation_id
                    and result.conversation_id
                    and result.response_id
                ):
                    share_conversation_id = result.conversation_id
                    share_response_id = result.response_id
                for image in result.images:
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
        share_url = ""
        share_image_url = ""
        share_image_source = ""
        share_image_expires_at = ""
        if return_share_url and share_conversation_id and share_response_id:
            share_url = await _create_image_share_link(
                token,
                share_conversation_id,
                share_response_id,
            )
            if share_url:
                (
                    share_image_url,
                    share_image_source,
                    share_image_expires_at,
                ) = await _resolve_share_image_details(share_url)
        usage_override = {
            "total_tokens": 0,
            "input_tokens": 0,
            "output_tokens": 0,
            "input_tokens_details": {"text_tokens": 0, "image_tokens": 0},
        }
        return ImageGenerationResult(
            stream=False,
            data=selected,
            usage_override=usage_override,
            share_url=share_url,
            share_image_url=share_image_url,
            share_image_source=share_image_source,
            share_image_expires_at=share_image_expires_at,
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
