"""Video generation service.

Supports:
  - OpenAI-style async ``/v1/videos`` jobs
  - ``/v1/chat/completions`` video output via the same core pipeline
"""

import asyncio
import hashlib
import html
import re
import time
import uuid
from dataclasses import dataclass
from pathlib import Path
from typing import Any, AsyncGenerator, Awaitable, Callable
from urllib.parse import urlparse

import orjson

from app.platform.config.snapshot import get_config
from app.platform.errors import AppError, ErrorKind, RateLimitError, UpstreamError, ValidationError
from app.platform.logging.logger import logger
from app.platform.runtime.clock import now_s
from app.platform.storage import video_files_dir, save_media_bytes
from app.control.account.enums import FeedbackKind
from app.control.model import registry as model_registry
from app.control.model.registry import resolve as resolve_model
from app.dataplane.proxy import get_proxy_runtime
from app.dataplane.proxy.adapters.headers import build_http_headers
from app.dataplane.proxy.adapters.session import ResettableSession, build_session_kwargs
from app.dataplane.reverse.protocol.xai_assets import resolve_asset_reference, resolve_download_url
from app.dataplane.reverse.protocol.xai_chat import classify_line
from app.dataplane.reverse.runtime.endpoint_table import CHAT
from app.dataplane.reverse.transport.asset_upload import (
    resolve_uploaded_asset_reference,
    upload_from_input,
)
from app.dataplane.reverse.transport.assets import download_asset
from app.dataplane.reverse.transport.media import create_media_post
from ._format import (
    make_chat_response,
    make_response_id,
    make_stream_chunk,
    make_thinking_chunk,
)
from .chat import _fail_sync, _quota_sync, _feedback_kind

_IMAGE_MEDIA_TYPE = "MEDIA_POST_TYPE_IMAGE"
_VIDEO_MEDIA_TYPE = "MEDIA_POST_TYPE_VIDEO"
_VIDEO_MODEL_NAME = "grok-3"
_VIDEO_QUALITY = "standard"
_VIDEO_OBJECT = "video"
_VIDEO_JOB_TTL_S = 3600
_VIDEO_EXTENSION_REF_TYPE = "ORIGINAL_REF_TYPE_VIDEO_EXTENSION"
_VIDEO_MAX_REFERENCES = 7
_SUPPORTED_VIDEO_LENGTHS = frozenset({6, 10, 12, 16, 20})
_VIDEO_SIZE_MAP: dict[str, tuple[str, str]] = {
    "720x1280": ("9:16", "720p"),
    "1280x720": ("16:9", "720p"),
    "1024x1024": ("1:1", "720p"),
    "1024x1792": ("9:16", "720p"),
    "1792x1024": ("16:9", "720p"),
}
_PRESET_FLAGS = {
    "fun": "--mode=extremely-crazy",
    "normal": "--mode=normal",
    "spicy": "--mode=extremely-spicy-or-crazy",
    "custom": "--mode=custom",
}
_REFERENCE_PLACEHOLDER_RE = re.compile(r"@(?:(?:图|image|img)\s*(\d+))", re.IGNORECASE)


@dataclass(slots=True)
class _VideoArtifact:
    video_url: str
    video_post_id: str
    asset_id: str
    thumbnail_url: str
    remixed_from_video_id: str | None = None


@dataclass(slots=True)
class _VideoReference:
    asset_id: str
    content_url: str


@dataclass(slots=True)
class _VideoJob:
    id: str
    model: str
    prompt: str
    seconds: str
    size: str
    quality: str
    created_at: int
    status: str = "queued"
    progress: int = 0
    completed_at: int | None = None
    error: dict[str, Any] | None = None
    remixed_from_video_id: str | None = None
    video_url: str = ""
    content_path: str = ""

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "id": self.id,
            "object": _VIDEO_OBJECT,
            "created_at": self.created_at,
            "status": self.status,
            "model": self.model,
            "progress": self.progress,
            "prompt": self.prompt,
            "seconds": self.seconds,
            "size": self.size,
            "quality": self.quality,
        }
        if self.completed_at is not None:
            payload["completed_at"] = self.completed_at
        if self.error is not None:
            payload["error"] = self.error
        if self.remixed_from_video_id:
            payload["remixed_from_video_id"] = self.remixed_from_video_id
        return payload


