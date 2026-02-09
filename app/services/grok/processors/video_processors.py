"""
视频响应处理器
"""

import asyncio
import re
import uuid
from typing import Any, AsyncGenerator, AsyncIterable, Optional
from urllib.parse import urlparse

import orjson
from curl_cffi.requests.errors import RequestsError

from app.core.config import get_config
from app.core.logger import logger
from app.core.exceptions import UpstreamException
from .base import (
    BaseProcessor,
    StreamIdleTimeoutError,
    _with_idle_timeout,
    _normalize_stream_line,
    _is_http2_stream_error,
)

_URL_PATTERN = re.compile(r"https?://[^\s\"'<>]+", re.IGNORECASE)
_PLACEHOLDER_HOSTS = {
    "example.com",
    "www.example.com",
    "example.org",
    "www.example.org",
    "example.net",
    "www.example.net",
    "localhost",
    "127.0.0.1",
    "0.0.0.0",
}


def _first_non_empty(*values: Any) -> str:
    for value in values:
        if isinstance(value, str) and value.strip():
            return value.strip()
    return ""


def _pick_string_candidates(value: Any) -> list[str]:
    if isinstance(value, str) and value.strip():
        return [value.strip()]
    if isinstance(value, list):
        result: list[str] = []
        for item in value:
            if isinstance(item, str) and item.strip():
                result.append(item.strip())
        return result
    return []


def _collect_nested_strings(
    obj: Any, keys: set[str], depth: int = 0, max_depth: int = 6
) -> list[str]:
    if depth > max_depth:
        return []

    results: list[str] = []
    if isinstance(obj, dict):
        for key in keys:
            if key in obj:
                results.extend(_pick_string_candidates(obj.get(key)))
        for value in obj.values():
            results.extend(_collect_nested_strings(value, keys, depth + 1, max_depth))
    elif isinstance(obj, list):
        for item in obj:
            results.extend(_collect_nested_strings(item, keys, depth + 1, max_depth))

    return results


def _collect_url_like_strings(obj: Any, depth: int = 0, max_depth: int = 8) -> list[str]:
    if depth > max_depth:
        return []

    urls: list[str] = []
    if isinstance(obj, str):
        for matched in _URL_PATTERN.findall(obj):
            cleaned = matched.rstrip(".,;)\"]'")
            if cleaned:
                urls.append(cleaned)
        return urls

    if isinstance(obj, dict):
        for value in obj.values():
            urls.extend(_collect_url_like_strings(value, depth + 1, max_depth))
    elif isinstance(obj, list):
        for item in obj:
            urls.extend(_collect_url_like_strings(item, depth + 1, max_depth))

    return urls


def _collect_key_hint_urls(
    obj: Any,
    key_hints: tuple[str, ...],
    depth: int = 0,
    max_depth: int = 8,
) -> list[str]:
    if depth > max_depth:
        return []

    urls: list[str] = []
    if isinstance(obj, dict):
        for key, value in obj.items():
            lowered_key = str(key).lower()
            if any(hint in lowered_key for hint in key_hints):
                urls.extend(_pick_string_candidates(value))
                urls.extend(_collect_url_like_strings(value, depth + 1, max_depth))
            urls.extend(_collect_key_hint_urls(value, key_hints, depth + 1, max_depth))
    elif isinstance(obj, list):
        for item in obj:
            urls.extend(_collect_key_hint_urls(item, key_hints, depth + 1, max_depth))

    return urls


def _unique_preserve(values: list[str]) -> list[str]:
    result: list[str] = []
    seen = set()
    for value in values:
        if not value or value in seen:
            continue
        seen.add(value)
        result.append(value)
    return result


def _is_video_like_url(url: str) -> bool:
    value = (url or "").lower()
    return value.endswith(".mp4") or "/video" in value or "video" in value


def _is_image_like_url(url: str) -> bool:
    value = (url or "").lower()
    # 仅基于文件扩展名判断，不检查路径中的 /image 或 /images 目录名
    # 图生视频场景下 /images/ 路径最终会返回视频内容，实际类型由 strict_media MIME 校验兜底
    return value.endswith((".png", ".jpg", ".jpeg", ".webp"))


def _url_host(url: str) -> str:
    try:
        parsed = urlparse(url)
        return (parsed.hostname or "").lower()
    except Exception:
        return ""


def _is_assets_url(url: str) -> bool:
    host = _url_host(url)
    return host == "assets.grok.com" or host.endswith(".assets.grok.com")


