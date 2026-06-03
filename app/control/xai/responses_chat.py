"""OpenAI Chat Completions ↔ xAI Responses API conversions (OAuth)."""

from __future__ import annotations

import time
import uuid
from typing import Any, AsyncGenerator

import orjson

from app.control.xai.constants import GROK_COMPOSER_MODEL_IDS


def uses_responses_api(model: str) -> bool:
    """Models that must call ``/responses`` on api.x.ai (not ``/chat/completions``)."""
    return model in GROK_COMPOSER_MODEL_IDS


def _content_to_text(content: Any) -> str:
    if content is None:
        return ""
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts: list[str] = []
        for part in content:
            if not isinstance(part, dict):
                continue
            kind = part.get("type") or ""
            if kind in ("text", "input_text", "output_text"):
                parts.append(str(part.get("text") or ""))
        return "\n".join(parts)
    return str(content)


def messages_to_responses_body(
    *,
    model: str,
    messages: list[dict],
    stream: bool,
    temperature: float,
    top_p: float,
) -> dict[str, Any]:
    """Build an xAI Responses request body from Chat Completions messages."""
    instructions: str | None = None
    input_items: list[dict[str, Any]] = []

    for msg in messages:
        role = str(msg.get("role") or "user")
        text = _content_to_text(msg.get("content"))
        if role == "system":
            instructions = f"{instructions}\n\n{text}".strip() if instructions else text
            continue
        if role == "developer":
            input_items.append(
                {
                    "type": "message",
                    "role": "developer",
                    "content": [{"type": "input_text", "text": text}],
                }
            )
        elif role == "user":
            input_items.append(
                {
                    "type": "message",
                    "role": "user",
                    "content": [{"type": "input_text", "text": text}],
                }
            )
        elif role == "assistant":
            input_items.append(
                {
                    "type": "message",
                    "role": "assistant",
                    "content": [{"type": "output_text", "text": text}],
                }
            )
        elif role == "tool":
            input_items.append(
                {
                    "type": "function_call_output",
                    "call_id": str(msg.get("tool_call_id") or ""),
                    "output": text,
                }
            )

    body: dict[str, Any] = {
        "model": model,
        "stream": stream,
        "temperature": temperature,
        "top_p": top_p,
        "store": False,
        "parallel_tool_calls": True,
    }
    if instructions:
        body["instructions"] = instructions

    if len(input_items) == 1 and input_items[0].get("role") == "user":
        body["input"] = input_items[0]["content"][0]["text"]
    elif input_items:
        body["input"] = input_items
    else:
        body["input"] = ""

    return body


def _extract_output_text(data: dict[str, Any]) -> tuple[str, str]:
    content = ""
    reasoning = ""
    for item in data.get("output") or []:
        if not isinstance(item, dict):
            continue
        kind = item.get("type")
        if kind == "message":
            for part in item.get("content") or []:
                if isinstance(part, dict) and part.get("type") == "output_text":
                    content += str(part.get("text") or "")
        elif kind == "reasoning":
            for part in item.get("summary") or []:
                if isinstance(part, dict) and part.get("type") == "summary_text":
                    reasoning += str(part.get("text") or "")

    if not content.strip() and reasoning.strip():
        head = reasoning.split("\n\n", 1)[0].strip()
        content = head or reasoning
    return content, reasoning


def _map_usage(usage: dict[str, Any]) -> dict[str, Any]:
    if not usage:
        return {}
    out: dict[str, Any] = {
        "prompt_tokens": usage.get("input_tokens", 0),
        "completion_tokens": usage.get("output_tokens", 0),
        "total_tokens": usage.get("total_tokens", 0),
    }
    in_details = usage.get("input_tokens_details") or {}
    out_details = usage.get("output_tokens_details") or {}
    if in_details or out_details:
        out["prompt_tokens_details"] = {
            "cached_tokens": in_details.get("cached_tokens", 0),
        }
        out["completion_tokens_details"] = {
            "reasoning_tokens": out_details.get("reasoning_tokens", 0),
        }
    return out


def responses_to_chat_completion(data: dict[str, Any], *, model: str) -> dict[str, Any]:
    """Convert a completed xAI Responses payload to Chat Completions JSON."""
    content, reasoning = _extract_output_text(data)
    message: dict[str, Any] = {"role": "assistant", "content": content or None}
    if reasoning.strip():
        message["reasoning_content"] = reasoning

    resp_id = str(data.get("id") or uuid.uuid4())
    created = int(data.get("created_at") or time.time())
    usage = _map_usage(data.get("usage") or {})
    meta = data.get("metadata") or {}

    return {
        "id": resp_id,
        "object": "chat.completion",
        "created": created,
        "model": model,
        "choices": [
            {
                "index": 0,
                "message": message,
                "finish_reason": "stop",
            }
        ],
        "usage": usage,
        "system_fingerprint": meta.get("system_fingerprint"),
        "service_tier": data.get("service_tier") or "default",
    }


def _chat_chunk(
    *,
    chunk_id: str,
    model: str,
    created: int,
    delta: dict[str, Any],
    finish_reason: str | None = None,
    usage: dict[str, Any] | None = None,
) -> str:
    choice: dict[str, Any] = {"index": 0, "delta": delta}
    if finish_reason is not None:
        choice["finish_reason"] = finish_reason
    payload: dict[str, Any] = {
        "id": chunk_id,
        "object": "chat.completion.chunk",
        "created": created,
        "model": model,
        "choices": [choice],
    }
    if usage:
        payload["usage"] = usage
    return f"data: {orjson.dumps(payload).decode()}\n\n"


async def translate_responses_stream_to_chat(
    lines: AsyncGenerator[str, None],
    *,
    model: str,
) -> AsyncGenerator[str, None]:
    """Translate xAI Responses SSE into OpenAI Chat Completions SSE."""
    chunk_id = f"chatcmpl-{uuid.uuid4().hex[:24]}"
    created = int(time.time())
    role_sent = False

    async for line in lines:
        if isinstance(line, bytes):
            raw = line.decode("utf-8", "replace").strip()
        else:
            raw = str(line).strip()
        if not raw:
            continue
        if raw.startswith("event:"):
            continue
        if not raw.startswith("data:"):
            continue
        data_str = raw[5:].strip()
        if data_str == "[DONE]":
            yield "data: [DONE]\n\n"
            continue
        try:
            event = orjson.loads(data_str)
        except orjson.JSONDecodeError:
            continue

        etype = event.get("type")
        delta: dict[str, Any] = {}
        if etype == "response.created":
            resp = event.get("response") or {}
            if resp.get("id"):
                chunk_id = str(resp["id"])
            continue
        if etype == "response.output_text.delta":
            delta["content"] = event.get("delta") or ""
        elif etype == "response.reasoning_text.delta":
            delta["reasoning_content"] = event.get("delta") or ""
        elif etype == "response.completed":
            resp = event.get("response") or {}
            usage = _map_usage(resp.get("usage") or {})
            if not role_sent:
                delta["role"] = "assistant"
            yield _chat_chunk(
                chunk_id=chunk_id,
                model=model,
                created=created,
                delta=delta or {"role": "assistant"},
                finish_reason="stop",
                usage=usage or None,
            )
            yield "data: [DONE]\n\n"
            return

        if not delta:
            continue
        if not role_sent:
            delta["role"] = "assistant"
            role_sent = True
        yield _chat_chunk(
            chunk_id=chunk_id,
            model=model,
            created=created,
            delta=delta,
        )


__all__ = [
    "uses_responses_api",
    "messages_to_responses_body",
    "responses_to_chat_completion",
    "translate_responses_stream_to_chat",
]