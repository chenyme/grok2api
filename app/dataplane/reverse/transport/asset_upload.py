"""Asset upload transport — direct base64 upload to Grok.

Calls POST /rest/app-chat/upload-file with base64-encoded content and
returns the file metadata ID used as a file attachment reference in chat.
"""

import asyncio
import base64
import mimetypes
import re
from urllib.parse import urlparse

import orjson

from app.platform.logging.logger import logger
from app.platform.config.snapshot import get_config
from app.platform.errors import UpstreamError, ValidationError
from app.dataplane.proxy import get_proxy_runtime
from app.dataplane.proxy.adapters.headers import build_sso_cookie
from app.dataplane.proxy.adapters.headers import build_http_headers
from app.dataplane.proxy.adapters.session import ResettableSession, build_session_kwargs
from app.dataplane.reverse.protocol.xai_assets import resolve_asset_reference
from app.control.proxy.feedback import build_feedback
from app.control.proxy.models import ProxyFeedback, ProxyFeedbackKind

_UPLOAD_URL = "https://grok.com/rest/app-chat/upload-file"
_X_USER_ID_RE = re.compile(r"(?:^|;\s*)x-userid=([^;]+)")
_URL_FETCH_HEADERS = {
    "Accept": "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8",
}

# Global semaphore — limits concurrent upload_file() calls across all requests.
# Initialised lazily on first call so the event loop is guaranteed to be running.
_upload_sem: asyncio.Semaphore | None = None

def _get_upload_sem() -> asyncio.Semaphore:
    global _upload_sem
    if _upload_sem is None:
        n = max(1, int(get_config("batch.asset_upload_concurrency", 10)))
        _upload_sem = asyncio.Semaphore(n)
    return _upload_sem


# ---------------------------------------------------------------------------
# File-input parsing
# ---------------------------------------------------------------------------

def _is_url(value: str) -> bool:
    try:
        p = urlparse(value)
        return bool(p.scheme in {"http", "https"} and p.netloc)
    except Exception:
        return False


def _mime_from_name(filename: str, fallback: str = "application/octet-stream") -> str:
    mime, _ = mimetypes.guess_type(filename)
    return mime or fallback


def parse_data_uri(data_uri: str) -> tuple[str, str, str]:
    """Split a data URI into (filename, base64_content, mime_type).

    Raises ``ValidationError`` on invalid input.
    """
    if not data_uri.startswith("data:"):
        raise ValidationError("File input must be a URL or data URI", param="content")

    try:
        header, b64 = data_uri.split(",", 1)
    except ValueError:
        raise ValidationError("Malformed data URI: missing comma separator", param="content")

    if ";base64" not in header:
        raise ValidationError("Data URI must be base64-encoded", param="content")

    mime = header[5:].split(";", 1)[0].strip() or "application/octet-stream"
    b64  = re.sub(r"\s+", "", b64)
    if not b64:
        raise ValidationError("Data URI has empty payload", param="content")

    ext  = mime.split("/")[-1] if "/" in mime else "bin"
    return f"file.{ext}", b64, mime


# ---------------------------------------------------------------------------
# Core upload function
# ---------------------------------------------------------------------------

async def upload_file(
    token:    str,
    filename: str,
    mime:     str,
    b64:      str,
) -> tuple[str, str]:
    """Upload base64-encoded file content to Grok.

    Args:
        token:    SSO session token.
        filename: Original file name (used for content-type inference).
        mime:     MIME type string (e.g. ``"image/png"``).
        b64:      Base64-encoded file content (no data-URI prefix).

    Returns:
        ``(file_id, file_uri)`` — file_id is used as a file attachment ref.

    Raises:
        ``UpstreamError`` on HTTP failure.
    """
    async with _get_upload_sem():
        return await _upload_file_inner(token, filename, mime, b64)


