"""Chat completion service — orchestrates account selection, reverse, streaming."""

import asyncio
import base64
import re
from typing import Any, AsyncGenerator
from urllib.parse import urlparse

import orjson

from app.platform.logging.logger import logger
from app.platform.config.snapshot import get_config
from app.platform.errors import RateLimitError, UpstreamError, ValidationError
from app.platform.runtime.clock import now_s
from app.platform.storage import save_local_image
from app.platform.tokens import (
    estimate_prompt_tokens,
    estimate_tokens,
    estimate_tool_call_tokens,
)
from app.control.account.runtime import get_refresh_service
from app.control.account.invalid_credentials import feedback_kind_for_error
from app.control.model.registry import resolve as resolve_model
from app.control.model.enums import ModeId
from app.control.account.enums import FeedbackKind
from app.dataplane.account.selector import current_strategy
from app.dataplane.proxy.adapters.headers import build_http_headers
from app.dataplane.proxy import get_proxy_runtime
from app.dataplane.proxy.adapters.session import (
    ResettableSession,
    build_session_kwargs,
)
from app.dataplane.reverse.protocol.xai_chat import (
    build_chat_payload,
    classify_line,
    StreamAdapter,
)
from app.dataplane.reverse.protocol.xai_console import (
    build_console_input,
    build_console_payload,
    classify_console_sse_line,
    ConsoleStreamAdapter,
    convert_openai_tool_choice,
    convert_openai_tools_to_console,
    extract_console_annotations,
    extract_console_reasoning,
    extract_console_search_sources,
    extract_console_text,
    extract_console_tool_calls,
    extract_console_usage,
    inject_web_search_tool,
    parse_console_error,
)
from app.dataplane.reverse.protocol.xai_usage import is_invalid_credentials_error
from app.dataplane.reverse.runtime.endpoint_table import CHAT, CONSOLE_RESPONSES
from app.dataplane.reverse.transport.asset_upload import upload_from_input
from app.dataplane.reverse.protocol.tool_prompt import (
    build_tool_system_prompt,
    extract_tool_names,
    inject_into_message,
    tool_calls_to_xml,
)
from app.dataplane.reverse.protocol.tool_parser import parse_tool_calls
from ._format import (
    make_response_id,
    make_stream_chunk,
    make_thinking_chunk,
    make_chat_response,
    make_tool_call_chunk,
    make_tool_call_done_chunk,
    make_tool_call_response,
    build_usage,
)
from ._tool_sieve import ToolSieve
from app.products._account_selection import reserve_account, selection_max_retries


def _to_chat_annotations(anns: list[dict]) -> list[dict]:
    """扁平 annotations → Chat Completions 嵌套格式（内层无 type）"""
    return (
        [
            {
                "type": "url_citation",
                "url_citation": {
                    "url": a["url"],
                    "title": a["title"],
                    "start_index": a["start_index"],
                    "end_index": a["end_index"],
                },
            } for a in anns
        ] if anns else [])


def _log_task_exception(task: "asyncio.Task") -> None:
    """Done-callback: log exceptions from fire-and-forget tasks."""
    exc = task.exception() if not task.cancelled() else None
    if exc:
        logger.warning("background task failed: task={} error={}", task.get_name(), exc)


def _upstream_body_excerpt(exc: UpstreamError, *, limit: int = 240) -> str:
    details = getattr(exc, "details", {})
    if not isinstance(details, dict):
        return "-"
    body = str(details.get("body", "") or "").replace("\n", "\\n")
    return body[:limit] or "-"


def _transport_upstream_error(exc: BaseException, *, context: str) -> UpstreamError:
    if isinstance(exc, UpstreamError):
        return exc
    body = str(exc).replace("\n", "\\n")[:400]
    return UpstreamError(
        f"{context}: {exc}",
        status=502,
        body=body,
    )


async def _quota_sync(token: str, mode_id: int) -> None:
    """Fire-and-forget: fetch real quota after a successful call."""
    try:
        if current_strategy() != "quota":
            return
        svc = get_refresh_service()
        if svc:
            await svc.refresh_call_async(token, mode_id)
    except Exception as exc:
        logger.warning(
            "chat quota sync failed: token={}... mode_id={} error={}",
            token[:10],
            mode_id,
            exc,
        )


async def _fail_sync(token: str, mode_id: int, exc: BaseException | None = None) -> None:
    """Fire-and-forget: persist failure metadata after a failed call.

    In random mode this helper must not trigger upstream quota probes. It still
    records failures so 401 invalidation and local failure accounting continue
    to work unchanged.
    """
    try:
        svc = get_refresh_service()
        if svc:
            await svc.record_failure_async(token, mode_id, exc)
            if (current_strategy() == "quota" and getattr(exc, "status", None) == 429):
                result = await svc.refresh_on_demand()
                logger.info(
                    "account on-demand refresh triggered: token={}... mode_id={} refreshed={} failed={} rate_limited={}",
                    token[:10],
                    mode_id,
                    result.refreshed,
                    result.failed,
                    result.rate_limited,
                )
    except Exception as e:
        logger.warning(
            "chat fail sync error: token={}... mode_id={} error={}",
            token[:10],
            mode_id,
            e,
        )