def _is_placeholder_url(url: str) -> bool:
    host = _url_host(url)
    return host in _PLACEHOLDER_HOSTS


def _video_candidate_score(url: str) -> int:
    if not url:
        return -10_000
    if _is_placeholder_url(url):
        return -10_000

    value = url.lower()
    score = 0

    if _is_video_like_url(url):
        score += 120
    if _is_assets_url(url):
        score += 45
    if "/generated/" in value:
        score += 10
    if "/content" in value:
        score += 20
    if _is_image_like_url(url):
        score -= 200
    if not _has_meaningful_url_path(url):
        score -= 120

    return score


def _pick_best_video_url(candidates: list[str]) -> str:
    best_url = ""
    best_score = -10_000

    for candidate in candidates:
        score = _video_candidate_score(candidate)
        if score > best_score:
            best_score = score
            best_url = candidate

    if best_score <= 0:
        return ""
    return best_url


def _pick_best_url(candidates: list[str], predicate, allow_fallback: bool = True) -> str:
    for value in candidates:
        if predicate(value):
            return value
    if allow_fallback:
        return candidates[0] if candidates else ""
    return ""


def _normalize_url_candidate(raw: str) -> str:
    value = (raw or "").strip()
    if not value:
        return ""
    if value.startswith("http://") or value.startswith("https://"):
        return value
    if value.startswith("//"):
        return f"https:{value}"
    if value.startswith("/users/") or value.startswith("/videos/") or value.startswith("/images/"):
        return f"https://assets.grok.com{value}"
    if value.startswith("users/") or value.startswith("videos/") or value.startswith("images/"):
        return f"https://assets.grok.com/{value}"
    if value.startswith("/"):
        return f"https://assets.grok.com{value}"
    return value if "assets.grok.com" in value else ""


def _video_exclude_key(url: str) -> str:
    normalized = _normalize_url_candidate(url)
    if not normalized:
        return ""

    try:
        parsed = urlparse(normalized)
        host = (parsed.netloc or "").lower()
        path = (parsed.path or "").rstrip("/")
        if host and path:
            return f"{host}{path}".lower()
        return (path or normalized).lower()
    except Exception:
        value = normalized.lower()
        value = value.split("#", 1)[0]
        value = value.split("?", 1)[0]
        return value.rstrip("/")


def _remember_rejected_url(target: set[str], url: str) -> None:
    raw = (url or "").strip().lower()
    if raw:
        target.add(raw)
    key = _video_exclude_key(url)
    if key:
        target.add(key)


def _resolve_provisional_video_url(url: str) -> str:
    """将延迟 URL 规范化为可返回给客户端的候选下载链接。"""
    normalized = _normalize_url_candidate(url)
    if not normalized:
        return ""
    if _is_placeholder_url(normalized):
        return ""
    if not _is_assets_url(normalized):
        return ""
    if not _has_meaningful_url_path(normalized):
        return ""
    return normalized


def _build_video_output_url(raw_url: str, app_url: str) -> tuple[str, str]:
    """将上游资产 URL 转为对外输出 URL，并返回对应 assets 路径。"""
    normalized = _normalize_url_candidate(raw_url)
    if not normalized:
        return "", ""

    asset_path = ""
    try:
        parsed = urlparse(normalized)
        host = (parsed.netloc or "").lower()
        if host and "assets.grok.com" not in host:
            return normalized, ""
        asset_path = (parsed.path or "").strip() if parsed.scheme else normalized
    except Exception:
        asset_path = normalized

    if not asset_path:
        return normalized, ""

    if not asset_path.startswith("/"):
        asset_path = f"/{asset_path.lstrip('/')}"

    if app_url:
        return f"{app_url.rstrip('/')}/v1/files/video{asset_path}", asset_path
    return normalized, asset_path


def _spawn_background_task(coro, label: str) -> None:
    try:
        loop = asyncio.get_running_loop()
    except RuntimeError:
        try:
            coro.close()
        except Exception:
            pass
        return

    task = loop.create_task(coro)

    def _done(t: asyncio.Task) -> None:
        try:
            t.result()
        except Exception as e:
            logger.debug(f"{label} failed: {e}")

    task.add_done_callback(_done)


