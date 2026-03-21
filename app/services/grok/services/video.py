"""
Grok video generation service.
"""

import asyncio
import math
import re
import time
import uuid
from dataclasses import dataclass, field
from typing import Any, AsyncGenerator, AsyncIterable, Dict, List, Optional, Tuple

import orjson
from curl_cffi.requests.errors import RequestsError

from app.core.config import get_config
from app.core.exceptions import (
    AppException,
    ErrorType,
    StreamIdleTimeoutError,
    UpstreamException,
    ValidationException,
)
from app.core.logger import logger
from app.services.grok.services.model import ModelService
from app.services.grok.utils.download import DownloadService
from app.services.grok.utils.process import _is_http2_error, _normalize_line, _with_idle_timeout
from app.services.grok.utils.retry import rate_limited
from app.services.grok.utils.stream import wrap_stream_with_usage
from app.services.reverse.app_chat import AppChatReverse
from app.services.reverse.media_post import MediaPostReverse
from app.services.reverse.media_post_link import MediaPostLinkReverse
from app.services.reverse.utils.session import ResettableSession
from app.services.reverse.video_upscale import VideoUpscaleReverse
from app.services.token import EffortType, get_token_manager
from app.services.token.manager import BASIC_POOL_NAME

async def _generate_continuation_prompt(original_prompt: str, round_index: int, token: str) -> str:
    """Use local Grok API to generate a unique continuation prompt for each extension round."""
    try:
        from curl_cffi.requests import AsyncSession
        api_key = get_config("app.api_key")
        headers = {"Content-Type": "application/json"}
        if api_key:
            headers["Authorization"] = f"Bearer {api_key}"
        system_msg = (
            f"You are a video director. The original video prompt is: '{original_prompt}'. "
            f"Generate a SHORT continuation prompt for extension round {round_index} that naturally "
            f"continues the scene with new action or movement. "
            f"Return ONLY the prompt text, no explanation, max 20 words."
        )
        payload = {
            "model": "grok-3-fast",
            "messages": [{"role": "user", "content": system_msg}],
            "temperature": 0.8,
            "max_tokens": 60,
            "stream": False,
        }
        async with AsyncSession() as session:
            resp = await session.post(
                "http://localhost:8000/v1/chat/completions",
                headers=headers,
                json=payload,
                timeout=15,
            )
            if resp.status_code == 200:
                data = resp.json()
                content = data.get("choices", [{}])[0].get("message", {}).get("content", "").strip()
                if content:
                    logger.info(f"LLM continuation prompt (round {round_index}): {content}")
                    return content
    except Exception as e:
        logger.warning(f"LLM prompt generation failed (round {round_index}): {e}")
    return original_prompt

_VIDEO_SEMAPHORE = None
_VIDEO_SEM_VALUE = 0
_APP_CHAT_MODEL = "grok-3"
_POST_ID_URL_PATTERN = r"/generated/([0-9a-fA-F-]{32,36})/"


@dataclass(frozen=True)
class VideoRoundPlan:
    round_index: int
    total_rounds: int
    is_extension: bool
    video_length: int
    extension_start_time: Optional[float] = None


@dataclass
class VideoRoundResult:
    response_id: str = ""
    post_id: Optional[str] = None
    post_id_rank: int = 999
    video_url: str = ""
    thumbnail_url: str = ""
    last_progress: Any = None
    saw_video_event: bool = False
    stream_errors: List[Any] = field(default_factory=list)


def _pick_str(value: Any) -> str:
    if isinstance(value, str):
        return value.strip()
    return ""


def _extract_post_id_from_video_url(video_url: str) -> Optional[str]:
    if not isinstance(video_url, str) or not video_url:
        return None
    match = re.search(_POST_ID_URL_PATTERN, video_url)
    if match:
        return match.group(1)
    return None


def _extract_video_id(video_url: str) -> str:
    if not video_url:
        return ""
    match = re.search(_POST_ID_URL_PATTERN, video_url)
    if match:
        return match.group(1)
    match = re.search(r"/([0-9a-fA-F-]{32,36})/generated_video", video_url)
    if match:
        return match.group(1)
    return ""


def _public_asset_enabled() -> bool:
    return bool(get_config("video.enable_public_asset", False))