def _parse_retry_codes(s: str) -> frozenset[int]:
    """Parse retry status-code config from either a CSV string or a list."""
    result: set[int] = set()
    parts: list[object]
    if isinstance(s, str):
        parts = [part.strip() for part in s.split(",")]
    elif isinstance(s, (list, tuple, set)):
        parts = list(s)
    else:
        return frozenset()
    for part in parts:
        text = str(part).strip()
        if text.isdigit():
            result.add(int(text))
    return frozenset(result)


def _configured_retry_codes(cfg) -> frozenset[int]:
    """Read retry codes from current config, including legacy array keys."""
    raw = cfg.get("retry.on_codes")
    if raw is None:
        raw = cfg.get("retry.retry_status_codes", "429,401,503")
    return _parse_retry_codes(raw)


def _should_retry_upstream(exc: UpstreamError, retry_codes: frozenset[int]) -> bool:
    """Return whether this upstream error should switch to another token."""
    return exc.status in retry_codes or is_invalid_credentials_error(exc)


def _feedback_kind(exc: BaseException) -> "FeedbackKind":
    """Map an upstream exception to the appropriate FeedbackKind."""
    return feedback_kind_for_error(exc)


async def _download_image_bytes(token: str, url: str) -> tuple[bytes, str]:
    """Download image bytes via the shared asset transport used by /v1/images."""
    from app.dataplane.reverse.protocol.xai_assets import infer_content_type
    from app.dataplane.reverse.transport.assets import download_asset

    try:
        stream, content_type = await download_asset(token, url)
        chunks: list[bytes] = []
        async for chunk in stream:
            chunks.append(chunk)
    except UpstreamError:
        raise
    except Exception as exc:
        raise UpstreamError(f"Image download failed: {exc}") from exc
    return b"".join(chunks), (content_type or infer_content_type(url) or "image/jpeg")


def _save_image(raw: bytes, mime: str, image_id: str) -> str:
    """Save raw bytes to ``${DATA_DIR}/files/images`` and return the file ID."""
    return save_local_image(raw, mime, image_id)


def _is_imagine_public_url(url: str) -> bool:
    try:
        host = urlparse(url or "").hostname or ""
    except Exception:
        return False
    return host.startswith("imagine-public")


async def _resolve_image(token: str, url: str, image_id: str) -> str:
    """Return the image embed text for the response body based on image_format config.

    Format values:
      grok_url  — raw CDN URL (no download)
      local_url — download + serve locally, return accessible URL
      grok_md   — ![image](grok_cdn_url) markdown
      local_md  — ![image](local_url) markdown
      base64    — ![image](data:...) markdown
    """
    cfg = get_config()
    fmt = _normalize_image_format(cfg.get_str("features.image_format", "grok_url"))

    proxy_imagine_public = (
        _is_imagine_public_url(url) and cfg.get_bool("features.imagine_public_image_proxy", False))

    # Formats that don't need downloading
    if fmt == "grok_url" and not proxy_imagine_public:
        return url
    if fmt == "grok_md" and not proxy_imagine_public:
        return f"![image]({url})"

    # Formats that require downloading
    try:
        raw, mime = await _download_image_bytes(token, url)
    except Exception as exc:
        logger.warning("chat image download failed: fallback_to=upstream_url error={}", exc)
        return url

    if fmt == "base64":
        b64 = base64.b64encode(raw).decode()
        return f"![image](data:{mime};base64,{b64})"

    # local_url / local_md: save to disk and return local path
    file_id = await asyncio.to_thread(_save_image, raw, mime, image_id)
    app_url = cfg.get_str("app.app_url", "").rstrip("/")
    local_url = (f"{app_url}/v1/files/image?id={file_id}" if app_url else f"/v1/files/image?id={file_id}")

    if fmt in {"grok_url", "local_url"}:
        return local_url
    return f"![image]({local_url})"  # grok_md / local_md


def _normalize_image_format(value: str | None) -> str:
    fmt = (value or "grok_url").strip().lower()
    if fmt not in {"grok_url", "local_url", "grok_md", "local_md", "base64"}:
        raise ValidationError(
            "image_format must be one of [grok_url, local_url, grok_md, local_md, base64]",
            param="features.image_format",
        )
    return fmt


# 精确匹配 grok2api 注入的 Sources 段落（含标记行），用于多轮对话剥离
_SOURCES_STRIP_RE = re.compile(r"(?:^|\r?\n\r?\n)## Sources\r?\n\[grok2api-sources\]: #\r?\n[\s\S]*$")


def _strip_generated_artifacts(text: str, *, strip_sources: bool = False) -> str:
    """Remove generated assistant artifacts before reusing conversation history."""
    if not text or not isinstance(text, str):
        return text
    if strip_sources:
        text = _SOURCES_STRIP_RE.sub("", text)
    return text.strip()


