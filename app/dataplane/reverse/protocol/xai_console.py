"""Console API protocol — payload builder and response parser.

The ``console.x.ai/v1/responses`` endpoint shares SSO cookies with grok.com
but exposes the OpenAI Responses API directly. Free/basic accounts can call
all models (grok-4.3, grok-4.20-*, etc.) through this endpoint, bypassing
the tier restrictions of the grok.com web chat API.

The upstream API supports:
  - Plain string input or structured input arrays (for multimodal / chat history)
  - Native function calling via ``tools`` field
  - Reasoning summary streaming
  - SSE streaming with OpenAI Responses API event names

Request format (string input):
    {"model": "grok-4.3", "input": "What is 1+1?", "stream": true}

Request format (structured input + tools):
    {
        "model": "grok-4.3",
        "input": [
            {"role": "user", "content": [
                {"type": "input_text", "text": "What's the weather?"},
                {"type": "input_image", "image_url": "https://...", "detail": "auto"}
            ]}
        ],
        "tools": [
            {"type": "function", "name": "get_weather",
             "description": "...", "parameters": {...}}
        ],
        "tool_choice": "auto"
    }

Response output items (non-streaming):
  - {"type": "reasoning", "summary": [{"type": "summary_text", "text": "..."}]}
  - {"type": "message", "role": "assistant",
     "content": [{"type": "output_text", "text": "...", "annotations": [...]}]}
  - {"type": "function_call", "call_id": "...", "name": "...", "arguments": "..."}
"""

from typing import Any

import orjson

from app.platform.errors import UpstreamError
from app.platform.logging.logger import logger

# ---------------------------------------------------------------------------
# Input conversion (OpenAI Chat Completions → console.x.ai input array)
# ---------------------------------------------------------------------------


def build_console_input(messages: list[dict[str, Any]], ) -> tuple[list[dict[str, Any]], str]:
    """Convert OpenAI Chat Completions messages → console structured input.

    Returns ``(input_array, instructions)``:
      - ``input_array`` is the list passed as Responses API ``input`` field.
      - ``instructions`` aggregates all ``role=system`` messages and is
        passed via the separate Responses API ``instructions`` field for
        better reasoning model behaviour.

    Mapping rules:
      - ``role=system``            → folded into ``instructions``
      - ``role=user/assistant``    → preserved with content blocks converted
      - Content block ``text``     → ``{type: input_text/output_text, text}``
      - Content block ``image_url`` → ``{type: input_image, image_url, detail}``
      - ``role=tool``              → ``{type: function_call_output,
                                        call_id, output}``
      - ``role=assistant`` with ``tool_calls`` → emit one ``function_call``
        item per call before any accompanying text.
    """
    instructions_parts: list[str] = []
    output: list[dict[str, Any]] = []

    for msg in messages:
        role = msg.get("role") or "user"
        content = msg.get("content")
        tool_calls = msg.get("tool_calls")

        # ── system → instructions ────────────────────────────────────────
        if role == "system":
            if isinstance(content, str) and content.strip():
                instructions_parts.append(content.strip())
            elif isinstance(content, list):
                for block in content:
                    if isinstance(block, dict) and block.get("type") == "text":
                        text = block.get("text") or ""
                        if text.strip():
                            instructions_parts.append(text.strip())
            continue

        # ── tool result → function_call_output ───────────────────────────
        if role == "tool":
            call_id = msg.get("tool_call_id") or ""
            text = content if isinstance(content, str) else _flatten_text(content)
            output.append({
                "type": "function_call_output",
                "call_id": call_id,
                "output": text or "",
            })
            continue

        # ── assistant with tool_calls → function_call items ──────────────
        if role == "assistant" and tool_calls:
            for tc in tool_calls:
                if not isinstance(tc, dict):
                    continue
                fn = tc.get("function") or {}
                output.append(
                    {
                        "type": "function_call",
                        "call_id": tc.get("id") or fn.get("name") or "",
                        "name": fn.get("name") or "",
                        "arguments": fn.get("arguments") or "{}",
                    })
            # Trailing assistant text (rare) is emitted as a normal message
            text = content if isinstance(content, str) else _flatten_text(content)
            if text and text.strip():
                output.append(
                    {
                        "role": "assistant",
                        "content": [{
                            "type": "output_text",
                            "text": text.strip()
                        }],
                    })
            continue

        # ── normal user / assistant message ──────────────────────────────
        blocks = _convert_content_blocks(content, role)
        if not blocks:
            continue
        output.append({"role": role, "content": blocks})

    instructions = "\n\n".join(instructions_parts).strip()
    return output, instructions


