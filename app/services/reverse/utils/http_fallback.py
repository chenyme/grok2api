"""
Shared aiohttp fallback helpers for reverse HTTP interfaces.
"""

from __future__ import annotations

from typing import Any, Iterable, Mapping, Optional

import aiohttp
import orjson

from app.core.exceptions import UpstreamException
from app.core.ssl_certs import create_ssl_context


class AiohttpJSONResponse:
    def __init__(self, body: str, headers: Mapping[str, str]):
        self._body = body
        self.headers = headers

    def json(self):
        return orjson.loads(self._body) if self._body else {}


class AiohttpBinaryResponse:
    def __init__(self, body: bytes, headers: Mapping[str, str]):
        self.content = body
        self.headers = headers

    async def aiter_content(self, chunk_size: int = 65536):
        for i in range(0, len(self.content), chunk_size):
            yield self.content[i : i + chunk_size]


async def request_with_aiohttp_fallback(
    *,
    method: str,
    url: str,
    headers: Mapping[str, str],
    timeout: float,
    logger_prefix: str,
    expected_statuses: Iterable[int],
    response_kind: str,
    params: Optional[Mapping[str, Any]] = None,
    data: Any = None,
    json_payload: Any = None,
    allow_redirects: bool = False,
):
    client_timeout = aiohttp.ClientTimeout(total=float(timeout or 60))
    async with aiohttp.ClientSession(timeout=client_timeout, trust_env=True) as session:
        async with session.request(
            method.upper(),
            url,
            headers=headers,
            params=params,
            data=data,
            json=json_payload,
            ssl=create_ssl_context(),
            allow_redirects=allow_redirects,
        ) as response:
            if response_kind == "binary":
                body = await response.read()
                error_body = body.decode("utf-8", errors="ignore")
            else:
                body = await response.text()
                error_body = body

            if response.status not in set(expected_statuses):
                raise UpstreamException(
                    message=f"{logger_prefix}: Request failed, {response.status}",
                    details={"status": response.status, "body": error_body},
                    status_code=response.status,
                )

            if response_kind == "binary":
                return AiohttpBinaryResponse(body=body, headers=response.headers)
            if response_kind == "json":
                return AiohttpJSONResponse(body=body, headers=response.headers)
            raise ValueError(f"Unsupported response_kind: {response_kind}")


__all__ = [
    "AiohttpBinaryResponse",
    "AiohttpJSONResponse",
    "request_with_aiohttp_fallback",
]