def _extract_message(messages: list[dict]) -> tuple[str, list[str]]:
    """Flatten OpenAI messages into a single prompt string + file attachments."""
    parts: list[str] = []
    files: list[str] = []

    for msg in messages:
        role = msg.get("role", "user")
        content = msg.get("content") or ""
        tool_calls = msg.get("tool_calls")

        # ── role=tool: tool execution result ─────────────────────────────────
        if role == "tool":
            tool_call_id = msg.get("tool_call_id", "")
            label = (f"[tool result for {tool_call_id}]" if tool_call_id else "[tool result]")
            text = content.strip() if isinstance(content, str) else ""
            if text:
                parts.append(f"{label}:\n{text}")
            continue

        # ── role=assistant with tool_calls: reconstruct as XML ────────────────
        if role == "assistant" and tool_calls:
            xml = tool_calls_to_xml(tool_calls)
            # Prepend any accompanying text content (rare but valid)
            text = content.strip() if isinstance(content, str) else ""
            if text:
                parts.append(f"[assistant]: {text}\n{xml}")
            else:
                parts.append(f"[assistant]:\n{xml}")
            continue

        # ── 剥离前轮 assistant 消息中 grok2api 注入的 Sources 段落 ────────────
        if role == "assistant" and isinstance(content, str):
            content = _strip_generated_artifacts(content, strip_sources=True)

        # ── normal content handling ───────────────────────────────────────────
        if isinstance(content, str):
            cleaned = _strip_generated_artifacts(content.strip())
            if cleaned:
                parts.append(f"[{role}]: {cleaned}")
        elif isinstance(content, list):
            for block in content:
                if not isinstance(block, dict):
                    continue
                btype = block.get("type")
                if btype == "text":
                    text = block.get("text") or ""
                    text = _strip_generated_artifacts(
                        text.strip(),
                        strip_sources=(role == "assistant"),
                    )
                    if text:
                        parts.append(f"[{role}]: {text}")
                elif btype == "image_url":
                    url = (block.get("image_url") or {}).get("url", "")
                    if url:
                        files.append(url)
                elif btype in ("input_audio", "file"):
                    inner = block.get(btype) or {}
                    data = inner.get("data") or inner.get("file_data", "")
                    if data:
                        files.append(data)

    return "\n\n".join(parts), files


async def _prepare_file_attachments(token: str, file_inputs: list[str]) -> list[str]:
    """Upload OpenAI-style multimodal inputs and return Grok chat attachment IDs."""
    attachments: list[str] = []
    for file_input in file_inputs:
        if not file_input:
            continue
        file_id, _file_uri = await upload_from_input(token, file_input)
        if file_id:
            attachments.append(file_id)
    return attachments


async def _stream_chat(
    token: str,
    mode_id: "ModeId",
    message: str,
    files: list[str],
    *,
    tool_overrides: dict | None = None,
    model_config_override: dict | None = None,
    request_overrides: dict | None = None,
    timeout_s: float = 120.0,
) -> AsyncGenerator[str, None]:
    """Yield raw SSE lines from the Grok app-chat endpoint."""
    proxy = await get_proxy_runtime()
    lease = await proxy.acquire()
    attachments = await _prepare_file_attachments(token, files)

    payload = build_chat_payload(
        message=message,
        mode_id=mode_id,
        file_attachments=attachments,
        tool_overrides=tool_overrides,
        model_config_override=model_config_override,
        request_overrides=request_overrides,
    )
    payload_bytes = orjson.dumps(payload)

    headers = build_http_headers(
        token,
        content_type="application/json",
        origin="https://grok.com",
        referer="https://grok.com/",
        lease=lease,
    )
    session_kwargs = build_session_kwargs(lease=lease)

    async with ResettableSession(**session_kwargs) as session:
        try:
            response = await session.post(
                CHAT,
                headers=headers,
                data=payload_bytes,
                timeout=timeout_s,
                stream=True,
            )
        except Exception as exc:
            raise _transport_upstream_error(exc, context="Chat transport failed") from exc

        if response.status_code != 200:
            try:
                body = response.content.decode("utf-8", "replace")[:400]
            except Exception:
                body = ""
            raise UpstreamError(
                f"Chat upstream returned {response.status_code}",
                status=response.status_code,
                body=body,
            )

        try:
            async for line in response.aiter_lines():
                yield line
        except Exception as exc:
            raise _transport_upstream_error(exc, context="Chat stream read failed") from exc


# ---------------------------------------------------------------------------
# Console API (console.x.ai/v1/responses) dispatch
# ---------------------------------------------------------------------------


async def _console_post(
    *,
    token: str,
    console_model: str,
    input: Any,
    instructions: str,
    stream: bool,
    temperature: float | None,
    top_p: float | None,
    tools: list[dict] | None,
    tool_choice: Any,
    timeout_s: float,
) -> Any:
    """POST to console.x.ai/v1/responses; return ``(session, response)``.

    For ``stream=True`` the response object's ``aiter_lines()`` must be
    consumed by the caller. For ``stream=False`` the caller should read
    ``response.content`` and parse it as JSON. The caller is responsible
    for closing the returned ``session`` via ``await session.__aexit__()``.
    """
    proxy = await get_proxy_runtime()
    lease = await proxy.acquire()

    payload = build_console_payload(
        console_model=console_model,
        input=input,
        instructions=instructions,
        stream=stream,
        temperature=temperature,
        top_p=top_p,
        tools=tools,
        tool_choice=tool_choice,
    )
    payload_bytes = orjson.dumps(payload)

    headers = build_http_headers(
        token,
        content_type="application/json",
        origin="https://console.x.ai",
        referer="https://console.x.ai/",
        lease=lease,
    )
    session_kwargs = build_session_kwargs(lease=lease)

    session = ResettableSession(**session_kwargs)
    await session.__aenter__()
    try:
        response = await session.post(
            CONSOLE_RESPONSES,
            headers=headers,
            data=payload_bytes,
            timeout=timeout_s,
            stream=stream,
        )
    except Exception as exc:
        await session.__aexit__(None, None, None)
        raise _transport_upstream_error(exc, context="Console transport failed") from exc

    if response.status_code != 200:
        try:
            body = response.content.decode("utf-8", "replace")[:400]
        except Exception:
            body = ""
        await session.__aexit__(None, None, None)
        raise parse_console_error(response.status_code, body)

    return session, response


