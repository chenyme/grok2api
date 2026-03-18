"""
Videos API route (OpenAI-compatible create endpoint).
"""

import base64
import re
import time
import uuid
from typing import Any, Dict, List, Optional, Tuple

import orjson
from fastapi import APIRouter, Request, UploadFile
from fastapi.responses import JSONResponse
from pydantic import BaseModel, ConfigDict, Field, ValidationError
from starlette.datastructures import UploadFile as StarletteUploadFile

from app.core.exceptions import UpstreamException, ValidationException
from app.services.grok.services.model import ModelService
from app.services.grok.services.video import VideoService
from app.services.grok.services.video_extend import VideoExtendService


router = APIRouter(tags=["Videos"])

VIDEO_MODEL_ID = "grok-imagine-1.0-video"
SIZE_TO_ASPECT = {
    "1280x720": "16:9",
    "720x1280": "9:16",
    "1792x1024": "3:2",
    "1024x1792": "2:3",
    "1024x1024": "1:1",
}
QUALITY_TO_RESOLUTION = {
    "standard": "480p",
    "high": "720p",
}


class VideoCreateRequest(BaseModel):
    """Supported create params only; unknown fields are ignored by design."""

    model_config = ConfigDict(extra="ignore")

    prompt: str = Field(..., description="Video prompt")
    model: Optional[str] = Field(VIDEO_MODEL_ID, description="Model id")
    size: Optional[str] = Field("1792x1024", description="Output size")
    seconds: Optional[int] = Field(6, description="Video length in seconds")
    quality: Optional[str] = Field("standard", description="Quality: standard/high")
    image_reference: Optional[Any] = Field(
        None,
        description="Image references using chat/completions content-block array format: [{type:'image_url', image_url:{url:'...'}}] or an array of plain URL strings",
    )
    input_reference: Optional[Any] = Field(
        None, description="Multipart input reference file"
    )


class VideoExtendDirectRequest(BaseModel):
    """Direct extension params (non-OpenAI-compatible)."""

    model_config = ConfigDict(extra="ignore")

    prompt: str = Field(..., description="Prompt text mapped to message/originalPrompt")
    reference_id: str = Field(
        ...,
        description="Reference id mapped to extendPostId/originalPostId/parentPostId",
    )
    start_time: float = Field(..., description="Mapped to videoExtensionStartTime")
    ratio: str = Field("2:3", description="Mapped to aspectRatio")
    length: int = Field(6, description="Mapped to videoLength")
    resolution: str = Field("480p", description="Mapped to resolutionName")


def _raise_validation_error(exc: ValidationError) -> None:
    errors = exc.errors()
    if errors:
        first = errors[0]
        loc = first.get("loc", [])
        msg = first.get("msg", "Invalid request")
        code = first.get("type", "invalid_value")
        param_parts = [
            str(x) for x in loc if not (isinstance(x, int) or str(x).isdigit())
        ]
        param = ".".join(param_parts) if param_parts else None
        raise ValidationException(message=msg, param=param, code=code)
    raise ValidationException(message="Invalid request", code="invalid_value")


def _extract_video_url(content: str) -> str:
    if not isinstance(content, str) or not content.strip():
        return ""

    md_match = re.search(r"\[video\]\(([^)\s]+)\)", content)
    if md_match:
        return md_match.group(1).strip()

    html_match = re.search(r"""<source[^>]+src=["']([^"']+)["']""", content)
    if html_match:
        return html_match.group(1).strip()

    url_match = re.search(r"""https?://[^\s"'<>]+""", content)
    if url_match:
        return url_match.group(0).strip().rstrip(".,)")

    return ""


def _normalize_model(model: Optional[str]) -> str:
    requested = (model or VIDEO_MODEL_ID).strip()
    if requested != VIDEO_MODEL_ID:
        raise ValidationException(
            message=f"The model `{VIDEO_MODEL_ID}` is required for video generation.",
            param="model",
            code="model_not_supported",
        )
    model_info = ModelService.get(requested)
    if not model_info or not model_info.is_video:
        raise ValidationException(
            message=f"The model `{requested}` is not supported for video generation.",
            param="model",
            code="model_not_supported",
        )
    return requested


def _normalize_size(size: Optional[str]) -> Tuple[str, str]:
    value = (size or "1792x1024").strip()
    aspect_ratio = SIZE_TO_ASPECT.get(value)
    if not aspect_ratio:
        raise ValidationException(
            message=f"size must be one of {sorted(SIZE_TO_ASPECT.keys())}",
            param="size",
            code="invalid_size",
        )
    return value, aspect_ratio


def _normalize_quality(quality: Optional[str]) -> Tuple[str, str]:
    value = (quality or "standard").strip().lower()
    resolution = QUALITY_TO_RESOLUTION.get(value)
    if not resolution:
        raise ValidationException(
            message=f"quality must be one of {sorted(QUALITY_TO_RESOLUTION.keys())}",
            param="quality",
            code="invalid_quality",
        )
    return value, resolution