async def _warm_video_cache_until_ready(
    token: str,
    asset_path: str,
    wait_timeout: float,
    poll_interval: float,
    model: str = "",
    post_id: str = "",
) -> None:
    token_value = (token or "").strip()
    path_value = (asset_path or "").strip()
    if not token_value or not path_value:
        return

    if not path_value.startswith("/"):
        path_value = f"/{path_value.lstrip('/')}"

    deadline = asyncio.get_running_loop().time() + max(15.0, float(wait_timeout))
    sleep_sec = max(0.5, float(poll_interval))

    from app.services.grok.services.assets import DownloadService

    download_service = DownloadService()
    try:
        while asyncio.get_running_loop().time() < deadline:
            try:
                cache_path, mime = await download_service.download(
                    path_value,
                    token_value,
                    media_type="video",
                )
                if str(mime or "").lower().startswith("video/"):
                    logger.info(
                        "Video cache warmed",
                        extra={"model": model, "post_id": post_id, "path": path_value},
                    )
                    return

                if cache_path and cache_path.exists():
                    try:
                        cache_path.unlink()
                    except Exception:
                        pass
            except Exception as e:
                logger.debug(f"Video cache warm attempt failed: {e}")

            now = asyncio.get_running_loop().time()
            if now >= deadline:
                break
            await asyncio.sleep(min(sleep_sec, max(0.1, deadline - now)))

        logger.info(
            "Video cache warm timed out",
            extra={"model": model, "post_id": post_id, "path": path_value},
        )
    finally:
        await download_service.close()


def _schedule_video_cache_warm(token: str, asset_path: str, model: str, post_id: str) -> None:
    if not (token and asset_path):
        return

    wait_timeout, poll_interval = _video_result_wait_settings()
    _spawn_background_task(
        _warm_video_cache_until_ready(
            token,
            asset_path,
            wait_timeout=wait_timeout,
            poll_interval=poll_interval,
            model=model,
            post_id=post_id,
        ),
        "video cache warm",
    )


def _has_meaningful_url_path(url: str) -> bool:
    try:
        parsed = urlparse(url)
        path = (parsed.path or "").strip()
    except Exception:
        return False

    if not path or path == "/":
        return False

    normalized = path.rstrip("/")
    if not normalized:
        return False

    last_segment = normalized.split("/")[-1].lower()
    if last_segment in {"video", "videos", "image", "images", "users", "assets", "files", "file"}:
        return False

    return True


def _normalize_progress(raw: Any) -> float:
    if isinstance(raw, str):
        text = raw.strip()
        if not text:
            return 0.0

        percent_match = re.search(r"(\d+(?:\.\d+)?)\s*%", text)
        if percent_match:
            try:
                return max(float(percent_match.group(1)), 0.0)
            except (TypeError, ValueError):
                pass

    try:
        progress = float(raw)
    except (TypeError, ValueError):
        return 0.0
    # 小于 1 的值视为小数比例（0.5 → 50%）；>= 1 的值视为已是百分比
    # Grok 上游发送整数百分比 1, 5, 21, ..., 100，不应被乘以 100
    if 0 < progress < 1:
        progress *= 100
    return max(progress, 0.0)


def _resolve_video_payload(*contexts: Any) -> dict[str, Any]:
    keys = (
        "streamingVideoGenerationResponse",
        "videoGenerationResponse",
        "videoResponse",
        "video_response",
    )

    def walk(obj: Any, depth: int = 0, max_depth: int = 8) -> dict[str, Any]:
        if depth > max_depth:
            return {}
        if isinstance(obj, dict):
            for key in keys:
                payload = obj.get(key)
                if isinstance(payload, dict):
                    return payload
            for value in obj.values():
                found = walk(value, depth + 1, max_depth)
                if found:
                    return found
        elif isinstance(obj, list):
            for item in obj:
                found = walk(item, depth + 1, max_depth)
                if found:
                    return found
        return {}

    for context in contexts:
        found = walk(context)
        if found:
            return found

    return {}


def _extract_direct_video_url(video_resp: dict) -> str:
    """从视频响应 payload 直接提取 videoUrl，不做深度递归搜索。

    只检查 video_resp 自身的已知字段名，避免误提取源图/缩略图 URL。
    """
    if not isinstance(video_resp, dict):
        return ""
    # 标量字段优先
    for key in ("videoUrl", "videoURL"):
        val = video_resp.get(key)
        if isinstance(val, str) and val.strip():
            return val.strip()
    # 数组字段（videoUrls / generatedVideoUrls）
    for key in ("videoUrls", "videoURLs", "generatedVideoUrls"):
        val = video_resp.get(key)
        if isinstance(val, list):
            for item in val:
                if isinstance(item, str) and item.strip():
                    return item.strip()
    return ""