def _console_input_to_text(input_array: list[dict]) -> str:
    """Flatten a console input array into plain text for token estimation."""
    parts: list[str] = []
    for item in input_array:
        if not isinstance(item, dict):
            continue
        content = item.get("content")
        if isinstance(content, str):
            parts.append(content)
        elif isinstance(content, list):
            for block in content:
                if not isinstance(block, dict):
                    continue
                if block.get("type") in ("input_text", "output_text", "text"):
                    text = block.get("text") or ""
                    if text:
                        parts.append(text)
    return "\n".join(parts)


async def _console_completions(
    *,
    spec,
    model: str,
    messages: list[dict],
    is_stream: bool,
    emit_think: bool,
    temperature: float = 0.8,
    top_p: float = 0.95,
    tools: list[dict] | None = None,
    tool_choice: Any = None,
) -> dict | AsyncGenerator[str, None]:
    """Dispatch a chat completion through console.x.ai/v1/responses.

    Used for models with ``spec.console_model`` set. SSO cookies from the
    grok.com account pool authenticate console.x.ai requests, allowing
    basic-tier (free) accounts to access all available models.

    Supports:
      - Multimodal input (text + images) via OpenAI Responses-style content blocks
      - Native function calling via the ``tools`` parameter
      - SSE streaming for both text and tool call arguments
      - URL citation annotations from upstream search results
    """
    cfg = get_config()
    console_model = spec.console_model

    # Convert OpenAI messages → console structured input + instructions.
    # System messages are folded into ``instructions`` for cleaner reasoning
    # behaviour; text/image blocks become input_text/input_image; assistant
    # tool_calls become function_call items; tool results become
    # function_call_output items.
    input_array, instructions = build_console_input(messages)
    if not input_array and not instructions:
        raise UpstreamError("Empty messages after conversion", status=400)

    # Convert OpenAI tools → console tools (flat name/description/parameters).
    console_tools = convert_openai_tools_to_console(tools) if tools else None
    console_tool_choice = (
        convert_openai_tool_choice(tool_choice) if console_tools and tool_choice is not None else None)

    # Always enable web search for console models — this is the primary
    # reason for selecting the console route. Costs $5/1000 calls from
    # the account's prepaid (trial) credits. Idempotent: existing
    # ``web_search`` tool in the request is preserved.
    console_tools = inject_web_search_tool(console_tools)

    from app.dataplane.account import _directory as _acct_dir

    if _acct_dir is None:
        raise RateLimitError("Account directory not initialised")
    directory = _acct_dir

    max_retries = selection_max_retries()
    retry_codes = _configured_retry_codes(cfg)
    response_id = make_response_id()
    timeout_s = cfg.get_float("chat.timeout", 120.0)
    prompt_text = _console_input_to_text(input_array)

    # ── Streaming path ────────────────────────────────────────────────────────
    if is_stream:

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

                token = acct.token
                success = False
                _retry = False
                fail_exc: BaseException | None = None
                adapter = ConsoleStreamAdapter()
                tool_calls_emitted = False

                try:
                    try:
                        session, response = await _console_post(
                            token=token,
                            console_model=console_model,
                            input=input_array,
                            instructions=instructions,
                            stream=True,
                            temperature=temperature,
                            top_p=top_p,
                            tools=console_tools,
                            tool_choice=console_tool_choice,
                            timeout_s=timeout_s,
                        )
                        try:
                            async for raw_line in response.aiter_lines():
                                kind, payload = classify_console_sse_line(raw_line)
                                if kind == "event":
                                    adapter.feed_event(payload)
                                    continue
                                if kind != "data" or not payload:
                                    continue
                                ev = adapter.feed_data(payload)
                                ev_kind = ev.get("kind")
                                if ev_kind == "text":
                                    chunk = make_stream_chunk(response_id, model, ev["content"])
                                    yield f"data: {orjson.dumps(chunk).decode()}\n\n"
                                elif ev_kind == "thinking" and emit_think:
                                    chunk = make_thinking_chunk(response_id, model, ev["content"])
                                    yield f"data: {orjson.dumps(chunk).decode()}\n\n"
                                elif ev_kind == "tool_call_start":
                                    # First chunk for this tool call: id + name + empty args
                                    tool_calls_emitted = True
                                    chunk = make_tool_call_chunk(
                                        response_id,
                                        model,
                                        ev["index"],
                                        ev["call_id"],
                                        ev["name"],
                                        "",
                                        is_first=True,
                                    )
                                    yield f"data: {orjson.dumps(chunk).decode()}\n\n"
                                elif ev_kind == "tool_call_args":
                                    # Subsequent chunks: incremental args delta
                                    chunk = make_tool_call_chunk(
                                        response_id,
                                        model,
                                        ev["index"],
                                        "",
                                        "",
                                        ev["delta"],
                                        is_first=False,
                                    )
                                    yield f"data: {orjson.dumps(chunk).decode()}\n\n"
                                elif ev_kind == "tool_call_done":
                                    # No-op; final done chunk is emitted after
                                    # all events have completed.
                                    pass
                                elif ev_kind == "error":
                                    raise UpstreamError(
                                        ev.get("message", "Console stream error"),
                                        status=502,
                                    )
                                elif ev_kind == "done":
                                    break
                        finally:
                            await session.__aexit__(None, None, None)

                        # Stream completed — emit appropriate final chunk
                        if tool_calls_emitted:
                            done_chunk = make_tool_call_done_chunk(response_id, model)
                            if adapter.search_sources:
                                done_chunk["search_sources"] = list(adapter.search_sources)
                            yield f"data: {orjson.dumps(done_chunk).decode()}\n\n"
                        else:
                            chat_anns = (
                                _to_chat_annotations(adapter.annotations) if adapter.annotations else None)
                            final = make_stream_chunk(
                                response_id,
                                model,
                                "",
                                is_final=True,
                                annotations=chat_anns,
                            )
                            # Inject search_sources at root level (parallel
                            # to grok.com path behaviour). Avoids putting
                            # them inside delta which would violate strict
                            # OpenAI schemas.
                            if adapter.search_sources:
                                final["search_sources"] = list(adapter.search_sources)
                            yield f"data: {orjson.dumps(final).decode()}\n\n"
                        yield "data: [DONE]\n\n"
                        success = True
                        logger.info(
                            "console stream completed: attempt={}/{} model={} text_len={} tool_calls={} sources={}",
                            attempt + 1,
                            max_retries + 1,
                            model,
                            sum(len(s) for s in adapter.text_buf),
                            len(adapter.tool_calls),
                            len(adapter.search_sources),
                        )

                    except UpstreamError as exc:
                        fail_exc = exc
                        if (_should_retry_upstream(exc, retry_codes) and attempt < max_retries):
                            _retry = True
                            logger.warning(
                                "console stream retry: attempt={}/{} status={} token={}...",
                                attempt + 1,
                                max_retries,
                                exc.status,
                                token[:8],
                            )
                        else:
                            logger.warning(
                                "console stream failed: attempt={}/{} model={} status={} body={}",
                                attempt + 1,
                                max_retries + 1,
                                model,
                                exc.status,
                                _upstream_body_excerpt(exc),
                            )
                            raise

                finally:
                    await directory.release(acct)
                    kind = (
                        FeedbackKind.SUCCESS
                        if success else _feedback_kind(fail_exc) if fail_exc else FeedbackKind.SERVER_ERROR)
                    await directory.feedback(token, kind, selected_mode_id, now_s_val=now_s())
                    if success:
                        asyncio.create_task(_quota_sync(
                            token, selected_mode_id)).add_done_callback(_log_task_exception)
                    else:
                        asyncio.create_task(_fail_sync(token, selected_mode_id,
                                                       fail_exc)).add_done_callback(_log_task_exception)

                if success or not _retry:
                    return
                excluded.append(token)

        return _run_stream()

    # ── Non-streaming path ────────────────────────────────────────────────────
    excluded: list[str] = []
    full_text = ""
    full_thinking = ""
    response_tool_calls: list[dict] = []
    response_annotations: list[dict] = []
    response_search_sources: list[dict] = []
    usage: dict[str, int] = {}
    for attempt in range(max_retries + 1):
        acct, selected_mode_id = await reserve_account(
            directory,
            spec,
            now_s_override=now_s(),
            exclude_tokens=excluded or None,
        )
        if acct is None:
            raise RateLimitError("No available accounts for this model tier")

        token = acct.token
        success = False
        _retry = False
        fail_exc: BaseException | None = None

        try:
            try:
                session, response = await _console_post(
                    token=token,
                    console_model=console_model,
                    input=input_array,
                    instructions=instructions,
                    stream=False,
                    temperature=temperature,
                    top_p=top_p,
                    tools=console_tools,
                    tool_choice=console_tool_choice,
                    timeout_s=timeout_s,
                )
                try:
                    body_bytes = response.content
                    if hasattr(body_bytes, "__await__"):
                        body_bytes = await body_bytes  # type: ignore[misc]
                finally:
                    await session.__aexit__(None, None, None)

                try:
                    response_json = orjson.loads(body_bytes)
                except (orjson.JSONDecodeError, ValueError, TypeError) as exc:
                    raise UpstreamError(
                        f"Console returned non-JSON body: {exc}",
                        status=502,
                        body=str(body_bytes)[:400],
                    ) from exc

                full_text = extract_console_text(response_json)
                full_thinking = (extract_console_reasoning(response_json) if emit_think else "")
                response_tool_calls = extract_console_tool_calls(response_json)
                response_annotations = extract_console_annotations(response_json)
                response_search_sources = extract_console_search_sources(response_json)
                usage = extract_console_usage(response_json)
                success = True

            except UpstreamError as exc:
                fail_exc = exc
                if _should_retry_upstream(exc, retry_codes) and attempt < max_retries:
                    _retry = True
                    logger.warning(
                        "console retry: attempt={}/{} status={} token={}...",
                        attempt + 1,
                        max_retries,
                        exc.status,
                        token[:8],
                    )
                else:
                    logger.warning(
                        "console request failed: attempt={}/{} model={} status={} body={}",
                        attempt + 1,
                        max_retries + 1,
                        model,
                        exc.status,
                        _upstream_body_excerpt(exc),
                    )
                    raise

        finally:
            await directory.release(acct)
            kind = (
                FeedbackKind.SUCCESS
                if success else _feedback_kind(fail_exc) if fail_exc else FeedbackKind.SERVER_ERROR)
            await directory.feedback(token, kind, selected_mode_id, now_s_val=now_s())
            if success:
                asyncio.create_task(_quota_sync(token, selected_mode_id)).add_done_callback(_log_task_exception)
            else:
                asyncio.create_task(_fail_sync(token, selected_mode_id,
                                               fail_exc)).add_done_callback(_log_task_exception)

        if success or not _retry:
            break
        excluded.append(token)

    logger.info(
        "console request completed: model={} text_len={} reasoning_len={} tool_calls={} sources={} usage={}",
        model,
        len(full_text),
        len(full_thinking),
        len(response_tool_calls),
        len(response_search_sources),
        usage,
    )

    # Use upstream usage when available; fall back to estimation otherwise.
    pt = usage.get("prompt_tokens") or estimate_prompt_tokens(prompt_text)
    ct = usage.get("completion_tokens") or estimate_tokens(full_text)
    rt = usage.get("reasoning_tokens") or (estimate_tokens(full_thinking) if full_thinking else 0)

    # If upstream returned tool calls, return the tool_calls response variant.
    if response_tool_calls:
        from app.dataplane.reverse.protocol.tool_parser import ParsedToolCall
        parsed_calls = [
            ParsedToolCall(
                call_id=tc["id"],
                name=tc["function"]["name"],
                arguments=tc["function"]["arguments"],
            ) for tc in response_tool_calls
        ]
        resp = make_tool_call_response(
            model,
            parsed_calls,
            prompt_content=prompt_text,
            response_id=response_id,
            usage=build_usage(pt, ct + rt, reasoning_tokens=rt),
        )
        if response_search_sources:
            resp["search_sources"] = response_search_sources
        return resp

    chat_anns = (_to_chat_annotations(response_annotations) if response_annotations else None)
    return make_chat_response(
        model,
        full_text,
        prompt_content=prompt_text,
        response_id=response_id,
        reasoning_content=full_thinking or None,
        search_sources=response_search_sources or None,
        annotations=chat_anns,
        usage=build_usage(pt, ct + rt, reasoning_tokens=rt),
    )


