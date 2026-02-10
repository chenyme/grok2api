"""
Security middlewares: request body size limit and rate limiting.
"""

from __future__ import annotations

import asyncio
import json
import time
from typing import Dict, Tuple
from urllib.parse import parse_qs

from app.core.config import get_config
from app.core.exceptions import ErrorType, error_response
from app.core.logger import logger
from app.core.network import trusted_proxy_ips


class _BodyTooLarge(Exception):
    pass


class BodySizeLimitMiddleware:
    """ASGI middleware to enforce max request body size."""

    def __init__(self, app):
        self.app = app

    async def __call__(self, scope, receive, send):
        if scope.get("type") != "http":
            return await self.app(scope, receive, send)

        max_mb = get_config("security.max_body_size_mb", 50)
        try:
            max_mb = float(max_mb)
        except Exception:
            max_mb = 50.0
        if max_mb <= 0:
            return await self.app(scope, receive, send)

        max_bytes = int(max_mb * 1024 * 1024)
        total = 0

        async def limited_receive():
            nonlocal total
            message = await receive()
            if message.get("type") == "http.request":
                body = message.get("body", b"") or b""
                total += len(body)
                if total > max_bytes:
                    raise _BodyTooLarge()
            return message

        try:
            return await self.app(scope, limited_receive, send)
        except _BodyTooLarge:
            payload = error_response(
                message="Request body too large",
                error_type=ErrorType.INVALID_REQUEST.value,
                code="request_too_large",
            )
            body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
            headers = [
                (b"content-type", b"application/json"),
                (b"content-length", str(len(body)).encode("ascii")),
            ]
            await send(
                {"type": "http.response.start", "status": 413, "headers": headers}
            )
            await send({"type": "http.response.body", "body": body})
            return


def _get_header(scope, name: str) -> str:
    name_bytes = name.lower().encode("latin-1")
    for k, v in scope.get("headers") or []:
        if k.lower() == name_bytes:
            try:
                return v.decode("latin-1")
            except Exception:
                return ""
    return ""


def _is_trusted_proxy(scope) -> bool:
    client = scope.get("client")
    if not (client and isinstance(client, (tuple, list)) and client):
        return False
    remote_ip = str(client[0])
    trusted = trusted_proxy_ips()
    return "*" in trusted or remote_ip in trusted


def _get_client_ip(scope) -> str:
    if _is_trusted_proxy(scope):
        forwarded = _get_header(scope, "x-forwarded-for")
        if forwarded:
            return forwarded.split(",")[0].strip()

        real_ip = _get_header(scope, "x-real-ip")
        if real_ip:
            return real_ip.strip()

    client = scope.get("client")
    if client and isinstance(client, (tuple, list)) and client:
        return str(client[0])
    return "unknown"


def _mask_identifier(value: str) -> str:
    data = (value or "").strip()
    if not data:
        return ""
    if len(data) <= 6:
        return "***"
    return f"***{data[-4:]}"


def _get_api_key(scope) -> str:
    auth = _get_header(scope, "authorization")
    if auth.lower().startswith("bearer "):
        return auth[7:].strip()
    # 仅在显式允许时读取 query string，防止绕过限速
    if not get_config("security.allow_query_api_key", False):
        return ""
    query_string = scope.get("query_string", b"")
    try:
        qs = parse_qs(query_string.decode("latin-1"))
    except Exception:
        qs = {}
    key = ""
    if "api_key" in qs and qs["api_key"]:
        key = str(qs["api_key"][0]).strip()
    return key


class _RateLimiter:
    """Token bucket limiter (in-memory per worker)."""

    def __init__(self):
        self._lock = asyncio.Lock()
        self._buckets: Dict[str, Tuple[float, float]] = {}
        self._last_cleanup = 0.0

    async def allow(
        self,
        key: str,
        rate_per_sec: float,
        burst: float,
        ttl_sec: float,
        now: float,
    ) -> bool:
        if not key:
            return True
        async with self._lock:
            tokens, last = self._buckets.get(key, (burst, now))
            elapsed = max(0.0, now - last)
            tokens = min(burst, tokens + elapsed * rate_per_sec)
            if tokens < 1.0:
                self._buckets[key] = (tokens, now)
                return False
            tokens -= 1.0
            self._buckets[key] = (tokens, now)

            if now - self._last_cleanup > 60:
                self._cleanup(now, ttl_sec)
            return True

    def _cleanup(self, now: float, ttl_sec: float) -> None:
        self._last_cleanup = now
        if ttl_sec <= 0:
            return
        expired = [k for k, (_, last) in self._buckets.items() if now - last > ttl_sec]
        for k in expired:
            self._buckets.pop(k, None)