def _is_generation_done(progress: float, status_text: str, video_url: str) -> bool:
    if progress >= 100:
        return True
    if status_text in {
        "completed",
        "complete",
        "done",
        "finished",
        "success",
        "succeeded",
        "generated",
        "ready",
    }:
        return True
    # 仅当 URL 明确像视频（含 .mp4 / video 等）才认为完成
    # 避免将源图 /content URL 误判为生成结果
    if video_url and progress <= 0 and not status_text and _is_video_like_url(video_url):
        return True
    return False


def _extract_progress_status(
    video_resp: dict[str, Any],
    resp: dict[str, Any],
    result: dict[str, Any],
    data: dict[str, Any],
) -> tuple[float, str]:
    progress_raw: Any = None
    progress_candidates = [
        video_resp.get("progress") if isinstance(video_resp, dict) else None,
        resp.get("progress"),
        result.get("progress") if isinstance(result, dict) else None,
        data.get("progress") if isinstance(data, dict) else None,
    ]
    for candidate in progress_candidates:
        if candidate is None:
            continue
        if isinstance(candidate, (int, float)):
            progress_raw = candidate
            break
        if isinstance(candidate, str) and candidate.strip():
            progress_raw = candidate.strip()
            break

    progress = _normalize_progress(progress_raw)
    status_text = _first_non_empty(
        video_resp.get("status") if isinstance(video_resp, dict) else None,
        video_resp.get("state") if isinstance(video_resp, dict) else None,
        resp.get("status"),
        resp.get("state"),
        result.get("status") if isinstance(result, dict) else None,
        result.get("state") if isinstance(result, dict) else None,
        data.get("status") if isinstance(data, dict) else None,
        data.get("state") if isinstance(data, dict) else None,
    ).lower()
    return progress, status_text


def _status_hint_cn(status_text: str) -> str:
    if not status_text:
        return ""

    mapping = {
        "queued": "任务已排队，等待开始生成...",
        "pending": "任务准备中，等待上游处理...",
        "processing": "正在生成视频内容...",
        "running": "正在渲染视频帧...",
        "rendering": "正在渲染视频帧...",
        "generated": "视频已生成，正在整理结果...",
        "completed": "视频生成完成，正在整理下载链接...",
        "complete": "视频生成完成，正在整理下载链接...",
        "done": "视频生成完成，正在整理下载链接...",
        "success": "视频生成成功，正在整理下载链接...",
        "succeeded": "视频生成成功，正在整理下载链接...",
        "ready": "结果已就绪，正在输出下载链接...",
    }
    return mapping.get(status_text, f"视频状态：{status_text}")


def _extract_video_assets(*contexts: Any) -> tuple[str, str]:
    video_raw_candidates: list[str] = []
    image_raw_candidates: list[str] = []

    for context in contexts:
        if not context:
            continue

        video_raw_candidates.extend(
            _collect_nested_strings(
                context,
                {
                    "videoUrl",
                    "videoURL",
                    "videoUrls",
                    "generatedVideoUrls",
                    "downloadUrl",
                    "assetUrl",
                    "playbackUrl",
                    "signedUrl",
                    "fileUrl",
                    "fileUri",
                    "uri",
                    "contentUri",
                    "mediaUrl",
                },
            )
        )
        image_raw_candidates.extend(
            _collect_nested_strings(
                context,
                {
                    "thumbnailImageUrl",
                    "thumbnailUrl",
                    "thumbnailURL",
                    "posterUrl",
                    "previewImageUrl",
                    "coverUrl",
                    "imageUrl",
                },
            )
        )

        video_raw_candidates.extend(
            _collect_key_hint_urls(
                context,
                ("video", "download", "asset", "playback", "signed", "file"),
            )
        )
        image_raw_candidates.extend(
            _collect_key_hint_urls(
                context,
                ("thumbnail", "poster", "preview", "cover", "image"),
            )
        )

        all_urls = _collect_url_like_strings(context)
        # 通用 URL 扫描仅保留可信视频候选，避免把 prompt 内示例链接误判为结果
        for raw_url in all_urls:
            normalized = _normalize_url_candidate(raw_url)
            if not normalized or _is_placeholder_url(normalized):
                continue
            if _is_video_like_url(normalized) or _is_assets_url(normalized):
                video_raw_candidates.append(normalized)
                image_raw_candidates.append(normalized)

    video_candidates = _unique_preserve(
        [
            u
            for u in (_normalize_url_candidate(v) for v in video_raw_candidates)
            if u and _has_meaningful_url_path(u) and not _is_placeholder_url(u)
        ]
    )
    thumbnail_candidates = _unique_preserve(
        [
            u
            for u in (_normalize_url_candidate(v) for v in image_raw_candidates)
            if u and _has_meaningful_url_path(u) and not _is_placeholder_url(u)
        ]
    )

    video_url = _pick_best_video_url(video_candidates)
    thumbnail_url = _pick_best_url(thumbnail_candidates, _is_image_like_url)

    if thumbnail_url == video_url:
        thumbnail_url = ""

    return video_url, thumbnail_url


