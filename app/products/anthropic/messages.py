"""Anthropic Messages API handler (/v1/messages).

Adapts Anthropic SDK requests to the internal Grok reverse-proxy pipeline,
then converts the response back to Anthropic Messages API format.

Reuses the same _stream_chat / StreamAdapter / ToolSieve machinery as
responses.py — only the input/output format conversion differs.
"""

import asyncio
import os
import time
from typing import Any, AsyncGenerator

import orjson

from app.platform.logging.logger import logger
from app.platform.config.snapshot import get_config
from app.platform.errors import RateLimitError, UpstreamError
from app.platform.runtime.clock import now_s
from app.platform.tokens import estimate_prompt_tokens, estimate_tokens, estimate_tool_call_tokens
from app.control.model.enums import ModeId
from app.control.model.registry import resolve as resolve_model
from app.control.account.enums import FeedbackKind
from app.dataplane.reverse.protocol.xai_chat import classify_line, StreamAdapter
from app.dataplane.reverse.protocol.tool_prompt import (
    build_tool_system_prompt, extract_tool_names, inject_into_message,
)
from app.dataplane.reverse.protocol.tool_parser import parse_tool_calls

from app.products.openai.chat import (
    _stream_chat, _extract_message, _resolve_image,
    _quota_sync, _fail_sync, _parse_retry_codes, _feedback_kind, _log_task_exception,
    _configured_retry_codes, _should_retry_upstream,
)
from app.products._account_selection import reserve_account
from app.products.openai._tool_sieve import ToolSieve


# ---------------------------------------------------------------------------
# ID helpers
# ---------------------------------------------------------------------------

def _make_msg_id() -> str:
    return f"msg_{int(time.time() * 1000)}{os.urandom(4).hex()}"


def _make_tool_id() -> str:
    return f"toolu_{int(time.time() * 1000)}{os.urandom(3).hex()}"


# ---------------------------------------------------------------------------
# SSE encoding (Anthropic event format)
# ---------------------------------------------------------------------------

def _sse(event: str, data: dict) -> str:
    return f"event: {event}\ndata: {orjson.dumps(data).decode()}\n\n"


# ---------------------------------------------------------------------------
# Request conversion: Anthropic → internal format
# ---------------------------------------------------------------------------

def _anthropic_content_to_internal(content: Any, role: str) -> list[dict]:
    """Convert Anthropic content (string or block list) to internal message list.

    Returns a list of internal messages (may be multiple when tool_result
    blocks need to become separate tool-role messages).
    """
    if isinstance(content, str):
        return [{"role": role, "content": content}]

    if not isinstance(content, list):
        return []

    # Check if content contains tool_use blocks (assistant calling tools)
    has_tool_use = any(
        isinstance(b, dict) and b.get("type") == "tool_use"
        for b in content
    )

    # Check if content contains tool_result blocks (user returning results)
    tool_result_blocks = [
        b for b in content
        if isinstance(b, dict) and b.get("type") == "tool_result"
    ]

    if tool_result_blocks:
        # Each tool_result → a separate tool-role message
        messages = []
        for block in tool_result_blocks:
            result_content = block.get("content", "")
            if isinstance(result_content, list):
                # array of text blocks → join
                result_content = "\n".join(
                    b.get("text", "") for b in result_content
                    if isinstance(b, dict) and b.get("type") == "text"
                )
            messages.append({
                "role":         "tool",
                "tool_call_id": block.get("tool_use_id", ""),
                "content":      result_content or "",
            })
        return messages

    if has_tool_use:
        # Build assistant message with tool_calls
        text_parts: list[str] = []
        tool_calls: list[dict] = []
        for block in content:
            if not isinstance(block, dict):
                continue
            btype = block.get("type")
            if btype == "text":
                text_parts.append(block.get("text", ""))
            elif btype == "tool_use":
                tool_calls.append({
                    "id":   block.get("id", _make_tool_id()),
                    "type": "function",
                    "function": {
                        "name":      block.get("name", ""),
                        "arguments": orjson.dumps(block.get("input") or {}).decode(),
                    },
                })
        msg: dict = {
            "role":       "assistant",
            "content":    " ".join(text_parts) if text_parts else None,
            "tool_calls": tool_calls,
        }
        return [msg]

    # Normal content: text + image + document blocks
    normalized: list[dict] = []
    for block in content:
        if not isinstance(block, dict):
            continue
        btype = block.get("type")
        if btype == "text":
            text = (block.get("text") or "").strip()
            if text:
                normalized.append({"type": "text", "text": text})
        elif btype == "image":
            source = block.get("source") or {}
            src_type = source.get("type", "")
            if src_type == "base64":
                media = source.get("media_type", "image/jpeg")
                data  = source.get("data", "")
                normalized.append({
                    "type":      "image_url",
                    "image_url": {"url": f"data:{media};base64,{data}"},
                })
            elif src_type == "url":
                normalized.append({
                    "type":      "image_url",
                    "image_url": {"url": source.get("url", "")},
                })
        elif btype == "document":
            source = block.get("source") or {}
            src_type = source.get("type", "")
            if src_type == "base64":
                media = source.get("media_type", "application/pdf")
                data  = source.get("data", "")
                normalized.append({
                    "type": "file",
                    "file": {"data": f"data:{media};base64,{data}"},
                })

    if not normalized:
        return []
    return [{"role": role, "content": normalized}]