def _normalize_seconds(seconds: Optional[int]) -> int:
    value = int(seconds or 6)
    if value < 6 or value > 30:
        raise ValidationException(
            message="seconds must be between 6 and 30",
            param="seconds",
            code="invalid_seconds",
        )
    return value


def _validate_reference_value(value: str, param: str) -> str:
    candidate = (value or "").strip()
    if not candidate:
        return ""
    if candidate.startswith("http://") or candidate.startswith("https://"):
        return candidate
    if candidate.startswith("data:"):
        return candidate
    raise ValidationException(
        message=f"{param} must be a URL or data URI",
        param=param,
        code="invalid_reference",
    )


def _parse_image_reference_item(value: Any, idx: int) -> str:
    """Parse a single image reference item inside an array."""
    param_prefix = f"image_reference[{idx}]" if idx is not None else "image_reference"

    if isinstance(value, str):
        stripped = value.strip()
        if not stripped:
            raise ValidationException(
                message=f"{param_prefix} cannot be empty",
                param=param_prefix,
                code="invalid_reference",
            )
        return _validate_reference_value(stripped, param_prefix)

    if isinstance(value, dict):
        block_type = value.get("type")
        if block_type != "image_url":
            raise ValidationException(
                message=f'{param_prefix} must have type="image_url"',
                param=f"{param_prefix}.type",
                code="invalid_reference",
            )
        inner = value.get("image_url")
        if not isinstance(inner, dict):
            raise ValidationException(
                message=f"{param_prefix}.image_url must be an object with a url field",
                param=f"{param_prefix}.image_url",
                code="invalid_reference",
            )
        url = inner.get("url", "")
        if not isinstance(url, str) or not url.strip():
            raise ValidationException(
                message=f"{param_prefix}.image_url.url cannot be empty",
                param=f"{param_prefix}.image_url.url",
                code="invalid_reference",
            )
        return _validate_reference_value(url.strip(), f"{param_prefix}.image_url.url")

    raise ValidationException(
        message=(
            f"{param_prefix} must be a URL string or "
            f'{{"type": "image_url", "image_url": {{"url": "..."}}}}'
        ),
        param=param_prefix,
        code="invalid_reference",
    )


def _parse_image_references(value: Any) -> List[str]:
    """Parse image_reference into a list of validated URL strings.

    Uses the same content-block format as chat/completions.
    Accepts:
      - None / ""  -> []
      - ["url", {"type": "image_url", ...}, ...] -> [url, ...]
      - JSON string of an array (for multipart/form-data)
    """
    if value is None or value == "":
        return []

    if isinstance(value, str):
        stripped = value.strip()
        if not stripped:
            return []
        if stripped[0] == "[":
            try:
                value = orjson.loads(stripped)
            except orjson.JSONDecodeError:
                raise ValidationException(
                    message="image_reference must be a JSON array string",
                    param="image_reference",
                    code="invalid_reference",
                )
        else:
            raise ValidationException(
                message="image_reference must be an array",
                param="image_reference",
                code="invalid_reference",
            )

    if isinstance(value, list):
        if not value:
            return []
        return [
            _parse_image_reference_item(item, idx=i) for i, item in enumerate(value)
        ]

    raise ValidationException(
        message="image_reference must be an array",
        param="image_reference",
        code="invalid_reference",
    )


async def _upload_to_data_uri(file: UploadFile, param: str) -> str:
    payload = await file.read()
    if not payload:
        raise ValidationException(
            message=f"{param} upload is empty",
            param=param,
            code="empty_file",
        )
    content_type = (file.content_type or "application/octet-stream").strip()
    encoded = base64.b64encode(payload).decode()
    return f"data:{content_type};base64,{encoded}"


async def _build_references_for_json(payload: BaseModel) -> List[str]:
    references: List[str] = []
    parsed_refs = _parse_image_references(getattr(payload, "image_reference", None))
    references.extend(parsed_refs)
    if getattr(payload, "input_reference", None) not in (None, ""):
        raise ValidationException(
            message="input_reference must be uploaded as multipart/form-data file",
            param="input_reference",
            code="invalid_reference",
        )
    return references


async def _build_payload_and_references_for_form(
    *,
    schema: type[BaseModel],
    prompt: Optional[str],
    model: Optional[str],
    size: Optional[str],
    seconds: Optional[int],
    quality: Optional[str],
    image_reference: Optional[str],
    input_reference: Optional[UploadFile],
) -> Tuple[BaseModel, List[str]]:
    try:
        payload = schema.model_validate(
            {
                "prompt": prompt,
                "model": model,
                "size": size,
                "seconds": seconds,
                "quality": quality,
                "image_reference": image_reference,
                "input_reference": None,
            }
        )
    except ValidationError as exc:
        _raise_validation_error(exc)

    references: List[str] = []
    if isinstance(input_reference, (UploadFile, StarletteUploadFile)):
        references.append(await _upload_to_data_uri(input_reference, "input_reference"))
    elif input_reference not in (None, ""):
        raise ValidationException(
            message="input_reference must be a file in multipart/form-data",
            param="input_reference",
            code="invalid_reference",
        )

    parsed_refs = _parse_image_references(payload.image_reference)
    references.extend(parsed_refs)
    return payload, references