_VIDEO_JOBS: dict[str, _VideoJob] = {}
_VIDEO_JOBS_LOCK = asyncio.Lock()


def _build_message(prompt: str, preset: str) -> str:
    message = f"{prompt} {_PRESET_FLAGS.get(preset, '--mode=custom')}".strip()
    return message


def _progress_reason(progress: int) -> str:
    return f"视频正在生成 {max(0, min(100, int(progress)))}%"


def _coerce_seconds(value: str | int | None) -> int:
    if value is None:
        return 6
    if isinstance(value, int):
        return value
    text = str(value).strip()
    if not text:
        return 6
    try:
        return int(text)
    except ValueError as exc:
        raise ValidationError("seconds must be an integer string", param="seconds") from exc


def validate_video_length(seconds: int) -> None:
    if seconds not in _SUPPORTED_VIDEO_LENGTHS:
        allowed = ", ".join(str(item) for item in sorted(_SUPPORTED_VIDEO_LENGTHS))
        raise ValidationError(f"seconds must be one of [{allowed}]", param="seconds")


def _resolve_video_size(size: str) -> tuple[str, str]:
    normalized = (size or "720x1280").strip()
    config = _VIDEO_SIZE_MAP.get(normalized)
    if config is None:
        allowed = ", ".join(_VIDEO_SIZE_MAP)
        raise ValidationError(f"size must be one of [{allowed}]", param="size")
    return config


def _resolve_video_resolution_name(value: str | None, *, default: str = "720p") -> str:
    normalized = (value or default).strip().lower()
    if normalized not in {"480p", "720p"}:
        raise ValidationError("resolution_name must be one of [480p, 720p]", param="resolution_name")
    return normalized


def _resolve_video_preset(value: str | None, *, default: str = "custom") -> str:
    normalized = (value or default).strip().lower()
    if normalized not in _PRESET_FLAGS:
        allowed = ", ".join(sorted(_PRESET_FLAGS))
        raise ValidationError(f"preset must be one of [{allowed}]", param="preset")
    return normalized


def _build_segment_lengths(seconds: int) -> list[int]:
    if seconds == 6:
        return [6]
    if seconds == 10:
        return [10]
    if seconds == 12:
        return [6, 6]
    if seconds == 16:
        return [10, 6]
    if seconds == 20:
        return [10, 10]
    validate_video_length(seconds)
    raise AssertionError("unreachable")


def _video_create_payload(
    *,
    prompt: str,
    parent_post_id: str,
    aspect_ratio: str,
    resolution_name: str,
    video_length: int,
    preset: str,
    image_references: list[str] | None = None,
    file_attachments: list[str] | None = None,
) -> dict[str, Any]:
    video_config: dict[str, Any] = {
        "parentPostId": parent_post_id,
        "aspectRatio": aspect_ratio,
        "videoLength": video_length,
        "resolutionName": resolution_name,
    }
    if image_references:
        video_config["imageReferences"] = list(image_references)
        video_config["isReferenceToVideo"] = True

    payload = {
        "temporary": True,
        "modelName": _VIDEO_MODEL_NAME,
        "message": _build_message(prompt, preset),
        "toolOverrides": {"videoGen": True},
        "enableSideBySide": True,
        "responseMetadata": {
            "experiments": [],
            "modelConfigOverride": {
                "modelMap": {
                    "videoGenModelConfig": video_config
                }
            },
        },
    }
    if file_attachments:
        payload["fileAttachments"] = file_attachments
    return payload


def _video_extend_start_time(seconds: int) -> float:
    return round(seconds + (1.0 / 24.0), 6)


def _video_extend_payload(
    *,
    prompt: str,
    parent_post_id: str,
    extend_post_id: str,
    aspect_ratio: str,
    resolution_name: str,
    video_length: int,
    preset: str,
    start_time_s: float,
) -> dict[str, Any]:
    return {
        "temporary": True,
        "modelName": _VIDEO_MODEL_NAME,
        "message": _build_message(prompt, preset),
        "toolOverrides": {"videoGen": True},
        "enableSideBySide": True,
        "responseMetadata": {
            "experiments": [],
            "modelConfigOverride": {
                "modelMap": {
                    "videoGenModelConfig": {
                        "isVideoExtension": True,
                        "videoExtensionStartTime": start_time_s,
                        "extendPostId": extend_post_id,
                        "stitchWithExtendPostId": True,
                        "originalPrompt": prompt,
                        "originalPostId": parent_post_id,
                        "originalRefType": _VIDEO_EXTENSION_REF_TYPE,
                        "mode": preset,
                        "aspectRatio": aspect_ratio,
                        "videoLength": video_length,
                        "resolutionName": resolution_name,
                        "parentPostId": parent_post_id,
                        "isVideoEdit": False,
                    }
                }
            },
        },
    }