async def _create_public_video_link(token: str, video_url: str) -> str:
    if not video_url or not _public_asset_enabled():
        return video_url

    video_id = _extract_video_id(video_url)
    if not video_id:
        logger.warning("Video public link skipped: unable to extract video id")
        return video_url

    try:
        async with _new_session() as session:
            response = await MediaPostLinkReverse.request(session, token, video_id)
        payload = response.json() if response is not None else {}
        share_link = _pick_str(payload.get("shareLink")) if isinstance(payload, dict) else ""
        if share_link:
            if share_link.endswith(".mp4"):
                logger.info(f"Video public link created: {share_link}")
                return share_link
            public_url = f"https://imagine-public.x.ai/imagine-public/share-videos/{video_id}.mp4?cache=1"
            logger.info(f"Video public link created: {public_url}")
            return public_url
    except Exception as e:
        logger.warning(f"Video public link failed: {e}")

    return video_url


def _build_mode_flag(preset: str) -> str:
    mode_map = {
        "fun": "--mode=extremely-crazy",
        "normal": "--mode=normal",
        "spicy": "--mode=extremely-spicy-or-crazy",
        "custom": "--mode=custom",
    }
    return mode_map.get(preset, "--mode=custom")


def _build_message(prompt: str, preset: str) -> str:
    return f"{prompt} {_build_mode_flag(preset)}".strip()


def _build_base_config(
    parent_post_id: str,
    aspect_ratio: str,
    resolution_name: str,
    video_length: int,
) -> Dict[str, Any]:
    return {
        "modelMap": {
            "videoGenModelConfig": {
                "aspectRatio": aspect_ratio,
                "parentPostId": parent_post_id,
                "resolutionName": resolution_name,
                "videoLength": video_length,
            }
        }
    }


def _build_extension_config(
    *,
    parent_post_id: str,
    extend_post_id: str,
    original_post_id: str,
    original_prompt: str,
    aspect_ratio: str,
    resolution_name: str,
    video_length: int,
    start_time: float,
) -> Dict[str, Any]:
    return {
        "modelMap": {
            "videoGenModelConfig": {
                "isVideoExtension": True,
                "videoExtensionStartTime": float(start_time),
                "extendPostId": extend_post_id,
                "stitchWithExtendPostId": True,
                "originalPrompt": original_prompt,
                "originalPostId": original_post_id,
                "originalRefType": "ORIGINAL_REF_TYPE_VIDEO_EXTENSION",
                "mode": "custom",
                "aspectRatio": aspect_ratio,
                "videoLength": video_length,
                "resolutionName": resolution_name,
                "parentPostId": parent_post_id,
                "isVideoEdit": False,
            }
        }
    }


def _choose_round_length(target_length: int, *, is_super: bool) -> int:
    if not is_super:
        return 6
    return 10 if target_length >= 10 else 6


def _build_round_plan(target_length: int, *, is_super: bool) -> List[VideoRoundPlan]:
    x = _choose_round_length(target_length, is_super=is_super)
    ext_rounds = int(math.ceil(max(target_length - x, 0) / x))
    total_rounds = 1 + ext_rounds

    plan: List[VideoRoundPlan] = [
        VideoRoundPlan(
            round_index=1,
            total_rounds=total_rounds,
            is_extension=False,
            video_length=x,
            extension_start_time=None,
        )
    ]

    for i in range(1, ext_rounds + 1):
        round_target = min(target_length, x * (i + 1))
        start_time = float(round_target - x)
        plan.append(
            VideoRoundPlan(
                round_index=i + 1,
                total_rounds=total_rounds,
                is_extension=True,
                video_length=x,
                extension_start_time=start_time,
            )
        )

    return plan


def _build_round_config(
    plan: VideoRoundPlan,
    *,
    seed_post_id: str,
    last_post_id: str,
    original_post_id: Optional[str],
    prompt: str,
    aspect_ratio: str,
    resolution_name: str,
) -> Dict[str, Any]:
    if not plan.is_extension:
        return _build_base_config(
            seed_post_id,
            aspect_ratio,
            resolution_name,
            plan.video_length,
        )

    if not original_post_id:
        raise UpstreamException(
            message="Video extension missing original_post_id",
            status_code=502,
            details={"type": "missing_post_id", "round": plan.round_index},
        )

    return _build_extension_config(
        parent_post_id=last_post_id,
        extend_post_id=last_post_id,
        original_post_id=original_post_id,
        original_prompt=prompt,
        aspect_ratio=aspect_ratio,
        resolution_name=resolution_name,
        video_length=plan.video_length,
        start_time=float(plan.extension_start_time or 0.0),
    )


def _append_unique_errors(bucket: List[Any], raw_errors: Any):
    if raw_errors is None:
        return

    items = raw_errors if isinstance(raw_errors, list) else [raw_errors]
    for item in items:
        if item is None:
            continue
        text = item if isinstance(item, str) else str(item)
        if text and text not in bucket:
            bucket.append(text)