def _asset_matches_post_id(asset: Any, post_id: str) -> bool:
    needle = (post_id or "").strip().lower()
    if not needle:
        return False
    try:
        payload = orjson.dumps(asset).decode("utf-8", errors="ignore").lower()
    except Exception:
        payload = str(asset).lower()
    return needle in payload


def _asset_video_hint_score(asset: dict[str, Any]) -> int:
    payload = str(asset).lower()
    score = 0

    video_hints = (
        "video/mp4",
        "video/",
        ".mp4",
        ".webm",
        ".mov",
        "media_post_type_video",
        "mediaposttypevideo",
        "assettypevideo",
        "typevideo",
    )
    image_hints = (
        "image/png",
        "image/jpeg",
        "image/jpg",
        "image/webp",
        "image/",
        ".png",
        ".jpg",
        ".jpeg",
        ".webp",
        "media_post_type_image",
        "mediaposttypeimage",
        "assettypeimage",
        "typeimage",
    )

    for hint in video_hints:
        if hint in payload:
            score += 2
    for hint in image_hints:
        if hint in payload:
            score -= 2

    return score


def _url_contains_post_id(url: str, post_id: str) -> bool:
    value = (url or "").lower()
    needle = (post_id or "").strip().lower()
    if not value or not needle:
        return False
    return needle in value


def _pick_video_from_assets(
    assets: list[dict[str, Any]],
    post_id: str,
    exclude_urls: set[str] | None = None,
) -> tuple[str, str]:
    if not assets:
        return "", ""

    excluded = set()
    for raw_url in exclude_urls or set():
        value = str(raw_url).strip()
        if not value:
            continue
        excluded.add(value.lower())
        key = _video_exclude_key(value)
        if key:
            excluded.add(key)

    matched: list[tuple[int, tuple[str, str]]] = []
    fallback: list[tuple[int, tuple[str, str]]] = []
    for asset in assets:
        if not isinstance(asset, dict):
            continue

        video_url, thumbnail_url = _extract_video_assets(asset)
        if not video_url:
            continue

        video_url_lower = video_url.lower()
        video_url_key = _video_exclude_key(video_url)
        if video_url_lower in excluded or (video_url_key and video_url_key in excluded):
            continue

        hint_score = _asset_video_hint_score(asset)
        post_id_match = _asset_matches_post_id(asset, post_id) if post_id else False
        # 明确是图片资产，排除；但匹配 post_id 的保留（图生视频场景下内容会从图片变为视频）
        if hint_score < 0 and not post_id_match:
            continue

        # 不再预过滤模糊 /content URL —— 由调用方 strict_media 校验实际 MIME 类型
        # 这保证图生视频的 post_id /content（初始为图片，生成后变为视频）不会被误杀

        score = _video_candidate_score(video_url) + hint_score
        if score <= 0:
            continue

        entry = (video_url, thumbnail_url)
        fallback.append((score, entry))

        if post_id_match:
            post_bonus = 20
            if hint_score > 0:
                post_bonus = 80
            elif hint_score < 0:
                post_bonus = 5
            matched.append((score + post_bonus, entry))

    candidates = fallback + matched
    if candidates:
        candidates.sort(key=lambda item: item[0], reverse=True)
        return candidates[0][1]
    return "", ""


