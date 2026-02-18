"""
Download service.

Download service for assets.grok.com.
"""

import asyncio
import base64
import hashlib
import os
from pathlib import Path
from typing import List, Optional, Tuple
from urllib.parse import urlparse

import aiofiles

from app.core.logger import logger
from app.core.storage import DATA_DIR
from app.core.config import get_config
from app.core.exceptions import AppException
from app.services.reverse.assets_download import AssetsDownloadReverse
from app.services.reverse.utils.session import ResettableSession
from app.services.grok.utils.locks import _get_download_semaphore, _file_lock


class DownloadService:
    """Assets download service."""

    def __init__(self):
        self._session: Optional[ResettableSession] = None
        base_dir = DATA_DIR / "tmp"
        self.image_dir = base_dir / "image"
        self.video_dir = base_dir / "video"
        self.image_dir.mkdir(parents=True, exist_ok=True)
        self.video_dir.mkdir(parents=True, exist_ok=True)
        self._cleanup_running = False

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

    async def resolve_url(
        self, path_or_url: str, token: str, media_type: str = "image"
    ) -> str:
        asset_url = path_or_url
        path = path_or_url
        if path_or_url.startswith("http"):
            parsed = urlparse(path_or_url)
            path = parsed.path or ""
            asset_url = path_or_url
        else:
            if not path_or_url.startswith("/"):
                path_or_url = f"/{path_or_url}"
            path = path_or_url
            asset_url = f"https://assets.grok.com{path_or_url}"

        app_url = get_config("app.app_url")
        if app_url:
            await self.download_file(asset_url, token, media_type)
            return f"{app_url.rstrip('/')}/v1/files/{media_type}{path}"
        return asset_url

    async def render_image(
        self, url: str, token: str, image_id: str = "image"
    ) -> str:
        fmt = get_config("app.image_format")
        fmt = fmt.lower() if isinstance(fmt, str) else "url"
        if fmt not in ("base64", "url", "markdown"):
            fmt = "url"
        try:
            if fmt == "base64":
                data_uri = await self.parse_b64(url, token, "image")
                return f"![{image_id}]({data_uri})"
            final_url = await self.resolve_url(url, token, "image")
            return f"![{image_id}]({final_url})"
        except Exception as e:
            logger.warning(f"Image render failed, fallback to URL: {e}")
            final_url = await self.resolve_url(url, token, "image")
            return f"![{image_id}]({final_url})"

    async def render_video(
        self, video_url: str, token: str, thumbnail_url: str = ""
    ) -> str:
        fmt = get_config("app.video_format")
        fmt = fmt.lower() if isinstance(fmt, str) else "url"
        if fmt not in ("url", "markdown", "html"):
            fmt = "url"
        final_video_url = await self.resolve_url(video_url, token, "video")
        final_thumb_url = ""
        if thumbnail_url:
            final_thumb_url = await self.resolve_url(thumbnail_url, token, "image")
        if fmt == "url":
            return f"{final_video_url}\n"
        if fmt == "markdown":
            return f"[video]({final_video_url})"
        import html

        safe_video_url = html.escape(final_video_url)
        safe_thumbnail_url = html.escape(final_thumb_url)
        poster_attr = f' poster="{safe_thumbnail_url}"' if safe_thumbnail_url else ""
        return f'''<video id="video" controls="" preload="none"{poster_attr}>
  <source id="mp4" src="{safe_video_url}" type="video/mp4">
</video>'''

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

    async def parse_b64(self, file_path: str, token: str, media_type: str = "image") -> str:
        """Download and return data URI."""
        try:
            if not isinstance(file_path, str) or not file_path.strip():
                raise AppException("Invalid file path", code="invalid_file_path")
            if file_path.startswith("data:"):
                raise AppException("Invalid file path", code="invalid_file_path")
            if not self._is_url(file_path):
                raise AppException("Invalid file path", code="invalid_file_path")

            file_path = self._normalize_path(file_path)
            lock_name = f"dl_b64_{hashlib.sha1(file_path.encode()).hexdigest()[:16]}"
            lock_timeout = max(1, int(get_config("asset.download_timeout")))
            async with _get_download_semaphore():
                async with _file_lock(lock_name, timeout=lock_timeout):
                    session = await self.create()
                    response = await AssetsDownloadReverse.request(
                        session, token, file_path
                    )

            if hasattr(response, "aiter_content"):
                data = bytearray()
                async for chunk in response.aiter_content():
                    if chunk:
                        data.extend(chunk)
                raw = bytes(data)
            else:
                raw = response.content

            content_type = response.headers.get(
                "content-type", "application/octet-stream"
            ).split(";")[0]
            data_uri = f"data:{content_type};base64,{base64.b64encode(raw).decode()}"

            return data_uri
        except Exception as e:
            logger.error(f"Failed to convert {file_path} to base64: {e}")
            raise

    def _normalize_path(self, file_path: str) -> str:
        """Normalize file path for download."""
        if not isinstance(file_path, str) or not file_path.strip():
            raise AppException("Invalid file path", code="invalid_file_path")
        parsed = urlparse(file_path)
        if not (parsed.scheme and parsed.netloc and parsed.scheme in ["http", "https"]):
            raise AppException("Invalid file path", code="invalid_file_path")
        path = parsed.path or ""
        if parsed.query:
            path = f"{path}?{parsed.query}"
        file_path = path
        if not file_path.startswith("/"):
            file_path = f"/{file_path}"
        return file_path

    async def download_file(self, file_path: str, token: str, media_type: str = "image") -> Tuple[Optional[Path], str]:
        """Download asset to local cache.

        Args:
            file_path: str, the path of the file to download.
            token: str, the SSO token.
            media_type: str, the media type of the file.

        Returns:
            Tuple[Optional[Path], str]: The path of the downloaded file and the MIME type.
        """
        async with _get_download_semaphore():
            file_path = self._normalize_path(file_path)
            cache_dir = self.image_dir if media_type == "image" else self.video_dir
            filename = file_path.lstrip("/").replace("/", "-")
            cache_path = cache_dir / filename

            lock_name = (
                f"dl_{media_type}_{hashlib.sha1(str(cache_path).encode()).hexdigest()[:16]}"
            )
            lock_timeout = max(1, int(get_config("asset.download_timeout")))
            async with _file_lock(lock_name, timeout=lock_timeout):
                session = await self.create()
                response = await AssetsDownloadReverse.request(session, token, file_path)

                tmp_path = cache_path.with_suffix(cache_path.suffix + ".tmp")
                try:
                    async with aiofiles.open(tmp_path, "wb") as f:
                        if hasattr(response, "aiter_content"):
                            async for chunk in response.aiter_content():
                                if chunk:
                                    await f.write(chunk)
                        else:
                            await f.write(response.content)
                    os.replace(tmp_path, cache_path)
                finally:
                    if tmp_path.exists() and not cache_path.exists():
                        try:
                            tmp_path.unlink()
                        except Exception:
                            pass

                mime = response.headers.get(
                    "content-type", "application/octet-stream"
                ).split(";")[0]
                logger.info(f"Downloaded: {file_path}")

                asyncio.create_task(self._check_limit())

            return cache_path, mime

    async def _check_limit(self):
        """Check cache limit and cleanup.

        Args:
            self: DownloadService, the download service instance.

        Returns:
            None
        """
        if self._cleanup_running or not get_config("cache.enable_auto_clean"):
            return

        self._cleanup_running = True
        try:
            try:
                async with _file_lock("cache_cleanup", timeout=5):
                    limit_mb = get_config("cache.limit_mb")
                    total_size = 0
                    all_files: List[Tuple[Path, float, int]] = []

                    for d in [self.image_dir, self.video_dir]:
                        if d.exists():
                            for f in d.glob("*"):
                                if f.is_file():
                                    try:
                                        stat = f.stat()
                                        total_size += stat.st_size
                                        all_files.append(
                                            (f, stat.st_mtime, stat.st_size)
                                        )
                                    except Exception:
                                        pass
                    current_mb = total_size / 1024 / 1024

                    if current_mb <= limit_mb:
                        return

                    logger.info(
                        f"Cache limit exceeded ({current_mb:.2f}MB > {limit_mb}MB), cleaning..."
                    )
                    all_files.sort(key=lambda x: x[1])

                    deleted_count = 0
                    deleted_size = 0
                    target_mb = limit_mb * 0.8

                    for f, _, size in all_files:
                        try:
                            f.unlink()
                            deleted_count += 1
                            deleted_size += size
                            total_size -= size
                            if (total_size / 1024 / 1024) <= target_mb:
                                break
                        except Exception:
                            pass

                    logger.info(
                        f"Cache cleanup: {deleted_count} files ({deleted_size / 1024 / 1024:.2f}MB)"
                    )
            except Exception as e:
                logger.warning(f"Cache cleanup failed: {e}")
        finally:
            self._cleanup_running = False


__all__ = ["DownloadService"]