def _flatten_text(content: Any) -> str:
    """Flatten an OpenAI content array into a single text string."""
    if isinstance(content, str):
        return content
    if not isinstance(content, list):
        return ""
    parts: list[str] = []
    for block in content:
        if not isinstance(block, dict):
            continue
        if block.get("type") == "text":
            text = block.get("text") or ""
            if text:
                parts.append(text)
    return "\n".join(parts)


def _convert_content_blocks(
    content: Any,
    role: str,
) -> list[dict[str, Any]]:
    """Convert one OpenAI message content (str or array) → console blocks."""
    text_type = "output_text" if role == "assistant" else "input_text"

    # Plain string content
    if isinstance(content, str):
        text = content.strip()
        if not text:
            return []
        return [{"type": text_type, "text": text}]

    # Already-structured array
    if not isinstance(content, list):
        return []

    blocks: list[dict[str, Any]] = []
    for block in content:
        if not isinstance(block, dict):
            continue
        btype = block.get("type")

        if btype == "text":
            text = block.get("text") or ""
            if text.strip():
                blocks.append({"type": text_type, "text": text})
        elif btype == "image_url":
            inner = block.get("image_url") or {}
            if isinstance(inner, str):
                url, detail = inner, "auto"
            else:
                url = inner.get("url") or ""
                detail = inner.get("detail") or "auto"
            if url:
                blocks.append({
                    "type": "input_image",
                    "image_url": url,
                    "detail": detail,
                })
        elif btype in ("input_text", "output_text", "input_image"):
            # Already in console format — pass through
            blocks.append(dict(block))

    return blocks


# ---------------------------------------------------------------------------
# Tool format conversion
# ---------------------------------------------------------------------------


def convert_openai_tools_to_console(tools: list[dict[str, Any]] | None, ) -> list[dict[str, Any]]:
    """Convert OpenAI Chat Completions tools → console (Responses API) tools.

    OpenAI Chat Completions:
        {"type": "function", "function": {"name", "description", "parameters"}}

    Console (Responses API):
        {"type": "function", "name", "description", "parameters"}

    Already-flat tools are passed through (e.g. ``web_search`` server-side
    tool, ``code_interpreter``, ``x_search`` etc.).
    """
    if not tools:
        return []
    out: list[dict[str, Any]] = []
    for t in tools:
        if not isinstance(t, dict):
            continue
        if t.get("type") != "function":
            # Pass through server-side tools (web_search, x_search, etc.)
            out.append(dict(t))
            continue
        fn = t.get("function") if isinstance(t.get("function"), dict) else None
        if fn is not None:
            out.append(
                {
                    "type": "function",
                    "name": fn.get("name") or "",
                    "description": fn.get("description") or "",
                    "parameters": fn.get("parameters") or {},
                })
        else:
            # Already flat
            out.append(dict(t))
    return out


def convert_openai_tool_choice(tool_choice: Any) -> Any:
    """Convert OpenAI tool_choice → console tool_choice.

    OpenAI:  "none" | "auto" | "required" | {"type":"function","function":{"name":"x"}}
    Console: "none" | "auto" | "required" | {"type":"function","name":"x"}
    """
    if isinstance(tool_choice, str):
        return tool_choice
    if isinstance(tool_choice, dict) and tool_choice.get("type") == "function":
        fn = tool_choice.get("function") if isinstance(tool_choice.get("function"), dict) else None
        if fn:
            return {"type": "function", "name": fn.get("name") or ""}
        return dict(tool_choice)
    return tool_choice