async def _upload_file_inner(
    token:    str,
    filename: str,
    mime:     str,
    b64:      str,
) -> tuple[str, str]:
    cfg       = get_config()
    timeout_s = cfg.get_float("asset.upload_timeout", 60.0)

    proxy = await get_proxy_runtime()
    lease = await proxy.acquire()

    payload = orjson.dumps({
        "fileName":     filename,
        "fileMimeType": mime,
        "content":      b64,
    })
    headers = build_http_headers(token, lease=lease)
    kwargs  = build_session_kwargs(lease=lease)

    try:
        async with ResettableSession(**kwargs) as session:
            response = await session.post(
                _UPLOAD_URL,
                headers = headers,
                data    = payload,
                timeout = timeout_s,
            )

        body_bytes = response.content
        if response.status_code != 200:
            body_text = body_bytes.decode("utf-8", "replace")[:300]
            logger.error(
                "asset upload request failed: status={} body={}",
                response.status_code, body_text,
            )
            is_cloudflare = "just a moment" in body_text.lower()
            await proxy.feedback(
                lease,
                build_feedback(response.status_code, is_cloudflare=is_cloudflare),
            )
            raise UpstreamError(
                f"Asset upload returned {response.status_code}",
                status = response.status_code,
                body   = body_text,
            )

        await proxy.feedback(
            lease,
            ProxyFeedback(kind=ProxyFeedbackKind.SUCCESS, status_code=200),
        )

        result   = orjson.loads(body_bytes)
        file_id  = result.get("fileMetadataId") or result.get("fileId", "")
        file_uri = result.get("fileUri", "")
        logger.info("asset upload completed: filename={!r} file_id={}", filename, file_id)
        return file_id, file_uri

    except UpstreamError:
        raise
    except Exception as exc:
        await proxy.feedback(
            lease,
            ProxyFeedback(kind=ProxyFeedbackKind.TRANSPORT_ERROR),
        )
        raise UpstreamError(f"Asset upload transport error: {exc}") from exc


async def upload_from_input(token: str, file_input: str) -> tuple[str, str]:
    """High-level helper: parse *file_input* (URL or data URI) and upload.

    Returns ``(file_id, file_uri)``.
    """
    if _is_url(file_input):
        # Fetch the remote URL and re-upload as base64.
        proxy = await get_proxy_runtime()
        lease = await proxy.acquire()
        try:
            kwargs  = build_session_kwargs(lease=lease)
            async with ResettableSession(**kwargs) as session:
                resp = await session.get(
                    file_input,
                    headers=_URL_FETCH_HEADERS,
                    timeout=30.0,
                )
            raw  = resp.content
            if resp.status_code != 200:
                await proxy.feedback(
                    lease,
                    ProxyFeedback(
                        kind        = ProxyFeedbackKind.UPSTREAM_5XX if resp.status_code >= 500
                                      else ProxyFeedbackKind.FORBIDDEN,
                        status_code = resp.status_code,
                    ),
                )
                raise UpstreamError(
                    f"Failed to fetch input URL: {resp.status_code}",
                    status = resp.status_code,
                )
            mime     = (resp.headers.get("content-type", "").split(";")[0].strip()
                        or "application/octet-stream")
            filename = file_input.split("/")[-1].split("?")[0] or "download"
            b64      = base64.b64encode(raw).decode()
        except UpstreamError:
            raise
        except Exception as exc:
            await proxy.feedback(lease, ProxyFeedback(kind=ProxyFeedbackKind.TRANSPORT_ERROR))
            raise UpstreamError(f"Asset fetch transport error: {exc}") from exc

        await proxy.feedback(lease, ProxyFeedback(kind=ProxyFeedbackKind.SUCCESS))
        return await upload_file(token, filename, mime, b64)

    # Data URI
    filename, b64, mime = parse_data_uri(file_input)
    return await upload_file(token, filename, mime, b64)


def resolve_uploaded_asset_reference(token: str, file_id: str, file_uri: str) -> str:
    """Resolve an uploaded asset to the content URL required by image-edit."""
    user_id = _extract_user_id(token)
    url = resolve_asset_reference(file_id, file_uri, user_id=user_id)
    if url:
        return url
    raise UpstreamError("Could not resolve uploaded asset reference URL")


def _extract_user_id(token: str) -> str | None:
    cookie = build_sso_cookie(token)
    match = _X_USER_ID_RE.search(cookie)
    if match:
        return match.group(1)
    return None


__all__ = [
    "upload_file",
    "upload_from_input",
    "parse_data_uri",
    "resolve_uploaded_asset_reference",
]
