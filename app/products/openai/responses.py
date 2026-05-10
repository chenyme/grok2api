"""OpenAI Responses API handler (/v1/responses).

Unsupported request fields are silently discarded.
Streaming emits standard Responses API SSE events.
"""

import asyncio
import re
from typing import Any, AsyncGenerator

from app.platform.logging.logger import logger
from app.platform.config.snapshot import get_config
from app.platform.errors import RateLimitError, UpstreamError
from app.platform.runtime.clock import now_s
from app.platform.tokens import estimate_prompt_tokens, estimate_tokens, estimate_tool_call_tokens
from app.control.model.enums import ModeId
from app.control.model.registry import resolve as resolve_model
from app.control.account.enums import FeedbackKind
from app.dataplane.reverse.protocol.xai_chat import classify_line, StreamAdapter
from app.products._account_selection import reserve_account, selection_max_retries

from .chat import _stream_chat, _extract_message, _resolve_image, _quota_sync, _fail_sync, _parse_retry_codes, _feedback_kind, _log_task_exception, _upstream_body_excerpt
from .chat import _configured_retry_codes, _should_retry_upstream
from ._format import (
    make_resp_id, build_resp_usage, make_resp_object, format_sse,
)
from app.dataplane.reverse.protocol.tool_prompt import (
    build_tool_system_prompt, extract_tool_names, inject_into_message, tool_calls_to_xml,
)
from app.dataplane.reverse.protocol.tool_parser import parse_tool_calls
from ._tool_sieve import ToolSieve


from ._codex_tools import (
    _PATCH_COMPLETED_RE,
    _PATCH_FAILURE_RE,
    _forced_tool_choice,
    _looks_like_codex_tool_run,
    _normalize_codex_tool_calls,
    _synthesize_codex_tool_call,
    _to_chat_tools,
)


async def _collect_chat_text(
    *,
    token: str,
    selected_mode_id: int,
    message: str,
    files: list[str],
    timeout_s: float,
) -> tuple[str, str, StreamAdapter]:
    adapter = StreamAdapter()
    async for line in _stream_chat(
        token     = token,
        mode_id   = ModeId(selected_mode_id),
        message   = message,
        files     = files,
        timeout_s = timeout_s,
    ):
        event_type, data = classify_line(line)
        if event_type == "done":
            break
        if event_type != "data" or not data:
            continue
        ended = False
        for ev in adapter.feed(data):
            if ev.kind == "soft_stop":
                ended = True
                break
        if ended:
            break
    return "".join(adapter.text_buf), "".join(adapter.thinking_buf), adapter


async def _repair_tool_calls(
    *,
    token: str,
    selected_mode_id: int,
    original_message: str,
    previous_text: str,
    chat_tools: list[dict],
    tool_names: list[str],
    timeout_s: float,
) -> list | None:
    if not _looks_like_codex_tool_run(tool_names, original_message, previous_text):
        return None
    if _PATCH_COMPLETED_RE.search(original_message) and _PATCH_FAILURE_RE.search(original_message):
        return None

    forced_choice = _forced_tool_choice(tool_names, original_message, previous_text)
    repair_prompt = build_tool_system_prompt(chat_tools, forced_choice)
    retry_hint = ""
    if _PATCH_FAILURE_RE.search(original_message) and re.search(r"patch:\s*failed|apply_patch.*failed|Invalid Context", original_message, re.I):
        retry_hint = (
            "\n[system]: The previous apply_patch failed. Inspect the target "
            "file if needed, then emit a corrected minimal apply_patch command. "
            "Do not repeat the same failed patch unchanged.\n"
        )
    repair_message = (
        f"[system]: {repair_prompt}\n\n"
        "[system]: The previous assistant response failed because it described "
        "a tool call instead of emitting a structured tool call. Convert the "
        "intended next action into the required XML now. Output XML only.\n\n"
        f"{retry_hint}"
        f"[conversation]:\n{original_message}\n\n"
        f"[previous assistant response]:\n{previous_text}"
    )
    repaired_text, _, _ = await _collect_chat_text(
        token=token,
        selected_mode_id=selected_mode_id,
        message=repair_message,
        files=[],
        timeout_s=timeout_s,
    )
    result = parse_tool_calls(repaired_text, tool_names)
    if result.calls:
        logger.info(
            "responses repaired missed tool call: tool_names={} call_count={}",
            tool_names,
            len(result.calls),
        )
        return result.calls
    synthetic = _synthesize_codex_tool_call(tool_names, original_message, previous_text)
    if synthetic:
        logger.info(
            "responses synthesized codex tool call: tool={} args={}",
            synthetic[0].name,
            synthetic[0].arguments,
        )
        return synthetic
    logger.warning(
        "responses tool repair failed: tool_names={} text_excerpt={}",
        tool_names,
        repaired_text[:500],
    )
    return None