# ---------------------------------------------------------------------------
# Payload builder
# ---------------------------------------------------------------------------


def build_console_payload(
    *,
    console_model: str,
    input: Any,
    instructions: str = "",
    stream: bool = False,
    temperature: float | None = None,
    top_p: float | None = None,
    reasoning_effort: str | None = None,
    tools: list[dict[str, Any]] | None = None,
    tool_choice: Any = None,
) -> dict[str, Any]:
    """Build the JSON payload for POST /v1/responses on console.x.ai.

    ``input`` may be a plain string or an array of structured input items
    (use :func:`build_console_input` to convert OpenAI messages).

    ``tools`` should already be in console format (use
    :func:`convert_openai_tools_to_console`).
    """
    payload: dict[str, Any] = {
        "model": console_model,
        "input": input,
    }
    if stream:
        payload["stream"] = True
    if instructions:
        payload["instructions"] = instructions
    if temperature is not None:
        payload["temperature"] = temperature
    if top_p is not None:
        payload["top_p"] = top_p
    if reasoning_effort:
        # Valid values: "minimal" | "low" | "medium" | "high"
        payload["reasoning"] = {"effort": reasoning_effort}
    if tools:
        payload["tools"] = tools
        if tool_choice is not None:
            payload["tool_choice"] = tool_choice

    if isinstance(input, str):
        msg_repr = f"len={len(input)}"
    elif isinstance(input, list):
        msg_repr = f"items={len(input)}"
    else:
        msg_repr = "unknown"
    logger.debug(
        "console payload built: model={} stream={} input_{} tools={}",
        console_model,
        stream,
        msg_repr,
        len(tools) if tools else 0,
    )
    return payload


# ---------------------------------------------------------------------------
# Non-streaming response parsing
# ---------------------------------------------------------------------------


def extract_console_text(response_json: dict[str, Any]) -> str:
    """Extract the assistant's final text from a non-streaming response."""
    output = response_json.get("output") or []
    for item in output:
        if not isinstance(item, dict):
            continue
        if item.get("type") != "message":
            continue
        contents = item.get("content") or []
        for c in contents:
            if not isinstance(c, dict):
                continue
            if c.get("type") == "output_text":
                return c.get("text") or ""
    return ""


def extract_console_reasoning(response_json: dict[str, Any]) -> str:
    """Extract reasoning summary text if present (non-streaming)."""
    output = response_json.get("output") or []
    for item in output:
        if not isinstance(item, dict):
            continue
        if item.get("type") == "reasoning":
            summary = item.get("summary") or []
            parts: list[str] = []
            for s in summary:
                if isinstance(s, dict):
                    text = s.get("text") or s.get("content") or ""
                    if text:
                        parts.append(text)
                elif isinstance(s, str):
                    parts.append(s)
            return "\n".join(parts)
    return ""


def extract_console_tool_calls(response_json: dict[str, Any], ) -> list[dict[str, Any]]:
    """Extract tool calls from a non-streaming response.

    Returns a list of OpenAI Chat Completions tool_call dicts:
        [{"id": "call_xxx", "type": "function",
          "function": {"name": "...", "arguments": "..."}}]

    Console responses include each tool call as a top-level output item
    of type ``function_call`` with a ``call_id``, ``name`` and
    JSON-serialised ``arguments`` string.
    """
    output = response_json.get("output") or []
    calls: list[dict[str, Any]] = []
    for item in output:
        if not isinstance(item, dict):
            continue
        if item.get("type") != "function_call":
            continue
        call_id = item.get("call_id") or item.get("id") or ""
        calls.append(
            {
                "id": call_id,
                "type": "function",
                "function": {
                    "name": item.get("name") or "",
                    "arguments": item.get("arguments") or "{}",
                },
            })
    return calls