def _extract_streaming_video_response(data: dict[str, Any]) -> dict[str, Any] | None:
    result = data.get("result")
    if not isinstance(result, dict):
        return None
    response = result.get("response")
    if not isinstance(response, dict):
        return None
    stream = response.get("streamingVideoGenerationResponse")
    return stream if isinstance(stream, dict) else None


def _extract_model_response_file_attachments(data: dict[str, Any]) -> list[str]:
    result = data.get("result")
    if not isinstance(result, dict):
        return []
    response = result.get("response")
    if not isinstance(response, dict):
        return []
    model_response = response.get("modelResponse")
    if not isinstance(model_response, dict):
        return []
    attachments = model_response.get("fileAttachments")
    if not isinstance(attachments, list):
        return []
    return [item for item in attachments if isinstance(item, str) and item]


async def _stream_video_request(
    token: str,
    payload: dict[str, Any],
    *,
    referer: str,
    timeout_s: float,
) -> AsyncGenerator[str, None]:
    proxy = await get_proxy_runtime()
    lease = await proxy.acquire()
    headers = build_http_headers(
        token,
        content_type="application/json",
        origin="https://grok.com",
        referer=referer,
        lease=lease,
    )
    kwargs = build_session_kwargs(lease=lease)

    async with ResettableSession(**kwargs) as session:
        response = await session.post(
            CHAT,
            headers=headers,
            data=orjson.dumps(payload),
            timeout=timeout_s,
            stream=True,
        )
        if response.status_code != 200:
            body = response.content.decode("utf-8", "replace")[:300]
            raise UpstreamError(
                f"Video upstream returned {response.status_code}",
                status=response.status_code,
                body=body,
            )
        async for line in response.aiter_lines():
            yield line


def _absolutize_video_url(url: str) -> str:
    full_url, _, _ = resolve_download_url(url)
    return full_url


def _is_upstream_asset_content_url(value: str) -> bool:
    parsed = urlparse(value)
    return (
        parsed.scheme == "https"
        and parsed.netloc == "assets.grok.com"
        and parsed.path.endswith("/content")
    )


def _extract_asset_id_from_content_url(value: str) -> str:
    parsed = urlparse(value)
    if parsed.scheme != "https" or parsed.netloc != "assets.grok.com":
        return ""
    parts = [part for part in parsed.path.split("/") if part]
    if len(parts) >= 2 and parts[-1] == "content":
        return parts[-2]
    return ""


def _replace_reference_placeholders(prompt: str, asset_ids: list[str]) -> str:
    """Replace @图N / @imageN placeholders with uploaded asset IDs."""

    def _replace(match: re.Match[str]) -> str:
        index = int(match.group(1)) - 1
        if index < 0 or index >= len(asset_ids):
            raise ValidationError(
                f"Reference placeholder {match.group(0)} has no matching uploaded image",
                param="prompt",
            )
        return f"@{asset_ids[index]}"

    return _REFERENCE_PLACEHOLDER_RE.sub(_replace, prompt)


