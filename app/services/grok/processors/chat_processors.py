"""
聊天响应处理器
"""

import asyncio
import uuid
import re
from typing import Any, AsyncGenerator, AsyncIterable

import orjson

from app.core.config import get_config
from .base import (
    BaseProcessor,
    _with_idle_timeout,
    _normalize_stream_line,
    _collect_image_urls,
    _handle_upstream_error,
)


class StreamProcessor(BaseProcessor):
    """流式响应处理器"""

    def __init__(self, model: str, token: str = "", think: bool = None):
        super().__init__(model, token)
        self.response_id: str = None
        self.fingerprint: str = ""
        self.think_opened: bool = False
        self.role_sent: bool = False
        self.filter_tags = get_config("chat.filter_tags")
        self.image_format = get_config("app.image_format")
        self._tag_buffer: str = ""
        self._in_filter_tag: bool = False

        if think is None:
            self.show_think = get_config("chat.thinking")
        else:
            self.show_think = think

    def _filter_token(self, token: str) -> str:
        """过滤 token 中的特殊标签（如 <grok:render>...</grok:render>），支持跨 token 的标签过滤"""
        if not self.filter_tags:
            return token

        result = []
        i = 0
        while i < len(token):
            char = token[i]

            if self._in_filter_tag:
                self._tag_buffer += char
                if char == ">":
                    if "/>" in self._tag_buffer:
                        self._in_filter_tag = False
                        self._tag_buffer = ""
                    else:
                        for tag in self.filter_tags:
                            if f"</{tag}>" in self._tag_buffer:
                                self._in_filter_tag = False
                                self._tag_buffer = ""
                                break
                i += 1
                continue

            if char == "<":
                remaining = token[i:]
                tag_started = False
                for tag in self.filter_tags:
                    if remaining.startswith(f"<{tag}"):
                        tag_started = True
                        break
                    if len(remaining) < len(tag) + 1:
                        for j in range(1, len(remaining) + 1):
                            if f"<{tag}".startswith(remaining[:j]):
                                tag_started = True
                                break

                if tag_started:
                    self._in_filter_tag = True
                    self._tag_buffer = char
                    i += 1
                    continue

            result.append(char)
            i += 1

        return "".join(result)

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
            "system_fingerprint": self.fingerprint,
            "choices": [{"index": 0, "delta": delta, "logprobs": None, "finish_reason": finish}],
        }
        return f"data: {orjson.dumps(chunk).decode()}\n\n"

    async def process(self, response: AsyncIterable[bytes]) -> AsyncGenerator[str, None]:
        """处理流式响应"""
        idle_timeout = get_config("timeout.stream_idle_timeout")

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

                if (llm := resp.get("llmInfo")) and not self.fingerprint:
                    self.fingerprint = llm.get("modelHash", "")
                if rid := resp.get("responseId"):
                    self.response_id = rid

                if not self.role_sent:
                    yield self._sse(role="assistant")
                    self.role_sent = True

                # 图像生成进度
                if img := resp.get("streamingImageGenerationResponse"):
                    if self.show_think:
                        if not self.think_opened:
                            yield self._sse("<think>\n")
                            self.think_opened = True
                        idx = img.get("imageIndex", 0) + 1
                        progress = img.get("progress", 0)
                        yield self._sse(f"正在生成第{idx}张图片中，当前进度{progress}%\n")
                    continue

                # modelResponse
                if mr := resp.get("modelResponse"):
                    if self.think_opened and self.show_think:
                        if msg := mr.get("message"):
                            yield self._sse(msg + "\n")
                        yield self._sse("</think>\n")
                        self.think_opened = False

                    # 处理生成的图片
                    for url in _collect_image_urls(mr):
                        parts = url.split("/")
                        img_id = parts[-2] if len(parts) >= 2 else "image"
                        resolved = await self.resolve_image(url)
                        if resolved:
                            yield self._sse(f"![{img_id}]({resolved})\n")

                    if (meta := mr.get("metadata", {})).get("llm_info", {}).get("modelHash"):
                        self.fingerprint = meta["llm_info"]["modelHash"]
                    continue

                # 普通 token
                if (token := resp.get("token")) is not None:
                    if token:
                        filtered = self._filter_token(token)
                        if filtered:
                            yield self._sse(filtered)

            if self.think_opened:
                yield self._sse("</think>\n")
            yield self._sse(finish="stop")
            yield "data: [DONE]\n\n"
        except (asyncio.CancelledError, Exception) as e:
            _handle_upstream_error(e, self.model, "Stream")
        finally:
            await self.close()


class CollectProcessor(BaseProcessor):
    """非流式响应处理器"""

    def __init__(self, model: str, token: str = ""):
        super().__init__(model, token)
        self.image_format = get_config("app.image_format")
        self.filter_tags = get_config("chat.filter_tags")

    def _filter_content(self, content: str) -> str:
        """过滤内容中的特殊标签"""
        if not content or not self.filter_tags:
            return content

        result = content
        for tag in self.filter_tags:
            pattern = rf"<{re.escape(tag)}[^>]*>.*?</{re.escape(tag)}>|<{re.escape(tag)}[^>]*/>"
            result = re.sub(pattern, "", result, flags=re.DOTALL)

        return result

    async def process(self, response: AsyncIterable[bytes]) -> dict[str, Any]:
        """处理并收集完整响应"""
        response_id = ""
        fingerprint = ""
        content = ""
        idle_timeout = get_config("timeout.stream_idle_timeout")

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

                if (llm := resp.get("llmInfo")) and not fingerprint:
                    fingerprint = llm.get("modelHash", "")

                if mr := resp.get("modelResponse"):
                    response_id = mr.get("responseId", "")
                    parts = [mr.get("message", "")]

                    if urls := _collect_image_urls(mr):
                        parts.append("\n")
                        for url in urls:
                            segments = url.split("/")
                            img_id = segments[-2] if len(segments) >= 2 else "image"
                            resolved = await self.resolve_image(url)
                            if resolved:
                                parts.append(f"![{img_id}]({resolved})\n")

                    content = "".join(parts)

                    if (meta := mr.get("metadata", {})).get("llm_info", {}).get("modelHash"):
                        fingerprint = meta["llm_info"]["modelHash"]

        except (asyncio.CancelledError, Exception) as e:
            _handle_upstream_error(e, self.model, "Collect")
        finally:
            await self.close()

        content = self._filter_content(content)

        return {
            "id": response_id,
            "object": "chat.completion",
            "created": self.created,
            "model": self.model,
            "system_fingerprint": fingerprint,
            "choices": [
                {
                    "index": 0,
                    "message": {
                        "role": "assistant",
                        "content": content,
                        "refusal": None,
                        "annotations": [],
                    },
                    "finish_reason": "stop",
                }
            ],
            "usage": {
                "prompt_tokens": 0,
                "completion_tokens": 0,
                "total_tokens": 0,
                "prompt_tokens_details": {
                    "cached_tokens": 0,
                    "text_tokens": 0,
                    "audio_tokens": 0,
                    "image_tokens": 0,
                },
                "completion_tokens_details": {
                    "text_tokens": 0,
                    "audio_tokens": 0,
                    "reasoning_tokens": 0,
                },
            },
        }


__all__ = ["StreamProcessor", "CollectProcessor"]