async def completions(
    *,
    model: str,
    messages: list[dict],
    stream: bool | None = None,
    emit_think: bool | None = None,
    tools: list[dict] | None = None,
    tool_choice: Any = None,
    temperature: float = 0.8,
    top_p: float = 0.95,
    request_overrides: dict | None = None,
) -> dict | AsyncGenerator[str, None]:
    """Entry point for /v1/chat/completions.

    Returns an async generator for streaming, or a dict for non-streaming.
    Supports transparent retry with a different account on configured HTTP
    status codes (chat.retry_on_codes) up to chat.max_retries times.
    """
    cfg = get_config()
    spec = resolve_model(model)
    is_stream = stream if stream is not None else cfg.get_bool("features.stream", True)
    if emit_think is None:
        emit_think = cfg.get_bool("features.thinking", True)

    logger.info(
        "chat request accepted: model={} stream={} message_count={}",
        model,
        is_stream,
        len(messages),
    )

    # ── Console API dispatch ──────────────────────────────────────────────────
    # Models with `console_model` set route through console.x.ai/v1/responses
    # using the same SSO cookies as grok.com, but support all models for
    # basic-tier (free) accounts. Supports multimodal input and native
    # function calling.
    if spec.is_console():
        return await _console_completions(
            spec=spec,
            model=model,
            messages=messages,
            is_stream=is_stream,
            emit_think=emit_think,
            temperature=temperature,
            top_p=top_p,
            tools=tools,
            tool_choice=tool_choice,
        )

    message, files = _extract_message(messages)
    if not message.strip():
        raise UpstreamError("Empty message after extraction", status=400)

    from app.dataplane.account import _directory as _acct_dir

    if _acct_dir is None:
        raise RateLimitError("Account directory not initialised")
    directory = _acct_dir

    max_retries = selection_max_retries()
    retry_codes = _configured_retry_codes(cfg)
    response_id = make_response_id()
    timeout_s = cfg.get_float("chat.timeout", 120.0)

    # ── Tool call setup ───────────────────────────────────────────────────────
    tool_names: list[str] = []
    if tools:
        tool_names = extract_tool_names(tools)
        tool_prompt = build_tool_system_prompt(tools, tool_choice)
        message = inject_into_message(message, tool_prompt)
    tool_overrides: dict | None = None

    # ── Streaming path ────────────────────────────────────────────────────────
    if is_stream:

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

                token = acct.token
                success = False
                _retry = False
                fail_exc: BaseException | None = None
                adapter = StreamAdapter()
                collected_annotations: list[dict] = []

                try:
                    try:
                        ended = False
                        sieve = ToolSieve(tool_names)
                        tool_calls_emitted = False
                        async for line in _stream_chat(
                                token=token,
                                mode_id=ModeId(selected_mode_id),
                                message=message,
                                files=files,
                                tool_overrides=tool_overrides,
                                request_overrides=request_overrides,
                                timeout_s=timeout_s,
                        ):
                            event_type, data = classify_line(line)
                            if event_type == "done":
                                break
                            if event_type != "data" or not data:
                                continue
                            events = adapter.feed(data)
                            for ev in events:
                                if tool_calls_emitted:
                                    break  # already sent [DONE], drop remaining events
                                if ev.kind == "text":
                                    if tool_names:
                                        safe_text, parsed_calls = sieve.feed(ev.content)
                                        if safe_text:
                                            chunk = make_stream_chunk(response_id, model, safe_text)
                                            yield f"data: {orjson.dumps(chunk).decode()}\n\n"
                                        if parsed_calls is not None:
                                            for i, tc in enumerate(parsed_calls):
                                                chunk = make_tool_call_chunk(
                                                    response_id,
                                                    model,
                                                    i,
                                                    tc.call_id,
                                                    tc.name,
                                                    tc.arguments,
                                                    is_first=True,
                                                )
                                                yield f"data: {orjson.dumps(chunk).decode()}\n\n"
                                            done_chunk = make_tool_call_done_chunk(response_id, model)
                                            yield f"data: {orjson.dumps(done_chunk).decode()}\n\n"
                                            yield "data: [DONE]\n\n"
                                            tool_calls_emitted = True
                                            success = True
                                            logger.info(
                                                "chat stream tool_calls: attempt={}/{} model={} call_count={}",
                                                attempt + 1,
                                                max_retries + 1,
                                                model,
                                                len(parsed_calls),
                                            )
                                            ended = True
                                            break  # stop processing remaining events in this batch
                                    else:
                                        chunk = make_stream_chunk(response_id, model, ev.content)
                                        yield f"data: {orjson.dumps(chunk).decode()}\n\n"
                                elif ev.kind == "thinking" and emit_think:
                                    chunk = make_thinking_chunk(response_id, model, ev.content)
                                    yield f"data: {orjson.dumps(chunk).decode()}\n\n"
                                elif ev.kind == "annotation" and ev.annotation_data:
                                    collected_annotations.append(ev.annotation_data)
                                elif ev.kind == "soft_stop":
                                    ended = True
                                    break
                            if ended:
                                break

                        if not tool_calls_emitted and tool_names:
                            # Stream ended — flush sieve for any buffered XML
                            flushed_calls = sieve.flush()
                            if flushed_calls:
                                for i, tc in enumerate(flushed_calls):
                                    chunk = make_tool_call_chunk(
                                        response_id,
                                        model,
                                        i,
                                        tc.call_id,
                                        tc.name,
                                        tc.arguments,
                                        is_first=True,
                                    )
                                    yield f"data: {orjson.dumps(chunk).decode()}\n\n"
                                done_chunk = make_tool_call_done_chunk(response_id, model)
                                # 注入结构化搜索信源（tool_calls 场景）
                                sources = adapter.search_sources_list()
                                if sources:
                                    done_chunk["search_sources"] = sources
                                yield f"data: {orjson.dumps(done_chunk).decode()}\n\n"
                                yield "data: [DONE]\n\n"
                                tool_calls_emitted = True
                                success = True
                                logger.info(
                                    "chat stream tool_calls (flushed): model={} call_count={}",
                                    model,
                                    len(flushed_calls),
                                )

                        if not tool_calls_emitted:
                            for url, img_id in adapter.image_urls:
                                img_text = await _resolve_image(token, url, img_id)
                                chunk = make_stream_chunk(response_id, model, img_text + "\n")
                                yield f"data: {orjson.dumps(chunk).decode()}\n\n"

                            references = adapter.references_suffix()
                            if references:
                                chunk = make_stream_chunk(response_id, model, references)
                                yield f"data: {orjson.dumps(chunk).decode()}\n\n"

                            chat_anns = _to_chat_annotations(collected_annotations)
                            final = make_stream_chunk(
                                response_id,
                                model,
                                "",
                                is_final=True,
                                annotations=chat_anns or None,
                            )
                            # 注入结构化搜索信源到 chunk 根对象（避免 delta strict schema 拒绝）
                            sources = adapter.search_sources_list()
                            if sources:
                                final["search_sources"] = sources
                            yield f"data: {orjson.dumps(final).decode()}\n\n"
                            yield "data: [DONE]\n\n"
                            success = True
                            logger.info(
                                "chat stream completed: attempt={}/{} model={} image_count={}",
                                attempt + 1,
                                max_retries + 1,
                                model,
                                len(adapter.image_urls),
                            )

                    except UpstreamError as exc:
                        fail_exc = exc
                        if (_should_retry_upstream(exc, retry_codes) and attempt < max_retries):
                            _retry = True
                            logger.warning(
                                "chat stream retry scheduled: attempt={}/{} status={} token={}...",
                                attempt + 1,
                                max_retries,
                                exc.status,
                                token[:8],
                            )
                        else:
                            logger.warning(
                                "chat stream upstream failed: attempt={}/{} model={} status={} body={}",
                                attempt + 1,
                                max_retries + 1,
                                model,
                                exc.status,
                                _upstream_body_excerpt(exc),
                            )
                            raise

                finally:
                    await directory.release(acct)
                    kind = (
                        FeedbackKind.SUCCESS
                        if success else _feedback_kind(fail_exc) if fail_exc else FeedbackKind.SERVER_ERROR)
                    await directory.feedback(token, kind, selected_mode_id, now_s_val=now_s())
                    if success:
                        asyncio.create_task(_quota_sync(
                            token, selected_mode_id)).add_done_callback(_log_task_exception)
                    else:
                        asyncio.create_task(_fail_sync(token, selected_mode_id,
                                                       fail_exc)).add_done_callback(_log_task_exception)

                if success or not _retry:
                    return
                excluded.append(token)

        return _run_stream()

    # ── Non-streaming path ────────────────────────────────────────────────────
    excluded: list[str] = []
    token = ""
    adapter = StreamAdapter()
    for attempt in range(max_retries + 1):
        acct, selected_mode_id = await reserve_account(
            directory,
            spec,
            now_s_override=now_s(),
            exclude_tokens=excluded or None,
        )
        if acct is None:
            raise RateLimitError("No available accounts for this model tier")

        token = acct.token
        success = False
        _retry = False
        fail_exc: BaseException | None = None
        adapter = StreamAdapter()  # fresh adapter per attempt

        try:
            try:
                async for line in _stream_chat(
                        token=token,
                        mode_id=ModeId(selected_mode_id),
                        message=message,
                        files=files,
                        tool_overrides=tool_overrides,
                        request_overrides=request_overrides,
                        timeout_s=timeout_s,
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
                    logger.warning(
                        "chat retry scheduled: attempt={}/{} status={} token={}...",
                        attempt + 1,
                        max_retries,
                        exc.status,
                        token[:8],
                    )
                else:
                    logger.warning(
                        "chat upstream failed: attempt={}/{} model={} status={} body={}",
                        attempt + 1,
                        max_retries + 1,
                        model,
                        exc.status,
                        _upstream_body_excerpt(exc),
                    )
                    raise

        finally:
            await directory.release(acct)
            kind = (
                FeedbackKind.SUCCESS
                if success else _feedback_kind(fail_exc) if fail_exc else FeedbackKind.SERVER_ERROR)
            await directory.feedback(token, kind, selected_mode_id, now_s_val=now_s())
            if success:
                asyncio.create_task(_quota_sync(token, selected_mode_id)).add_done_callback(_log_task_exception)
            else:
                asyncio.create_task(_fail_sync(token, selected_mode_id,
                                               fail_exc)).add_done_callback(_log_task_exception)

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
                logger.warning("chat image resolve failed: error={}", img_text)
            elif isinstance(img_text, str):
                if full_text:
                    full_text += "\n\n"
                full_text += img_text

    references = adapter.references_suffix()
    if references:
        full_text += references

    thinking_text = ("".join(adapter.thinking_buf) or None) if emit_think else None

    # ── Tool call detection (non-streaming) ──────────────────────────────────
    if tool_names:
        parse_result = parse_tool_calls(full_text, tool_names)
        if parse_result.calls:
            logger.info(
                "chat request tool_calls: attempt={}/{} model={} call_count={}",
                attempt + 1,
                max_retries + 1,
                model,
                len(parse_result.calls),
            )
            pt = estimate_prompt_tokens(message)
            resp = make_tool_call_response(
                model,
                parse_result.calls,
                prompt_content=message,
                response_id=response_id,
                usage=build_usage(pt, estimate_tool_call_tokens(parse_result.calls)),
            )
            # 注入结构化搜索信源（tool_calls 场景）
            sources = adapter.search_sources_list()
            if sources:
                resp["search_sources"] = sources
            return resp

    logger.info(
        "chat request completed: attempt={}/{} model={} text_len={} reasoning_len={} image_count={}",
        attempt + 1,
        max_retries + 1,
        model,
        len(full_text),
        len(thinking_text or ""),
        len(adapter.image_urls),
    )

    pt = estimate_prompt_tokens(message)
    ct = estimate_tokens(full_text)
    rt = estimate_tokens(thinking_text) if thinking_text else 0
    chat_anns = _to_chat_annotations(adapter.annotations_list())
    return make_chat_response(
        model,
        full_text,
        prompt_content=message,
        response_id=response_id,
        reasoning_content=thinking_text,
        search_sources=adapter.search_sources_list(),
        annotations=chat_anns or None,
        usage=build_usage(pt, ct + rt, reasoning_tokens=rt),
    )


__all__ = [
    "completions",
    "_configured_retry_codes",
    "_should_retry_upstream",
]