async def _prepare_video_reference(
    token: str,
    input_reference: dict[str, Any],
    *,
    index: int,
) -> _VideoReference:
    file_id = str(input_reference.get("file_id") or "").strip()
    image_input = str(input_reference.get("image_url") or "").strip()
    param_base = f"input_reference.{index}" if index >= 0 else "input_reference"

    if file_id and image_input:
        raise ValidationError(
            "input_reference accepts only one of file_id or image_url",
            param=param_base,
        )
    if file_id:
        raise ValidationError(
            "input_reference.file_id is not supported yet",
            param=f"{param_base}.file_id",
        )
    if not image_input:
        raise ValidationError(
            "input_reference.image_url is required",
            param=f"{param_base}.image_url",
        )

    if _is_upstream_asset_content_url(image_input):
        content_url = image_input
        asset_id = _extract_asset_id_from_content_url(image_input)
        if not asset_id:
            raise ValidationError(
                "input_reference.image_url must include a valid upstream asset ID",
                param=f"{param_base}.image_url",
            )
    else:
        try:
            uploaded_file_id, uploaded_file_uri = await upload_from_input(token, image_input)
            content_url = resolve_uploaded_asset_reference(token, uploaded_file_id, uploaded_file_uri)
            asset_id = uploaded_file_id
        except ValidationError as exc:
            raise ValidationError(exc.message, param=f"{param_base}.image_url") from exc
        except UpstreamError as exc:
            raise UpstreamError(
                f"Video input reference {index + 1} upload failed: {exc.message}",
                status=exc.status,
                body=exc.details.get("body", ""),
            ) from exc
        except Exception as exc:
            raise UpstreamError(f"Video input reference {index + 1} upload failed: {exc}") from exc

    return _VideoReference(asset_id=asset_id, content_url=content_url)


async def _prepare_video_references(
    token: str,
    input_references: list[dict[str, Any]],
) -> list[_VideoReference]:
    if len(input_references) > _VIDEO_MAX_REFERENCES:
        raise ValidationError(
            f"Video generation supports at most {_VIDEO_MAX_REFERENCES} reference images",
            param="input_reference",
        )

    results: list[_VideoReference | None] = [None] * len(input_references)

    async def _runner(index: int, input_reference: dict[str, Any]) -> None:
        results[index] = await _prepare_video_reference(token, input_reference, index=index)

    async with asyncio.TaskGroup() as tg:
        for index, input_reference in enumerate(input_references):
            tg.create_task(_runner(index, input_reference), name=f"video-reference-{index}")

    return [result for result in results if result is not None]


async def _collect_video_segment(
    *,
    token: str,
    payload: dict[str, Any],
    referer: str,
    timeout_s: float,
    progress_cb: Callable[[int], Awaitable[None]] | None = None,
) -> _VideoArtifact:
    final_url = ""
    final_asset_id = ""
    final_thumbnail = ""
    video_post_id = ""

    async for line in _stream_video_request(
        token,
        payload,
        referer=referer,
        timeout_s=timeout_s,
    ):
        event_type, data = classify_line(line)
        if event_type == "done":
            break
        if event_type != "data" or not data:
            continue
        try:
            obj = orjson.loads(data)
        except Exception:
            continue

        stream = _extract_streaming_video_response(obj)
        if stream:
            try:
                progress = int(stream.get("progress") or 0)
            except (TypeError, ValueError):
                progress = 0
            if progress_cb is not None:
                await progress_cb(progress)

            video_post_id = str(
                stream.get("videoPostId")
                or stream.get("videoId")
                or video_post_id
                or ""
            ).strip()

            if progress >= 100 and not stream.get("moderated"):
                raw_url = stream.get("videoUrl")
                asset_id = stream.get("assetId")
                thumbnail = stream.get("thumbnailImageUrl")
                if isinstance(raw_url, str) and raw_url:
                    final_url = _absolutize_video_url(raw_url)
                if isinstance(asset_id, str) and asset_id:
                    final_asset_id = asset_id
                if isinstance(thumbnail, str) and thumbnail:
                    final_thumbnail = _absolutize_video_url(thumbnail)

        attachments = _extract_model_response_file_attachments(obj)
        if attachments and not final_asset_id:
            final_asset_id = attachments[0]

    if not final_url and final_asset_id:
        final_url = resolve_asset_reference(final_asset_id, "", user_id=None) or ""

    if not final_url and final_asset_id:
        raise UpstreamError("Video segment returned only assetId without a resolvable URL")
    if not final_url:
        raise UpstreamError("Video generation returned no final video URL")

    return _VideoArtifact(
        video_url=final_url,
        video_post_id=video_post_id or final_asset_id,
        asset_id=final_asset_id,
        thumbnail_url=final_thumbnail,
    )


async def _download_video_bytes(token: str, url: str) -> tuple[bytes, str]:
    try:
        stream, content_type = await download_asset(token, url)
        chunks: list[bytes] = []
        async for chunk in stream:
            chunks.append(chunk)
    except UpstreamError:
        raise
    except Exception as exc:
        raise UpstreamError(f"Video download failed: {exc}") from exc
    return b"".join(chunks), (content_type or "video/mp4")