def _video_result_wait_settings() -> tuple[float, float]:
    wait_timeout = get_config("timeout.video_result_wait_timeout", 45.0)
    poll_interval = get_config("timeout.video_result_poll_interval", 1.0)

    try:
        wait_timeout = float(wait_timeout)
    except (TypeError, ValueError):
        wait_timeout = 45.0

    try:
        poll_interval = float(poll_interval)
    except (TypeError, ValueError):
        poll_interval = 1.0

    wait_timeout = max(0.0, wait_timeout)
    poll_interval = max(0.5, poll_interval)
    return wait_timeout, poll_interval


def _video_result_candidate_attempts() -> int:
    attempts = get_config("timeout.video_result_candidate_attempts", 3)
    try:
        attempts = int(attempts)
    except (TypeError, ValueError):
        attempts = 3
    return max(1, min(attempts, 8))


async def _poll_video_asset_url(
    token: str,
    post_id: str,
    wait_timeout: float,
    poll_interval: float,
) -> tuple[str, str]:
    token_value = (token or "").strip()
    post_value = (post_id or "").strip()
    if not token_value or not post_value or wait_timeout <= 0:
        return "", ""

    deadline = asyncio.get_running_loop().time() + wait_timeout
    while True:
        try:
            video_url, thumbnail_url = await _fetch_video_asset_once(token_value, post_value)
            if video_url:
                return video_url, thumbnail_url
        except Exception as e:
            logger.debug(f"Video assets polling failed: {e}")

        now = asyncio.get_running_loop().time()
        if now >= deadline:
            return "", ""

        await asyncio.sleep(min(poll_interval, max(0.1, deadline - now)))


async def _fetch_video_asset_once(
    token: str,
    post_id: str,
    exclude_urls: set[str] | None = None,
) -> tuple[str, str]:
    token_value = (token or "").strip()
    post_value = (post_id or "").strip()
    if not token_value or not post_value:
        return "", ""

    from app.services.grok.services.assets import ListService

    list_service = ListService()
    try:
        max_pages = get_config("timeout.video_result_scan_pages", 8)
        max_assets = get_config("timeout.video_result_scan_assets", 500)
        try:
            max_pages = max(1, int(max_pages))
        except Exception:
            max_pages = 8
        try:
            max_assets = max(20, int(max_assets))
        except Exception:
            max_assets = 500

        scanned_assets: list[dict[str, Any]] = []
        scanned_pages = 0
        async for page_assets in list_service.iter_assets(token_value):
            if isinstance(page_assets, list):
                scanned_assets.extend(page_assets)
            scanned_pages += 1

            if scanned_pages >= max_pages or len(scanned_assets) >= max_assets:
                break

        if len(scanned_assets) > max_assets:
            scanned_assets = scanned_assets[:max_assets]

        return _pick_video_from_assets(scanned_assets, post_value, exclude_urls=exclude_urls)
    finally:
        await list_service.close()