def _extract_post_id_candidates(resp: Dict[str, Any]) -> List[Tuple[int, str]]:
    candidates: List[Tuple[int, str]] = []

    model_resp = resp.get("modelResponse")
    if isinstance(model_resp, dict):
        file_attachments = model_resp.get("fileAttachments")
        if isinstance(file_attachments, list) and file_attachments:
            first = _pick_str(file_attachments[0])
            if first:
                candidates.append((1, first))

    video_resp = resp.get("streamingVideoGenerationResponse")
    if isinstance(video_resp, dict):
        value = _pick_str(video_resp.get("videoPostId"))
        if value:
            candidates.append((2, value))
        value = _pick_str(video_resp.get("postId"))
        if value:
            candidates.append((3, value))

    post = resp.get("post")
    if isinstance(post, dict):
        value = _pick_str(post.get("id"))
        if value:
            candidates.append((4, value))

    for key in ("postId", "post_id", "parentPostId", "originalPostId"):
        value = _pick_str(resp.get(key))
        if value:
            candidates.append((5, value))

    return candidates


def _apply_post_id_candidates(result: VideoRoundResult, candidates: List[Tuple[int, str]]):
    for rank, value in candidates:
        if rank < result.post_id_rank:
            result.post_id_rank = rank
            result.post_id = value


async def _close_stream_resource(obj: Any):
    if obj is None:
        return

    aclose = getattr(obj, "aclose", None)
    if callable(aclose):
        try:
            await aclose()
        except Exception:
            pass

    close = getattr(obj, "close", None)
    if callable(close):
        try:
            close()
        except Exception:
            pass


async def _iter_round_events(
    response: AsyncIterable[bytes],
    *,
    model: str,
    source: str,
) -> AsyncGenerator[Tuple[str, Any], None]:
    result = VideoRoundResult()
    idle_timeout = float(get_config("video.stream_timeout") or 60)
    chunk_index = 0

    iterator = None
    try:
        iterator = _with_idle_timeout(response, idle_timeout, model)
        async for raw_line in iterator:
            line = _normalize_line(raw_line)
            if not line:
                continue

            chunk_index += 1
            try:
                payload = orjson.loads(line)
            except orjson.JSONDecodeError:
                continue

            root = payload.get("result") if isinstance(payload, dict) else None
            resp = root.get("response") if isinstance(root, dict) else None
            if not isinstance(resp, dict):
                continue

            response_id = _pick_str(resp.get("responseId"))
            if response_id:
                result.response_id = response_id

            _append_unique_errors(result.stream_errors, resp.get("streamErrors"))

            model_resp = resp.get("modelResponse")
            if isinstance(model_resp, dict):
                rid = _pick_str(model_resp.get("responseId"))
                if rid:
                    result.response_id = rid
                _append_unique_errors(result.stream_errors, model_resp.get("streamErrors"))

            _apply_post_id_candidates(result, _extract_post_id_candidates(resp))

            video_resp = resp.get("streamingVideoGenerationResponse")
            progress = None
            if isinstance(video_resp, dict):
                result.saw_video_event = True
                progress = video_resp.get("progress")
                result.last_progress = progress

                url = _pick_str(video_resp.get("videoUrl"))
                if url:
                    result.video_url = url

                thumbnail = _pick_str(video_resp.get("thumbnailImageUrl"))
                if thumbnail:
                    result.thumbnail_url = thumbnail

            if not result.post_id and result.video_url:
                result.post_id = _extract_post_id_from_video_url(result.video_url)
                if result.post_id:
                    result.post_id_rank = 6

            if progress is not None:
                yield "progress", progress

        if not result.post_id and result.video_url:
            result.post_id = _extract_post_id_from_video_url(result.video_url)
            if result.post_id:
                result.post_id_rank = 6

        yield "done", result
    except StreamIdleTimeoutError as e:
        raise UpstreamException(
            message=f"Video stream idle timeout after {e.idle_seconds}s",
            status_code=504,
            details={
                "type": "stream_idle_timeout",
                "source": source,
                "idle_seconds": e.idle_seconds,
                "error": str(e),
            },
        )
    except RequestsError as e:
        if _is_http2_error(e):
            raise UpstreamException(
                message="Upstream connection closed unexpectedly",
                status_code=502,
                details={
                    "type": "http2_stream_error",
                    "source": source,
                    "error": str(e),
                },
            )
        raise UpstreamException(
            message=f"Upstream request failed: {e}",
            status_code=502,
            details={
                "type": "upstream_request_failed",
                "source": source,
                "error": str(e),
            },
        )
    finally:
        await _close_stream_resource(iterator)
        await _close_stream_resource(response)


async def _collect_round_result(
    response: AsyncIterable[bytes],
    *,
    model: str,
    source: str,
) -> VideoRoundResult:
    result = VideoRoundResult()
    async for event_type, payload in _iter_round_events(response, model=model, source=source):
        if event_type == "done":
            result = payload
    return result