def _multipart_create_schema(default_seconds: int) -> Dict[str, Any]:
    return {
        "type": "object",
        "required": ["prompt"],
        "properties": {
            "prompt": {"type": "string"},
            "model": {"type": "string", "default": VIDEO_MODEL_ID},
            "size": {"type": "string", "default": "1792x1024"},
            "seconds": {"type": "integer", "default": default_seconds},
            "quality": {"type": "string", "default": "standard"},
            "image_reference": {
                "type": "string",
                "description": "JSON string for image_reference array",
            },
            "input_reference": {"type": "string", "format": "binary"},
        },
    }


def _build_create_response(
    *,
    model: str,
    prompt: str,
    size: str,
    seconds: int,
    quality: str,
    url: str,
) -> Dict[str, Any]:
    ts = int(time.time())
    return {
        "id": f"video_{uuid.uuid4().hex[:24]}",
        "object": "video",
        "created_at": ts,
        "completed_at": ts,
        "status": "completed",
        "model": model,
        "prompt": prompt,
        "size": size,
        "seconds": str(seconds),
        "quality": quality,
        "url": url,
    }


async def _create_video_from_payload(
    payload: BaseModel,
    references: List[str],
    *,
    require_extension: bool = False,
) -> JSONResponse:
    prompt = (payload.prompt or "").strip()
    if not prompt:
        raise ValidationException(
            message="prompt is required",
            param="prompt",
            code="invalid_request_error",
        )

    model = _normalize_model(payload.model)
    size, aspect_ratio = _normalize_size(payload.size)
    quality, resolution = _normalize_quality(payload.quality)
    seconds = _normalize_seconds(payload.seconds)
    if require_extension and seconds <= 6:
        raise ValidationException(
            message="seconds must be between 7 and 30 for /video/extend",
            param="seconds",
            code="invalid_seconds",
        )

    content: List[Dict[str, Any]] = [{"type": "text", "text": prompt}]
    for ref in references:
        content.append({"type": "image_url", "image_url": {"url": ref}})

    result = await VideoService.completions(
        model=model,
        messages=[{"role": "user", "content": content}],
        stream=False,
        reasoning_effort=None,
        aspect_ratio=aspect_ratio,
        video_length=seconds,
        resolution=resolution,
        preset="custom",
    )

    choices = result.get("choices") if isinstance(result, dict) else None
    if not isinstance(choices, list) or not choices:
        raise UpstreamException("Video generation failed: empty result")

    msg = choices[0].get("message", {}) if isinstance(choices[0], dict) else {}
    rendered = msg.get("content", "") if isinstance(msg, dict) else ""
    video_url = _extract_video_url(rendered)
    if not video_url:
        raise UpstreamException("Video generation failed: missing video URL")

    return JSONResponse(
        content=_build_create_response(
            model=model,
            prompt=prompt,
            size=size,
            seconds=seconds,
            quality=quality,
            url=video_url,
        )
    )


@router.post(
    "/videos",
    openapi_extra={
        "requestBody": {
            "required": True,
            "content": {
                "application/json": {"schema": VideoCreateRequest.model_json_schema()},
                "multipart/form-data": {"schema": _multipart_create_schema(6)},
            },
        }
    },
)
async def create_video(request: Request):
    """
    Videos create endpoint.
    Supports JSON and multipart/form-data using only reverse-supported params.
    """
    content_type = (request.headers.get("content-type") or "").lower()
    if "application/json" in content_type:
        try:
            raw = await request.json()
        except ValueError:
            raise ValidationException(
                message=(
                    "Invalid JSON in request body. Please check for trailing commas or syntax errors."
                ),
                param="body",
                code="json_invalid",
            )
        if not isinstance(raw, dict):
            raise ValidationException(
                message="Request body must be a JSON object",
                param="body",
                code="invalid_request_error",
            )
        try:
            payload = VideoCreateRequest.model_validate(raw)
        except ValidationError as exc:
            _raise_validation_error(exc)
        references = await _build_references_for_json(payload)
        return await _create_video_from_payload(
            payload, references, require_extension=False
        )

    form = await request.form()
    payload, references = await _build_payload_and_references_for_form(
        schema=VideoCreateRequest,
        prompt=form.get("prompt"),
        model=form.get("model"),
        size=form.get("size"),
        seconds=form.get("seconds"),
        quality=form.get("quality"),
        image_reference=form.get("image_reference"),
        input_reference=form.get("input_reference"),
    )
    return await _create_video_from_payload(
        payload, references, require_extension=False
    )


@router.post(
    "/video/extend",
)
async def extend_video(request: VideoExtendDirectRequest):
    """
    Extension endpoint (non-OpenAI-compatible direct mapping).
    """
    result = await VideoExtendService.extend(
        prompt=request.prompt,
        reference_id=request.reference_id,
        start_time=request.start_time,
        ratio=request.ratio,
        length=request.length,
        resolution=request.resolution,
    )
    return JSONResponse(content=result)


__all__ = ["router"]