class VideoStreamProcessor(BaseProcessor):
    """视频流式响应处理器"""

    def __init__(self, model: str, token: str = "", think: bool = None, post_id: str = ""):
        super().__init__(model, token)
        self.response_id: Optional[str] = None
        self.fallback_id: str = f"chatcmpl-{uuid.uuid4().hex[:24]}"
        self.think_opened: bool = False
        self.role_sent: bool = False
        self.video_sent: bool = False
        self.last_progress: int = -1
        self.last_status_text: str = ""
        self._think_started_at: float | None = None
        self.video_format = str(get_config("app.video_format")).lower()
        self.post_id: str = (post_id or "").strip()

        if think is None:
            self.show_think = get_config("chat.thinking")
        else:
            self.show_think = think

    def _sse(self, content: str = "", role: str = None, finish: str = None) -> str:
        """构建 SSE 响应"""
        delta = {}
        if role:
            delta["role"] = role
            delta["content"] = ""
        elif content:
            delta["content"] = content

        chunk = {
            "id": self.response_id or self.fallback_id,
            "object": "chat.completion.chunk",
            "created": self.created,
            "model": self.model,
            "choices": [{"index": 0, "delta": delta, "logprobs": None, "finish_reason": finish}],
        }
        return f"data: {orjson.dumps(chunk).decode()}\n\n"

    def _render_video_content(self, video_url: str) -> str:
        """渲染视频内容，按后台配置选择格式"""
        if self.video_format == "url":
            return f"{video_url}\n"
        return f"🎬 视频已生成：[点击下载]({video_url})\n"

    def _get_video_min_think_duration(self) -> float:
        value = get_config("chat.video_think_min_sec", 2.5)
        try:
            value = float(value)
        except (TypeError, ValueError):
            value = 2.5
        return max(0.0, min(value, 10.0))

    def _ensure_think_open(self) -> str:
        if not (self.show_think and not self.think_opened):
            return ""
        self.think_opened = True
        self._think_started_at = asyncio.get_running_loop().time()
        return self._sse("<think>\n")

    async def _close_think_block(self) -> AsyncGenerator[str, None]:
        if not (self.think_opened and self.show_think):
            return

        min_duration = self._get_video_min_think_duration()
        if self._think_started_at is not None and min_duration > 0:
            now = asyncio.get_running_loop().time()
            remain = min_duration - max(0.0, now - self._think_started_at)
            if remain > 0:
                yield self._sse("正在整理输出结果...\n")
                while remain > 0:
                    sleep_for = min(remain, 1.0)
                    await asyncio.sleep(sleep_for)
                    remain -= sleep_for

        yield self._sse("</think>\n")
        self.think_opened = False
        self._think_started_at = None

    def _emit_progress_line(self, progress: int) -> str:
        progress = max(0, min(100, int(progress)))
        return self._sse(f"视频已生成{progress}%\n")

    async def process(self, response: AsyncIterable[bytes]) -> AsyncGenerator[str, None]:
        """处理视频流式响应，输出稳定的过程进度与最终下载链接"""
        idle_timeout = get_config("timeout.video_idle_timeout")
        _line_count = 0

        try:
            if not self.role_sent:
                yield self._sse(role="assistant")
                self.role_sent = True

            async for line in _with_idle_timeout(response, idle_timeout, self.model):
                line = _normalize_stream_line(line)
                if not line:
                    continue
                _line_count += 1
                try:
                    data = orjson.loads(line)
                except orjson.JSONDecodeError:
                    continue

                result = data.get("result", {})
                resp = result.get("response", {})

                if rid := resp.get("responseId"):
                    self.response_id = rid

                video_resp = _resolve_video_payload(resp, result, data)
                progress, status_text = _extract_progress_status(video_resp, resp, result, data)
                progress_int = max(0, min(100, int(progress)))

                if self.show_think and not self.video_sent:
                    if progress_int > 0 and progress_int > self.last_progress:
                        opened = self._ensure_think_open()
                        if opened:
                            yield opened
                        yield self._emit_progress_line(progress_int)
                        self.last_progress = progress_int

                    if status_text and status_text != self.last_status_text:
                        self.last_status_text = status_text

                # 直接从 videoUrl 字段提取，避免深度搜索误提取源图 URL
                video_url = _extract_direct_video_url(video_resp)
                done = _is_generation_done(progress, status_text, video_url)

                if done and not self.video_sent and not video_url:
                    # 进度完成但无 URL，记录上游返回的 video_resp 供排查
                    logger.debug(
                        "Video done but videoUrl empty",
                        extra={
                            "progress": progress,
                            "status": status_text,
                            "video_resp_keys": (
                                list(video_resp.keys()) if isinstance(video_resp, dict) else None
                            ),
                            "resp_keys": list(resp.keys()) if isinstance(resp, dict) else None,
                        },
                    )

                if done and not self.video_sent and video_url:
                    # 直接构建代理 URL，后台异步缓存（不阻塞等待 MIME 校验）
                    emitted_url, asset_path = _build_video_output_url(video_url, self.app_url)
                    if asset_path and self.token:
                        _schedule_video_cache_warm(self.token, asset_path, self.model, self.post_id)

                    if self.show_think:
                        opened = self._ensure_think_open()
                        if opened:
                            yield opened
                        if self.last_progress < 100:
                            yield self._emit_progress_line(100)
                            self.last_progress = 100
                        async for think_chunk in self._close_think_block():
                            yield think_chunk

                    yield self._sse(self._render_video_content(emitted_url or video_url))
                    self.video_sent = True
                    logger.info(f"Video generated: {video_url}")
                    yield self._sse(finish="stop")
                    yield "data: [DONE]\n\n"
                    return

            if not self.video_sent:
                if self.think_opened:
                    async for think_chunk in self._close_think_block():
                        yield think_chunk

                logger.warning(
                    "Video generation completed but no video url found in stream",
                    extra={
                        "model": self.model,
                        "last_progress": self.last_progress,
                        "last_status": self.last_status_text,
                        "stream_lines": _line_count,
                    },
                )
                yield self._sse("视频生成完成，但上游未返回可用下载链接，请稍后重试。\n")

            yield self._sse(finish="stop")
            yield "data: [DONE]\n\n"
        except asyncio.CancelledError:
            logger.debug("Video stream cancelled", extra={"model": self.model})
        except StreamIdleTimeoutError as e:
            raise UpstreamException(
                message=f"Video stream idle timeout after {e.idle_seconds}s",
                status_code=504,
                details={
                    "error": str(e),
                    "type": "stream_idle_timeout",
                    "idle_seconds": e.idle_seconds,
                },
            )
        except RequestsError as e:
            if _is_http2_stream_error(e):
                logger.warning(f"HTTP/2 stream error in video: {e}", extra={"model": self.model})
                raise UpstreamException(
                    message="Upstream connection closed unexpectedly",
                    status_code=502,
                    details={"error": str(e), "type": "http2_stream_error"},
                )
            logger.error(f"Video stream request error: {e}", extra={"model": self.model})
            raise UpstreamException(
                message=f"Upstream request failed: {e}",
                status_code=502,
                details={"error": str(e)},
            )
        except Exception as e:
            logger.error(
                f"Video stream processing error: {e}",
                extra={"model": self.model, "error_type": type(e).__name__},
            )
            # 确保 think block 关闭并输出兜底内容，避免客户端收到残缺 SSE 流
            if self.think_opened:
                yield self._sse("</think>\n")
                self.think_opened = False
            if not self.video_sent:
                yield self._sse("视频处理过程中发生错误，请稍后重试。\n")
            yield self._sse(finish="stop")
            yield "data: [DONE]\n\n"
        finally:
            await self.close()