def _save_video_bytes(raw: bytes, file_id: str) -> Path:
    out_dir = video_files_dir()
    path = out_dir / f"{file_id}.mp4"
    return save_media_bytes(raw, path, media_type="video")


def _local_video_url(file_id: str) -> str:
    app_url = get_config().get_str("app.app_url", "").rstrip("/")
    return f"{app_url}/v1/files/video?id={file_id}" if app_url else f"/v1/files/video?id={file_id}"


def _normalize_video_format(value: str | None) -> str:
    fmt = (value or "grok_url").strip().lower()
    if fmt not in {"grok_url", "local_url", "grok_html", "local_html"}:
        raise ValidationError(
            "video_format must be one of [grok_url, local_url, grok_html, local_html]",
            param="features.video_format",
        )
    return fmt


def _render_video_html(url: str) -> str:
    safe_url = html.escape(url, quote=True)
    return f'<video controls src="{safe_url}"></video>'


async def _resolve_video_output(*, token: str, url: str, file_id: str) -> str:
    fmt = _normalize_video_format(get_config().get_str("features.video_format", "grok_url"))
    if fmt == "grok_url":
        return url
    if fmt == "grok_html":
        return _render_video_html(url)

    try:
        raw, _mime = await _download_video_bytes(token, url)
        _save_video_bytes(raw, file_id)
    except Exception as exc:
        logger.warning("video download failed: fallback_to=upstream_url error={}", exc)
        return url if fmt == "local_url" else _render_video_html(url)

    local_url = _local_video_url(file_id)
    return local_url if fmt == "local_url" else _render_video_html(local_url)


async def _generate_video_with_token(
    *,
    token: str,
    prompt: str,
    aspect_ratio: str,
    resolution_name: str,
    seconds: int,
    preset: str,
    timeout_s: float,
    input_references: list[dict[str, Any]] | None = None,
    progress_cb: Callable[[int], Awaitable[None]] | None = None,
) -> _VideoArtifact:
    references: list[_VideoReference] = []
    prompt_text = prompt
    if input_references:
        references = await _prepare_video_references(token, input_references)
        prompt_text = _replace_reference_placeholders(
            prompt_text,
            [reference.asset_id for reference in references],
        )

    post = await create_media_post(
        token,
        media_type=_VIDEO_MEDIA_TYPE,
        prompt=prompt_text,
        referer="https://grok.com/imagine",
    )
    post_data = post.get("post")
    if not isinstance(post_data, dict):
        raise UpstreamError("Video create-post returned no post payload")
    parent_post_id = str(post_data.get("id") or "").strip()
    if not parent_post_id:
        raise UpstreamError("Video create-post returned no post id")

    segments = _build_segment_lengths(seconds)
    total_segments = len(segments)
    artifact: _VideoArtifact | None = None
    extend_post_id = parent_post_id
    elapsed_seconds = 0

    for index, segment_length in enumerate(segments):
        if index == 0:
            payload = _video_create_payload(
                prompt=prompt_text,
                parent_post_id=parent_post_id,
                aspect_ratio=aspect_ratio,
                resolution_name=resolution_name,
                video_length=segment_length,
                preset=preset,
                image_references=[reference.content_url for reference in references] if references else None,
                file_attachments=[reference.asset_id for reference in references] if references else None,
            )
            referer = "https://grok.com/imagine"
        else:
            payload = _video_extend_payload(
                prompt=prompt_text,
                parent_post_id=parent_post_id,
                extend_post_id=extend_post_id,
                aspect_ratio=aspect_ratio,
                resolution_name=resolution_name,
                video_length=segment_length,
                preset=preset,
                start_time_s=_video_extend_start_time(elapsed_seconds),
            )
            referer = f"https://grok.com/imagine/post/{parent_post_id}"

        async def _segment_progress(progress: int) -> None:
            if progress_cb is None:
                return
            scaled = int(((index + (max(0, min(100, progress)) / 100.0)) / total_segments) * 100)
            await progress_cb(scaled)

        artifact = await _collect_video_segment(
            token=token,
            payload=payload,
            referer=referer,
            timeout_s=timeout_s,
            progress_cb=_segment_progress if progress_cb is not None else None,
        )
        if index == 0 and total_segments > 1:
            artifact.remixed_from_video_id = artifact.video_post_id or parent_post_id
        extend_post_id = artifact.video_post_id or artifact.asset_id or parent_post_id
        elapsed_seconds += segment_length

    if artifact is None:
        raise UpstreamError("Video generation returned no artifact")
    return artifact


