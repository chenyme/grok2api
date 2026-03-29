"""
Grok share-page image resolver.
"""

from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from html import unescape
from html.parser import HTMLParser
import inspect
import re
from typing import Any, List, Optional
from urllib.parse import parse_qs, urljoin, urlparse

from app.core.config import get_config
from app.core.exceptions import UpstreamException, ValidationException
from app.core.logger import logger
from app.core.proxy_pool import build_http_proxies, get_current_proxy_from
from app.services.grok.utils.process import _collect_images
from app.services.reverse.utils.session import ResettableSession

_VALID_SHARE_HOSTS = {"grok.com", "www.grok.com"}
_ASSET_CANDIDATE_PATTERNS = (
    ("assets", re.compile(r"https://assets\.grok\.com/users/[^\s\"'<>]+", re.I)),
    (
        "shared_assets",
        re.compile(r"https://assets\.grokusercontent\.com/users/[^\s\"'<>]+", re.I),
    ),
)
_PREVIEW_PATH_RE = re.compile(r"/(opengraph-image|twitter-image)/", re.I)


@dataclass
class ShareImageResolution:
    share_url: str
    image_url: str = ""
    source: str = ""
    expires_at: str = ""


class _ShareMetaParser(HTMLParser):
    def __init__(self, base_url: str):
        super().__init__(convert_charrefs=True)
        self.base_url = base_url
        self.meta: dict[str, str] = {}
        self.preload_images: List[str] = []

    def handle_starttag(self, tag: str, attrs):
        attr_map = {str(k).lower(): str(v) for k, v in attrs if k and v is not None}
        if tag.lower() == "meta":
            key = (attr_map.get("property") or attr_map.get("name") or "").strip().lower()
            content = (attr_map.get("content") or "").strip()
            if key and content:
                self.meta[key] = urljoin(self.base_url, content)
            return

        if tag.lower() == "link":
            rel = (attr_map.get("rel") or "").strip().lower()
            as_type = (attr_map.get("as") or "").strip().lower()
            href = (attr_map.get("href") or "").strip()
            if rel == "preload" and as_type == "image" and href:
                self.preload_images.append(urljoin(self.base_url, href))


def normalize_grok_share_url(raw_url: str) -> str:
    value = (raw_url or "").strip()
    if not value:
        raise ValidationException(
            message="share_url cannot be empty",
            param="share_url",
            code="empty_share_url",
        )

    parsed = urlparse(value)
    if parsed.scheme not in {"http", "https"} or parsed.hostname not in _VALID_SHARE_HOSTS:
        raise ValidationException(
            message="share_url must be a Grok share URL",
            param="share_url",
            code="invalid_share_url",
        )

    parts = [part for part in parsed.path.split("/") if part]
    if len(parts) < 2 or parts[0] != "share":
        raise ValidationException(
            message="share_url must point to /share/<id>",
            param="share_url",
            code="invalid_share_url",
        )

    share_id = parts[1].strip()
    if not share_id:
        raise ValidationException(
            message="share_url is missing share id",
            param="share_url",
            code="invalid_share_url",
        )

    return f"https://grok.com/share/{share_id}"


def _to_rfc3339(value: datetime) -> str:
    return value.astimezone(timezone.utc).isoformat().replace("+00:00", "Z")


def _extract_signed_expiry(raw_url: str) -> str:
    parsed = urlparse((raw_url or "").strip())
    if not parsed.scheme or not parsed.netloc:
        return ""

    query = parse_qs(parsed.query)

    se = (query.get("se") or [""])[0].strip()
    if se:
        try:
            return _to_rfc3339(datetime.fromisoformat(se.replace("Z", "+00:00")))
        except ValueError:
            pass

    for key in ("Expires", "expires", "exp"):
        raw = (query.get(key) or [""])[0].strip()
        if not raw:
            continue
        try:
            unix = int(raw)
        except ValueError:
            continue
        if unix > 0:
            return _to_rfc3339(datetime.fromtimestamp(unix, tz=timezone.utc))

    x_amz_date = (query.get("X-Amz-Date") or [""])[0].strip()
    x_amz_expires = (query.get("X-Amz-Expires") or [""])[0].strip()
    if x_amz_date and x_amz_expires:
        try:
            base = datetime.strptime(x_amz_date, "%Y%m%dT%H%M%SZ").replace(
                tzinfo=timezone.utc
            )
            ttl = int(x_amz_expires)
            if ttl > 0:
                return _to_rfc3339(base + timedelta(seconds=ttl))
        except ValueError:
            pass

    return ""


