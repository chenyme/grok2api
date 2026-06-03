"""Chat completions via Grok Build OAuth (api.x.ai + CLI proxy).

Uses the grok-cli OAuth Bearer token.  Only subscription-entitled models are
accepted (see ``app.control.xai.constants.XAI_OAUTH_MODEL_IDS``).
"""

from typing import Any, AsyncGenerator

import orjson

from app.platform.config.snapshot import get_config
from app.platform.errors import RateLimitError, UpstreamError, ValidationError
from app.platform.logging.logger import logger
from app.control.account.repository import AccountRepository
from app.control.xai import account as xai_account
from app.control.xai.constants import (
    DEFAULT_API_BASE,
    XAI_OAUTH_MODEL_IDS,
    resolve_chat_base_url,
)
from app.control.xai import _http
from app.control.xai.responses_chat import (
    messages_to_responses_body,
    responses_to_chat_completion,
    translate_responses_stream_to_chat,
    uses_responses_api,
)
from app.dataplane.proxy import get_proxy_runtime


def _validate_model(model: str) -> None:
    if model not in XAI_OAUTH_MODEL_IDS:
        allowed = ", ".join(sorted(XAI_OAUTH_MODEL_IDS))
        raise ValidationError(
            f"Model {model!r} is not available via Grok Build OAuth. "
            f"Allowed models: {allowed}. "
            "Use an API key (XAI_API_KEY) for other api.x.ai models.",
            param="model",
            code="xai_oauth_model_not_allowed",
        )


def _build_body(
    *,
    model: str,
    messages: list[dict],
    stream: bool,
    temperature: float,
    top_p: float,
    tools: list[dict] | None,
    tool_choice: Any,
) -> dict:
    """Build the chat-completions request body (OpenAI-compatible)."""
    body: dict[str, Any] = {
        "model": model,
        "messages": messages,
        "stream": stream,
        "temperature": temperature,
        "top_p": top_p,
    }
    if tools:
        body["tools"] = tools
    if tool_choice is not None:
        body["tool_choice"] = tool_choice
    return body


async def completions(
    repo: AccountRepository,
    *,
    model: str,
    messages: list[dict],
    stream: bool,
    temperature: float = 0.8,
    top_p: float = 0.95,
    tools: list[dict] | None = None,
    tool_choice: Any = None,
) -> dict | AsyncGenerator[str, None]:
    """Forward a chat request using the Grok Build OAuth account."""
    cfg = get_config()
    if not cfg.get_bool("xai.enabled", True):
        raise RateLimitError("xAI provider is disabled")

    _validate_model(model)

    account = await xai_account.get_xai_account(repo)
    if account is None:
        raise RateLimitError(
            "No xAI account available — log in via the admin panel first"
        )

    access_token = await xai_account.ensure_fresh(repo, account)
    if not access_token:
        raise UpstreamError("xAI account has no usable access token", status=401)

    api_base = cfg.get_str("xai.base_url", "") or DEFAULT_API_BASE
    base_url = resolve_chat_base_url(
        model,
        api_base=(account.ext or {}).get("base_url") or api_base,
    )
    via_responses = uses_responses_api(model)
    path = "/responses" if via_responses else "/chat/completions"
    url = base_url.rstrip("/") + path
    timeout_s = cfg.get_float("xai.timeout", 120.0)

    if via_responses:
        body = messages_to_responses_body(
            model=model,
            messages=messages,
            stream=stream,
            temperature=temperature,
            top_p=top_p,
        )
    else:
        body = _build_body(
            model=model,
            messages=messages,
            stream=stream,
            temperature=temperature,
            top_p=top_p,
            tools=tools,
            tool_choice=tool_choice,
        )
    payload = orjson.dumps(body)
    headers = {
        "Authorization": f"Bearer {access_token}",
        "Content-Type": "application/json",
        "Accept": "text/event-stream" if stream else "application/json",
    }

    proxy = await get_proxy_runtime()
    lease = await proxy.acquire()

    if stream:
        async def _relay() -> AsyncGenerator[str, None]:
            raw = _http.post_stream_raw(
                url, payload, headers=headers, lease=lease, timeout_s=timeout_s
            )
            if via_responses:
                async for chunk in translate_responses_stream_to_chat(raw, model=model):
                    yield chunk
                return
            async for line in raw:
                if isinstance(line, bytes):
                    line = line.decode("utf-8", "replace")
                if line.strip():
                    yield f"{line}\n\n"

        logger.info(
            "xai chat stream started: model={} base={} path={}", model, base_url, path
        )
        return _relay()

    result = await _http.post_json_raw(
        url, payload, headers=headers, lease=lease, timeout_s=timeout_s
    )
    if via_responses:
        result = responses_to_chat_completion(result, model=model)
    logger.info("xai chat completed: model={} base={} path={}", model, base_url, path)
    return result


__all__ = ["completions"]