def _round_error_details(
    result: VideoRoundResult,
    *,
    err_type: str,
    round_index: int,
    total_rounds: int,
) -> Dict[str, Any]:
    return {
        "type": err_type,
        "round": round_index,
        "total_rounds": total_rounds,
        "response_id": result.response_id,
        "last_progress": result.last_progress,
        "stream_errors": result.stream_errors,
    }


def _ensure_round_result(
    result: VideoRoundResult,
    *,
    round_index: int,
    total_rounds: int,
    final_round: bool,
):
    if not result.post_id:
        err_type = "moderated_or_stream_errors" if result.stream_errors else "missing_post_id"
        raise UpstreamException(
            message=f"Video round {round_index}/{total_rounds} missing post_id",
            status_code=502,
            details=_round_error_details(
                result,
                err_type=err_type,
                round_index=round_index,
                total_rounds=total_rounds,
            ),
        )

    if not final_round:
        return

    if result.video_url:
        return

    if result.stream_errors:
        err_type = "moderated_or_stream_errors"
    elif result.saw_video_event:
        err_type = "missing_video_url"
    else:
        err_type = "empty_video_stream"

    raise UpstreamException(
        message=f"Video round {round_index}/{total_rounds} missing final video_url",
        status_code=502,
        details=_round_error_details(
            result,
            err_type=err_type,
            round_index=round_index,
            total_rounds=total_rounds,
        ),
    )


def _format_progress(value: Any) -> str:
    if isinstance(value, bool):
        return str(int(value))
    if isinstance(value, int):
        return str(value)
    if isinstance(value, float):
        if value.is_integer():
            return str(int(value))
        return f"{value:.2f}".rstrip("0").rstrip(".")
    if isinstance(value, str):
        stripped = value.strip()
        if stripped:
            return stripped
    return str(value)


def _get_video_semaphore() -> asyncio.Semaphore:
    """Reverse 接口并发控制（video 服务）。"""
    global _VIDEO_SEMAPHORE, _VIDEO_SEM_VALUE
    value = max(1, int(get_config("video.concurrent")))
    if value != _VIDEO_SEM_VALUE:
        _VIDEO_SEM_VALUE = value
        _VIDEO_SEMAPHORE = asyncio.Semaphore(value)
    return _VIDEO_SEMAPHORE


def _new_session() -> ResettableSession:
    browser = get_config("proxy.browser")
    if browser:
        return ResettableSession(impersonate=browser)
    return ResettableSession()


async def _request_round_stream(
    *,
    token: str,
    message: str,
    model_config_override: Dict[str, Any],
) -> AsyncGenerator[bytes, None]:
    async def _stream():
        session = _new_session()
        try:
            async with _get_video_semaphore():
                stream_response = await AppChatReverse.request(
                    session,
                    token,
                    message=message,
                    model=_APP_CHAT_MODEL,
                    tool_overrides={"videoGen": True},
                    model_config_override=model_config_override,
                )
                async for line in stream_response:
                    yield line
        finally:
            try:
                await session.close()
            except Exception:
                pass

    return _stream()


async def _upscale_video_url(token: str, video_url: str) -> Tuple[str, bool]:
    """
    Returns:
        (url, upscaled)
    """
    video_id = _extract_video_id(video_url)
    if not video_id:
        logger.warning("Video upscale skipped: unable to extract video id")
        return video_url, False

    try:
        async with _new_session() as session:
            response = await VideoUpscaleReverse.request(session, token, video_id)
        payload = response.json() if response is not None else {}
        hd_url = payload.get("hdMediaUrl") if isinstance(payload, dict) else None
        hd_url = _pick_str(hd_url)
        if hd_url:
            logger.info(f"Video upscale completed: {hd_url}")
            return hd_url, True
    except Exception as e:
        logger.warning(f"Video upscale failed: {e}")

    return video_url, False


def _resolve_upscale_timing() -> str:
    raw = get_config("video.upscale_timing", "complete")
    value = str(raw or "complete").strip().lower()
    if value in {"single", "complete"}:
        return value
    logger.warning(f"Invalid video.upscale_timing={raw!r}, fallback to 'complete'")
    return "complete"