async def _run_video_generation(
    *,
    model: str,
    prompt: str,
    aspect_ratio: str,
    resolution_name: str,
    seconds: int,
    preset: str = "custom",
    input_references: list[dict[str, Any]] | None = None,
    progress_cb: Callable[[int], Awaitable[None]] | None = None,
) -> _VideoArtifact:
    async def _runner(token: str, timeout_s: float) -> _VideoArtifact:
        return await _generate_video_with_token(
            token=token,
            prompt=prompt,
            aspect_ratio=aspect_ratio,
            resolution_name=resolution_name,
            seconds=seconds,
            preset=preset,
            timeout_s=timeout_s,
            input_references=input_references,
            progress_cb=progress_cb,
        )

    return await _run_video_with_account(model=model, runner=_runner)


async def _run_video_with_account(
    *,
    model: str,
    runner: Callable[[str, float], Awaitable[Any]],
) -> Any:
    cfg = get_config()
    timeout_s = cfg.get_float("video.timeout", 180.0)
    spec = resolve_model(model)
    if not spec.is_video():
        raise ValidationError(f"Model {model!r} is not a video model", param="model")

    from app.dataplane.account import _directory as _acct_dir
    if _acct_dir is None:
        raise RateLimitError("Account directory not initialised")

    acct = await _acct_dir.reserve(
        pool_candidates=spec.pool_candidates(),
        mode_id=int(spec.mode_id),
        now_s_override=now_s(),
    )
    if acct is None:
        raise RateLimitError("No available accounts for video generation")

    token = acct.token
    success = False
    fail_exc: BaseException | None = None
    try:
        artifact = await runner(token, timeout_s)
        success = True
        return artifact
    except BaseException as exc:
        fail_exc = exc
        raise
    finally:
        await _acct_dir.release(acct)
        kind = FeedbackKind.SUCCESS if success else _feedback_kind(fail_exc) if fail_exc else FeedbackKind.SERVER_ERROR
        await _acct_dir.feedback(token, kind, int(spec.mode_id))
        if success:
            asyncio.create_task(_quota_sync(token, int(spec.mode_id)))
        else:
            asyncio.create_task(_fail_sync(token, int(spec.mode_id), fail_exc))


async def _put_video_job(job: _VideoJob) -> None:
    async with _VIDEO_JOBS_LOCK:
        _VIDEO_JOBS[job.id] = job


async def get_video_job(video_id: str) -> _VideoJob | None:
    async with _VIDEO_JOBS_LOCK:
        return _VIDEO_JOBS.get(video_id)


async def _expire_video_job(video_id: str, ttl_s: int = _VIDEO_JOB_TTL_S) -> None:
    await asyncio.sleep(ttl_s)
    async with _VIDEO_JOBS_LOCK:
        _VIDEO_JOBS.pop(video_id, None)


async def _set_job_status(job: _VideoJob, *, status: str, progress: int | None = None) -> None:
    async with _VIDEO_JOBS_LOCK:
        job.status = status
        if progress is not None:
            job.progress = max(0, min(100, progress))


def _job_error_payload(message: str) -> dict[str, Any]:
    return {"code": "video_generation_failed", "message": message}