def _parse_anthropic_messages(
    messages: list[dict],
    system:   str | list | None,
) -> list[dict]:
    """Convert Anthropic messages + system prompt to internal format."""
    internal: list[dict] = []

    # System prompt
    if system:
        if isinstance(system, str):
            system_text = system
        elif isinstance(system, list):
            system_text = "\n".join(
                b.get("text", "") for b in system
                if isinstance(b, dict) and b.get("type") == "text"
            )
        else:
            system_text = str(system)
        if system_text.strip():
            internal.append({"role": "system", "content": system_text})

    for msg in messages:
        role    = msg.get("role", "user")
        content = msg.get("content", "")
        internal.extend(_anthropic_content_to_internal(content, role))

    return internal


def _convert_tools(tools: list[dict]) -> list[dict]:
    """Convert Anthropic tool definitions to internal Chat Completions format.

    Anthropic:  {name, description, input_schema}
    Internal:   {type:"function", function:{name, description, parameters}}
    """
    result = []
    for tool in tools:
        result.append({
            "type": "function",
            "function": {
                "name":        tool.get("name", ""),
                "description": tool.get("description", ""),
                "parameters":  tool.get("input_schema"),
            },
        })
    return result


def _convert_tool_choice(tool_choice: Any) -> Any:
    """Map Anthropic tool_choice → internal format."""
    if tool_choice is None:
        return "auto"
    if isinstance(tool_choice, str):
        return tool_choice
    if isinstance(tool_choice, dict):
        tc_type = tool_choice.get("type", "auto")
        if tc_type == "auto":
            return "auto"
        if tc_type == "any":
            return "required"
        if tc_type == "tool":
            return {"type": "function", "function": {"name": tool_choice.get("name", "")}}
    return "auto"


# ---------------------------------------------------------------------------
# Response format helpers
# ---------------------------------------------------------------------------

def _finish_reason_to_stop_reason(finish_reason: str | None) -> str:
    mapping = {"stop": "end_turn", "tool_calls": "tool_use", "length": "max_tokens"}
    return mapping.get(finish_reason or "stop", "end_turn")


def _build_message_response(
    msg_id:      str,
    model:       str,
    content:     list[dict],
    stop_reason: str,
    input_tokens:  int,
    output_tokens: int,
) -> dict:
    return {
        "id":            msg_id,
        "type":          "message",
        "role":          "assistant",
        "model":         model,
        "content":       content,
        "stop_reason":   stop_reason,
        "stop_sequence": None,
        "usage": {
            "input_tokens":  input_tokens,
            "output_tokens": output_tokens,
        },
    }