_rate_limiter = _RateLimiter()


class _RedisRateLimiter:
    """Fixed-window limiter on Redis (global across workers)."""

    _SCRIPT = """
    local key = KEYS[1]
    local ttl = tonumber(ARGV[1])
    local limit = tonumber(ARGV[2])

    local current = redis.call('INCR', key)
    if current == 1 then
        redis.call('EXPIRE', key, ttl)
    end

    if current > limit then
        return 0
    end
    return 1
    """

    async def allow(self, key: str, limit: int, window_sec: int):
        if not key:
            return True
        if limit <= 0 or window_sec <= 0:
            return True

        try:
            from app.core.storage import get_storage, RedisStorage

            storage = get_storage()
            if not isinstance(storage, RedisStorage):
                return None

            now_bucket = int(time.time() // window_sec)
            bucket_key = f"grok2api:ratelimit:{key}:{now_bucket}"
            result = await storage.redis.eval(
                self._SCRIPT,
                1,
                bucket_key,
                str(window_sec),
                str(limit),
            )
            return bool(int(result) == 1)
        except Exception:
            return None


_redis_rate_limiter = _RedisRateLimiter()


class RateLimitMiddleware:
    """ASGI middleware for simple rate limiting."""

    def __init__(self, app):
        self.app = app

    async def __call__(self, scope, receive, send):
        if scope.get("type") != "http":
            return await self.app(scope, receive, send)

        if not get_config("security.rate_limit_enabled", False):
            return await self.app(scope, receive, send)

        path = scope.get("path", "") or ""
        exempt = get_config("security.rate_limit_exempt_paths", [])
        if isinstance(exempt, list):
            for prefix in exempt:
                if prefix and path.startswith(str(prefix)):
                    return await self.app(scope, receive, send)

        per_minute = get_config("security.rate_limit_per_minute", 120)
        burst = get_config("security.rate_limit_burst", 60)
        ttl = get_config("security.rate_limit_ttl_sec", 600)
        try:
            per_minute = float(per_minute)
        except Exception:
            per_minute = 120.0
        try:
            burst = float(burst)
        except Exception:
            burst = 60.0
        try:
            ttl = float(ttl)
        except Exception:
            ttl = 600.0

        rate = max(0.1, per_minute / 60.0)
        burst = max(1.0, burst)
        ttl = max(0.0, ttl)

        key_mode = str(get_config("security.rate_limit_key", "ip")).lower()
        api_key = _get_api_key(scope)
        if key_mode in ("api_key", "key", "token"):
            key = api_key or _get_client_ip(scope)
        elif key_mode == "auto":
            key = api_key or _get_client_ip(scope)
        else:
            key = _get_client_ip(scope)

        backend = str(get_config("security.rate_limit_backend", "memory")).lower()
        allowed = None

        if backend == "redis":
            window_sec = get_config("security.rate_limit_window_sec", 60)
            try:
                window_sec = int(window_sec)
            except Exception:
                window_sec = 60
            global_limit = max(1, int(per_minute + burst))
            allowed = await _redis_rate_limiter.allow(
                key, global_limit, max(1, window_sec)
            )

        if allowed is None:
            now = time.monotonic()
            allowed = await _rate_limiter.allow(key, rate, burst, ttl, now)

        if allowed:
            return await self.app(scope, receive, send)

        logger.warning(
            "Rate limit exceeded", extra={"path": path, "key": _mask_identifier(key)}
        )
        payload = error_response(
            message="Rate limit exceeded",
            error_type=ErrorType.RATE_LIMIT.value,
            code="rate_limit_exceeded",
        )
        body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        headers = [
            (b"content-type", b"application/json"),
            (b"content-length", str(len(body)).encode("ascii")),
            (b"retry-after", b"1"),
        ]
        await send({"type": "http.response.start", "status": 429, "headers": headers})
        await send({"type": "http.response.body", "body": body})
        return


__all__ = ["BodySizeLimitMiddleware", "RateLimitMiddleware"]