async def _run_video_job(
    job: _VideoJob,
    *,
    size: str,
    resolution_name: str | None,
    prompt: str,
    seconds: int,
    preset: str | None,
    input_references: list[dict[str, Any]] | None = None,
) -> None:
    try:
        await _set_job_status(job, status="in_progress", progress=1)
        aspect_ratio, default_resolution_name = _resolve_video_size(size)
        resolved_resolution_name = _resolve_video_resolution_name(
            resolution_name,
            default=default_resolution_name,
        )
        resolved_preset = _resolve_video_preset(preset)
        spec = resolve_model(job.model)

        from app.dataplane.account import _directory as _acct_dir
        if _acct_dir is None:
            raise RateLimitError("Account directory not initialised")

        acct = await _acct_dir.reserve(
            pool_candidates=spec.pool_candidates(),
            mode_id=int(spec.mode_id),
            now_s_override=now_s(),
        )
        if acct is None:
            raise RateLimitError("No available accounts for video generation")

        token = acct.token
        success = False
        fail_exc: BaseException | None = None
        try:
            cfg = get_config()
            timeout_s = cfg.get_float("video.timeout", 180.0)

            async def _progress(progress: int) -> None:
                await _set_job_status(job, status="in_progress", progress=max(1, progress))

            artifact = await _generate_video_with_token(
                token=token,
                prompt=prompt,
                aspect_ratio=aspect_ratio,
                resolution_name=resolved_resolution_name,
                seconds=seconds,
                preset=resolved_preset,
                timeout_s=timeout_s,
                input_references=input_references,
                progress_cb=_progress,
            )
            raw, _mime = await _download_video_bytes(token, artifact.video_url)
            success = True
        except BaseException as exc:
            fail_exc = exc
            raise
        finally:
            await _acct_dir.release(acct)
            kind = FeedbackKind.SUCCESS if success else _feedback_kind(fail_exc) if fail_exc else FeedbackKind.SERVER_ERROR
            await _acct_dir.feedback(token, kind, int(spec.mode_id))
            if success:
                asyncio.create_task(_quota_sync(token, int(spec.mode_id)))
            else:
                asyncio.create_task(_fail_sync(token, int(spec.mode_id), fail_exc))

        path = _save_video_bytes(raw, job.id)
        async with _VIDEO_JOBS_LOCK:
            job.status = "completed"
            job.progress = 100
            job.completed_at = int(time.time())
            job.video_url = artifact.video_url
            job.content_path = str(path)
            job.remixed_from_video_id = artifact.remixed_from_video_id
    except Exception as exc:
        logger.exception("video job failed: job_id={} error={}", job.id, exc)
        async with _VIDEO_JOBS_LOCK:
            job.status = "failed"
            job.error = _job_error_payload(str(exc))


async def create_video(
    *,
    model: str,
    prompt: str,
    seconds: str | int | None = None,
    size: str | None = None,
    resolution_name: str | None = None,
    preset: str | None = None,
    input_references: list[dict[str, Any]] | None = None,
) -> dict[str, Any]:
    spec = model_registry.get(model)
    if spec is None or not spec.enabled or not spec.is_video():
        raise ValidationError(f"Model {model!r} is not a video model", param="model")

    cleaned_prompt = (prompt or "").strip()
    if not cleaned_prompt:
        raise ValidationError("prompt cannot be empty", param="prompt")

    normalized_seconds = _coerce_seconds(seconds)
    validate_video_length(normalized_seconds)
    normalized_size = (size or "720x1280").strip()
    _aspect_ratio, default_resolution_name = _resolve_video_size(normalized_size)
    _resolve_video_resolution_name(resolution_name, default=default_resolution_name)
    _resolve_video_preset(preset)

    job = _VideoJob(
        id=f"video_{uuid.uuid4().hex}",
        model=model,
        prompt=cleaned_prompt,
        seconds=str(normalized_seconds),
        size=normalized_size,
        quality=_VIDEO_QUALITY,
        created_at=int(time.time()),
    )
    await _put_video_job(job)
    asyncio.create_task(
        _run_video_job(
            job,
            size=normalized_size,
            resolution_name=resolution_name,
            prompt=cleaned_prompt,
            seconds=normalized_seconds,
            preset=preset,
            input_references=input_references,
        )
    )
    asyncio.create_task(_expire_video_job(job.id))
    return job.to_dict()


async def retrieve(video_id: str) -> dict[str, Any]:
    job = await get_video_job(video_id)
    if job is None:
        raise ValidationError(f"Video {video_id!r} not found", param="video_id")
    return job.to_dict()


async def content_path(video_id: str) -> Path:
    job = await get_video_job(video_id)
    if job is None:
        raise ValidationError(f"Video {video_id!r} not found", param="video_id")
    if job.status != "completed" or not job.content_path:
        raise AppError(
            "Video content is not ready yet",
            kind=ErrorKind.VALIDATION,
            code="video_not_ready",
            status=409,
        )
    path = Path(job.content_path)
    if not path.exists():
        raise ValidationError(f"Video content for {video_id!r} not found", param="video_id")
    return path