# ---------------------------------------------------------------------------
# Main handler
# ---------------------------------------------------------------------------

async def create(
    *,
    model:        str,
    messages:     list[dict],
    system:       str | list | None = None,
    stream:       bool,
    emit_think:   bool,
    temperature:  float,
    top_p:        float,
    tools:        list[dict] | None = None,
    tool_choice:  Any = None,
) -> dict | AsyncGenerator[str, None]:

    cfg     = get_config()
    spec    = resolve_model(model)
    mode_id = int(spec.mode_id)

    # Build internal message list
    internal_messages = _parse_anthropic_messages(messages, system)
    internal_message, files = _extract_message(internal_messages)
    if not internal_message.strip():
        raise UpstreamError("Empty message after extraction", status=400)

    # Tool injection
    tool_names: list[str] = []
    internal_tool_choice: Any = None
    if tools:
        chat_tools       = _convert_tools(tools)
        tool_names       = extract_tool_names(chat_tools)
        internal_tool_choice = _convert_tool_choice(tool_choice)
        tool_prompt      = build_tool_system_prompt(chat_tools, internal_tool_choice)
        internal_message = inject_into_message(internal_message, tool_prompt)
        logger.info("messages tool injection: tool_names={} choice={}", tool_names, internal_tool_choice)

    from app.dataplane.account import _directory as _acct_dir
    if _acct_dir is None:
        raise RateLimitError("Account directory not initialised")
    directory = _acct_dir

    max_retries = cfg.get_int("retry.max_retries", 1)
    retry_codes = _configured_retry_codes(cfg)
    timeout_s   = cfg.get_float("chat.timeout", 120.0)
    msg_id      = _make_msg_id()

    # -------------------------------------------------------------------------
    # Streaming
    # -------------------------------------------------------------------------
    async def _run_stream() -> AsyncGenerator[str, None]:
        excluded: list[str] = []
        for attempt in range(max_retries + 1):
            acct, selected_mode_id = await reserve_account(
                directory,
                spec,
                now_s_override=now_s(),
                exclude_tokens=excluded or None,
            )
            if acct is None:
                raise RateLimitError("No available accounts for this model tier")

            token   = acct.token
            success = False
            _retry  = False
            fail_exc: BaseException | None = None
            adapter               = StreamAdapter()
            think_buf:  list[str] = []
            text_buf:   list[str] = []
            think_started         = False
            think_closed          = False
            text_started          = False
            sieve                 = ToolSieve(tool_names) if tool_names else None
            tool_calls_emitted    = False
            tool_output_tokens    = 0
            block_index           = 0  # tracks next content_block index
            collected_annotations: list[dict] = []

            try:
                try:
                    # message_start
                    yield _sse("message_start", {
                        "type": "message_start",
                        "message": {
                            "id":          msg_id,
                            "type":        "message",
                            "role":        "assistant",
                            "model":       model,
                            "content":     [],
                            "stop_reason": None,
                            "usage":       {"input_tokens": estimate_prompt_tokens(internal_message), "output_tokens": 0},
                        },
                    })
                    yield _sse("ping", {"type": "ping"})

                    ended = False
                    async for line in _stream_chat(
                        token     = token,
                        mode_id   = ModeId(selected_mode_id),
                        message   = internal_message,
                        files     = files,
                        timeout_s = timeout_s,
                    ):
                        if tool_calls_emitted:
                            break

                        event_type, data = classify_line(line)
                        if event_type == "done":
                            break
                        if event_type != "data" or not data:
                            continue

                        for ev in adapter.feed(data):

                            if ev.kind == "thinking" and emit_think and not think_closed:
                                if not think_started:
                                    think_started = True
                                    yield _sse("content_block_start", {
                                        "type":          "content_block_start",
                                        "index":         block_index,
                                        "content_block": {"type": "thinking", "thinking": ""},
                                    })
                                think_buf.append(ev.content)
                                yield _sse("content_block_delta", {
                                    "type":  "content_block_delta",
                                    "index": block_index,
                                    "delta": {"type": "thinking_delta", "thinking": ev.content},
                                })

                            elif ev.kind == "text":
                                # Close thinking block if open
                                if think_started and not think_closed:
                                    think_closed = True
                                    yield _sse("content_block_stop", {
                                        "type":  "content_block_stop",
                                        "index": block_index,
                                    })
                                    block_index += 1

                                # Feed through ToolSieve if tools active
                                if sieve is not None:
                                    safe_text, calls = sieve.feed(ev.content)
                                    if calls is not None:
                                        # Emit tool_use blocks
                                        for call in calls:
                                            yield _sse("content_block_start", {
                                                "type":  "content_block_start",
                                                "index": block_index,
                                                "content_block": {
                                                    "type":  "tool_use",
                                                    "id":    call.call_id,
                                                    "name":  call.name,
                                                    "input": {},
                                                },
                                            })
                                            yield _sse("content_block_delta", {
                                                "type":  "content_block_delta",
                                                "index": block_index,
                                                "delta": {
                                                    "type":         "input_json_delta",
                                                    "partial_json": call.arguments,
                                                },
                                            })
                                            yield _sse("content_block_stop", {
                                                "type":  "content_block_stop",
                                                "index": block_index,
                                            })
                                            block_index += 1
                                        tool_output_tokens = estimate_tool_call_tokens(calls)
                                        tool_calls_emitted = True
                                        ended = True
                                        break
                                    text_chunk = safe_text
                                else:
                                    text_chunk = ev.content

                                if text_chunk:
                                    if not text_started:
                                        text_started = True
                                        yield _sse("content_block_start", {
                                            "type":          "content_block_start",
                                            "index":         block_index,
                                            "content_block": {"type": "text", "text": ""},
                                        })
                                    text_buf.append(text_chunk)
                                    yield _sse("content_block_delta", {
                                        "type":  "content_block_delta",
                                        "index": block_index,
                                        "delta": {"type": "text_delta", "text": text_chunk},
                                    })

                            elif ev.kind == "annotation" and ev.annotation_data:
                                collected_annotations.append(ev.annotation_data)

                            elif ev.kind == "soft_stop":
                                ended = True
                                break

                        if ended:
                            break

                    # Flush sieve — incomplete XML at end of stream
                    if sieve is not None and not tool_calls_emitted:
                        calls = sieve.flush()
                        if calls:
                            # Close text block if open
                            if text_started:
                                yield _sse("content_block_stop", {
                                    "type":  "content_block_stop",
                                    "index": block_index,
                                })
                                block_index += 1
                                text_started = False
                            for call in calls:
                                yield _sse("content_block_start", {
                                    "type":  "content_block_start",
                                    "index": block_index,
                                    "content_block": {
                                        "type":  "tool_use",
                                        "id":    call.call_id,
                                        "name":  call.name,
                                        "input": {},
                                    },
                                })
                                yield _sse("content_block_delta", {
                                    "type":  "content_block_delta",
                                    "index": block_index,
                                    "delta": {
                                        "type":         "input_json_delta",
                                        "partial_json": call.arguments,
                                    },
                                })
                                yield _sse("content_block_stop", {
                                    "type":  "content_block_stop",
                                    "index": block_index,
                                })
                                block_index += 1
                            tool_output_tokens = estimate_tool_call_tokens(calls)
                            tool_calls_emitted = True

                    if tool_calls_emitted:
                        # 构建 tool_use 的 message_delta，注入搜索信源
                        tool_delta: dict = {"stop_reason": "tool_use", "stop_sequence": None}
                        sources = adapter.search_sources_list()
                        if sources:
                            tool_delta["search_sources"] = sources
                        yield _sse("message_delta", {
                            "type":  "message_delta",
                            "delta": tool_delta,
                            "usage": {"output_tokens": tool_output_tokens},
                        })
                        yield _sse("message_stop", {"type": "message_stop"})
                        yield "data: [DONE]\n\n"
                        success = True
                        logger.info("messages stream tool_calls: attempt={}/{} model={}",
                                    attempt + 1, max_retries + 1, model)
                    else:
                        # Resolve image attachments and references
                        for url, img_id in adapter.image_urls:
                            img_text = await _resolve_image(token, url, img_id)
                            if isinstance(img_text, str):
                                chunk = img_text + "\n"
                                text_buf.append(chunk)
                                if text_started:
                                    yield _sse("content_block_delta", {
                                        "type":  "content_block_delta",
                                        "index": block_index,
                                        "delta": {"type": "text_delta", "text": chunk},
                                    })

                        references = adapter.references_suffix()
                        if references:
                            text_buf.append(references)
                            if text_started:
                                yield _sse("content_block_delta", {
                                    "type":  "content_block_delta",
                                    "index": block_index,
                                    "delta": {"type": "text_delta", "text": references},
                                })

                        # Close open blocks
                        if think_started and not think_closed:
                            yield _sse("content_block_stop", {
                                "type":  "content_block_stop",
                                "index": block_index,
                            })
                            block_index += 1

                        if text_started:
                            yield _sse("content_block_stop", {
                                "type":  "content_block_stop",
                                "index": block_index,
                            })

                        full_text  = "".join(text_buf)
                        full_think = "".join(think_buf)
                        out_tokens = estimate_tokens(full_text)
                        if full_think:
                            out_tokens += estimate_tokens(full_think)

                        # 构建 message_delta，注入结构化搜索信源和 annotations
                        msg_delta: dict = {"stop_reason": "end_turn", "stop_sequence": None}
                        sources = adapter.search_sources_list()
                        if sources:
                            msg_delta["search_sources"] = sources
                        if collected_annotations:
                            msg_delta["annotations"] = collected_annotations
                        yield _sse("message_delta", {
                            "type":  "message_delta",
                            "delta": msg_delta,
                            "usage": {"output_tokens": out_tokens},
                        })
                        yield _sse("message_stop", {"type": "message_stop"})
                        yield "data: [DONE]\n\n"
                        success = True
                        logger.info(
                            "messages stream completed: attempt={}/{} model={} text_len={} think_len={} images={}",
                            attempt + 1, max_retries + 1, model,
                            len(full_text), len(full_think), len(adapter.image_urls),
                        )

                except UpstreamError as exc:
                    fail_exc = exc
                    if _should_retry_upstream(exc, retry_codes) and attempt < max_retries:
                        _retry = True
                        logger.warning(
                            "messages stream retry: attempt={}/{} status={} token={}...",
                            attempt + 1, max_retries, exc.status, token[:8],
                        )
                    else:
                        raise

            finally:
                await directory.release(acct)
                kind = (
                    FeedbackKind.SUCCESS if success
                    else _feedback_kind(fail_exc) if fail_exc
                    else FeedbackKind.SERVER_ERROR
                )
                await directory.feedback(token, kind, selected_mode_id, now_s_val=now_s())
                if success:
                    asyncio.create_task(_quota_sync(token, selected_mode_id)).add_done_callback(_log_task_exception)
                else:
                    asyncio.create_task(_fail_sync(token, selected_mode_id, fail_exc)).add_done_callback(_log_task_exception)

            if success or not _retry:
                return
            excluded.append(token)

    if stream:
        return _run_stream()

    # -------------------------------------------------------------------------
    # Non-streaming
    # -------------------------------------------------------------------------
    excluded: list[str] = []
    token    = ""
    adapter  = StreamAdapter()

    for attempt in range(max_retries + 1):
        acct, selected_mode_id = await reserve_account(
            directory,
            spec,
            now_s_override=now_s(),
            exclude_tokens=excluded or None,
        )
        if acct is None:
            raise RateLimitError("No available accounts for this model tier")

        token    = acct.token
        success  = False
        _retry   = False
        fail_exc: BaseException | None = None
        adapter  = StreamAdapter()

        try:
            try:
                ended = False
                async for line in _stream_chat(
                    token     = token,
                    mode_id   = ModeId(selected_mode_id),
                    message   = internal_message,
                    files     = files,
                    timeout_s = timeout_s,
                ):
                    event_type, data = classify_line(line)
                    if event_type == "done":
                        break
                    if event_type != "data" or not data:
                        continue
                    for ev in adapter.feed(data):
                        if ev.kind == "soft_stop":
                            ended = True
                            break
                    if ended:
                        break
                success = True

            except UpstreamError as exc:
                fail_exc = exc
                if _should_retry_upstream(exc, retry_codes) and attempt < max_retries:
                    _retry = True
                    logger.warning(
                        "messages retry: attempt={}/{} status={} token={}...",
                        attempt + 1, max_retries, exc.status, token[:8],
                    )
                else:
                    raise

        finally:
            await directory.release(acct)
            kind = (
                FeedbackKind.SUCCESS if success
                else _feedback_kind(fail_exc) if fail_exc
                else FeedbackKind.SERVER_ERROR
            )
            await directory.feedback(token, kind, selected_mode_id, now_s_val=now_s())
            if success:
                asyncio.create_task(_quota_sync(token, selected_mode_id)).add_done_callback(_log_task_exception)
            else:
                asyncio.create_task(_fail_sync(token, selected_mode_id, fail_exc)).add_done_callback(_log_task_exception)

        if success or not _retry:
            break
        excluded.append(token)

    full_text = "".join(adapter.text_buf)

    # Resolve image attachments
    if adapter.image_urls:
        img_texts = await asyncio.gather(
            *[_resolve_image(token, url, img_id) for url, img_id in adapter.image_urls],
            return_exceptions=True,
        )
        for img_text in img_texts:
            if isinstance(img_text, BaseException):
                logger.warning("messages image resolve failed: error={}", img_text)
            elif isinstance(img_text, str):
                full_text = (full_text + "\n\n" if full_text else "") + img_text

    references = adapter.references_suffix()
    if references:
        full_text += references

    full_think = ("".join(adapter.thinking_buf) or "") if emit_think else ""

    in_tokens  = estimate_prompt_tokens(internal_message)
    out_tokens = estimate_tokens(full_text)
    if full_think:
        out_tokens += estimate_tokens(full_think)

    # Check for tool calls
    if tool_names:
        tc_result = parse_tool_calls(full_text, tool_names)
        if tc_result.calls:
            content: list[dict] = []
            for call in tc_result.calls:
                try:
                    parsed_input = orjson.loads(call.arguments)
                except (orjson.JSONDecodeError, ValueError):
                    parsed_input = {}
                content.append({
                    "type":  "tool_use",
                    "id":    call.call_id,
                    "name":  call.name,
                    "input": parsed_input,
                })
            ct = estimate_tool_call_tokens(tc_result.calls)
            logger.info("messages tool_calls: model={} calls={}", model, len(tc_result.calls))
            resp = _build_message_response(msg_id, model, content, "tool_use", in_tokens, ct)
            # 注入结构化搜索信源（tool_use 场景）
            sources = adapter.search_sources_list()
            if sources:
                resp["search_sources"] = sources
            return resp

    logger.info(
        "messages request completed: model={} text_len={} think_len={} images={}",
        model, len(full_text), len(full_think), len(adapter.image_urls),
    )

    content = [{"type": "text", "text": full_text}]
    anns = adapter.annotations_list()
    if anns:
        content[0]["annotations"] = anns
    resp = _build_message_response(msg_id, model, content, "end_turn", in_tokens, out_tokens)
    sources = adapter.search_sources_list()
    if sources:
        resp["search_sources"] = sources
    return resp


__all__ = ["create"]