# ---------------------------------------------------------------------------
# Tool call helpers (Responses API format)
# ---------------------------------------------------------------------------

def _build_fc_items(calls: list) -> list[dict]:
    """Allocate stable IDs and build function_call output items for response.completed."""
    return [
        {
            "id":        make_resp_id("fc"),
            "type":      "function_call",
            "call_id":   tc.call_id,
            "name":      tc.name,
            "arguments": tc.arguments,
            "status":    "completed",
        }
        for tc in calls
    ]


async def _emit_fc_events(items: list[dict], base_idx: int):
    """Async generator — yields SSE events for pre-built function_call items.

    Using pre-built items ensures the IDs in the streaming events match those
    in the final response.completed payload.
    """
    for i, item in enumerate(items):
        out_idx    = base_idx + i
        fc_item_id = item["id"]
        yield format_sse("response.output_item.added", {
            "type":         "response.output_item.added",
            "output_index": out_idx,
            "item": {
                "id":        fc_item_id,
                "type":      "function_call",
                "call_id":   item["call_id"],
                "name":      item["name"],
                "arguments": "",
                "status":    "in_progress",
            },
        })
        yield format_sse("response.function_call_arguments.delta", {
            "type":         "response.function_call_arguments.delta",
            "item_id":      fc_item_id,
            "output_index": out_idx,
            "delta":        item["arguments"],
        })
        yield format_sse("response.function_call_arguments.done", {
            "type":         "response.function_call_arguments.done",
            "item_id":      fc_item_id,
            "output_index": out_idx,
            "arguments":    item["arguments"],
        })
        yield format_sse("response.output_item.done", {
            "type":         "response.output_item.done",
            "output_index": out_idx,
            "item":         item,
        })


async def _emit_response_start(response_id: str, model: str):
    """Emit the standard Responses API stream opening events."""
    response = make_resp_object(response_id, model, "in_progress", [])
    yield format_sse("response.created", {
        "type":     "response.created",
        "response": response,
    })
    yield format_sse("response.in_progress", {
        "type":     "response.in_progress",
        "response": response,
    })


# ---------------------------------------------------------------------------
# Input normalisation
# ---------------------------------------------------------------------------