def _extract_video_prompt_and_references(messages: list[dict]) -> tuple[str, list[dict[str, Any]] | None]:
    prompt = ""
    reference_urls: list[str] = []

    for msg in messages:
        content = msg.get("content", "")
        if isinstance(content, str) and content.strip():
            prompt = content.strip()
            continue
        if not isinstance(content, list):
            continue

        text_parts: list[str] = []
        block_references: list[str] = []
        for item in content:
            if not isinstance(item, dict):
                continue
            item_type = item.get("type")
            if item_type == "text":
                text = str(item.get("text") or "").strip()
                if text:
                    text_parts.append(text)
            elif item_type == "image_url":
                image_url = item.get("image_url")
                if isinstance(image_url, dict):
                    url = str(image_url.get("url") or "").strip()
                elif isinstance(image_url, str):
                    url = image_url.strip()
                else:
                    url = ""
                if url:
                    block_references.append(url)

        if text_parts:
            prompt = " ".join(text_parts)
        if block_references:
            reference_urls.extend(block_references)

    if not prompt:
        raise ValidationError("Video prompt cannot be empty", param="messages")

    input_references = [{"image_url": reference_url} for reference_url in reference_urls] or None
    return prompt, input_references


async def completions(
    *,
    model: str,
    messages: list[dict],
    stream: bool | None = None,
    seconds: int = 6,
    size: str = "720x1280",
    resolution_name: str | None = None,
    preset: str | None = None,
) -> dict | AsyncGenerator[str, None]:
    """Chat-completions video support on top of the same core flow."""
    validate_video_length(seconds)
    aspect_ratio, default_resolution_name = _resolve_video_size(size)
    resolved_resolution_name = _resolve_video_resolution_name(
        resolution_name,
        default=default_resolution_name,
    )
    resolved_preset = _resolve_video_preset(preset)
    prompt, input_references = _extract_video_prompt_and_references(messages)

    cfg = get_config()
    is_stream = stream if stream is not None else cfg.get_bool("features.stream", False)
    response_id = make_response_id()

    async def _run(progress_cb: Callable[[int], Awaitable[None]] | None = None) -> str:
        async def _runner(token: str, timeout_s: float) -> str:
            artifact = await _generate_video_with_token(
                token=token,
                prompt=prompt,
                aspect_ratio=aspect_ratio,
                resolution_name=resolved_resolution_name,
                seconds=seconds,
                preset=resolved_preset,
                timeout_s=timeout_s,
                input_references=input_references,
                progress_cb=progress_cb,
            )
            file_id = hashlib.sha1(artifact.video_url.encode("utf-8")).hexdigest()[:32]
            return await _resolve_video_output(
                token=token,
                url=artifact.video_url,
                file_id=file_id,
            )

        return await _run_video_with_account(model=model, runner=_runner)

    if is_stream:
        async def _sse() -> AsyncGenerator[str, None]:
            queue: asyncio.Queue[int] = asyncio.Queue()
            last_progress = -1

            async def _progress(progress: int) -> None:
                await queue.put(max(0, min(100, progress)))

            task = asyncio.create_task(_run(progress_cb=_progress))
            while not task.done() or not queue.empty():
                try:
                    progress = await asyncio.wait_for(queue.get(), timeout=0.1)
                except asyncio.TimeoutError:
                    continue
                if progress > last_progress:
                    last_progress = progress
                    chunk = make_thinking_chunk(response_id, model, _progress_reason(progress))
                    yield f"data: {orjson.dumps(chunk).decode()}\n\n"

            content = await task
            chunk = make_stream_chunk(response_id, model, content)
            yield f"data: {orjson.dumps(chunk).decode()}\n\n"
            final = make_stream_chunk(response_id, model, "", is_final=True)
            yield f"data: {orjson.dumps(final).decode()}\n\n"
            yield "data: [DONE]\n\n"

        return _sse()

    progress_updates: list[str] = []

    async def _progress(progress: int) -> None:
        reason = _progress_reason(progress)
        if not progress_updates or progress_updates[-1] != reason:
            progress_updates.append(reason)

    content = await _run(progress_cb=_progress)
    reasoning = "\n".join(progress_updates) if progress_updates else None
    return make_chat_response(
        model,
        content,
        prompt_content=prompt,
        response_id=response_id,
        reasoning_content=reasoning,
    )


__all__ = [
    "create_video",
    "retrieve",
    "content_path",
    "validate_video_length",
    "completions",
    "_build_segment_lengths",
    "_resolve_video_size",
]
