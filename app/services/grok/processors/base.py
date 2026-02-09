"""
响应处理器基类和通用工具
"""

import asyncio
import re
import time
from typing import Any, AsyncGenerator, Optional, AsyncIterable, List, TypeVar

from curl_cffi.requests.errors import RequestsError

from app.core.config import get_config
from app.core.exceptions import UpstreamException
from app.core.logger import logger
from app.services.grok.services.assets import DownloadService

ASSET_URL = "https://assets.grok.com/"

T = TypeVar("T")


def _is_http2_stream_error(e: Exception) -> bool:
    """检查是否为 HTTP/2 流错误"""
    err_str = str(e).lower()
    return "http/2" in err_str or "curl: (92)" in err_str or "stream" in err_str


def _handle_upstream_error(e: BaseException, model: str, operation: str = "Stream"):
    """处理上游错误，统一异常链

    处理 CancelledError（静默）、StreamIdleTimeoutError（504）、
    RequestsError（502）和其他异常（重新抛出）。
    """
    if isinstance(e, asyncio.CancelledError):
        logger.debug(f"{operation} cancelled by client", extra={"model": model})
        return
    if isinstance(e, StreamIdleTimeoutError):
        raise UpstreamException(
            message=f"{operation} idle timeout after {e.idle_seconds}s",
            status_code=504,
            details={
                "error": str(e),
                "type": "stream_idle_timeout",
                "idle_seconds": e.idle_seconds,
            },
        ) from e
    if isinstance(e, RequestsError):
        if _is_http2_stream_error(e):
            logger.warning(
                f"HTTP/2 stream error in {operation.lower()}: {e}",
                extra={"model": model},
            )
            raise UpstreamException(
                message="Upstream connection closed unexpectedly",
                status_code=502,
                details={"error": str(e), "type": "http2_stream_error"},
            ) from e
        logger.error(f"{operation} request error: {e}", extra={"model": model})
        raise UpstreamException(
            message=f"Upstream request failed: {e}",
            status_code=502,
            details={"error": str(e)},
        ) from e
    logger.error(
        f"{operation} processing error: {e}",
        extra={"model": model, "error_type": type(e).__name__},
    )
    raise


def _normalize_stream_line(line: Any) -> Optional[str]:
    """规范化流式响应行，兼容 SSE data 前缀与空行"""
    if line is None:
        return None
    if isinstance(line, (bytes, bytearray)):
        text = line.decode("utf-8", errors="ignore")
    else:
        text = str(line)
    text = text.strip()
    if not text:
        return None
    if text.startswith("data:"):
        text = text[5:].strip()
    if text == "[DONE]":
        return None
    return text


def _collect_image_urls(obj: Any) -> List[str]:
    """递归收集响应中的图片 URL"""
    urls: List[str] = []
    seen = set()

    url_pattern = re.compile(r"https?://[^\s\"'<>]+", re.IGNORECASE)

    def normalize(raw: str) -> str:
        value = (raw or "").strip().strip("\"'")
        if not value:
            return ""
        if value.startswith("//"):
            return f"https:{value}"
        if value.startswith(("/users/", "/images/", "/image/", "/assets/")):
            return f"https://assets.grok.com{value}"
        if value.startswith(("users/", "images/", "image/", "assets/")):
            return f"https://assets.grok.com/{value}"
        return value

    def is_probable_image_url(url: str) -> bool:
        value = (url or "").lower()
        if not (value.startswith("http://") or value.startswith("https://")):
            return False
        if ".mp4" in value or "/video" in value:
            return False
        if value.endswith((".png", ".jpg", ".jpeg", ".webp", ".gif", ".bmp", ".avif")):
            return True
        if "/image" in value or "/images/" in value:
            return True
        if "assets.grok.com" in value:
            return True
        return False

    def add(raw: str):
        url = normalize(raw)
        if not url or url in seen or not is_probable_image_url(url):
            return
        seen.add(url)
        urls.append(url)

    def add_from_text(text: str):
        for matched in url_pattern.findall(text or ""):
            cleaned = matched.rstrip(".,;)\"]'")
            if cleaned:
                add(cleaned)

    def walk(value: Any):
        if isinstance(value, dict):
            for key, item in value.items():
                key_lower = str(key).lower()
                if key_lower in {
                    "generatedimageurls",
                    "generatedimageurl",
                    "imageurls",
                    "imageurl",
                    "image_urls",
                }:
                    if isinstance(item, list):
                        for url in item:
                            if isinstance(url, str):
                                add(url)
                    elif isinstance(item, str):
                        add(item)
                    continue

                if any(hint in key_lower for hint in ("image", "thumbnail", "poster")):
                    if isinstance(item, str):
                        add(item)
                    elif isinstance(item, list):
                        for candidate in item:
                            if isinstance(candidate, str):
                                add(candidate)

                if key_lower in {"message", "content", "text"} and isinstance(
                    item, str
                ):
                    add_from_text(item)

                walk(item)
        elif isinstance(value, list):
            for item in value:
                walk(item)
        elif isinstance(value, str) and (
            "assets.grok.com" in value.lower()
            or "http://" in value
            or "https://" in value
        ):
            add_from_text(value)

    walk(obj)
    return urls


