"""Reverse pipeline planner — build_plan() from model spec and request metadata.

Determines the endpoint URL, transport kind, pool/mode IDs, and timeout
for a given operation.  Does NOT execute anything — pure data transform.
"""

from typing import Any

from app.control.model.spec import ModelSpec
from app.dataplane.reverse.runtime.endpoint_table import (
    CHAT,
    CONSOLE_RESPONSES,
    MEDIA_POST,
    WS_IMAGINE,
)
from .types import ReversePlan, TransportKind

# ---------------------------------------------------------------------------
# Profile defaults (timeout / content-type per transport)
# ---------------------------------------------------------------------------

_DEFAULTS: dict[TransportKind, dict[str, Any]] = {
    TransportKind.HTTP_SSE: {
        "timeout_s": 120.0,
        "content_type": "application/json"
    },
    TransportKind.HTTP_JSON: {
        "timeout_s": 30.0,
        "content_type": "application/json"
    },
    TransportKind.WEBSOCKET: {
        "timeout_s": 300.0,
        "content_type": "application/json"
    },
    TransportKind.GRPC_WEB: {
        "timeout_s": 15.0,
        "content_type": "application/grpc-web+proto"
    },
}

# ---------------------------------------------------------------------------
# Public API
# ---------------------------------------------------------------------------


def build_plan(spec: ModelSpec, request: dict[str, Any] | None = None) -> ReversePlan:
    """Produce a ReversePlan for the given model spec.

    ``request`` is the raw API request body — used to refine the plan
    (e.g. detect image-edit vs image-gen) but may be ``None``.
    """
    endpoint, tkind = _resolve_endpoint(spec, request or {})
    defaults = _DEFAULTS.get(tkind, _DEFAULTS[TransportKind.HTTP_JSON])

    return ReversePlan(
        endpoint=endpoint,
        transport_kind=tkind,
        pool_candidates=spec.pool_candidates(),
        mode_id=int(spec.mode_id),
        timeout_s=defaults["timeout_s"],
        content_type=defaults["content_type"],
    )


# ---------------------------------------------------------------------------
# Internal routing logic
# ---------------------------------------------------------------------------


def _resolve_endpoint(
    spec: ModelSpec,
    request: dict[str, Any],
) -> tuple[str, TransportKind]:
    """Determine (endpoint_url, transport_kind) for the given capability."""

    # Console models route through console.x.ai/v1/responses (OpenAI Responses API)
    if spec.is_console() and spec.is_chat():
        # When stream=true the response is SSE; otherwise plain JSON.
        # Use HTTP_SSE as the default since both streaming and non-streaming
        # share the same content-type and the timeout profile is more
        # permissive (long-running responses).
        return CONSOLE_RESPONSES, TransportKind.HTTP_SSE

    if spec.is_chat():
        return CHAT, TransportKind.HTTP_SSE

    if spec.is_image():
        return WS_IMAGINE, TransportKind.WEBSOCKET

    if spec.is_image_edit():
        return CHAT, TransportKind.HTTP_SSE

    if spec.is_video():
        return MEDIA_POST, TransportKind.HTTP_JSON

    if spec.is_voice():
        # LiveKit negotiation is HTTP JSON, actual voice is WebSocket.
        return CHAT, TransportKind.HTTP_SSE

    # Fallback: treat as chat.
    return CHAT, TransportKind.HTTP_SSE


__all__ = ["build_plan"]
