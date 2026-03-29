"""
响应处理器基类和通用工具
"""

import asyncio
import json
import re
import time
from dataclasses import dataclass
from typing import Any, AsyncGenerator, Optional, AsyncIterable, List, TypeVar
from urllib.parse import urlparse

from app.core.config import get_config
from app.core.logger import logger
from app.core.exceptions import StreamIdleTimeoutError
from app.services.grok.utils.download import DownloadService


T = TypeVar("T")

_GENERATED_PATH_RE = re.compile(r"/generated/([^/?#]+)/")
_GENERATED_PART_SUFFIX_RE = re.compile(r"-part-\d+$")


@dataclass
class ImageCandidate:
    """Normalized image candidate extracted from Grok responses."""

    url: str
    key: str
    priority: int
    order: int
    source: str


def _is_http2_error(e: Exception) -> bool:
    """检查是否为 HTTP/2 流错误"""
    err_str = str(e).lower()
    return "http/2" in err_str or "curl: (92)" in err_str or "stream" in err_str


def _normalize_line(line: Any) -> Optional[str]:
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


def _image_candidate_path(url: str) -> str:
    if not isinstance(url, str):
        return ""
    value = url.strip()
    if not value:
        return ""
    if value.startswith("http://") or value.startswith("https://"):
        parsed = urlparse(value)
        path = parsed.path or ""
        if parsed.query:
            path = f"{path}?{parsed.query}"
        return path or value
    return value


def _image_candidate_key(url: str) -> str:
    path = _image_candidate_path(url)
    match = _GENERATED_PATH_RE.search(path)
    if match:
        return _GENERATED_PART_SUFFIX_RE.sub("", match.group(1))
    return path or url.strip()


def _is_preview_image_url(url: str) -> bool:
    path = _image_candidate_path(url)
    if not path:
        return False
    if re.search(r"/generated/[^/?#]+-part-\d+/", path):
        return True
    lowered = path.lower()
    return any(flag in lowered for flag in ("/preview/", "/thumbnail/", "/thumb/"))


def _is_final_image_url(url: str) -> bool:
    path = _image_candidate_path(url)
    if not path:
        return False
    if _is_preview_image_url(url):
        return False
    return bool(_GENERATED_PATH_RE.search(path))


def _image_candidate_priority(url: str, source: str) -> int:
    priority = {
        "card_original": 500,
        "original": 450,
        "generated_list": 300,
        "generic_original": 280,
        "generic_image_url": 220,
        "image_chunk": 120,
    }.get(source, 200)

    if _is_final_image_url(url):
        priority += 40
    if _is_preview_image_url(url):
        priority -= 120
    if "assets.grok.com" in url or "assets.grokusercontent.com" in url:
        priority += 20
    return priority


def _pick_preferred_image_candidate(
    existing: Optional[ImageCandidate], incoming: ImageCandidate
) -> ImageCandidate:
    if existing is None:
        return incoming

    if incoming.priority > existing.priority:
        incoming.order = existing.order
        return incoming
    if incoming.priority < existing.priority:
        return existing

    if not _is_preview_image_url(incoming.url) and _is_preview_image_url(existing.url):
        incoming.order = existing.order
        return incoming
    return existing


def _collect_image_candidates(obj: Any) -> List[ImageCandidate]:
    """Recursively collect image candidates and keep the best candidate per image."""
    best_by_key: dict[str, ImageCandidate] = {}
    next_order = 0

    def add(url: str, source: str):
        nonlocal next_order
        if not isinstance(url, str):
            return
        normalized = url.strip()
        if not normalized:
            return

        candidate = ImageCandidate(
            url=normalized,
            key=_image_candidate_key(normalized),
            priority=_image_candidate_priority(normalized, source),
            order=next_order,
            source=source,
        )
        existing = best_by_key.get(candidate.key)
        preferred = _pick_preferred_image_candidate(existing, candidate)
        if existing is None:
            best_by_key[candidate.key] = preferred
            next_order += 1
            return
        if preferred is not existing:
            best_by_key[candidate.key] = preferred

    def collect_card_attachment(value: Any, *, from_json_data: bool = False):
        if not isinstance(value, dict):
            return

        image_chunk = value.get("image_chunk") or {}
        if isinstance(image_chunk, dict):
            image_url = image_chunk.get("imageUrl")
            add(image_url, "image_chunk")

        image = value.get("image") or {}
        if isinstance(image, dict):
            original = image.get("original")
            add(original, "card_original" if from_json_data else "original")

    def parse_card_json(raw: Any):
        if not isinstance(raw, str) or not raw.strip():
            return
        try:
            collect_card_attachment(json.loads(raw), from_json_data=True)
        except Exception:
            return

    def walk(value: Any):
        if isinstance(value, dict):
            for key, item in value.items():
                if key in {"generatedImageUrls", "imageUrls", "imageURLs"}:
                    if isinstance(item, list):
                        for url in item:
                            add(url, "generated_list")
                    else:
                        add(item, "generated_list")
                    continue
                if key == "cardAttachmentsJson" and isinstance(item, list):
                    for raw in item:
                        parse_card_json(raw)
                    continue
                if key == "cardAttachment" and isinstance(item, dict):
                    json_data = item.get("jsonData")
                    if isinstance(json_data, str) and json_data.strip():
                        parse_card_json(json_data)
                    else:
                        collect_card_attachment(item)
                    continue
                if key == "imageUrl":
                    add(item, "generic_image_url")
                    continue
                if key == "original":
                    add(item, "generic_original")
                    continue
                walk(item)
        elif isinstance(value, list):
            for item in value:
                walk(item)

    walk(obj)
    return sorted(best_by_key.values(), key=lambda item: item.order)


def _collect_images(obj: Any) -> List[str]:
    """Collect best image URLs after candidate prioritization."""
    return [candidate.url for candidate in _collect_image_candidates(obj)]


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

    async def _maybe_aclose(it):
        aclose = getattr(it, "aclose", None)
        if not aclose:
            return
        try:
            await aclose()
        except Exception:
            pass

    while True:
        try:
            item = await asyncio.wait_for(iterator.__anext__(), timeout=idle_timeout)
            yield item
        except asyncio.TimeoutError:
            logger.warning(
                f"Stream idle timeout after {idle_timeout}s",
                extra={"model": model, "idle_timeout": idle_timeout},
            )
            await _maybe_aclose(iterator)
            raise StreamIdleTimeoutError(idle_timeout)
        except asyncio.CancelledError:
            await _maybe_aclose(iterator)
            raise
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
        dl_service = self._get_dl()
        return await dl_service.resolve_url(path, self.token, media_type)


__all__ = [
    "ImageCandidate",
    "BaseProcessor",
    "_with_idle_timeout",
    "_normalize_line",
    "_collect_image_candidates",
    "_collect_images",
    "_image_candidate_key",
    "_is_final_image_url",
    "_is_preview_image_url",
    "_is_http2_error",
    "_pick_preferred_image_candidate",
]