class StreamIdleTimeoutError(Exception):
    """流空闲超时错误"""

    def __init__(self, idle_seconds: float):
        self.idle_seconds = idle_seconds
        super().__init__(f"Stream idle timeout after {idle_seconds}s")


async def _with_idle_timeout(
    iterable: AsyncIterable[T], idle_timeout: float, model: str = ""
) -> AsyncGenerator[T, None]:
    """
    包装异步迭代器，添加空闲超时检测

    Args:
        iterable: 原始异步迭代器
        idle_timeout: 空闲超时时间(秒)，0 表示禁用
        model: 模型名称(用于日志)
    """
    if idle_timeout <= 0:
        async for item in iterable:
            yield item
        return

    iterator = iterable.__aiter__()
    while True:
        try:
            item = await asyncio.wait_for(iterator.__anext__(), timeout=idle_timeout)
            yield item
        except asyncio.TimeoutError:
            logger.warning(
                f"Stream idle timeout after {idle_timeout}s",
                extra={"model": model, "idle_timeout": idle_timeout},
            )
            raise StreamIdleTimeoutError(idle_timeout)
        except StopAsyncIteration:
            break


class BaseProcessor:
    """基础处理器"""

    def __init__(self, model: str, token: str = ""):
        self.model = model
        self.token = token
        self.created = int(time.time())
        self.app_url = get_config("app.app_url")
        self._dl_service: Optional[DownloadService] = None

    def _get_dl(self) -> DownloadService:
        """获取下载服务实例（复用）"""
        if self._dl_service is None:
            self._dl_service = DownloadService()
        return self._dl_service

    async def close(self):
        """释放下载服务资源"""
        if self._dl_service:
            await self._dl_service.close()
            self._dl_service = None

    async def process_url(
        self,
        path: str,
        media_type: str = "image",
        strict_media: bool = False,
    ) -> str:
        """处理资产 URL"""
        if path.startswith("http"):
            from urllib.parse import urlparse

            parsed = urlparse(path)
            host = (parsed.netloc or "").lower()

            # 非 Grok 资产域名：直接返回原始 URL，避免错误改写为本地 /v1/files/*/
            if host and "assets.grok.com" not in host:
                return path

            path = parsed.path

            # 防止根路径被拼成本地无效文件链接（如 /v1/files/video/）
            if not path or path == "/":
                return path or ""

        if not path.startswith("/"):
            path = f"/{path}"

        if self.app_url:
            dl_service = self._get_dl()
            try:
                cache_path, mime = await dl_service.download(
                    path, self.token, media_type
                )
            except Exception as dl_err:
                logger.warning(
                    "Asset download failed, skipping",
                    extra={"model": self.model, "path": path, "error": str(dl_err)},
                )
                return ""

            if strict_media:
                mime_lower = (mime or "").lower()
                expected_prefix = "video/" if media_type == "video" else "image/"
                if not mime_lower.startswith(expected_prefix):
                    try:
                        if cache_path and cache_path.exists():
                            cache_path.unlink()
                    except Exception:
                        pass

                    log_kwargs = {
                        "extra": {
                            "model": self.model,
                            "requested": media_type,
                            "mime": mime,
                            "path": path,
                        }
                    }
                    if media_type == "video" and "/content" in path:
                        logger.info(
                            "Video asset not ready yet, waiting for next poll",
                            **log_kwargs,
                        )
                    else:
                        logger.warning(
                            "Media type mismatch while processing asset URL",
                            **log_kwargs,
                        )
                    return ""

            return f"{self.app_url.rstrip('/')}/v1/files/{media_type}{path}"
        else:
            return f"{ASSET_URL.rstrip('/')}{path}"

    async def resolve_image(self, url: str, image_format: str = None) -> str:
        """解析图片 URL，支持 base64 转换并自动降级到 URL"""
        fmt = image_format or getattr(self, "image_format", "url")
        if fmt == "base64":
            try:
                dl_service = self._get_dl()
                base64_data = await dl_service.to_base64(url, self.token, "image")
                if base64_data:
                    return base64_data
            except Exception as e:
                logger.warning(
                    f"Failed to convert image to base64, falling back to URL: {e}"
                )
        return await self.process_url(url, "image")


__all__ = [
    "BaseProcessor",
    "StreamIdleTimeoutError",
    "_with_idle_timeout",
    "_normalize_stream_line",
    "_collect_image_urls",
    "_is_http2_stream_error",
    "_handle_upstream_error",
]