class VideoCollectProcessor(BaseProcessor):
    """视频非流式响应处理器"""

    def __init__(self, model: str, token: str = "", post_id: str = ""):
        super().__init__(model, token)
        self.video_format = str(get_config("app.video_format")).lower()
        self.post_id: str = (post_id or "").strip()

    def _render_video_content(self, video_url: str) -> str:
        """渲染视频内容，按后台配置选择格式"""
        if self.video_format == "url":
            return video_url
        return f"🎬 视频已生成：[点击下载]({video_url})\n"

    async def process(self, response: AsyncIterable[bytes]) -> dict[str, Any]:
        """处理并收集视频响应"""
        response_id = ""
        content = ""
        idle_timeout = get_config("timeout.video_idle_timeout")

        try:
            async for line in _with_idle_timeout(response, idle_timeout, self.model):
                line = _normalize_stream_line(line)
                if not line:
                    continue
                try:
                    data = orjson.loads(line)
                except orjson.JSONDecodeError:
                    continue

                result = data.get("result", {})
                resp = result.get("response", {})
                response_id = resp.get("responseId", "") or response_id

                video_resp = _resolve_video_payload(resp, result, data)
                if not video_resp:
                    continue

                progress, status_text = _extract_progress_status(video_resp, resp, result, data)
                video_url = _extract_direct_video_url(video_resp)

                if _is_generation_done(progress, status_text, video_url) and video_url:
                    # 直接构建代理 URL，后台异步缓存
                    emitted_url, asset_path = _build_video_output_url(video_url, self.app_url)
                    if asset_path and self.token:
                        _schedule_video_cache_warm(self.token, asset_path, self.model, self.post_id)
                    content = self._render_video_content(emitted_url or video_url)
                    logger.info(f"Video generated: {video_url}")
                    break

        except asyncio.CancelledError:
            logger.debug("Video collect cancelled", extra={"model": self.model})
        except StreamIdleTimeoutError as e:
            logger.warning(f"Video collect idle timeout: {e}", extra={"model": self.model})
        except RequestsError as e:
            if _is_http2_stream_error(e):
                logger.warning(
                    f"HTTP/2 stream error in video collect: {e}",
                    extra={"model": self.model},
                )
            else:
                logger.error(f"Video collect request error: {e}", extra={"model": self.model})
        except Exception as e:
            logger.error(
                f"Video collect error: {e}",
                extra={"model": self.model, "error_type": type(e).__name__},
            )
        finally:
            await self.close()

        if not content:
            content = "视频生成完成，但上游未返回可用下载链接，请稍后重试。"

        return {
            "id": response_id,
            "object": "chat.completion",
            "created": self.created,
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


__all__ = ["VideoStreamProcessor", "VideoCollectProcessor"]
