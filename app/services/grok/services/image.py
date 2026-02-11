"""
Grok Imagine WebSocket image service.
"""

import asyncio
import certifi
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

WS_URL = "wss://grok.com/ws/imagine/listen"


class _BlockedError(Exception):
    pass


class ImageService:
    """Grok Imagine WebSocket image service."""

    def __init__(self):
        self._ssl_context = ssl.create_default_context()
        self._ssl_context.load_verify_locations(certifi.where())
        self._url_pattern = re.compile(r"/images/([a-f0-9-]+)\.(png|jpg|jpeg)")

    def _resolve_proxy(self) -> tuple[aiohttp.BaseConnector, Optional[str]]:
        proxy_url = get_config("network.base_proxy_url")
        if not proxy_url:
            return aiohttp.TCPConnector(ssl=self._ssl_context), None

        scheme = urlparse(proxy_url).scheme.lower()
        if scheme.startswith("socks"):
            logger.info(f"Using SOCKS proxy: {proxy_url}")
            return ProxyConnector.from_url(proxy_url, ssl=self._ssl_context), None

        logger.info(f"Using HTTP proxy: {proxy_url}")
        return aiohttp.TCPConnector(ssl=self._ssl_context), proxy_url

    def _get_ws_headers(self, token: str) -> Dict[str, str]:
        cookie = build_sso_cookie(token, include_rw=True)
        user_agent = get_config("security.user_agent")
        return {
            "Cookie": cookie,
            "Origin": "https://grok.com",
            "User-Agent": user_agent,
            "Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
            "Cache-Control": "no-cache",
            "Pragma": "no-cache",
        }

    def _extract_image_id(self, url: str) -> Optional[str]:
        match = self._url_pattern.search(url or "")
        return match.group(1) if match else None

    def _is_final_image(self, url: str, blob_size: int) -> bool:
        return (url or "").lower().endswith(
            (".jpg", ".jpeg")
        ) and blob_size > get_config("image.image_ws_final_min_bytes")

    def _classify_image(self, url: str, blob: str) -> Optional[Dict[str, object]]:
        if not url or not blob:
            return None

        image_id = self._extract_image_id(url) or uuid.uuid4().hex
        blob_size = len(blob)
        is_final = self._is_final_image(url, blob_size)

        stage = (
            "final"
            if is_final
            else (
                "medium"
                if blob_size > get_config("image.image_ws_medium_min_bytes")
                else "preview"
            )
        )

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
        retries = max(1, max_retries if max_retries is not None else 1)
        logger.info(
            f"Image generation: prompt='{prompt[:50]}...', n={n}, ratio={aspect_ratio}, nsfw={enable_nsfw}"
        )

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
                if yielded_any or attempt + 1 >= retries:
                    if not yielded_any:
                        yield {
                            "type": "error",
                            "error_code": "blocked",
                            "error": "blocked_no_final_image",
                        }
                    return
                logger.warning(f"WebSocket blocked, retry {attempt + 1}/{retries}")
            except Exception as e:
                logger.error(f"WebSocket stream failed: {e}")
                return

    async def _stream_once(
        self,
        token: str,
        prompt: str,
        aspect_ratio: str,
        n: int,
        enable_nsfw: bool,
    ) -> AsyncGenerator[Dict[str, object], None]:
        request_id = str(uuid.uuid4())
        headers = self._get_ws_headers(token)
        timeout = float(get_config("network.timeout"))
        blocked_seconds = float(get_config("image.image_ws_blocked_seconds"))

        try:
            connector, proxy = self._resolve_proxy()
        except Exception as e:
            logger.error(f"WebSocket proxy setup failed: {e}")
            return

        try:
            async with aiohttp.ClientSession(connector=connector) as session:
                async with session.ws_connect(
                    WS_URL,
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
                    logger.info(f"WebSocket request sent: {prompt[:80]}...")

                    images = {}
                    completed = 0
                    start_time = last_activity = time.time()
                    medium_received_time = None

                    while time.time() - start_time < timeout:
                        try:
                            ws_msg = await asyncio.wait_for(ws.receive(), timeout=5.0)
                        except asyncio.TimeoutError:
                            if (
                                medium_received_time
                                and completed == 0
                                and time.time() - medium_received_time
                                > min(10, blocked_seconds)
                            ):
                                raise _BlockedError()
                            if completed > 0 and time.time() - last_activity > 10:
                                logger.info(
                                    f"WebSocket idle timeout, collected {completed} images"
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

                                if (
                                    info["stage"] == "medium"
                                    and medium_received_time is None
                                ):
                                    medium_received_time = time.time()

                                if info["is_final"] and not existing.get("is_final"):
                                    completed += 1
                                    logger.debug(
                                        f"Final image received: id={image_id}, size={info['blob_size']}"
                                    )

                                images[image_id] = {
                                    "is_final": info["is_final"]
                                    or existing.get("is_final")
                                }
                                yield info

                            elif msg_type == "error":
                                logger.warning(
                                    f"WebSocket error: {msg.get('err_code', '')} - {msg.get('err_msg', '')}"
                                )
                                yield {
                                    "type": "error",
                                    "error_code": msg.get("err_code", ""),
                                    "error": msg.get("err_msg", ""),
                                }
                                return

                            if completed >= n:
                                logger.info(
                                    f"WebSocket collected {completed} final images"
                                )
                                break

                            if (
                                medium_received_time
                                and completed == 0
                                and time.time() - medium_received_time > blocked_seconds
                            ):
                                raise _BlockedError()

                        elif ws_msg.type in (
                            aiohttp.WSMsgType.CLOSED,
                            aiohttp.WSMsgType.ERROR,
                        ):
                            logger.warning(f"WebSocket closed/error: {ws_msg.type}")
                            yield {
                                "type": "error",
                                "error_code": "ws_closed",
                                "error": f"websocket closed: {ws_msg.type}",
                            }
                            break

        except aiohttp.ClientError as e:
            logger.error(f"WebSocket connection error: {e}")
            yield {"type": "error", "error_code": "connection_failed", "error": str(e)}


image_service = ImageService()

__all__ = ["image_service", "ImageService"]