def extract_console_search_sources(response_json: dict[str, Any], ) -> list[dict[str, Any]]:
    """Extract the search sources list from web_search_call output items.

    Returns a deduplicated list of source dicts in the format used by the
    existing grok.com path:
        [{"url": "https://...", "title": ""}, ...]

    Two upstream variants are handled:

    1. Single-agent models (grok-4.3, grok-4.20-reasoning) emit a
       ``web_search_call`` output item per search with full sources:
         ``{"type": "search", "sources": [{"url": "..."}, ...]}``
       or ``{"type": "open_page", "url": "..."}``.

    2. Multi-agent models (grok-4.20-multi-agent) skip ``web_search_call``
       items entirely and embed URLs only as document-level annotations on
       the final assistant message with ``start_index == end_index == 0``.
       We fall back to those annotation URLs so callers always see a
       useful citation list regardless of the upstream emission format.
    """
    seen: set[str] = set()
    out: list[dict[str, Any]] = []
    for item in response_json.get("output") or []:
        if not isinstance(item, dict):
            continue
        if item.get("type") != "web_search_call":
            continue
        action = item.get("action") or {}
        if not isinstance(action, dict):
            continue
        # Search action with sources list
        for src in action.get("sources") or []:
            if not isinstance(src, dict):
                continue
            url = src.get("url") or ""
            if not url or url in seen:
                continue
            seen.add(url)
            out.append({
                "url": url,
                "title": src.get("title") or "",
            })
        # Page-open action — single URL
        if action.get("type") == "open_page":
            url = action.get("url") or ""
            if url and url not in seen:
                seen.add(url)
                out.append({"url": url, "title": ""})

    # Fallback: harvest URLs from message annotations. Multi-agent
    # responses publish citations only here. We dedupe against the
    # web_search_call sources collected above so single-agent paths
    # remain unchanged.
    for item in response_json.get("output") or []:
        if not isinstance(item, dict) or item.get("type") != "message":
            continue
        for content in item.get("content") or []:
            if not isinstance(content, dict):
                continue
            for ann in content.get("annotations") or []:
                if not isinstance(ann, dict):
                    continue
                if ann.get("type") not in (None, "url_citation"):
                    continue
                url = ann.get("url") or ""
                if not url or url in seen:
                    continue
                seen.add(url)
                title = ann.get("title") or ""
                # Multi-agent annotations sometimes set title=url; strip
                # the duplicate so the source list reads cleanly.
                if title == url:
                    title = ""
                out.append({"url": url, "title": title})
    return out


def inject_web_search_tool(tools: list[dict[str, Any]] | None, ) -> list[dict[str, Any]]:
    """Ensure a ``web_search`` tool is present in the console tools list.

    If the user already supplied any ``web_search`` tool (with or without
    options), it's left untouched. Otherwise a default ``{"type":
    "web_search"}`` entry is appended. xAI charges $5/1000 calls for web
    search; this is consumed from the account's prepaid (trial) credits.
    """
    existing = list(tools or [])
    for t in existing:
        if isinstance(t, dict) and t.get("type") == "web_search":
            return existing
    existing.append({"type": "web_search"})
    return existing


def extract_console_annotations(response_json: dict[str, Any], ) -> list[dict[str, Any]]:
    """Extract URL citation annotations from a non-streaming response.

    Returns a flat list of citation dicts in chat-completions format:
        [{"url": "...", "title": "...", "start_index": 0, "end_index": 0}]
    """
    out: list[dict[str, Any]] = []
    output = response_json.get("output") or []
    for item in output:
        if not isinstance(item, dict):
            continue
        if item.get("type") != "message":
            continue
        contents = item.get("content") or []
        for c in contents:
            if not isinstance(c, dict):
                continue
            anns = c.get("annotations") or []
            for a in anns:
                if not isinstance(a, dict):
                    continue
                if a.get("type") not in (None, "url_citation"):
                    continue
                url = a.get("url") or ""
                if not url:
                    continue
                out.append(
                    {
                        "url": url,
                        "title": a.get("title") or "",
                        "start_index": int(a.get("start_index") or 0),
                        "end_index": int(a.get("end_index") or 0),
                    })
    return out


