"""Image services — generation (/v1/images/generations) and editing (/v1/images/edits)."""

import asyncio
import base64
import binascii
import hashlib
import re
import time
from dataclasses import dataclass
from typing import Any, AsyncGenerator, Awaitable, Callable
from urllib.parse import urlparse

import orjson

from app.platform.logging.logger import logger
from app.platform.config.snapshot import get_config
from app.platform.errors import RateLimitError, UpstreamError, ValidationError
from app.platform.runtime.clock import now_s
from app.platform.storage import image_files_dir, save_media_bytes
from app.control.model.registry import resolve as resolve_model
from app.control.model.enums import ModeId
from app.control.model.spec import ModelSpec
from app.control.account.enums import FeedbackKind
from app.dataplane.reverse.transport.imagine_ws import stream_images
from app.dataplane.reverse.protocol.xai_chat import (
    StreamAdapter,
    build_chat_payload,
    classify_line,
)
from app.dataplane.reverse.protocol.xai_assets import infer_content_type, resolve_asset_reference, resolve_download_url
from app.dataplane.reverse.protocol.xai_image_edit import (
    IMAGE_EDIT_GENERATION_COUNT,
    IMAGE_POST_MEDIA_TYPE,
    build_image_edit_payload,
    extract_model_response_file_attachments,
    extract_model_response_urls,
    extract_streaming_response,
)
from app.dataplane.reverse.transport.assets import download_asset
from app.dataplane.reverse.transport.asset_upload import (
    resolve_uploaded_asset_reference,
    upload_from_input,
)
from app.dataplane.reverse.transport.media import create_media_post
from app.dataplane.proxy import get_proxy_runtime
from app.dataplane.proxy.adapters.headers import build_http_headers, build_sso_cookie
from app.dataplane.proxy.adapters.session import ResettableSession, build_session_kwargs
from app.dataplane.reverse.runtime.endpoint_table import CHAT
from ._format import (
    make_chat_response,
    make_response_id,
    make_stream_chunk,
    make_thinking_chunk,
)
from .chat import _quota_sync, _fail_sync, _feedback_kind

_X_USER_ID_RE = re.compile(r"(?:^|;\s*)x-userid=([^;]+)")


# ---------------------------------------------------------------------------
# Aspect-ratio helpers (generation)
# ---------------------------------------------------------------------------

_RATIO_MAP: dict[str, str] = {
    "1280x720":  "16:9", "16:9": "16:9",
    "720x1280":  "9:16", "9:16": "9:16",
    "1792x1024": "3:2",  "3:2":  "3:2",
    "1024x1792": "2:3",  "2:3":  "2:3",
    "1024x1024": "1:1",  "1:1":  "1:1",
}


def resolve_aspect_ratio(size: str) -> str:
    return _RATIO_MAP.get(size, "2:3")


@dataclass(slots=True)
class _ImageOutput:
    api_value:      str
    markdown_value: str


def _clamp_progress(value: int) -> int:
    return max(0, min(100, int(value)))