def _decode_escaped_text(html: str) -> str:
    return (
        unescape(html)
        .replace("\\u002F", "/")
        .replace("\\u002f", "/")
        .replace("\\u002E", ".")
        .replace("\\u002e", ".")
        .replace("\\/", "/")
    )


def _share_id_from_url(share_url: str) -> str:
    return share_url.rstrip("/").rsplit("/", 1)[-1].strip()


def _normalize_asset_url(raw_url: str) -> str:
    value = (raw_url or "").strip()
    if not value:
        return ""
    if value.startswith(("http://", "https://")):
        return value
    return f"https://assets.grok.com/{value.lstrip('/')}"


def _is_preview_url(url: str) -> bool:
    return bool(_PREVIEW_PATH_RE.search((url or "").strip()))


def _candidate_priority(source: str, image_url: str) -> int:
    url = (image_url or "").strip()
    source_key = (source or "").strip().lower()
    if not url:
        return 999
    if "assets.grok.com/users/" in url.lower():
        return 0
    if "assets.grokusercontent.com/users/" in url.lower():
        return 1
    if source_key == "public_json":
        return 2
    if source_key in {"assets", "shared_assets"}:
        return 3
    if source_key == "og:image":
        return 4
    if source_key == "twitter:image":
        return 5
    if source_key == "preload":
        return 6
    if not _is_preview_url(url):
        return 7
    return 8


def _pick_better_resolution(
    first: Optional[ShareImageResolution],
    second: Optional[ShareImageResolution],
) -> ShareImageResolution:
    if first and first.image_url and not second:
        return first
    if second and second.image_url and not first:
        return second
    if not first:
        return second or ShareImageResolution(share_url="")
    if not second:
        return first
    first_rank = _candidate_priority(first.source, first.image_url)
    second_rank = _candidate_priority(second.source, second.image_url)
    if second_rank < first_rank:
        return second
    return first


def _pick_best_candidate(share_url: str, html: str) -> ShareImageResolution:
    parser = _ShareMetaParser(share_url)
    parser.feed(html)

    normalized = _decode_escaped_text(html)
    seen: set[str] = set()
    candidates: List[tuple[int, str, str]] = []

    for source, pattern in _ASSET_CANDIDATE_PATTERNS:
        for match in pattern.finditer(normalized):
            url = match.group(0).rstrip("\\")
            if url and url not in seen:
                seen.add(url)
                candidates.append((0, source, url))

    for source, url in (
        ("og:image", parser.meta.get("og:image", "")),
        ("twitter:image", parser.meta.get("twitter:image", "")),
    ):
        if url and url not in seen:
            seen.add(url)
            candidates.append((1 if source == "og:image" else 2, source, url))

    for url in parser.preload_images:
        if url and url not in seen:
            seen.add(url)
            candidates.append((3, "preload", url))

    if not candidates:
        return ShareImageResolution(share_url=share_url)

    candidates.sort(key=lambda item: item[0])
    _, source, image_url = candidates[0]
    return ShareImageResolution(
        share_url=share_url,
        image_url=image_url,
        source=source,
        expires_at=_extract_signed_expiry(image_url),
    )