def extract_console_usage(response_json: dict[str, Any]) -> dict[str, int]:
    """Extract usage tokens from a non-streaming response."""
    usage = response_json.get("usage") or {}
    return {
        "prompt_tokens": int(usage.get("input_tokens") or 0),
        "completion_tokens": int(usage.get("output_tokens") or 0),
        "total_tokens": int(usage.get("total_tokens") or 0),
        "reasoning_tokens": int(
            (usage.get("output_tokens_details") or {}).get("reasoning_tokens") or
            usage.get("reasoning_tokens") or 0),
    }


def parse_console_error(status_code: int, body: str) -> UpstreamError:
    """Convert a non-200 console response into an UpstreamError."""
    message = f"Console upstream returned {status_code}"
    try:
        obj = orjson.loads(body) if body else {}
        if isinstance(obj, dict):
            err = obj.get("error") or obj.get("code") or ""
            if isinstance(err, dict):
                err = err.get("message") or err.get("code") or ""
            if err:
                message = f"{message}: {err}"
    except (orjson.JSONDecodeError, ValueError, TypeError):
        pass
    return UpstreamError(message, status=status_code, body=body[:400])


# ---------------------------------------------------------------------------
# SSE streaming event parsing
# ---------------------------------------------------------------------------


def classify_console_sse_line(line: str | bytes) -> tuple[str, str]:
    """Return (kind, payload) for one raw SSE line.

    kind:
      - 'data'  — SSE data line; payload is the JSON string
      - 'event' — SSE event name line; payload is the event name
      - 'skip'  — comment / blank / unrecognized
    """
    if isinstance(line, bytes):
        line = line.decode("utf-8", "replace")
    line = line.strip()
    if not line:
        return "skip", ""
    if line.startswith("event:"):
        return "event", line[6:].strip()
    if line.startswith("data:"):
        data = line[5:].strip()
        return "data", data
    if line.startswith("{"):
        return "data", line
    return "skip", ""


