"""
响应处理器基类和通用工具
"""

import asyncio
import time
from typing import Any, AsyncGenerator, Optional, AsyncIterable, List, TypeVar

from app.core.config import get_config
from app.core.logger import logger
from app.services.grok.services.assets import DownloadService


ASSET_URL = "https://assets.grok.com/"

T = TypeVar("T")


def _is_http2_stream_error(e: Exception) -> bool:
    """检查是否为 HTTP/2 流错误"""
    err_str = str(e).lower()
    return "http/2" in err_str or "curl: (92)" in err_str or "stream" in err_str


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

    def add(url: str):
        if not url or url in seen:
            return
        seen.add(url)
        urls.append(url)

    def walk(value: Any):
        if isinstance(value, dict):
            for key, item in value.items():
                if key in {"generatedImageUrls", "imageUrls", "imageURLs"}:
                    if isinstance(item, list):
                        for url in item:
                            if isinstance(url, str):
                                add(url)
                    elif isinstance(item, str):
                        add(item)
                    continue
                walk(item)
        elif isinstance(value, list):
            for item in value:
                walk(item)

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

    async def process_url(self, path: str, media_type: str = "image") -> str:
        """处理资产 URL"""
        if path.startswith("http"):
            from urllib.parse import urlparse

            path = urlparse(path).path

        if not path.startswith("/"):
            path = f"/{path}"

        if self.app_url:
            dl_service = self._get_dl()
            await dl_service.download(path, self.token, media_type)
            return f"{self.app_url.rstrip('/')}/v1/files/{media_type}{path}"
        else:
            return f"{ASSET_URL.rstrip('/')}{path}"


__all__ = [
    "BaseProcessor",
    "StreamIdleTimeoutError",
    "_with_idle_timeout",
    "_normalize_stream_line",
    "_collect_image_urls",
    "_is_http2_stream_error",
]