def _parse_input(input_val: str | list) -> list[dict]:
    """Convert Responses API input to our internal messages list.

    Handles message, function_call, and function_call_output item types.
    function_call → assistant message with tool_calls
    function_call_output → tool result message
    """
    if isinstance(input_val, str):
        return [{"role": "user", "content": input_val}]

    messages: list[dict] = []
    for item in input_val:
        if not isinstance(item, dict):
            continue
        item_type = item.get("type", "message" if "role" in item else None)

        if item_type == "function_call":
            # Reconstruct as assistant message with tool_calls (Chat Completions format)
            call_id = item.get("call_id", "")
            name    = item.get("name", "")
            args    = item.get("arguments", "{}")
            messages.append({
                "role":    "assistant",
                "content": None,
                "tool_calls": [{
                    "id":   call_id,
                    "type": "function",
                    "function": {"name": name, "arguments": args},
                }],
            })
            continue

        if item_type == "function_call_output":
            # Reconstruct as tool result message
            call_id = item.get("call_id", "")
            output  = item.get("output", "")
            messages.append({
                "role":         "tool",
                "tool_call_id": call_id,
                "content":      output,
            })
            continue

        if item_type != "message":
            continue  # skip reasoning items, etc.

        role    = item.get("role", "user")
        content = item.get("content", "")

        if isinstance(content, list):
            normalized: list[dict] = []
            for part in content:
                if not isinstance(part, dict):
                    continue
                ptype = part.get("type", "")
                if ptype in ("input_text", "output_text"):
                    normalized.append({"type": "text", "text": part.get("text", "")})
                elif ptype == "image":
                    src = part.get("image_url") or part.get("source") or {}
                    url = src.get("url", "")
                    if url:
                        normalized.append({"type": "image_url", "image_url": {"url": url}})
                elif ptype == "input_image":
                    src = part.get("image_url") or part.get("source") or {}
                    if isinstance(src, dict):
                        url = src.get("url", "")
                    else:
                        url = str(src or "")
                    if url:
                        normalized.append({"type": "image_url", "image_url": {"url": url}})
                else:
                    normalized.append(part)
            content = normalized

        messages.append({"role": role, "content": content})
    return messages


# ---------------------------------------------------------------------------
# Main handler
# ---------------------------------------------------------------------------