async def _fetch_public_share_payload(share_url: str) -> dict[str, Any]:
    timeout = get_config("image.timeout") or get_config("chat.timeout") or 30
    browser = get_config("proxy.browser")
    user_agent = str(get_config("proxy.user_agent") or "").strip() or (
        "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "
        "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"
    )
    share_id = _share_id_from_url(share_url)
    api = f"https://grok.com/rest/app-chat/share_links/{share_id}"
    headers = {
        "Accept": "application/json, text/plain, */*",
        "Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
        "Cache-Control": "no-cache",
        "Pragma": "no-cache",
        "Origin": "https://grok.com",
        "Referer": share_url,
        "User-Agent": user_agent,
    }

    async with ResettableSession(impersonate=browser) as session:
        _, proxy_url = get_current_proxy_from("proxy.base_proxy_url")
        proxies = build_http_proxies(proxy_url)
        response = await session.get(
            api,
            headers=headers,
            proxies=proxies,
            timeout=timeout,
            allow_redirects=True,
            impersonate=browser,
        )
        if response.status_code != 200:
            body = ""
            try:
                text_value = getattr(response, "text", "")
                if callable(text_value):
                    text_value = text_value()
                if inspect.isawaitable(text_value):
                    text_value = await text_value
                if isinstance(text_value, str):
                    body = text_value
            except Exception:
                pass
            raise UpstreamException(
                message=f"Share public API fetch failed, {response.status_code}",
                details={"status": response.status_code, "body": body[:1000]},
                code="share_public_api_fetch_failed",
            )
        payload = response.json()
        if isinstance(payload, dict):
            return payload
        return {}


async def _resolve_share_image_via_public_api(share_url: str) -> ShareImageResolution:
    payload = await _fetch_public_share_payload(share_url)
    for image_url in _collect_images(payload):
        direct_url = _normalize_asset_url(image_url)
        if not direct_url:
            continue
        return ShareImageResolution(
            share_url=share_url,
            image_url=direct_url,
            source="public_json",
            expires_at=_extract_signed_expiry(direct_url),
        )
    return ShareImageResolution(share_url=share_url, source="public_json")


async def _fetch_share_html(share_url: str) -> str:
    timeout = get_config("image.timeout") or get_config("chat.timeout") or 30
    browser = get_config("proxy.browser")
    user_agent = str(get_config("proxy.user_agent") or "").strip() or (
        "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "
        "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"
    )
    headers = {
        "Accept": "text/html,application/xhtml+xml",
        "Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
        "Cache-Control": "no-cache",
        "Pragma": "no-cache",
        "Referer": "https://grok.com/",
        "User-Agent": user_agent,
    }

    async with ResettableSession(impersonate=browser) as session:
        _, proxy_url = get_current_proxy_from("proxy.base_proxy_url")
        proxies = build_http_proxies(proxy_url)
        response = await session.get(
            share_url,
            headers=headers,
            proxies=proxies,
            timeout=timeout,
            allow_redirects=True,
            impersonate=browser,
        )
        if response.status_code != 200:
            body = ""
            try:
                text_value = getattr(response, "text", "")
                if callable(text_value):
                    text_value = text_value()
                if inspect.isawaitable(text_value):
                    text_value = await text_value
                if isinstance(text_value, str):
                    body = text_value
            except Exception:
                pass
            raise UpstreamException(
                message=f"Share page fetch failed, {response.status_code}",
                details={"status": response.status_code, "body": body[:1000]},
                code="share_page_fetch_failed",
            )
        text_value = getattr(response, "text", "")
        if callable(text_value):
            text_value = text_value()
        if inspect.isawaitable(text_value):
            text_value = await text_value
        if isinstance(text_value, str):
            return text_value

        content = getattr(response, "content", b"")
        if isinstance(content, (bytes, bytearray)):
            return bytes(content).decode("utf-8", "ignore")
        return str(content or "")


async def resolve_grok_share_image(raw_share_url: str) -> ShareImageResolution:
    share_url = normalize_grok_share_url(raw_share_url)
    public_result = ShareImageResolution(share_url=share_url)
    try:
        public_result = await _resolve_share_image_via_public_api(share_url)
        if _candidate_priority(public_result.source, public_result.image_url) <= 1:
            return public_result
    except Exception as exc:
        logger.debug("Share resolver public API path failed for {}: {}", share_url, exc)

    result = public_result
    try:
        html = await _fetch_share_html(share_url)
        result = _pick_better_resolution(result, _pick_best_candidate(share_url, html))
    except Exception:
        if result.image_url:
            return result
        raise

    if not result.image_url:
        logger.warning("No share image candidate found for {}", share_url)
    return result


__all__ = [
    "ShareImageResolution",
    "normalize_grok_share_url",
    "resolve_grok_share_image",
]
