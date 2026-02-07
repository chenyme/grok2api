"""
视频响应处理器
"""

import asyncio
import uuid
from typing import Any, AsyncGenerator, AsyncIterable, Optional

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


class VideoStreamProcessor(BaseProcessor):
    """视频流式响应处理器"""

    def __init__(self, model: str, token: str = "", think: bool = None):
        super().__init__(model, token)
        self.response_id: Optional[str] = None
        self.think_opened: bool = False
        self.role_sent: bool = False
        self.video_format = str(get_config("app.video_format")).lower()

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
            "id": self.response_id or f"chatcmpl-{uuid.uuid4().hex[:24]}",
            "object": "chat.completion.chunk",
            "created": self.created,
            "model": self.model,
            "choices": [
                {"index": 0, "delta": delta, "logprobs": None, "finish_reason": finish}
            ],
        }
        return f"data: {orjson.dumps(chunk).decode()}\n\n"

    def _build_video_html(self, video_url: str, thumbnail_url: str = "") -> str:
        """构建视频 HTML 标签"""
        import html

        safe_video_url = html.escape(video_url)
        safe_thumbnail_url = html.escape(thumbnail_url)
        poster_attr = f' poster="{safe_thumbnail_url}"' if safe_thumbnail_url else ""
        return f'''<video id="video" controls="" preload="none"{poster_attr}>
  <source id="mp4" src="{safe_video_url}" type="video/mp4">
</video>'''

    async def process(
        self, response: AsyncIterable[bytes]
    ) -> AsyncGenerator[str, None]:
        """处理视频流式响应"""
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

                resp = data.get("result", {}).get("response", {})

                if rid := resp.get("responseId"):
                    self.response_id = rid

                if not self.role_sent:
                    yield self._sse(role="assistant")
                    self.role_sent = True

                # 视频生成进度
                if video_resp := resp.get("streamingVideoGenerationResponse"):
                    progress = video_resp.get("progress", 0)

                    if self.show_think:
                        if not self.think_opened:
                            yield self._sse("<think>\n")
                            self.think_opened = True
                        yield self._sse(f"正在生成视频中，当前进度{progress}%\n")

                    if progress == 100:
                        video_url = video_resp.get("videoUrl", "")
                        thumbnail_url = video_resp.get("thumbnailImageUrl", "")

                        if self.think_opened and self.show_think:
                            yield self._sse("</think>\n")
                            self.think_opened = False

                        if video_url:
                            final_video_url = await self.process_url(video_url, "video")
                            final_thumbnail_url = ""
                            if thumbnail_url:
                                final_thumbnail_url = await self.process_url(
                                    thumbnail_url, "image"
                                )

                            if self.video_format == "url":
                                yield self._sse(final_video_url)
                            else:
                                video_html = self._build_video_html(
                                    final_video_url, final_thumbnail_url
                                )
                                yield self._sse(video_html)

                            logger.info(f"Video generated: {video_url}")
                    continue

            if self.think_opened:
                yield self._sse("</think>\n")
            yield self._sse(finish="stop")
            yield "data: [DONE]\n\n"
        except asyncio.CancelledError:
            logger.debug(
                "Video stream cancelled by client", extra={"model": self.model}
            )
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
                logger.warning(
                    f"HTTP/2 stream error in video: {e}", extra={"model": self.model}
                )
                raise UpstreamException(
                    message="Upstream connection closed unexpectedly",
                    status_code=502,
                    details={"error": str(e), "type": "http2_stream_error"},
                )
            logger.error(
                f"Video stream request error: {e}", extra={"model": self.model}
            )
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
        finally:
            await self.close()


class VideoCollectProcessor(BaseProcessor):
    """视频非流式响应处理器"""

    def __init__(self, model: str, token: str = ""):
        super().__init__(model, token)
        self.video_format = str(get_config("app.video_format")).lower()

    def _build_video_html(self, video_url: str, thumbnail_url: str = "") -> str:
        poster_attr = f' poster="{thumbnail_url}"' if thumbnail_url else ""
        return f'''<video id="video" controls="" preload="none"{poster_attr}>
  <source id="mp4" src="{video_url}" type="video/mp4">
</video>'''

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

                resp = data.get("result", {}).get("response", {})

                if video_resp := resp.get("streamingVideoGenerationResponse"):
                    if video_resp.get("progress") == 100:
                        response_id = resp.get("responseId", "")
                        video_url = video_resp.get("videoUrl", "")
                        thumbnail_url = video_resp.get("thumbnailImageUrl", "")

                        if video_url:
                            final_video_url = await self.process_url(video_url, "video")
                            final_thumbnail_url = ""
                            if thumbnail_url:
                                final_thumbnail_url = await self.process_url(
                                    thumbnail_url, "image"
                                )

                            if self.video_format == "url":
                                content = final_video_url
                            else:
                                content = self._build_video_html(
                                    final_video_url, final_thumbnail_url
                                )
                            logger.info(f"Video generated: {video_url}")

        except asyncio.CancelledError:
            logger.debug(
                "Video collect cancelled by client", extra={"model": self.model}
            )
        except StreamIdleTimeoutError as e:
            logger.warning(
                f"Video collect idle timeout: {e}", extra={"model": self.model}
            )
        except RequestsError as e:
            if _is_http2_stream_error(e):
                logger.warning(
                    f"HTTP/2 stream error in video collect: {e}",
                    extra={"model": self.model},
                )
            else:
                logger.error(
                    f"Video collect request error: {e}", extra={"model": self.model}
                )
        except Exception as e:
            logger.error(
                f"Video collect processing error: {e}",
                extra={"model": self.model, "error_type": type(e).__name__},
            )
        finally:
            await self.close()

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