class ConsoleStreamAdapter:
    """Parse upstream Console SSE frames and emit text/reasoning/tool deltas.

    The console.x.ai SSE protocol uses OpenAI Responses API event names:
      - response.created
      - response.output_item.added                  ← announces a new item
      - response.content_part.added
      - response.output_text.delta                  ← text chunks
      - response.output_text.done
      - response.reasoning_summary_text.delta       ← reasoning chunks
      - response.function_call_arguments.delta      ← tool args streaming
      - response.function_call_arguments.done       ← tool args complete
      - response.output_item.done                   ← completed item
      - response.output_text.annotation.added       ← citation annotation
      - response.completed
      - response.failed / response.cancelled / response.error
    """

    __slots__ = (
        "_current_event",
        "_active_tool_index",
        "_tool_args_buf",
        "_seen_source_urls",
        "tool_calls",
        "annotations",
        "search_sources",
        "text_buf",
        "thinking_buf",
        "_usage",
    )

    def __init__(self) -> None:
        self._current_event: str = ""
        self._active_tool_index: dict[str, int] = {}  # item_id → index
        self._tool_args_buf: dict[str, list[str]] = {}  # item_id → args chunks
        self._seen_source_urls: set[str] = set()
        self.tool_calls: list[dict[str, Any]] = []
        self.annotations: list[dict[str, Any]] = []
        self.search_sources: list[dict[str, Any]] = []
        self.text_buf: list[str] = []
        self.thinking_buf: list[str] = []
        self._usage: dict[str, int] = {}

    def feed_event(self, event_name: str) -> None:
        """Record the most recent ``event:`` name from the SSE stream."""
        self._current_event = event_name

    def feed_data(self, data: str) -> dict[str, Any]:
        """Parse one SSE data frame; return the kind/content classification.

        Returns a dict like:
          {"kind": "text", "content": "Two"}
          {"kind": "thinking", "content": "Let me think..."}
          {"kind": "tool_call_start", "index": 0, "call_id": "...", "name": "..."}
          {"kind": "tool_call_args", "index": 0, "delta": "..."}
          {"kind": "tool_call_done", "index": 0}
          {"kind": "annotation", "annotation_data": {...}}
          {"kind": "done"}
          {"kind": "error", "message": "..."}
          {"kind": "skip"}
        """
        if not data or data == "[DONE]":
            return {"kind": "done"}
        try:
            obj = orjson.loads(data)
        except (orjson.JSONDecodeError, ValueError, TypeError):
            return {"kind": "skip"}
        if not isinstance(obj, dict):
            return {"kind": "skip"}

        # Event-specific dispatch (event: line precedes data: line in SSE).
        ev = self._current_event or obj.get("type") or ""

        # ── Text delta ────────────────────────────────────────────────────────
        if ev == "response.output_text.delta" or obj.get("type") == "response.output_text.delta":
            delta = obj.get("delta") or ""
            if isinstance(delta, str) and delta:
                self.text_buf.append(delta)
                return {"kind": "text", "content": delta}
            return {"kind": "skip"}

        # ── Reasoning summary delta (thinking) ────────────────────────────────
        if ev in (
                "response.reasoning_summary_text.delta",
                "response.reasoning_summary.delta",
        ) or obj.get("type") in (
                "response.reasoning_summary_text.delta",
                "response.reasoning_summary.delta",
        ):
            delta = obj.get("delta") or ""
            if isinstance(delta, str) and delta:
                self.thinking_buf.append(delta)
                return {"kind": "thinking", "content": delta}
            return {"kind": "skip"}

        # ── Tool call start (output_item.added with type=function_call) ──────
        if ev == "response.output_item.added" or obj.get("type") == "response.output_item.added":
            item = obj.get("item") or {}
            if isinstance(item, dict) and item.get("type") == "function_call":
                item_id = item.get("id") or item.get("call_id") or ""
                call_id = item.get("call_id") or item_id
                name = item.get("name") or ""
                idx = len(self.tool_calls)
                self._active_tool_index[item_id] = idx
                self._tool_args_buf[item_id] = []
                self.tool_calls.append(
                    {
                        "id": call_id,
                        "type": "function",
                        "function": {
                            "name": name,
                            "arguments": ""
                        },
                    })
                return {
                    "kind": "tool_call_start",
                    "index": idx,
                    "call_id": call_id,
                    "name": name,
                }
            return {"kind": "skip"}

        # ── Web search call done — collect sources ───────────────────────────
        if ev == "response.output_item.done" or obj.get("type") == "response.output_item.done":
            item = obj.get("item") or {}
            if isinstance(item, dict) and item.get("type") == "web_search_call":
                action = item.get("action") or {}
                if isinstance(action, dict):
                    for src in action.get("sources") or []:
                        if not isinstance(src, dict):
                            continue
                        url = src.get("url") or ""
                        if url and url not in self._seen_source_urls:
                            self._seen_source_urls.add(url)
                            self.search_sources.append({
                                "url": url,
                                "title": src.get("title") or "",
                            })
                    if action.get("type") == "open_page":
                        url = action.get("url") or ""
                        if url and url not in self._seen_source_urls:
                            self._seen_source_urls.add(url)
                            self.search_sources.append({
                                "url": url,
                                "title": "",
                            })
            return {"kind": "skip"}

        # ── Tool call argument delta ──────────────────────────────────────────
        if ev == "response.function_call_arguments.delta" or obj.get(
                "type") == "response.function_call_arguments.delta":
            item_id = obj.get("item_id") or ""
            delta = obj.get("delta") or ""
            if not isinstance(delta, str) or not delta:
                return {"kind": "skip"}
            idx = self._active_tool_index.get(item_id)
            if idx is None:
                return {"kind": "skip"}
            self._tool_args_buf.setdefault(item_id, []).append(delta)
            return {"kind": "tool_call_args", "index": idx, "delta": delta}

        # ── Tool call complete ────────────────────────────────────────────────
        if ev == "response.function_call_arguments.done" or obj.get(
                "type") == "response.function_call_arguments.done":
            item_id = obj.get("item_id") or ""
            idx = self._active_tool_index.get(item_id)
            if idx is None:
                return {"kind": "skip"}
            # Prefer upstream-provided final arguments string when present.
            final_args = obj.get("arguments")
            if not isinstance(final_args, str) or not final_args:
                final_args = "".join(self._tool_args_buf.get(item_id, []))
            self.tool_calls[idx]["function"]["arguments"] = final_args
            return {"kind": "tool_call_done", "index": idx}

        # ── URL citation annotation ───────────────────────────────────────────
        if ev == "response.output_text.annotation.added" or obj.get(
                "type") == "response.output_text.annotation.added":
            ann = obj.get("annotation") or {}
            if isinstance(ann, dict) and ann.get("type") in (None, "url_citation"):
                url = ann.get("url") or ""
                if url:
                    title = ann.get("title") or ""
                    if title == url:
                        # Multi-agent often duplicates URL into title; clean it.
                        title = ""
                    record = {
                        "url": url,
                        "title": title,
                        "start_index": int(ann.get("start_index") or 0),
                        "end_index": int(ann.get("end_index") or 0),
                    }
                    self.annotations.append(record)
                    # Fallback for multi-agent: harvest citation URL into
                    # search_sources too. Dedupe against web_search_call
                    # sources to avoid duplicating single-agent entries.
                    if url not in self._seen_source_urls:
                        self._seen_source_urls.add(url)
                        self.search_sources.append({
                            "url": url,
                            "title": title,
                        })
                    return {"kind": "annotation", "annotation_data": record}
            return {"kind": "skip"}

        # ── Final completion frame — capture usage for accounting ────────────
        if ev == "response.completed" or obj.get("type") == "response.completed":
            resp = obj.get("response") or obj
            usage = resp.get("usage") or {}
            if usage:
                self._usage = {
                    "prompt_tokens": int(usage.get("input_tokens") or 0),
                    "completion_tokens": int(usage.get("output_tokens") or 0),
                    "total_tokens": int(usage.get("total_tokens") or 0),
                    "reasoning_tokens": int(
                        (usage.get("output_tokens_details") or {}).get("reasoning_tokens") or
                        usage.get("reasoning_tokens") or 0),
                }
            return {"kind": "done"}

        # ── Error frames ──────────────────────────────────────────────────────
        if ev in ("response.failed", "response.error", "error") or obj.get("type") in (
                "response.failed",
                "response.error",
                "error",
        ):
            err = obj.get("error") or obj.get("response", {}).get("error") or {}
            if isinstance(err, dict):
                msg = err.get("message") or err.get("code") or "Console stream error"
            else:
                msg = str(err) or "Console stream error"
            return {"kind": "error", "message": str(msg)}

        return {"kind": "skip"}

    @property
    def usage(self) -> dict[str, int]:
        """Return collected usage tokens (populated after stream completion)."""
        return dict(self._usage)


__all__ = [
    "build_console_input",
    "build_console_payload",
    "convert_openai_tools_to_console",
    "convert_openai_tool_choice",
    "inject_web_search_tool",
    "extract_console_text",
    "extract_console_reasoning",
    "extract_console_tool_calls",
    "extract_console_annotations",
    "extract_console_search_sources",
    "extract_console_usage",
    "parse_console_error",
    "classify_console_sse_line",
    "ConsoleStreamAdapter",
]
