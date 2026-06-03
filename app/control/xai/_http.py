"""Minimal curl_cffi HTTP helper for the xAI official API + OAuth endpoints.

Unlike ``app.dataplane.reverse.transport.http`` (which injects grok.com headers
and SSO cookies), these helpers send clean requests suitable for ``auth.x.ai``
and ``api.x.ai`` with only the headers the caller provides.  An optional proxy
lease is honoured via the shared ``build_session_kwargs`` builder.
"""

from typing import Any, AsyncGenerator

import orjson

from app.platform.errors import UpstreamError
from app.control.proxy.models import ProxyLease
from app.dataplane.proxy.adapters.session import build_session_kwargs


def _session(lease: ProxyLease | None):
    """Create a curl_cffi AsyncSession with proxy support (no grok headers)."""
    from curl_cffi.requests import AsyncSession

    kwargs = build_session_kwargs(lease=lease)
    return AsyncSession(**kwargs)


async def get_json(
    url: str,
    *,
    headers: dict[str, str] | None = None,
    lease: ProxyLease | None = None,
    timeout_s: float = 30.0,
) -> dict[str, Any]:
    """GET a URL and return parsed JSON, raising UpstreamError on non-2xx."""
    async with _session(lease) as session:
        try:
            resp = await session.get(url, headers=headers, timeout=timeout_s)
        except Exception as exc:  # noqa: BLE001 — wrap transport errors uniformly
            raise UpstreamError(f"xAI GET failed: {exc}", status=502) from exc
    if resp.status_code // 100 != 2:
        body = _excerpt(resp)
        raise UpstreamError(
            f"xAI GET {url} returned {resp.status_code}",
            status=resp.status_code,
            body=body,
        )
    return orjson.loads(resp.content)


async def post_form_json(
    url: str,
    form: dict[str, str],
    *,
    headers: dict[str, str] | None = None,
    lease: ProxyLease | None = None,
    timeout_s: float = 30.0,
) -> dict[str, Any]:
    """POST application/x-www-form-urlencoded data and return parsed JSON."""
    hdrs = {"Content-Type": "application/x-www-form-urlencoded", **(headers or {})}
    async with _session(lease) as session:
        try:
            resp = await session.post(url, data=form, headers=hdrs, timeout=timeout_s)
        except Exception as exc:  # noqa: BLE001
            raise UpstreamError(f"xAI POST failed: {exc}", status=502) from exc
    if resp.status_code // 100 != 2:
        body = _excerpt(resp)
        raise UpstreamError(
            f"xAI POST {url} returned {resp.status_code}",
            status=resp.status_code,
            body=body,
        )
    return orjson.loads(resp.content)


async def post_json_raw(
    url: str,
    payload: bytes,
    *,
    headers: dict[str, str],
    lease: ProxyLease | None = None,
    timeout_s: float = 120.0,
) -> dict[str, Any]:
    """POST a raw JSON body and return parsed JSON (non-streaming)."""
    async with _session(lease) as session:
        try:
            resp = await session.post(
                url, data=payload, headers=headers, timeout=timeout_s
            )
        except Exception as exc:  # noqa: BLE001
            raise UpstreamError(f"xAI POST failed: {exc}", status=502) from exc
        if resp.status_code // 100 != 2:
            body = _excerpt(resp)
            raise UpstreamError(
                f"xAI POST {url} returned {resp.status_code}",
                status=resp.status_code,
                body=body,
            )
        return orjson.loads(resp.content)


async def post_stream_raw(
    url: str,
    payload: bytes,
    *,
    headers: dict[str, str],
    lease: ProxyLease | None = None,
    timeout_s: float = 120.0,
) -> AsyncGenerator[str, None]:
    """POST a raw JSON body and yield SSE lines from the upstream response."""
    async with _session(lease) as session:
        try:
            resp = await session.post(
                url, data=payload, headers=headers, timeout=timeout_s, stream=True
            )
        except Exception as exc:  # noqa: BLE001
            raise UpstreamError(f"xAI POST failed: {exc}", status=502) from exc

        if resp.status_code // 100 != 2:
            try:
                body = (await resp.acontent()).decode("utf-8", "replace")[:400]
            except Exception:  # noqa: BLE001
                body = ""
            raise UpstreamError(
                f"xAI stream {url} returned {resp.status_code}",
                status=resp.status_code,
                body=body,
            )

        try:
            async for line in resp.aiter_lines():
                yield line
        except Exception as exc:  # noqa: BLE001
            raise UpstreamError(f"xAI stream read failed: {exc}", status=502) from exc


def _excerpt(resp, *, limit: int = 400) -> str:
    try:
        return resp.content.decode("utf-8", "replace")[:limit]
    except Exception:  # noqa: BLE001
        return ""


__all__ = ["get_json", "post_form_json", "post_json_raw", "post_stream_raw"]
