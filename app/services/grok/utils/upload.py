"""
Upload service.

Upload service for assets.grok.com.
"""

import base64
import hashlib
import mimetypes
import re
from pathlib import Path
from typing import AsyncIterator, Optional, Tuple
from urllib.parse import urlparse

import aiofiles

from app.core.config import get_config
from app.core.exceptions import AppException, UpstreamException, ValidationException
from app.core.logger import logger
from app.core.storage import DATA_DIR
from app.services.reverse.assets_upload import AssetsUploadReverse
from app.services.reverse.utils.session import ResettableSession
from app.services.grok.utils.locks import _get_upload_semaphore, _file_lock


class UploadService:
    """Assets upload service."""

    def __init__(self):
        self._session: Optional[ResettableSession] = None
        self._chunk_size = 64 * 1024

    async def create(self) -> ResettableSession:
        """Create or reuse a session."""
        if self._session is None:
            self._session = ResettableSession()
        return self._session

    async def close(self):
        """Close the session."""
        if self._session:
            await self._session.close()
            self._session = None

    @staticmethod
    def _is_url(value: str) -> bool:
        """Check if the value is a URL."""
        try:
            parsed = urlparse(value)
            return bool(
                parsed.scheme and parsed.netloc and parsed.scheme in ["http", "https"]
            )
        except Exception:
            return False

    @staticmethod
    def _infer_mime(filename: str, fallback: str = "application/octet-stream") -> str:
        mime, _ = mimetypes.guess_type(filename)
        return mime or fallback

    @staticmethod
    async def _encode_b64_stream(chunks: AsyncIterator[bytes]) -> str:
        parts = []
        remain = b""
        async for chunk in chunks:
            if not chunk:
                continue
            chunk = remain + chunk
            keep = len(chunk) % 3
            if keep:
                remain = chunk[-keep:]
                chunk = chunk[:-keep]
            else:
                remain = b""
            if chunk:
                parts.append(base64.b64encode(chunk).decode())
        if remain:
            parts.append(base64.b64encode(remain).decode())
        return "".join(parts)

    async def _read_local_file(self, local_type: str, name: str) -> Tuple[str, str, str]:
        base_dir = DATA_DIR / "tmp"
        if local_type == "video":
            local_dir = base_dir / "video"
            mime = "video/mp4"
        else:
            local_dir = base_dir / "image"
            suffix = Path(name).suffix.lower()
            if suffix == ".png":
                mime = "image/png"
            elif suffix == ".webp":
                mime = "image/webp"
            elif suffix == ".gif":
                mime = "image/gif"
            else:
                mime = "image/jpeg"

        local_path = local_dir / name
        lock_name = f"ul_local_{hashlib.sha1(str(local_path).encode()).hexdigest()[:16]}"
        lock_timeout = max(1, int(get_config("asset.upload_timeout")))
        async with _file_lock(lock_name, timeout=lock_timeout):
            if not local_path.exists():
                raise ValidationException(f"Local file not found: {local_path}")
            if not local_path.is_file():
                raise ValidationException(f"Invalid local file: {local_path}")

            async def _iter_file() -> AsyncIterator[bytes]:
                async with aiofiles.open(local_path, "rb") as f:
                    while True:
                        chunk = await f.read(self._chunk_size)
                        if not chunk:
                            break
                        yield chunk

            b64 = await self._encode_b64_stream(_iter_file())
        filename = name or "file"
        return filename, b64, mime

    async def parse_b64(self, url: str) -> Tuple[str, str, str]:
        """Fetch URL content and return (filename, base64, mime)."""
        try:
            app_url = get_config("app.app_url") or ""
            if app_url and self._is_url(url):
                parsed = urlparse(url)
                app_parsed = urlparse(app_url)
                if (
                    parsed.scheme == app_parsed.scheme
                    and parsed.netloc == app_parsed.netloc
                    and parsed.path.startswith("/v1/files/")
                ):
                    parts = parsed.path.strip("/").split("/", 3)
                    if len(parts) >= 4:
                        local_type = parts[2]
                        name = parts[3].replace("/", "-")
                        return await self._read_local_file(local_type, name)

            lock_name = f"ul_url_{hashlib.sha1(url.encode()).hexdigest()[:16]}"
            timeout = float(get_config("asset.upload_timeout"))
            proxy_url = get_config("proxy.base_proxy_url")
            proxies = {"http": proxy_url, "https": proxy_url} if proxy_url else None

            lock_timeout = max(1, int(get_config("asset.upload_timeout")))
            async with _file_lock(lock_name, timeout=lock_timeout):
                session = await self.create()
                response = await session.get(
                    url, timeout=timeout, proxies=proxies, stream=True
                )
                if response.status_code >= 400:
                    raise UpstreamException(
                        message=f"Failed to fetch: {response.status_code}",
                        details={"url": url, "status": response.status_code},
                    )

                filename = url.split("/")[-1].split("?")[0] or "download"
                content_type = response.headers.get(
                    "content-type", ""
                ).split(";")[0].strip()
                if not content_type:
                    content_type = self._infer_mime(filename)
                if hasattr(response, "aiter_content"):
                    b64 = await self._encode_b64_stream(response.aiter_content())
                else:
                    b64 = base64.b64encode(response.content).decode()

                logger.debug(f"Fetched: {url}")
                return filename, b64, content_type
        except Exception as e:
            if isinstance(e, AppException):
                raise
            logger.error(f"Fetch failed: {url} - {e}")
            raise UpstreamException(f"Fetch failed: {str(e)}", details={"url": url})

    @staticmethod
    def format_b64(data_uri: str) -> Tuple[str, str, str]:
        """Format data URI to (filename, base64, mime)."""
        if not data_uri.startswith("data:"):
            raise ValidationException("Invalid file input: not a data URI")

        try:
            header, b64 = data_uri.split(",", 1)
        except ValueError:
            raise ValidationException("Invalid data URI format")

        if ";base64" not in header:
            raise ValidationException("Invalid data URI: missing base64 marker")

        mime = header[5:].split(";", 1)[0] or "application/octet-stream"
        b64 = re.sub(r"\s+", "", b64)
        if not mime or not b64:
            raise ValidationException("Invalid data URI: empty content")
        ext = mime.split("/")[-1] if "/" in mime else "bin"
        return f"file.{ext}", b64, mime

    async def check_format(self, file_input: str) -> Tuple[str, str, str]:
        """Check file input format and return (filename, base64, mime)."""
        if not isinstance(file_input, str) or not file_input.strip():
            raise ValidationException("Invalid file input: empty content")

        if self._is_url(file_input):
            return await self.parse_b64(file_input)

        if file_input.startswith("data:"):
            return self.format_b64(file_input)

        raise ValidationException("Invalid file input: must be URL or base64")

    async def upload_file(self, file_input: str, token: str) -> Tuple[str, str]:
        """
        Upload file to Grok.

        Args:
            file_input: str, the file input.
            token: str, the SSO token.

        Returns:
            Tuple[str, str]: The file ID and URI.
        """
        async with _get_upload_semaphore():
            filename, b64, mime = await self.check_format(file_input)

            logger.debug(
                f"Upload prepare: filename={filename}, type={mime}, size={len(b64)}"
            )

            if not b64:
                raise ValidationException("Invalid file input: empty content")

            session = await self.create()
            response = await AssetsUploadReverse.request(
                session,
                token,
                filename,
                mime,
                b64,
            )

            result = response.json()
            file_id = result.get("fileMetadataId", "")
            file_uri = result.get("fileUri", "")
            logger.info(f"Upload success: {filename} -> {file_id}")
            return file_id, file_uri


__all__ = ["UploadService"]