def _compute_progress_percent(progress_map: dict[object, int], total: int) -> int:
    if total <= 0:
        return 100
    if not progress_map:
        return 0
    values = sorted((_clamp_progress(value) for value in progress_map.values()), reverse=True)[:total]
    return _clamp_progress(sum(values) // total)


def _progress_reason(label: str, progress: int, *, completed: int | None = None, total: int | None = None) -> str:
    reason = f"{label}正在生成 {progress}%"
    if completed is not None and total is not None and total > 0:
        reason += f" ({completed}/{total})"
    return reason


def _append_reason_update(
    updates: list[str],
    label: str,
    progress: int,
    *,
    completed: int | None = None,
    total: int | None = None,
) -> None:
    reason = _progress_reason(label, progress, completed=completed, total=total)
    if not updates or updates[-1] != reason:
        updates.append(reason)


def _completed_items(progress_map: dict[object, int]) -> int:
    return sum(1 for value in progress_map.values() if _clamp_progress(value) >= 100)


async def _lite_progress_updates(
    *,
    idx: int,
    progress: int,
    total: int,
    progress_map: dict[int, int],
    updates: list[str],
    enabled: bool,
) -> None:
    progress_map[idx] = _clamp_progress(progress)
    if enabled:
        _append_reason_update(
            updates,
            "图片",
            _compute_progress_percent(progress_map, total),
            completed=_completed_items(progress_map),
            total=total,
        )


def _normalize_response_format(response_format: str) -> str:
    fmt = (response_format or "url").strip().lower()
    if fmt not in {"url", "b64_json"}:
        raise ValidationError(
            "response_format must be one of ['url', 'b64_json']",
            param="response_format",
        )
    return fmt


def _normalize_configured_image_format(value: str | None) -> str:
    fmt = (value or "grok_url").strip().lower()
    if fmt not in {"grok_url", "local_url", "grok_md", "local_md", "base64"}:
        raise ValidationError(
            "image_format must be one of [grok_url, local_url, grok_md, local_md, base64]",
            param="features.image_format",
        )
    return fmt


def _app_url() -> str:
    return get_config().get_str("app.app_url", "").rstrip("/")


def _local_image_url(file_id: str) -> str:
    app_url = _app_url()
    return f"{app_url}/v1/files/image?id={file_id}"


def _extract_image_file_id(url: str) -> str:
    parsed = urlparse(url)
    parts = [part for part in parsed.path.split("/") if part]
    if parsed.scheme == "https" and parsed.netloc == "assets.grok.com" and len(parts) >= 2:
        last_stem = parts[-1].split(".", 1)[0].lower()
        if last_stem == "content":
            asset_id = parts[-2].strip()
            if asset_id:
                return asset_id
    for part in reversed(parts):
        stem = part.split(".", 1)[0]
        if stem and stem.lower() not in {"image", "original", "thumbnail", "content"}:
            return stem
    return hashlib.sha1(url.encode("utf-8")).hexdigest()[:32]


def _save_image(raw: bytes, mime: str, file_id: str) -> str:
    img_dir = image_files_dir()
    ext = ".png" if "png" in mime else ".jpg"
    path = img_dir / f"{file_id}{ext}"
    save_media_bytes(raw, path, media_type="image")
    return file_id


async def _download_image_bytes(token: str, url: str) -> tuple[bytes, str]:
    try:
        stream, content_type = await download_asset(token, url)
        chunks: list[bytes] = []
        async for chunk in stream:
            chunks.append(chunk)
    except UpstreamError:
        raise
    except Exception as exc:
        raise UpstreamError(f"Image download failed: {exc}") from exc
    return b"".join(chunks), (content_type or infer_content_type(url) or "image/jpeg")


async def _resolve_image_output(
    *,
    token: str,
    url: str,
    response_format: str,
    blob_b64: str | None = None,
) -> _ImageOutput:
    fmt = _normalize_response_format(response_format)
    if fmt == "b64_json":
        mime = infer_content_type(url) or "image/jpeg"
        if blob_b64 is not None:
            try:
                raw = base64.b64decode(blob_b64)
            except (ValueError, TypeError, binascii.Error) as exc:
                raise UpstreamError(f"Invalid upstream image blob: {exc}") from exc
        else:
            raw, mime = await _download_image_bytes(token, url)

        b64 = blob_b64 or base64.b64encode(raw).decode()
        data_uri = f"data:{mime};base64,{b64}"
        return _ImageOutput(api_value=b64, markdown_value=f"![image]({data_uri})")

    image_format = _normalize_configured_image_format(
        get_config().get_str("features.image_format", "grok_url")
    )
    if image_format in {"grok_url", "grok_md"}:
        return _ImageOutput(api_value=url, markdown_value=f"![image]({url})")

    mime = infer_content_type(url) or "image/jpeg"
    if blob_b64 is not None:
        try:
            raw = base64.b64decode(blob_b64)
        except (ValueError, TypeError, binascii.Error) as exc:
            raise UpstreamError(f"Invalid upstream image blob: {exc}") from exc
    else:
        raw, mime = await _download_image_bytes(token, url)

    if image_format == "base64":
        b64 = blob_b64 or base64.b64encode(raw).decode()
        data_uri = f"data:{mime};base64,{b64}"
        return _ImageOutput(api_value=data_uri, markdown_value=f"![image]({data_uri})")

    file_id = _save_image(raw, mime, _extract_image_file_id(url))
    local_url = _local_image_url(file_id)
    return _ImageOutput(api_value=local_url, markdown_value=f"![image]({local_url})")


def _output_content(image: _ImageOutput, *, chat_format: bool) -> str:
    return image.markdown_value if chat_format else image.api_value


# ---------------------------------------------------------------------------
# Image generation
# ---------------------------------------------------------------------------

# Models that use the chat endpoint for image generation (no WS, no params).
_LITE_IMAGE_MODELS = frozenset({"grok-imagine-image-lite"})
# WS models that use quality mode (enable_pro=True).
_PRO_IMAGE_MODELS  = frozenset({"grok-imagine-image-pro"})


async def generate(
    *,
    model:           str,
    prompt:          str,
    n:               int  = 1,
    size:            str  = "1024x1024",
    response_format: str  = "url",
    stream:          bool = False,
    chat_format:     bool = False,
) -> dict | AsyncGenerator[str, None]:
    """Generate images.

    Routes to the appropriate backend based on model:
      grok-imagine-image-lite  → chat endpoint (no aspect-ratio control, all pools)
      grok-imagine-image       → WebSocket speed mode (super+)
      grok-imagine-image-pro   → WebSocket quality mode (super+)

    Returns:
      Non-streaming: OpenAI images.generations dict, or chat dict if chat_format=True.
      Streaming:     async generator of SSE strings.
    """
    cfg          = get_config()
    spec         = resolve_model(model)
    aspect_ratio = resolve_aspect_ratio(size)
    enable_nsfw  = cfg.get_bool("features.enable_nsfw", True)

    from app.dataplane.account import _directory as _acct_dir
    if _acct_dir is None:
        raise RateLimitError("Account directory not initialised")

    # Lite model: chat-based generation (no WS, ignores aspect_ratio).
    if model in _LITE_IMAGE_MODELS:
        return await _generate_lite(
            spec            = spec,
            prompt          = prompt,
            n               = n,
            response_format = response_format,
            stream          = stream,
            chat_format     = chat_format,
        )

    acct = await _acct_dir.reserve(
        pool_candidates = spec.pool_candidates(),
        mode_id         = int(spec.mode_id),
        now_s_override  = now_s(),
    )
    if acct is None:
        raise RateLimitError("No available accounts for image generation")

    token       = acct.token
    response_id = make_response_id()
    enable_pro  = model in _PRO_IMAGE_MODELS

    if stream:
        async def _sse_stream() -> AsyncGenerator[str, None]:
            success = False
            fail_exc: BaseException | None = None
            progress_map: dict[object, int] = {}
            completed_ids: set[object] = set()
            last_progress = -1
            try:
                async for ev in stream_images(
                    token, prompt,
                    aspect_ratio = aspect_ratio,
                    n            = n,
                    enable_nsfw  = enable_nsfw,
                    enable_pro   = enable_pro,
                ):
                    ev_type = ev.get("type")
                    if ev_type == "error":
                        raise UpstreamError(f"Image error: {ev.get('error', '')}")
                    if ev_type == "moderated":
                        logger.warning("image generation slot moderated: image_id={}", ev.get("image_id", "")[:8])
                        continue
                    if ev_type == "progress":
                        key = ev.get("image_id") or f"progress-{len(progress_map)}"
                        progress_map[key] = _clamp_progress(ev.get("progress") or 0)
                        aggregate = _compute_progress_percent(progress_map, n)
                        if chat_format and aggregate > last_progress:
                            last_progress = aggregate
                            reason = _progress_reason(
                                "图片",
                                aggregate,
                                completed=len(completed_ids),
                                total=n,
                            )
                            chunk = make_thinking_chunk(response_id, model, reason)
                            yield f"data: {orjson.dumps(chunk).decode()}\n\n"
                        continue
                    if not ev.get("is_final"):
                        continue
                    key = ev.get("image_id") or f"final-{len(completed_ids)}"
                    progress_map[key] = 100
                    completed_ids.add(key)
                    aggregate = _compute_progress_percent(progress_map, n)
                    if chat_format and aggregate > last_progress:
                        last_progress = aggregate
                        reason = _progress_reason("图片", aggregate, completed=len(completed_ids), total=n)
                        chunk = make_thinking_chunk(response_id, model, reason)
                        yield f"data: {orjson.dumps(chunk).decode()}\n\n"
                    image = await _resolve_image_output(
                        token=token,
                        url=ev.get("url", ""),
                        response_format=response_format,
                        blob_b64=ev.get("blob") or None,
                    )
                    content = _output_content(image, chat_format=chat_format)
                    chunk = make_stream_chunk(response_id, model, content)
                    yield f"data: {orjson.dumps(chunk).decode()}\n\n"

                final = make_stream_chunk(response_id, model, "", is_final=True)
                yield f"data: {orjson.dumps(final).decode()}\n\n"
                yield "data: [DONE]\n\n"
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

        return _sse_stream()

    # Non-streaming: collect all final images.
    finals: list[_ImageOutput] = []
    reasoning_updates: list[str] = []
    progress_map: dict[object, int] = {}
    completed_ids: set[object] = set()
    success = False
    fail_exc: BaseException | None = None
    try:
        async for ev in stream_images(
            token, prompt,
            aspect_ratio = aspect_ratio,
            n            = n,
            enable_nsfw  = enable_nsfw,
            enable_pro   = enable_pro,
        ):
            ev_type = ev.get("type")
            if ev_type == "error":
                raise UpstreamError(f"Image generation failed: {ev.get('error', 'unknown')}")
            if ev_type == "moderated":
                logger.warning("image generation slot moderated: image_id={}", ev.get("image_id", "")[:8])
                continue
            if ev_type == "progress":
                key = ev.get("image_id") or f"progress-{len(progress_map)}"
                progress_map[key] = _clamp_progress(ev.get("progress") or 0)
                if chat_format:
                    _append_reason_update(
                        reasoning_updates,
                        "图片",
                        _compute_progress_percent(progress_map, n),
                        completed=len(completed_ids),
                        total=n,
                    )
                continue
            if ev.get("is_final"):
                key = ev.get("image_id") or f"final-{len(completed_ids)}"
                progress_map[key] = 100
                completed_ids.add(key)
                if chat_format:
                    _append_reason_update(
                        reasoning_updates,
                        "图片",
                        _compute_progress_percent(progress_map, n),
                        completed=len(completed_ids),
                        total=n,
                    )
                image = await _resolve_image_output(
                    token=token,
                    url=ev.get("url", ""),
                    response_format=response_format,
                    blob_b64=ev.get("blob") or None,
                )
                finals.append(image)
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

    if chat_format:
        content = "\n\n".join(image.markdown_value for image in finals)
        reasoning = "\n".join(reasoning_updates) if reasoning_updates else None
        return make_chat_response(
            model,
            content,
            prompt_content=prompt,
            response_id=response_id,
            reasoning_content=reasoning,
        )

    data = [
        {"b64_json": image.api_value}
        if _normalize_response_format(response_format) == "b64_json"
        else {"url": image.api_value}
        for image in finals
    ]
    return {"created": int(time.time()), "data": data}


# ---------------------------------------------------------------------------
# Lite image generation (chat-based, no WS, no aspect-ratio control)
# ---------------------------------------------------------------------------

async def _generate_lite(
    *,
    spec:            ModelSpec,
    prompt:          str,
    n:               int,
    response_format: str,
    stream:          bool,
    chat_format:     bool,
) -> dict | AsyncGenerator[str, None]:
    """Generate images via the chat endpoint (Aurora model path).

    Does not support aspect ratio or quality control.  All account pools
    can serve this model.
    """
    response_id = make_response_id()
    cfg         = get_config()
    timeout_s   = cfg.get_float("chat.timeout", 120.0)
    logger.debug("lite image fan-out started: request_count={} mode={}", n, spec.mode_id.name.lower())

    if stream:
        async def _sse_stream() -> AsyncGenerator[str, None]:
            progress_map: dict[int, int] = {}
            last_progress = -1
            queue: asyncio.Queue[tuple[int, int]] = asyncio.Queue()

            async def _progress(idx: int, progress: int) -> None:
                progress_map[idx] = _clamp_progress(progress)
                await queue.put((
                    _compute_progress_percent(progress_map, n),
                    _completed_items(progress_map),
                ))

            task = asyncio.create_task(
                _run_lite_batch(
                    spec=spec,
                    prompt=prompt,
                    n=n,
                    timeout_s=timeout_s,
                    response_format=response_format,
                    progress_cb=_progress,
                )
            )

            while not task.done() or not queue.empty():
                try:
                    aggregate, completed = await asyncio.wait_for(queue.get(), timeout=0.1)
                except asyncio.TimeoutError:
                    continue
                if chat_format and aggregate > last_progress:
                    last_progress = aggregate
                    chunk = make_thinking_chunk(
                        response_id,
                        spec.model_name,
                        _progress_reason("图片", aggregate, completed=completed, total=n),
                    )
                    yield f"data: {orjson.dumps(chunk).decode()}\n\n"

            images = await task
            for image in images:
                chunk = make_stream_chunk(
                    response_id,
                    spec.model_name,
                    _output_content(image, chat_format=chat_format),
                )
                yield f"data: {orjson.dumps(chunk).decode()}\n\n"

            final = make_stream_chunk(response_id, spec.model_name, "", is_final=True)
            yield f"data: {orjson.dumps(final).decode()}\n\n"
            yield "data: [DONE]\n\n"

        return _sse_stream()

    reasoning_updates: list[str] = []
    progress_map: dict[int, int] = {}
    images = await _run_lite_batch(
        spec=spec,
        prompt=prompt,
        n=n,
        timeout_s=timeout_s,
        response_format=response_format,
        progress_cb=lambda idx, progress: _lite_progress_updates(
            idx=idx,
            progress=progress,
            total=n,
            progress_map=progress_map,
            updates=reasoning_updates,
            enabled=chat_format,
        ),
    )
    if chat_format:
        content = "\n\n".join(image.markdown_value for image in images)
        reasoning = "\n".join(reasoning_updates) if reasoning_updates else None
        return make_chat_response(
            spec.model_name,
            content,
            prompt_content=prompt,
            response_id=response_id,
            reasoning_content=reasoning,
        )

    return {
        "created": int(time.time()),
        "data": [
            {"b64_json": image.api_value}
            if _normalize_response_format(response_format) == "b64_json"
            else {"url": image.api_value}
            for image in images
        ],
    }


# ---------------------------------------------------------------------------
# Image editing
# ---------------------------------------------------------------------------

_EDIT_MAX_REFERENCES = 5
_EDIT_DEFAULT_SIZE = "1024x1024"
_EDIT_MAX_N = 2
_EDIT_MAX_ATTEMPTS = 2


def _normalize_edit_inputs(image_inputs: list[str]) -> list[str]:
    """Validate and normalize image-edit reference inputs."""
    cleaned = [item.strip() for item in image_inputs if isinstance(item, str) and item.strip()]
    if not cleaned:
        raise ValidationError("Image edit requires at least one image_url content block", param="messages")
    return cleaned[-_EDIT_MAX_REFERENCES:]


def _normalize_edit_size(size: str) -> str:
    """Validate the only upstream-backed image-edit size."""
    normalized = (size or _EDIT_DEFAULT_SIZE).strip().lower()
    if normalized != _EDIT_DEFAULT_SIZE:
        raise ValidationError(
            f"image edit currently only supports size {_EDIT_DEFAULT_SIZE!r}",
            param="size",
        )
    return _EDIT_DEFAULT_SIZE


async def _prepare_edit_reference(token: str, image_input: str, index: int) -> str:
    """Upload one edit reference and resolve it to the upstream content URL."""
    try:
        file_id, file_uri = await upload_from_input(token, image_input)
        return resolve_uploaded_asset_reference(token, file_id, file_uri)
    except ValidationError as exc:
        raise ValidationError(exc.message, param=f"image.{index}") from exc
    except UpstreamError as exc:
        raise UpstreamError(
            f"Image edit reference {index + 1} upload failed: {exc.message}",
            status=exc.status,
            body=exc.details.get("body", ""),
        ) from exc
    except Exception as exc:
        raise UpstreamError(f"Image edit reference {index + 1} upload failed: {exc}") from exc


async def _prepare_edit_references(token: str, image_inputs: list[str]) -> list[str]:
    """Upload edit references concurrently and preserve caller order."""
    results: list[str | None] = [None] * len(image_inputs)

    async def _runner(index: int, image_input: str) -> None:
        results[index] = await _prepare_edit_reference(token, image_input, index)

    async with asyncio.TaskGroup() as tg:
        for index, image_input in enumerate(image_inputs):
            tg.create_task(_runner(index, image_input), name=f"image-edit-ref-{index}")

    return [result for result in results if result is not None]


def _extract_edit_prompt_and_inputs(messages: list[dict]) -> tuple[str, list[str]]:
    """Extract the final prompt and ordered image references from messages."""
    prompt = ""
    image_inputs: list[str] = []

    for msg in messages:
        content = msg.get("content", "")
        if isinstance(content, str) and content.strip():
            prompt = content.strip()
            continue
        if not isinstance(content, list):
            continue
        for block in content:
            if not isinstance(block, dict):
                continue
            btype = block.get("type")
            if btype == "text":
                text = (block.get("text") or "").strip()
                if text:
                    prompt = text
            elif btype == "image_url":
                url = (block.get("image_url") or {}).get("url", "")
                if url:
                    image_inputs.append(url)

    if not prompt:
        raise ValidationError("Image edit requires a non-empty text prompt", param="messages")
    return prompt, _normalize_edit_inputs(image_inputs)


def _absolutize_asset_url(url: str) -> str:
    """Resolve an assets URL/path to an absolute URL."""
    full_url, _, _ = resolve_download_url(url)
    return full_url


def _extract_user_id(token: str) -> str | None:
    cookie = build_sso_cookie(token)
    match = _X_USER_ID_RE.search(cookie)
    if match:
        return match.group(1)
    return None


def _resolve_edit_final_url(
    *,
    raw_url: str | None,
    asset_id: str | None,
    user_id: str | None,
) -> str | None:
    """Prefer stable asset content URLs over generated image paths for image-edit finals."""
    if asset_id and user_id:
        resolved = resolve_asset_reference(asset_id, "", user_id=user_id)
        if resolved:
            return resolved
    if raw_url:
        return _absolutize_asset_url(raw_url)
    return None


def _collect_edit_results(
    *,
    obj: dict[str, Any],
    final_urls: dict[int, str],
    user_id: str | None,
) -> None:
    """Update final edit image URLs from one parsed SSE payload."""
    stream = extract_streaming_response(obj)
    if stream:
        try:
            progress = int(stream.get("progress") or 0)
        except (TypeError, ValueError):
            progress = 0
        if progress >= 100 and not stream.get("moderated"):
            raw_url = stream.get("imageUrl")
            asset_id = stream.get("assetId")
            resolved_url = _resolve_edit_final_url(
                raw_url=raw_url if isinstance(raw_url, str) and raw_url else None,
                asset_id=asset_id if isinstance(asset_id, str) and asset_id else None,
                user_id=user_id,
            )
            if resolved_url:
                index = _parse_image_index(stream.get("imageIndex"))
                if index is not None:
                    final_urls[index] = resolved_url

    for index, asset_id in enumerate(extract_model_response_file_attachments(obj)):
        resolved_url = _resolve_edit_final_url(raw_url=None, asset_id=asset_id, user_id=user_id)
        if resolved_url:
            final_urls.setdefault(index, resolved_url)

    for index, url in enumerate(extract_model_response_urls(obj)):
        final_urls.setdefault(index, _absolutize_asset_url(url))


def _parse_image_index(value: Any) -> int | None:
    """Return a non-negative image index from upstream metadata."""
    try:
        index = int(value)
    except (TypeError, ValueError):
        return None
    return index if index >= 0 else None


async def _collect_edit_final_urls(
    *,
    token: str,
    prompt: str,
    image_references: list[str],
    parent_post_id: str,
    timeout_s: float,
    progress_cb: Callable[[int, int], Awaitable[None]] | None = None,
) -> dict[int, str]:
    """Collect final image URLs from the dedicated image-edit SSE stream."""
    final_urls: dict[int, str] = {}
    user_id = _extract_user_id(token)
    async for line in _stream_image_edit(
        token,
        prompt,
        image_references,
        parent_post_id,
        timeout_s=timeout_s,
    ):
        ev_type, data = classify_line(line)
        if ev_type == "done":
            break
        if ev_type != "data" or not data:
            continue
        try:
            obj = orjson.loads(data)
        except Exception:
            continue
        stream = extract_streaming_response(obj)
        if stream and progress_cb is not None:
            index = _parse_image_index(stream.get("imageIndex"))
            if index is not None:
                try:
                    progress = _clamp_progress(int(stream.get("progress") or 0))
                except (TypeError, ValueError):
                    progress = 0
                await progress_cb(index, progress)
        _collect_edit_results(obj=obj, final_urls=final_urls, user_id=user_id)

    if not final_urls:
        raise UpstreamError("Image edit returned no images")
    return final_urls


async def _collect_edit_images(
    *,
    token: str,
    prompt: str,
    image_references: list[str],
    parent_post_id: str,
    requested_n: int,
    response_format: str,
    timeout_s: float,
    progress_cb: Callable[[int, int], Awaitable[None]] | None = None,
) -> list[_ImageOutput]:
    """Collect up to *requested_n* edit results.

    Upstream image-edit requests are issued with a fixed batch size of 2.
    If one batch returns fewer usable finals than requested, issue one more
    attempt and merge unique results.
    """
    images: list[_ImageOutput] = []
    seen_urls: set[str] = set()

    for _ in range(_EDIT_MAX_ATTEMPTS):
        final_urls = await _collect_edit_final_urls(
            token=token,
            prompt=prompt,
            image_references=image_references,
            parent_post_id=parent_post_id,
            timeout_s=timeout_s,
            progress_cb=progress_cb,
        )
        for _, url in sorted(final_urls.items()):
            if url in seen_urls:
                continue
            seen_urls.add(url)
            images.append(
                await _resolve_image_output(
                    token=token,
                    url=url,
                    response_format=response_format,
                )
            )
            if len(images) >= requested_n:
                return images[:requested_n]

        if len(images) >= requested_n:
            break

    if len(images) < requested_n:
        logger.warning(
            "image edit returned fewer images than requested: requested={} received={}",
            requested_n, len(images),
        )
    return images[:requested_n]


async def _stream_image_edit(
    token: str,
    prompt: str,
    image_references: list[str],
    parent_post_id: str,
    *,
    timeout_s: float = 120.0,
) -> AsyncGenerator[str, None]:
    proxy = await get_proxy_runtime()
    lease = await proxy.acquire()
    payload = build_image_edit_payload(
        prompt=prompt,
        image_references=image_references,
        parent_post_id=parent_post_id,
    )
    headers = build_http_headers(
        token,
        lease=lease,
        origin="https://grok.com",
        referer=f"https://grok.com/imagine/post/{parent_post_id}",
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
                f"Image-edit upstream returned {response.status_code}",
                status=response.status_code,
                body=body,
            )
        async for line in response.aiter_lines():
            yield line


async def _stream_lite_generate(
    token:       str,
    message:     str,
    mode_id:     ModeId,
    *,
    timeout_s: float = 120.0,
) -> AsyncGenerator[str, None]:
    proxy   = await get_proxy_runtime()
    lease   = await proxy.acquire()
    payload = build_chat_payload(
        message           = f"Drawing: {message}",
        mode_id           = mode_id,
        file_attachments  = [],
        request_overrides = {"imageGenerationCount": 2},
    )
    headers = build_http_headers(token, lease=lease)
    kwargs  = build_session_kwargs(lease=lease)

    async with ResettableSession(**kwargs) as session:
        response = await session.post(
            CHAT,
            headers = headers,
            data    = orjson.dumps(payload),
            timeout = timeout_s,
            stream  = True,
        )
        if response.status_code != 200:
            body = response.content.decode("utf-8", "replace")[:300]
            raise UpstreamError(
                f"Image-generation upstream returned {response.status_code}",
                status = response.status_code,
                body   = body,
            )
        async for line in response.aiter_lines():
            yield line


async def _run_lite_request(
    *,
    spec:      ModelSpec,
    prompt:    str,
    timeout_s: float,
    response_format: str,
    progress_cb: Callable[[int], Awaitable[None]] | None = None,
) -> _ImageOutput:
    from app.dataplane.account import _directory as _acct_dir

    if _acct_dir is None:
        raise RateLimitError("Account directory not initialised")

    acct = await _acct_dir.reserve(
        pool_candidates = spec.pool_candidates(),
        mode_id         = int(spec.mode_id),
        now_s_override  = now_s(),
    )
    if acct is None:
        raise RateLimitError("No available accounts for image generation")

    token   = acct.token
    adapter = StreamAdapter()
    success = False
    fail_exc: BaseException | None = None
    try:
        async for line in _stream_lite_generate(
            token,
            prompt,
            spec.mode_id,
            timeout_s=timeout_s,
        ):
            ev_type, data = classify_line(line)
            if ev_type == "done":
                break
            if ev_type != "data" or not data:
                continue
            for ev in adapter.feed(data):
                if ev.kind == "image_progress":
                    if progress_cb is not None:
                        try:
                            await progress_cb(_clamp_progress(int(ev.content or "0")))
                        except ValueError:
                            pass
                if ev.kind == "image" and ev.content:
                    if progress_cb is not None:
                        await progress_cb(100)
                    image = await _resolve_image_output(
                        token=token,
                        url=ev.content,
                        response_format=response_format,
                    )
                    success = True
                    return image
        raise UpstreamError("Image generation returned no images")
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


async def _run_lite_batch(
    *,
    spec:      ModelSpec,
    prompt:    str,
    n: int,
    timeout_s: float,
    response_format: str,
    progress_cb: Callable[[int, int], Awaitable[None]] | None = None,
) -> list[_ImageOutput]:
    results: list[_ImageOutput | None] = [None] * n

    async def _runner(idx: int) -> None:
        results[idx] = await _run_lite_request(
            spec=spec,
            prompt=prompt,
            timeout_s=timeout_s,
            response_format=response_format,
            progress_cb=None if progress_cb is None else lambda progress: progress_cb(idx, progress),
        )

    async with asyncio.TaskGroup() as tg:
        for idx in range(n):
            tg.create_task(_runner(idx), name=f"lite-image-{idx}")

    return [result for result in results if result is not None]


async def edit(
    *,
    model:           str,
    messages:        list[dict],
    n:               int  = 1,
    size:            str  = "1024x1024",
    response_format: str  = "url",
    stream:          bool = False,
    chat_format:     bool = False,
) -> dict | AsyncGenerator[str, None]:
    """Edit images via media/post/create + imagine-image-edit chat payload."""
    cfg = get_config()
    spec = resolve_model(model)
    timeout_s = cfg.get_float("chat.timeout", 120.0)
    if not (1 <= n <= _EDIT_MAX_N):
        raise ValidationError("image edit n must be between 1 and 2", param="n")
    _normalize_edit_size(size)

    prompt, image_inputs = _extract_edit_prompt_and_inputs(messages)

    from app.dataplane.account import _directory as _acct_dir
    if _acct_dir is None:
        raise RateLimitError("Account directory not initialised")

    acct = await _acct_dir.reserve(
        pool_candidates = spec.pool_candidates(),
        mode_id         = int(spec.mode_id),
        now_s_override  = now_s(),
    )
    if acct is None:
        raise RateLimitError("No available accounts for image edit")

    token       = acct.token
    response_id = make_response_id()

    try:
        image_references = await _prepare_edit_references(token, image_inputs)
        if not image_references:
            raise UpstreamError("All image uploads failed; cannot proceed with image edit")

        post = await create_media_post(
            token,
            media_type=IMAGE_POST_MEDIA_TYPE,
            prompt=prompt,
        )
        post_data = post.get("post")
        if not isinstance(post_data, dict):
            raise UpstreamError("Image edit create-post returned no post payload")
        parent_post_id = str(post_data.get("id") or "").strip()
        if not parent_post_id:
            raise UpstreamError("Image edit create-post returned no post id")
    except Exception:
        await _acct_dir.release(acct)
        raise

    if stream:
        async def _sse_stream() -> AsyncGenerator[str, None]:
            success = False
            fail_exc: BaseException | None = None
            progress_map: dict[int, int] = {}
            last_progress = -1
            queue: asyncio.Queue[tuple[int, int]] = asyncio.Queue()
            try:
                async def _progress(index: int, progress: int) -> None:
                    progress_map[index] = _clamp_progress(progress)
                    await queue.put((
                        _compute_progress_percent(progress_map, n),
                        _completed_items(progress_map),
                    ))

                task = asyncio.create_task(
                    _collect_edit_images(
                        token=token,
                        prompt=prompt,
                        image_references=image_references,
                        parent_post_id=parent_post_id,
                        requested_n=n,
                        response_format=response_format,
                        timeout_s=timeout_s,
                        progress_cb=_progress,
                    )
                )
                while not task.done() or not queue.empty():
                    try:
                        aggregate, completed = await asyncio.wait_for(queue.get(), timeout=0.1)
                    except asyncio.TimeoutError:
                        continue
                    if chat_format and aggregate > last_progress:
                        last_progress = aggregate
                        chunk = make_thinking_chunk(
                            response_id,
                            model,
                            _progress_reason("图片", aggregate, completed=completed, total=n),
                        )
                        yield f"data: {orjson.dumps(chunk).decode()}\n\n"
                images = await task
                for image in images:
                    content = _output_content(image, chat_format=chat_format)
                    chunk   = make_stream_chunk(response_id, model, content)
                    yield f"data: {orjson.dumps(chunk).decode()}\n\n"

                final = make_stream_chunk(response_id, model, "", is_final=True)
                yield f"data: {orjson.dumps(final).decode()}\n\n"
                yield "data: [DONE]\n\n"
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

        return _sse_stream()

    success = False
    fail_exc: BaseException | None = None
    reasoning_updates: list[str] = []
    progress_map: dict[int, int] = {}
    try:
        async def _progress(index: int, progress: int) -> None:
            progress_map[index] = _clamp_progress(progress)
            if chat_format:
                _append_reason_update(
                    reasoning_updates,
                    "图片",
                    _compute_progress_percent(progress_map, n),
                    completed=_completed_items(progress_map),
                    total=n,
                )

        images = await _collect_edit_images(
            token=token,
            prompt=prompt,
            image_references=image_references,
            parent_post_id=parent_post_id,
            requested_n=n,
            response_format=response_format,
            timeout_s=timeout_s,
            progress_cb=_progress,
        )
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

    if chat_format:
        content = "\n\n".join(image.markdown_value for image in images)
        reasoning = "\n".join(reasoning_updates) if reasoning_updates else None
        return make_chat_response(
            model,
            content,
            prompt_content=prompt,
            response_id=response_id,
            reasoning_content=reasoning,
        )

    data_list = [
        {"b64_json": image.api_value}
        if _normalize_response_format(response_format) == "b64_json"
        else {"url": image.api_value}
        for image in images
    ]
    return {"created": int(time.time()), "data": data_list}


__all__ = ["generate", "edit", "resolve_aspect_ratio"]