class _VideoChainSSEWriter:
    def __init__(self, model: str, show_think: bool):
        self.model = model
        self.show_think = bool(show_think)
        self.created = int(time.time())
        self.response_id = f"chatcmpl-{uuid.uuid4().hex[:24]}"
        self.role_sent = False
        self.think_opened = False

    def _sse(self, content: str = "", role: str = None, finish: str = None) -> str:
        delta: Dict[str, Any] = {}
        if role:
            delta["role"] = role
            delta["content"] = ""
        elif content:
            delta["content"] = content

        chunk = {
            "id": self.response_id,
            "object": "chat.completion.chunk",
            "created": self.created,
            "model": self.model,
            "choices": [
                {
                    "index": 0,
                    "delta": delta,
                    "logprobs": None,
                    "finish_reason": finish,
                }
            ],
        }
        return f"data: {orjson.dumps(chunk).decode()}\n\n"

    def ensure_role(self) -> List[str]:
        if self.role_sent:
            return []
        self.role_sent = True
        return [self._sse(role="assistant")]

    def emit_progress(self, *, round_index: int, total_rounds: int, progress: Any) -> List[str]:
        if not self.show_think:
            return []

        chunks = self.ensure_role()
        if not self.think_opened:
            self.think_opened = True
            chunks.append(self._sse("<think>\n"))

        progress_text = _format_progress(progress)
        chunks.append(
            self._sse(f"[round={round_index}/{total_rounds}] progress={progress_text}%\n")
        )
        return chunks

    def emit_note(self, text: str) -> List[str]:
        if not self.show_think:
            return []

        chunks = self.ensure_role()
        if not self.think_opened:
            self.think_opened = True
            chunks.append(self._sse("<think>\n"))
        chunks.append(self._sse(text))
        return chunks

    def emit_content(self, text: str) -> List[str]:
        chunks = self.ensure_role()
        if self.think_opened:
            self.think_opened = False
            chunks.append(self._sse("\n</think>\n"))
        if text:
            chunks.append(self._sse(text))
        return chunks

    def finish(self) -> List[str]:
        chunks = self.ensure_role()
        if self.think_opened:
            self.think_opened = False
            chunks.append(self._sse("\n</think>\n"))
        chunks.append(self._sse(finish="stop"))
        chunks.append("data: [DONE]\n\n")
        return chunks