async def create(
    *,
    model:        str,
    input_val:    str | list,
    instructions: str | None,
    stream:       bool,
    emit_think:   bool,
    temperature:  float,
    top_p:        float,
    tools:        list[dict] | None = None,
    tool_choice:  Any = None,
) -> dict | AsyncGenerator[str, None]:

    cfg     = get_config()
    spec    = resolve_model(model)
    mode_id = int(spec.mode_id)   # cast once, reuse everywhere

    messages: list[dict] = []
    if instructions:
        messages.append({"role": "system", "content": instructions})
    messages.extend(_parse_input(input_val))

    message, files = _extract_message(messages)
    if not message.strip():
        raise UpstreamError("Empty message after extraction", status=400)

    # Tool prompt injection — only modify the message text, never the Grok payload
    # Normalise to Chat Completions format first (Responses API uses a flat structure)
    tool_names: list[str] = []
    chat_tools: list[dict] = []
    if tools:
        chat_tools = _to_chat_tools(tools)
        tool_names = extract_tool_names(chat_tools)
        if chat_tools:
            tool_prompt = build_tool_system_prompt(chat_tools, tool_choice)
            message = inject_into_message(message, tool_prompt)
            logger.info("responses tool injection: tool_names={} choice={}", tool_names, tool_choice)

    from app.dataplane.account import _directory as _acct_dir
    if _acct_dir is None:
        raise RateLimitError("Account directory not initialised")
    directory = _acct_dir

    max_retries  = selection_max_retries()
    retry_codes  = _configured_retry_codes(cfg)
    response_id  = make_resp_id("resp")
    reasoning_id = make_resp_id("rs")
    message_id   = make_resp_id("msg")
    timeout_s    = cfg.get_float("chat.timeout", 120.0)

    # -------------------------------------------------------------------------
    # Streaming
    # -------------------------------------------------------------------------
    async def _run_stream() -> AsyncGenerator[str, None]:
        pre_calls = _normalize_codex_tool_calls(
            _synthesize_codex_tool_call(tool_names, message, "") if tool_names else [],
            tool_names=tool_names,
            message=message,
        )
        if pre_calls:
            async for evt in _emit_response_start(response_id, model):
                yield evt
            fc_items = _build_fc_items(pre_calls)
            async for evt in _emit_fc_events(fc_items, 0):
                yield evt
            pt = estimate_prompt_tokens(message)
            ct = estimate_tool_call_tokens(pre_calls)
            yield format_sse("response.completed", {
                "type":     "response.completed",
                "response": make_resp_object(
                    response_id, model, "completed", fc_items,
                    build_resp_usage(pt, ct, 0),
                ),
            })
            yield "data: [DONE]\n\n"
            logger.info(
                "responses stream pre-synthesized codex tool_calls: model={} call_count={}",
                model,
                len(pre_calls),
            )
            return

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
            adapter           = StreamAdapter()
            think_buf:  list[str] = []
            text_buf:   list[str] = []
            reasoning_started   = False
            reasoning_closed    = False
            message_started     = False
            sieve               = ToolSieve(tool_names) if tool_names else None
            tool_calls_emitted  = False
            detected_fc_items: list[dict] = []
            collected_annotations: list[dict] = []

            try:
                try:
                    if tool_names:
                        async for evt in _emit_response_start(response_id, model):
                            yield evt
                        full_text, full_think, tool_adapter = await _collect_chat_text(
                            token=token,
                            selected_mode_id=selected_mode_id,
                            message=message,
                            files=files,
                            timeout_s=timeout_s,
                        )
                        parse_result = parse_tool_calls(full_text, tool_names)
                        calls = parse_result.calls
                        parsed_calls = list(calls)
                        if not calls:
                            repaired = await _repair_tool_calls(
                                token=token,
                                selected_mode_id=selected_mode_id,
                                original_message=message,
                                previous_text=full_text or full_think,
                                chat_tools=chat_tools,
                                tool_names=tool_names,
                                timeout_s=timeout_s,
                            )
                            calls = repaired or []
                        calls = _normalize_codex_tool_calls(
                            calls,
                            tool_names=tool_names,
                            message=message,
                        )
                        if parsed_calls and not calls and _PATCH_COMPLETED_RE.search(message):
                            full_text = "Done."

                        if calls:
                            fc_items = _build_fc_items(calls)
                            async for evt in _emit_fc_events(fc_items, 0):
                                yield evt
                            pt = estimate_prompt_tokens(message)
                            ct = estimate_tool_call_tokens(calls)
                            yield format_sse("response.completed", {
                                "type":     "response.completed",
                                "response": make_resp_object(
                                    response_id, model, "completed", fc_items,
                                    build_resp_usage(pt, ct, 0),
                                ),
                            })
                            yield "data: [DONE]\n\n"
                            success = True
                            logger.info("responses stream tool_calls buffered: attempt={}/{} model={} call_count={}",
                                        attempt + 1, max_retries + 1, model, len(calls))
                        else:
                            if tool_adapter.image_urls:
                                for url, img_id in tool_adapter.image_urls:
                                    img_text = await _resolve_image(token, url, img_id)
                                    full_text += ("\n\n" if full_text else "") + img_text
                            references = tool_adapter.references_suffix()
                            if references:
                                full_text += references
                            msg_item = {
                                "id":      message_id,
                                "type":    "message",
                                "role":    "assistant",
                                "content": [{"type": "output_text", "text": full_text, "annotations": tool_adapter.annotations_list()}],
                                "status":  "completed",
                            }
                            yield format_sse("response.output_item.added", {
                                "type":         "response.output_item.added",
                                "output_index": 0,
                                "item":         {
                                    "id": message_id, "type": "message",
                                    "role": "assistant", "content": [], "status": "in_progress",
                                },
                            })
                            yield format_sse("response.content_part.added", {
                                "type":          "response.content_part.added",
                                "item_id":       message_id,
                                "output_index":  0,
                                "content_index": 0,
                                "part":          {"type": "output_text", "text": "", "annotations": []},
                            })
                            if full_text:
                                yield format_sse("response.output_text.delta", {
                                    "type":          "response.output_text.delta",
                                    "item_id":       message_id,
                                    "output_index":  0,
                                    "content_index": 0,
                                    "delta":         full_text,
                                })
                            yield format_sse("response.output_text.done", {
                                "type":          "response.output_text.done",
                                "item_id":       message_id,
                                "output_index":  0,
                                "content_index": 0,
                                "text":          full_text,
                            })
                            yield format_sse("response.content_part.done", {
                                "type":          "response.content_part.done",
                                "item_id":       message_id,
                                "output_index":  0,
                                "content_index": 0,
                                "part":          msg_item["content"][0],
                            })
                            yield format_sse("response.output_item.done", {
                                "type":         "response.output_item.done",
                                "output_index": 0,
                                "item":         msg_item,
                            })
                            pt = estimate_prompt_tokens(message)
                            ct = estimate_tokens(full_text)
                            yield format_sse("response.completed", {
                                "type":     "response.completed",
                                "response": make_resp_object(
                                    response_id, model, "completed", [msg_item],
                                    build_resp_usage(pt, ct, 0),
                                ),
                            })
                            yield "data: [DONE]\n\n"
                            success = True
                            logger.info("responses stream text buffered: attempt={}/{} model={} text_len={}",
                                        attempt + 1, max_retries + 1, model, len(full_text))
                        return

                    async for evt in _emit_response_start(response_id, model):
                        yield evt

                    ended = False
                    async for line in _stream_chat(
                        token     = token,
                        mode_id   = ModeId(selected_mode_id),
                        message   = message,
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

                            if ev.kind == "thinking" and emit_think and not reasoning_closed:
                                if not reasoning_started:
                                    reasoning_started = True
                                    yield format_sse("response.output_item.added", {
                                        "type":         "response.output_item.added",
                                        "output_index": 0,
                                        "item":         {
                                            "id": reasoning_id, "type": "reasoning",
                                            "summary": [], "status": "in_progress",
                                        },
                                    })
                                    yield format_sse("response.reasoning_summary_part.added", {
                                        "type":          "response.reasoning_summary_part.added",
                                        "item_id":       reasoning_id,
                                        "output_index":  0,
                                        "summary_index": 0,
                                        "part":          {"type": "summary_text", "text": ""},
                                    })
                                think_buf.append(ev.content)
                                yield format_sse("response.reasoning_summary_text.delta", {
                                    "type":          "response.reasoning_summary_text.delta",
                                    "item_id":       reasoning_id,
                                    "output_index":  0,
                                    "summary_index": 0,
                                    "delta":         ev.content,
                                })

                            elif ev.kind == "text":
                                if reasoning_started and not reasoning_closed:
                                    reasoning_closed = True
                                    full_think = "".join(think_buf)
                                    yield format_sse("response.reasoning_summary_text.done", {
                                        "type":          "response.reasoning_summary_text.done",
                                        "item_id":       reasoning_id,
                                        "output_index":  0,
                                        "summary_index": 0,
                                        "text":          full_think,
                                    })
                                    yield format_sse("response.reasoning_summary_part.done", {
                                        "type":          "response.reasoning_summary_part.done",
                                        "item_id":       reasoning_id,
                                        "output_index":  0,
                                        "summary_index": 0,
                                        "part":          {"type": "summary_text", "text": full_think},
                                    })
                                    yield format_sse("response.output_item.done", {
                                        "type":         "response.output_item.done",
                                        "output_index": 0,
                                        "item":         {
                                            "id":      reasoning_id,
                                            "type":    "reasoning",
                                            "summary": [{"type": "summary_text", "text": full_think}],
                                            "status":  "completed",
                                        },
                                    })

                                # Feed through ToolSieve if tools are active
                                if sieve is not None:
                                    safe_text, calls = sieve.feed(ev.content)
                                    if calls is not None:
                                        fc_items = _build_fc_items(calls)
                                        detected_fc_items = fc_items
                                        base_idx = 1 if reasoning_started else 0
                                        async for evt in _emit_fc_events(fc_items, base_idx):
                                            yield evt
                                        tool_calls_emitted = True
                                        ended = True
                                        break
                                    text_chunk = safe_text
                                else:
                                    text_chunk = ev.content

                                if text_chunk:
                                    msg_idx = 1 if reasoning_started else 0
                                    if not message_started:
                                        message_started = True
                                        yield format_sse("response.output_item.added", {
                                            "type":         "response.output_item.added",
                                            "output_index": msg_idx,
                                            "item":         {
                                                "id": message_id, "type": "message",
                                                "role": "assistant", "content": [], "status": "in_progress",
                                            },
                                        })
                                        yield format_sse("response.content_part.added", {
                                            "type":          "response.content_part.added",
                                            "item_id":       message_id,
                                            "output_index":  msg_idx,
                                            "content_index": 0,
                                            "part":          {"type": "output_text", "text": "", "annotations": []},
                                        })

                                    text_buf.append(text_chunk)
                                    yield format_sse("response.output_text.delta", {
                                        "type":          "response.output_text.delta",
                                        "item_id":       message_id,
                                        "output_index":  msg_idx,
                                        "content_index": 0,
                                        "delta":         text_chunk,
                                    })

                            elif ev.kind == "annotation" and ev.annotation_data:
                                if message_started:
                                    collected_annotations.append(ev.annotation_data)
                                    msg_idx = 1 if reasoning_started else 0
                                    yield format_sse("response.output_text.annotation.added", {
                                        "type":             "response.output_text.annotation.added",
                                        "item_id":          message_id,
                                        "output_index":     msg_idx,
                                        "content_index":    0,
                                        "annotation_index": len(collected_annotations) - 1,
                                        "annotation":       ev.annotation_data,
                                    })

                            elif ev.kind == "soft_stop":
                                ended = True
                                break

                        if ended:
                            break

                    # Flush sieve after stream ends (incomplete XML at end of stream)
                    if sieve is not None and not tool_calls_emitted:
                        calls = sieve.flush()
                        if calls:
                            fc_items = _build_fc_items(calls)
                            detected_fc_items = fc_items
                            base_idx = 1 if reasoning_started else 0
                            async for evt in _emit_fc_events(fc_items, base_idx):
                                yield evt
                            tool_calls_emitted = True

                    if tool_calls_emitted:
                        # Build output with function_call items only
                        full_think = "".join(think_buf)
                        output: list[dict] = []
                        if reasoning_started and full_think:
                            output.append({
                                "id":      reasoning_id,
                                "type":    "reasoning",
                                "summary": [{"type": "summary_text", "text": full_think}],
                                "status":  "completed",
                            })
                        output.extend(detected_fc_items)
                        pt = estimate_prompt_tokens(message)
                        ct = estimate_tool_call_tokens(detected_fc_items)
                        rt = estimate_tokens(full_think) if full_think else 0
                        yield format_sse("response.completed", {
                            "type":     "response.completed",
                            "response": make_resp_object(
                                response_id, model, "completed", output,
                                build_resp_usage(pt, ct + rt, rt),
                            ),
                        })
                        yield "data: [DONE]\n\n"
                        success = True
                        logger.info("responses stream tool_calls: attempt={}/{} model={}",
                                    attempt + 1, max_retries + 1, model)
                    else:
                        # Normal text path
                        msg_idx = 1 if reasoning_started else 0
                        for url, img_id in adapter.image_urls:
                            img_text = await _resolve_image(token, url, img_id)
                            img_md   = img_text + "\n"
                            text_buf.append(img_md)
                            if message_started:
                                yield format_sse("response.output_text.delta", {
                                    "type":          "response.output_text.delta",
                                    "item_id":       message_id,
                                    "output_index":  msg_idx,
                                    "content_index": 0,
                                    "delta":         img_md,
                                })

                        references = adapter.references_suffix()
                        if references:
                            text_buf.append(references)
                            if message_started:
                                yield format_sse("response.output_text.delta", {
                                    "type":          "response.output_text.delta",
                                    "item_id":       message_id,
                                    "output_index":  msg_idx,
                                    "content_index": 0,
                                    "delta":         references,
                                })

                        full_text = "".join(text_buf)
                        if message_started:
                            yield format_sse("response.output_text.done", {
                                "type":          "response.output_text.done",
                                "item_id":       message_id,
                                "output_index":  msg_idx,
                                "content_index": 0,
                                "text":          full_text,
                            })
                            yield format_sse("response.content_part.done", {
                                "type":          "response.content_part.done",
                                "item_id":       message_id,
                                "output_index":  msg_idx,
                                "content_index": 0,
                                "part":          {"type": "output_text", "text": full_text, "annotations": collected_annotations},
                            })
                            # 构建 message item（流式 output_item.done + response.completed 共用）
                            sources = adapter.search_sources_list()
                            msg_item: dict = {
                                "id":      message_id,
                                "type":    "message",
                                "role":    "assistant",
                                "content": [{"type": "output_text", "text": full_text, "annotations": collected_annotations}],
                                "status":  "completed",
                            }
                            if sources:
                                msg_item["search_sources"] = sources
                            yield format_sse("response.output_item.done", {
                                "type":         "response.output_item.done",
                                "output_index": msg_idx,
                                "item":         msg_item,
                            })

                        full_think = "".join(think_buf)
                        output = []
                        if reasoning_started and full_think:
                            output.append({
                                "id":      reasoning_id,
                                "type":    "reasoning",
                                "summary": [{"type": "summary_text", "text": full_think}],
                                "status":  "completed",
                            })
                        # 复用 msg_item（message_started 时已构建）；未启动时重新构建
                        if not message_started:
                            msg_item = {
                                "id":      message_id,
                                "type":    "message",
                                "role":    "assistant",
                                "content": [{"type": "output_text", "text": full_text, "annotations": adapter.annotations_list()}],
                                "status":  "completed",
                            }
                            sources = adapter.search_sources_list()
                            if sources:
                                msg_item["search_sources"] = sources
                        output.append(msg_item)

                        pt  = estimate_prompt_tokens(message)
                        ct  = estimate_tokens(full_text)
                        rt  = estimate_tokens(full_think) if full_think else 0
                        yield format_sse("response.completed", {
                            "type":     "response.completed",
                            "response": make_resp_object(
                                response_id, model, "completed", output,
                                build_resp_usage(pt, ct + rt, rt),
                            ),
                        })
                        yield "data: [DONE]\n\n"
                        success = True
                        logger.info("responses stream completed: attempt={}/{} model={} text_len={} reasoning_len={} image_count={}",
                                    attempt + 1, max_retries + 1, model,
                                    len(full_text), len(full_think), len(adapter.image_urls))

                except UpstreamError as exc:
                    fail_exc = exc
                    if _should_retry_upstream(exc, retry_codes) and attempt < max_retries:
                        _retry = True
                        logger.warning("responses stream retry scheduled: attempt={}/{} status={} token={}...",
                                       attempt + 1, max_retries, exc.status, token[:8])
                    else:
                        logger.warning(
                            "responses stream upstream failed: attempt={}/{} model={} status={} body={}",
                            attempt + 1,
                            max_retries + 1,
                            model,
                            exc.status,
                            _upstream_body_excerpt(exc),
                        )
                        raise

            finally:
                await directory.release(acct)
                kind = FeedbackKind.SUCCESS if success else _feedback_kind(fail_exc) if fail_exc else FeedbackKind.SERVER_ERROR
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
    pre_calls = _normalize_codex_tool_calls(
        _synthesize_codex_tool_call(tool_names, message, "") if tool_names else [],
        tool_names=tool_names,
        message=message,
    )
    if pre_calls:
        output = _build_fc_items(pre_calls)
        pt = estimate_prompt_tokens(message)
        ct = estimate_tool_call_tokens(pre_calls)
        logger.info("responses pre-synthesized codex tool_calls: model={} call_count={}", model, len(pre_calls))
        return make_resp_object(
            response_id, model, "completed", output,
            build_resp_usage(pt, ct, 0),
        )

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
        adapter  = StreamAdapter()   # fresh adapter per attempt

        try:
            try:
                async for line in _stream_chat(
                    token     = token,
                    mode_id   = ModeId(selected_mode_id),
                    message   = message,
                    files     = files,
                    timeout_s = timeout_s,
                ):
                    event_type, data = classify_line(line)
                    if event_type == "done":
                        break
                    if event_type != "data" or not data:
                        continue
                    ended = False
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
                    logger.warning("responses retry scheduled: attempt={}/{} status={} token={}...",
                                   attempt + 1, max_retries, exc.status, token[:8])
                else:
                    logger.warning(
                        "responses upstream failed: attempt={}/{} model={} status={} body={}",
                        attempt + 1,
                        max_retries + 1,
                        model,
                        exc.status,
                        _upstream_body_excerpt(exc),
                    )
                    raise

        finally:
            await directory.release(acct)
            kind = FeedbackKind.SUCCESS if success else _feedback_kind(fail_exc) if fail_exc else FeedbackKind.SERVER_ERROR
            await directory.feedback(token, kind, selected_mode_id)
            if success:
                asyncio.create_task(_quota_sync(token, selected_mode_id)).add_done_callback(_log_task_exception)
            else:
                asyncio.create_task(_fail_sync(token, selected_mode_id, fail_exc)).add_done_callback(_log_task_exception)

        if success or not _retry:
            break
        excluded.append(token)

    full_text = "".join(adapter.text_buf)
    if adapter.image_urls:
        img_texts = await asyncio.gather(
            *[_resolve_image(token, url, img_id) for url, img_id in adapter.image_urls],
            return_exceptions=True,
        )
        for img_text in img_texts:
            if isinstance(img_text, BaseException):
                logger.warning("responses image resolve failed: error={}", img_text)
            elif isinstance(img_text, str):
                if full_text:
                    full_text += "\n\n"
                full_text += img_text
    references = adapter.references_suffix()
    if references:
        full_text += references

    full_think = ("".join(adapter.thinking_buf) or "") if emit_think else ""

    # Check for tool calls in the accumulated text
    if tool_names:
        tc_result = parse_tool_calls(full_text, tool_names)
        calls = tc_result.calls
        parsed_calls = list(calls)
        if not calls:
            repaired = await _repair_tool_calls(
                token=token,
                selected_mode_id=selected_mode_id,
                original_message=message,
                previous_text=full_text or full_think,
                chat_tools=chat_tools,
                tool_names=tool_names,
                timeout_s=timeout_s,
            )
            calls = repaired or []
        calls = _normalize_codex_tool_calls(
            calls,
            tool_names=tool_names,
            message=message,
        )
        if parsed_calls and not calls and _PATCH_COMPLETED_RE.search(message):
            full_text = "Done."
        if calls:
            output: list[dict] = []
            if full_think:
                output.append({
                    "id":      reasoning_id,
                    "type":    "reasoning",
                    "summary": [{"type": "summary_text", "text": full_think}],
                    "status":  "completed",
                })
            output.extend(_build_fc_items(calls))
            pt = estimate_prompt_tokens(message)
            ct = estimate_tool_call_tokens(calls)
            rt = estimate_tokens(full_think) if full_think else 0
            logger.info("responses tool_calls: model={} calls={}", model, len(calls))
            return make_resp_object(
                response_id, model, "completed", output,
                build_resp_usage(pt, ct + rt, rt),
            )

    logger.info("responses request completed: model={} text_len={} reasoning_len={} image_count={}",
                model, len(full_text), len(full_think), len(adapter.image_urls))

    output = []
    if full_think:
        output.append({
            "id":      reasoning_id,
            "type":    "reasoning",
            "summary": [{"type": "summary_text", "text": full_think}],
            "status":  "completed",
        })
    msg_item: dict = {
        "id":      message_id,
        "type":    "message",
        "role":    "assistant",
        "content": [{"type": "output_text", "text": full_text, "annotations": adapter.annotations_list()}],
        "status":  "completed",
    }
    sources = adapter.search_sources_list()
    if sources:
        msg_item["search_sources"] = sources
    output.append(msg_item)

    pt = estimate_prompt_tokens(message)
    ct = estimate_tokens(full_text)
    rt = estimate_tokens(full_think) if full_think else 0
    return make_resp_object(
        response_id, model, "completed", output,
        build_resp_usage(pt, ct + rt, rt),
    )


__all__ = ["create"]
