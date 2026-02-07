"""
Grok Imagine WebSocket image service.
"""

import asyncio
import json
import re
import ssl
import time
import uuid
from typing import AsyncGenerator, Dict, Optional
from urllib.parse import urlparse

import aiohttp
from aiohttp_socks import ProxyConnector

from app.core.config import get_config
from app.core.logger import logger
from app.services.grok.utils.headers import build_sso_cookie


TIMEOUT = 120
BLOCKED_SECONDS = 15
FINAL_MIN_BYTES = 100000
MEDIUM_MIN_BYTES = 30000

WS_URL = "wss://grok.com/ws/imagine/listen"

USER_AGENT = (
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
    "AppleWebKit/537.36 (KHTML, like Gecko) "
    "Chrome/131.0.0.0 Safari/537.36"
)


class _BlockedError(Exception):
    pass


class ImageService:
    """Grok Imagine WebSocket image service."""

    def __init__(self):
        self._ssl_context = ssl.create_default_context()
        self._url_pattern = re.compile(r"/images/([a-f0-9-]+)\.(png|jpg|jpeg)")

    def _get_ws_url(self) -> str:
        return WS_URL

    def _get_timeout(self) -> float:
        return float(get_config("grok.timeout", TIMEOUT))

    def _get_blocked_seconds(self) -> float:
        return float(get_config("grok.image_ws_blocked_seconds", BLOCKED_SECONDS))

    def _resolve_proxy(self) -> tuple[aiohttp.BaseConnector, Optional[str]]:
        proxy_url = get_config("grok.base_proxy_url", "")
        if not proxy_url:
            return aiohttp.TCPConnector(ssl=self._ssl_context), None

        scheme = urlparse(proxy_url).scheme.lower()
        if scheme.startswith("socks"):
            logger.info(f"Grok Imagine WebSocket using SOCKS proxy: {proxy_url}")
            return ProxyConnector.from_url(proxy_url, ssl=self._ssl_context), None

        logger.info(f"Grok Imagine WebSocket using HTTP proxy: {proxy_url}")
        return aiohttp.TCPConnector(ssl=self._ssl_context), proxy_url

    def _get_ws_headers(self, token: str) -> Dict[str, str]:
        cookie = build_sso_cookie(token, include_rw=True)
        return {
            "Cookie": cookie,
            "Origin": "https://grok.com",
            "User-Agent": USER_AGENT,
            "Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
            "Cache-Control": "no-cache",
            "Pragma": "no-cache",
        }

    def _extract_image_id(self, url: str) -> Optional[str]:
        match = self._url_pattern.search(url or "")
        if match:
            return match.group(1)
        return None

    def _is_final_image(self, url: str, blob_size: int) -> bool:
        min_bytes = int(get_config("grok.image_ws_final_min_bytes", FINAL_MIN_BYTES))
        return (url or "").lower().endswith((".jpg", ".jpeg")) and blob_size > min_bytes

    def _classify_image(self, url: str, blob: str) -> Optional[Dict[str, object]]:
        if not url or not blob:
            return None

        image_id = self._extract_image_id(url) or uuid.uuid4().hex
        blob_size = len(blob)
        is_final = self._is_final_image(url, blob_size)
        if is_final:
            stage = "final"
        elif blob_size > MEDIUM_MIN_BYTES:
            stage = "medium"
        else:
            stage = "preview"

        return {
            "type": "image",
            "image_id": image_id,
            "stage": stage,
            "blob": blob,
            "blob_size": blob_size,
            "url": url,
            "is_final": is_final,
        }

    async def stream(
        self,
        token: str,
        prompt: str,
        aspect_ratio: str = "2:3",
        n: int = 1,
        enable_nsfw: bool = True,
        max_retries: int = None,
    ) -> AsyncGenerator[Dict[str, object], None]:
        if max_retries is None:
            retries = 1
        else:
            retries = max_retries
        retries = max(1, retries)

        for attempt in range(retries):
            try:
                yielded_any = False
                async for item in self._stream_once(
                    token, prompt, aspect_ratio, n, enable_nsfw
                ):
                    yielded_any = True
                    yield item
                return
            except _BlockedError:
                if yielded_any:
                    return
                if attempt + 1 < retries:
                    logger.warning(
                        f"Grok Imagine WebSocket blocked, retry {attempt + 1}/{retries}"
                    )
                    continue
                yield {
                    "type": "error",
                    "error_code": "blocked",
                    "error": "blocked_no_final_image",
                }
                return
            except Exception as e:
                logger.error(f"Grok Imagine WebSocket stream failed: {e}")
                return

    async def _stream_once(
        self,
        token: str,
        prompt: str,
        aspect_ratio: str,
        n: int,
        enable_nsfw: bool,
    ) -> AsyncGenerator[Dict[str, object], None]:
        ws_url = self._get_ws_url()

        request_id = str(uuid.uuid4())
        headers = self._get_ws_headers(token)
        timeout = self._get_timeout()
        blocked_seconds = self._get_blocked_seconds()

        try:
            connector, proxy = self._resolve_proxy()
        except Exception as e:
            logger.error(f"Grok Imagine WebSocket proxy setup failed: {e}")
            return

        try:
            async with aiohttp.ClientSession(connector=connector) as session:
                async with session.ws_connect(
                    ws_url,
                    headers=headers,
                    heartbeat=20,
                    receive_timeout=timeout,
                    proxy=proxy,
                ) as ws:
                    message = {
                        "type": "conversation.item.create",
                        "timestamp": int(time.time() * 1000),
                        "item": {
                            "type": "message",
                            "content": [
                                {
                                    "requestId": request_id,
                                    "text": prompt,
                                    "type": "input_text",
                                    "properties": {
                                        "section_count": 0,
                                        "is_kids_mode": False,
                                        "enable_nsfw": enable_nsfw,
                                        "skip_upsampler": False,
                                        "is_initial": False,
                                        "aspect_ratio": aspect_ratio,
                                    },
                                }
                            ],
                        },
                    }

                    await ws.send_json(message)
                    logger.info(f"Grok Imagine WebSocket request sent: {prompt[:80]}...")

                    images: Dict[str, Dict[str, object]] = {}
                    completed = 0
                    start_time = time.time()
                    last_activity = time.time()
                    medium_received_time = None

                    while time.time() - start_time < timeout:
                        try:
                            ws_msg = await asyncio.wait_for(ws.receive(), timeout=5.0)
                        except asyncio.TimeoutError:
                            if medium_received_time and completed == 0:
                                if time.time() - medium_received_time > min(
                                    10, blocked_seconds
                                ):
                                    raise _BlockedError()
                            if completed > 0 and time.time() - last_activity > 10:
                                logger.info(
                                    f"Grok Imagine WebSocket idle timeout, collected {completed} images"
                                )
                                break
                            continue

                        if ws_msg.type == aiohttp.WSMsgType.TEXT:
                            last_activity = time.time()
                            msg = json.loads(ws_msg.data)
                            msg_type = msg.get("type")

                            if msg_type == "image":
                                info = self._classify_image(
                                    msg.get("url", ""), msg.get("blob", "")
                                )
                                if not info:
                                    continue

                                image_id = info["image_id"]
                                existing = images.get(image_id, {})
                                if info["stage"] == "medium" and medium_received_time is None:
                                    medium_received_time = time.time()

                                if info["is_final"] and not existing.get("is_final"):
                                    completed += 1

                                images[image_id] = {
                                    "is_final": info["is_final"] or existing.get("is_final")
                                }

                                yield info

                            elif msg_type == "error":
                                error_code = msg.get("err_code", "")
                                error_msg = msg.get("err_msg", "")
                                logger.warning(
                                    f"Grok Imagine WebSocket error: {error_code} - {error_msg}"
                                )
                                yield {
                                    "type": "error",
                                    "error_code": error_code,
                                    "error": error_msg,
                                }
                                return

                            if completed >= n:
                                logger.info(
                                    f"Grok Imagine WebSocket collected {completed} final images"
                                )
                                break

                            if medium_received_time and completed == 0:
                                if time.time() - medium_received_time > blocked_seconds:
                                    raise _BlockedError()

                        elif ws_msg.type in (
                            aiohttp.WSMsgType.CLOSED,
                            aiohttp.WSMsgType.ERROR,
                        ):
                            logger.warning(
                                f"Grok Imagine WebSocket closed/error: {ws_msg.type}"
                            )
                            yield {
                                "type": "error",
                                "error_code": "ws_closed",
                                "error": f"websocket closed: {ws_msg.type}",
                            }
                            break

        except aiohttp.ClientError as e:
            logger.error(f"Grok Imagine WebSocket connection error: {e}")
            yield {
                "type": "error",
                "error_code": "connection_failed",
                "error": str(e),
            }


image_service = ImageService()


__all__ = ["image_service", "ImageService"]