class VideoService:
    """Video generation service."""

    async def create_post(
        self,
        token: str,
        prompt: str,
        media_type: str = "MEDIA_POST_TYPE_VIDEO",
        media_url: str = None,
    ) -> str:
        """Create media post and return post ID."""
        try:
            if media_type == "MEDIA_POST_TYPE_IMAGE" and not media_url:
                raise ValidationException("media_url is required for image posts")

            prompt_value = prompt if media_type == "MEDIA_POST_TYPE_VIDEO" else ""
            media_value = media_url or ""

            async with _new_session() as session:
                async with _get_video_semaphore():
                    response = await MediaPostReverse.request(
                        session,
                        token,
                        media_type,
                        media_value,
                        prompt=prompt_value,
                    )

            post_id = _pick_str(response.json().get("post", {}).get("id"))
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
        return await self.create_post(
            token, prompt="", media_type="MEDIA_POST_TYPE_IMAGE", media_url=image_url
        )

    async def generate(
        self,
        token: str,
        prompt: str,
        aspect_ratio: str = "3:2",
        video_length: int = 6,
        resolution_name: str = "480p",
        preset: str = "normal",
    ) -> AsyncGenerator[bytes, None]:
        """Single-round video generation stream."""
        post_id = await self.create_post(token, prompt)
        model_config_override = _build_base_config(
            post_id,
            aspect_ratio,
            resolution_name,
            video_length,
        )
        return await _request_round_stream(
            token=token,
            message=_build_message(prompt, preset),
            model_config_override=model_config_override,
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
        """Single-round image-to-video generation stream."""
        post_id = await self.create_image_post(token, image_url)
        model_config_override = _build_base_config(
            post_id,
            aspect_ratio,
            resolution,
            video_length,
        )
        return await _request_round_stream(
            token=token,
            message=_build_message(prompt, preset),
            model_config_override=model_config_override,
        )

    @staticmethod
    async def completions(
        model: str,
        messages: list,
        stream: bool = None,
        reasoning_effort: str | None = None,
        aspect_ratio: str = "3:2",
        video_length: int = 6,
        resolution: str = "480p",
        preset: str = "normal",
    ):
        token_mgr = await get_token_manager()
        await token_mgr.reload_if_stale()

        is_stream = stream if stream is not None else get_config("app.stream")
        if reasoning_effort is None:
            show_think = bool(get_config("app.thinking"))
        else:
            show_think = reasoning_effort != "none"

        from app.services.grok.services.chat import MessageExtractor
        from app.services.grok.utils.upload import UploadService

        prompt, _, image_attachments = MessageExtractor.extract(messages)

        pool_candidates = ModelService.pool_candidates_for_model(model)
        token_info = token_mgr.get_token_for_video(
            resolution=resolution,
            video_length=video_length,
            pool_candidates=pool_candidates,
        )

        if not token_info:
            raise AppException(
                message="No available tokens. Please try again later.",
                error_type=ErrorType.RATE_LIMIT.value,
                code="rate_limit_exceeded",
                status_code=429,
            )

        token = token_info.token
        if token.startswith("sso="):
            token = token[4:]

        pool_name = token_mgr.get_pool_name_for_token(token) or BASIC_POOL_NAME
        is_super_pool = pool_name != BASIC_POOL_NAME

        requested_resolution = resolution
        should_upscale = requested_resolution == "720p" and pool_name == BASIC_POOL_NAME
        generation_resolution = "480p" if should_upscale else requested_resolution
        upscale_timing = _resolve_upscale_timing() if should_upscale else "complete"

        target_length = int(video_length or 6)
        round_plan = _build_round_plan(target_length, is_super=is_super_pool)
        total_rounds = len(round_plan)

        service = VideoService()
        message = _build_message(prompt, preset)

        image_url = None
        if image_attachments:
            upload_service = UploadService()
            try:
                if len(image_attachments) > 1:
                    logger.info(
                        "Video generation supports a single reference image; using the first one."
                    )
                attach_data = image_attachments[0]
                _, file_uri = await upload_service.upload_file(attach_data, token)
                image_url = f"https://assets.grok.com/{file_uri}"
                logger.info(f"Image uploaded for video: {image_url}")
            finally:
                await upload_service.close()

        if image_url:
            seed_post_id = await service.create_image_post(token, image_url)
        else:
            seed_post_id = await service.create_post(token, prompt)

        model_info = ModelService.get(model)
        effort = (
            EffortType.HIGH
            if (model_info and model_info.cost.value == "high")
            else EffortType.LOW
        )

        async def _run_round_collect(
            plan: VideoRoundPlan,
            *,
            seed_id: str,
            last_id: str,
            original_id: Optional[str],
            source: str,
            round_prompt: str = None,
        ) -> VideoRoundResult:
            rp = round_prompt if round_prompt is not None else prompt
            config_override = _build_round_config(
                plan,
                seed_post_id=seed_id,
                last_post_id=last_id,
                original_post_id=original_id,
                prompt=rp,
                aspect_ratio=aspect_ratio,
                resolution_name=generation_resolution,
            )
            response = await _request_round_stream(
                token=token,
                message=_build_message(rp, preset),
                model_config_override=config_override,
            )
            return await _collect_round_result(response, model=model, source=source)

        async def _stream_chain() -> AsyncGenerator[str, None]:
            writer = _VideoChainSSEWriter(model, show_think)
            seed_id = seed_post_id
            last_id = seed_id
            original_id: Optional[str] = seed_id
            final_result: Optional[VideoRoundResult] = None

            try:
                for plan in round_plan:
                    round_prompt = prompt
                    if plan.is_extension:
                        round_prompt = await _generate_continuation_prompt(prompt, plan.round_index, token)
                    config_override = _build_round_config(
                        plan,
                        seed_post_id=seed_id,
                        last_post_id=last_id,
                        original_post_id=original_id,
                        prompt=round_prompt,
                        aspect_ratio=aspect_ratio,
                        resolution_name=generation_resolution,
                    )
                    response = await _request_round_stream(
                        token=token,
                        message=_build_message(round_prompt, preset),
                        model_config_override=config_override,
                    )

                    round_result = VideoRoundResult()
                    async for event_type, payload in _iter_round_events(
                        response,
                        model=model,
                        source=f"stream-round-{plan.round_index}",
                    ):                        if event_type == "progress":
                            for chunk in writer.emit_progress(
                                round_index=plan.round_index,
                                total_rounds=plan.total_rounds,
                                progress=payload,
                            ):
                                yield chunk
                        elif event_type == "done":
                            round_result = payload

                    _ensure_round_result(
                        round_result,
                        round_index=plan.round_index,
                        total_rounds=plan.total_rounds,
                        final_round=(plan.round_index == plan.total_rounds),
                    )

                    if should_upscale and upscale_timing == "single" and round_result.video_url:
                        for chunk in writer.emit_note(
                            f"[round={plan.round_index}/{plan.total_rounds}] 正在对当前轮结果进行超分辨率\n"
                        ):
                            yield chunk
                        upgraded_url, upscaled = await _upscale_video_url(
                            token, round_result.video_url
                        )
                        if upscaled:
                            round_result.video_url = upgraded_url
                        else:
                            logger.warning(
                                "Video upscale failed in single mode, fallback to 480p result"
                            )

                    if plan.round_index == 1 and round_result.post_id:
                        original_id = round_result.post_id
                    if round_result.post_id:
                        last_id = round_result.post_id

                    if plan.round_index == plan.total_rounds:
                        final_result = round_result

                if final_result is None:
                    raise UpstreamException(
                        message="Video generation produced no final round",
                        status_code=502,
                        details={"type": "empty_video_stream"},
                    )

                final_video_url = final_result.video_url
                if should_upscale and upscale_timing == "complete":
                    for chunk in writer.emit_note("正在对视频进行超分辨率\n"):
                        yield chunk
                    final_video_url, upscaled = await _upscale_video_url(token, final_video_url)
                    if not upscaled:
                        logger.warning("Video upscale failed, fallback to 480p result")

                if _public_asset_enabled():
                    for chunk in writer.emit_note("正在生成可公开访问链接\n"):
                        yield chunk
                    final_video_url = await _create_public_video_link(token, final_video_url)

                dl_service = DownloadService()
                try:
                    rendered = await dl_service.render_video(
                        final_video_url,
                        token,
                        final_result.thumbnail_url,
                    )
                finally:
                    await dl_service.close()

                for chunk in writer.emit_content(rendered):
                    yield chunk
                for chunk in writer.finish():
                    yield chunk
            except asyncio.CancelledError:
                logger.debug("Video stream chain cancelled by client", extra={"model": model})
                raise
            except UpstreamException as e:
                if rate_limited(e):
                    await token_mgr.mark_rate_limited(token)
                raise

        async def _collect_chain() -> Dict[str, Any]:
            seed_id = seed_post_id
            last_id = seed_id
            original_id: Optional[str] = seed_id
            final_result: Optional[VideoRoundResult] = None

            for plan in round_plan:
                round_prompt = prompt
                if plan.is_extension:
                    round_prompt = await _generate_continuation_prompt(prompt, plan.round_index, token)
                round_result = await _run_round_collect(
                    plan,
                    seed_id=seed_id,
                    last_id=last_id,
                    original_id=original_id,
                    source=f"collect-round-{plan.round_index}",
                    round_prompt=round_prompt,
                )

                _ensure_round_result(
                    round_result,
                    round_index=plan.round_index,
                    total_rounds=plan.total_rounds,
                    final_round=(plan.round_index == plan.total_rounds),
                )

                if should_upscale and upscale_timing == "single" and round_result.video_url:
                    upgraded_url, upscaled = await _upscale_video_url(
                        token, round_result.video_url
                    )
                    if upscaled:
                        round_result.video_url = upgraded_url
                    else:
                        logger.warning(
                            "Video upscale failed in single mode, fallback to 480p result"
                        )

                if plan.round_index == 1 and round_result.post_id:
                    original_id = round_result.post_id
                if round_result.post_id:
                    last_id = round_result.post_id

                if plan.round_index == plan.total_rounds:
                    final_result = round_result

            if final_result is None:
                raise UpstreamException(
                    message="Video generation produced no final round",
                    status_code=502,
                    details={"type": "empty_video_stream"},
                )

            final_video_url = final_result.video_url
            if should_upscale and upscale_timing == "complete":
                final_video_url, upscaled = await _upscale_video_url(token, final_video_url)
                if not upscaled:
                    logger.warning("Video upscale failed, fallback to 480p result")

            if _public_asset_enabled():
                final_video_url = await _create_public_video_link(token, final_video_url)

            dl_service = DownloadService()
            try:
                content = await dl_service.render_video(
                    final_video_url,
                    token,
                    final_result.thumbnail_url,
                )
            finally:
                await dl_service.close()

            return {
                "id": final_result.response_id,
                "object": "chat.completion",
                "created": int(time.time()),
                "model": model,
                "choices": [
                    {
                        "index": 0,
                        "message": {
                            "role": "assistant",
                            "content": content,
                            "refusal": None,
                        },
                        "finish_reason": "stop",
                    }
                ],
                "usage": {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
            }

        if is_stream:
            return wrap_stream_with_usage(_stream_chain(), token_mgr, token, model)

        try:
            result = await _collect_chain()
        except UpstreamException as e:
            if rate_limited(e):
                await token_mgr.mark_rate_limited(token)
            raise

        try:
            await token_mgr.consume(token, effort)
            logger.debug(
                f"Video completed, recorded usage (effort={effort.value})"
            )
        except Exception as e:
            logger.warning(f"Failed to record video usage: {e}")

        return result


class VideoStreamProcessor:
    """Single-round video stream response processor."""

    def __init__(
        self,
        model: str,
        token: str = "",
        show_think: bool = None,
        upscale_on_finish: bool = False,
        round_index: int = 1,
        round_total: int = 1,
    ):
        self.model = model
        self.token = token
        self.show_think = bool(show_think)
        self.upscale_on_finish = bool(upscale_on_finish)
        self.enable_public_asset = _public_asset_enabled()
        self.round_index = max(1, int(round_index or 1))
        self.round_total = max(self.round_index, int(round_total or self.round_index))

        self.writer = _VideoChainSSEWriter(model, self.show_think)
        self._dl_service: Optional[DownloadService] = None

    @property
    def role_sent(self) -> bool:
        return self.writer.role_sent

    @role_sent.setter
    def role_sent(self, value: bool):
        self.writer.role_sent = bool(value)

    @property
    def think_opened(self) -> bool:
        return self.writer.think_opened

    @think_opened.setter
    def think_opened(self, value: bool):
        self.writer.think_opened = bool(value)

    def _get_dl(self) -> DownloadService:
        if self._dl_service is None:
            self._dl_service = DownloadService()
        return self._dl_service

    async def close(self):
        if self._dl_service:
            await self._dl_service.close()
            self._dl_service = None

    async def process(self, response: AsyncIterable[bytes]) -> AsyncGenerator[str, None]:
        result = VideoRoundResult()
        try:
            async for event_type, payload in _iter_round_events(
                response,
                model=self.model,
                source=f"single-stream-round-{self.round_index}",
            ):
                if event_type == "progress":
                    for chunk in self.writer.emit_progress(
                        round_index=self.round_index,
                        total_rounds=self.round_total,
                        progress=payload,
                    ):
                        yield chunk
                elif event_type == "done":
                    result = payload

            _ensure_round_result(
                result,
                round_index=self.round_index,
                total_rounds=self.round_total,
                final_round=True,
            )

            final_video_url = result.video_url
            if self.upscale_on_finish:
                for chunk in self.writer.emit_note("正在对视频进行超分辨率\n"):
                    yield chunk
                final_video_url, upscaled = await _upscale_video_url(self.token, final_video_url)
                if not upscaled:
                    logger.warning("Video upscale failed, fallback to 480p result")

            if self.enable_public_asset:
                for chunk in self.writer.emit_note("正在生成可公开访问链接\n"):
                    yield chunk
                final_video_url = await _create_public_video_link(self.token, final_video_url)

            rendered = await self._get_dl().render_video(
                final_video_url,
                self.token,
                result.thumbnail_url,
            )
            for chunk in self.writer.emit_content(rendered):
                yield chunk
            for chunk in self.writer.finish():
                yield chunk
        except asyncio.CancelledError:
            logger.debug("Video stream cancelled by client", extra={"model": self.model})
            raise
        finally:
            await self.close()


class VideoCollectProcessor:
    """Single-round non-stream video response processor."""

    def __init__(
        self,
        model: str,
        token: str = "",
        upscale_on_finish: bool = False,
        round_index: int = 1,
        round_total: int = 1,
    ):
        self.model = model
        self.token = token
        self.upscale_on_finish = bool(upscale_on_finish)
        self.enable_public_asset = _public_asset_enabled()
        self.round_index = max(1, int(round_index or 1))
        self.round_total = max(self.round_index, int(round_total or self.round_index))
        self._dl_service: Optional[DownloadService] = None

    def _get_dl(self) -> DownloadService:
        if self._dl_service is None:
            self._dl_service = DownloadService()
        return self._dl_service

    async def close(self):
        if self._dl_service:
            await self._dl_service.close()
            self._dl_service = None

    async def process(self, response: AsyncIterable[bytes]) -> Dict[str, Any]:
        try:
            result = await _collect_round_result(
                response,
                model=self.model,
                source=f"single-collect-round-{self.round_index}",
            )

            _ensure_round_result(
                result,
                round_index=self.round_index,
                total_rounds=self.round_total,
                final_round=True,
            )

            final_video_url = result.video_url
            if self.upscale_on_finish:
                final_video_url, upscaled = await _upscale_video_url(self.token, final_video_url)
                if not upscaled:
                    logger.warning("Video upscale failed, fallback to 480p result")

            if self.enable_public_asset:
                final_video_url = await _create_public_video_link(self.token, final_video_url)

            content = await self._get_dl().render_video(
                final_video_url,
                self.token,
                result.thumbnail_url,
            )

            return {
                "id": result.response_id,
                "object": "chat.completion",
                "created": int(time.time()),
                "model": self.model,
                "choices": [
                    {
                        "index": 0,
                        "message": {
                            "role": "assistant",
                            "content": content,
                            "refusal": None,
                        },
                        "finish_reason": "stop",
                    }
                ],
                "usage": {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
            }
        finally:
            await self.close()


__all__ = ["VideoService", "VideoStreamProcessor", "VideoCollectProcessor"]
