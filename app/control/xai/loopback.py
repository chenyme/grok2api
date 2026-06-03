"""Local loopback OAuth callback server (127.0.0.1:56121/callback)."""

import asyncio
from urllib.parse import parse_qs, urlparse

from app.platform.logging.logger import logger

from .constants import (
    OAUTH_CALLBACK_HOST,
    OAUTH_CALLBACK_PATH,
    OAUTH_CALLBACK_PORT,
    OAUTH_CORS_ORIGIN_ALLOWLIST,
    OAUTH_WAIT_TIMEOUT_S,
)

_SUCCESS_HTML = """<!doctype html><html lang="zh"><head><meta charset="utf-8">
<title>xAI 登录成功</title><meta name="viewport" content="width=device-width,initial-scale=1">
<style>body{font-family:system-ui,sans-serif;background:#0b0f17;color:#e5e7eb;
display:flex;align-items:center;justify-content:center;height:100vh;margin:0}
.card{background:#111827;border:1px solid #1f2937;border-radius:14px;padding:32px 40px;
max-width:440px;text-align:center}.t{color:#16a34a;font-size:20px;font-weight:600}
.m{color:#9ca3af;font-size:14px;margin-top:8px;line-height:1.6}</style></head>
<body><div class="card"><div class="t">xAI 授权成功</div>
<div class="m">可以关闭此页面并返回 grok2api 管理后台。</div></div></body></html>"""

_FAIL_HTML = """<!doctype html><html lang="zh"><head><meta charset="utf-8">
<title>xAI 登录失败</title></head><body style="font-family:system-ui;background:#0b0f17;
color:#e5e7eb;text-align:center;padding:40px"><h2 style="color:#dc2626">xAI 授权失败</h2>
<p style="color:#9ca3af">{message}</p></body></html>"""


class LoopbackOAuthServer:
    """Singleton loopback listener for the grok-cli registered redirect URI."""

    def __init__(self) -> None:
        self._server: asyncio.Server | None = None
        self._waiters: dict[str, asyncio.Future[str]] = {}
        self._start_lock = asyncio.Lock()

    def register(self, state: str) -> asyncio.Future[str]:
        loop = asyncio.get_running_loop()
        fut: asyncio.Future[str] = loop.create_future()
        self._waiters[state] = fut
        return fut

    def cancel_waiter(self, state: str) -> None:
        fut = self._waiters.pop(state, None)
        if fut is not None and not fut.done():
            fut.cancel()

    def _resolve(self, state: str, code: str) -> bool:
        fut = self._waiters.pop(state, None)
        if fut is None or fut.done():
            return False
        fut.set_result(code)
        return True

    async def ensure_started(self) -> bool:
        async with self._start_lock:
            if self._server is not None:
                return True
            try:
                self._server = await asyncio.start_server(
                    self._handle_connection,
                    OAUTH_CALLBACK_HOST,
                    OAUTH_CALLBACK_PORT,
                )
                logger.info(
                    "xai oauth loopback listening: {}",
                    f"http://{OAUTH_CALLBACK_HOST}:{OAUTH_CALLBACK_PORT}{OAUTH_CALLBACK_PATH}",
                )
                return True
            except OSError as exc:
                logger.warning("xai oauth loopback unavailable: error={}", exc)
                return False

    async def wait_for_code(self, state: str) -> str:
        fut = self.register(state)
        return await asyncio.wait_for(fut, timeout=OAUTH_WAIT_TIMEOUT_S)

    async def _handle_connection(
        self,
        reader: asyncio.StreamReader,
        writer: asyncio.StreamWriter,
    ) -> None:
        try:
            request_line = (await reader.readline()).decode("utf-8", errors="replace").strip()
            if not request_line:
                return
            parts = request_line.split()
            if len(parts) < 2:
                return
            method, target = parts[0].upper(), parts[1]

            headers: dict[str, str] = {}
            while True:
                line = await reader.readline()
                if line in (b"\r\n", b"\n", b""):
                    break
                decoded = line.decode("utf-8", errors="replace").strip()
                if ":" in decoded:
                    key, value = decoded.split(":", 1)
                    headers[key.strip().lower()] = value.strip()

            parsed = urlparse(target)
            path = parsed.path or "/"
            origin = headers.get("origin", "")

            if method == "OPTIONS" and path == OAUTH_CALLBACK_PATH:
                await self._write_response(
                    writer,
                    status=204,
                    headers=self._cors_headers(origin),
                    body=b"",
                )
                return

            if method != "GET" or path != OAUTH_CALLBACK_PATH:
                await self._write_response(
                    writer,
                    status=404,
                    headers={"Content-Type": "text/plain"},
                    body=b"Not Found",
                )
                return

            qs = parse_qs(parsed.query, keep_blank_values=False)
            if qs.get("error"):
                err = (qs.get("error_description") or qs.get("error") or ["unknown"])[0]
                await self._write_response(
                    writer,
                    status=400,
                    headers={"Content-Type": "text/html; charset=utf-8"},
                    body=_FAIL_HTML.format(message=err).encode("utf-8"),
                )
                return

            codes = qs.get("code") or []
            states = qs.get("state") or []
            if not codes or not states:
                await self._write_response(
                    writer,
                    status=400,
                    headers={"Content-Type": "text/html; charset=utf-8"},
                    body=_FAIL_HTML.format(message="缺少 code 或 state 参数").encode("utf-8"),
                )
                return

            code = str(codes[0]).strip()
            state = str(states[0]).strip()
            if not self._resolve(state, code):
                await self._write_response(
                    writer,
                    status=400,
                    headers={"Content-Type": "text/html; charset=utf-8"},
                    body=_FAIL_HTML.format(
                        message="登录会话无效或已过期，请回到管理后台重新发起登录。"
                    ).encode("utf-8"),
                )
                return

            await self._write_response(
                writer,
                status=200,
                headers={"Content-Type": "text/html; charset=utf-8"},
                body=_SUCCESS_HTML.encode("utf-8"),
            )
        except Exception as exc:  # noqa: BLE001
            logger.warning("xai oauth loopback handler error: {}", exc)
        finally:
            try:
                writer.close()
                await writer.wait_closed()
            except Exception:  # noqa: BLE001
                pass

    @staticmethod
    def _cors_headers(origin: str) -> dict[str, str]:
        if origin in OAUTH_CORS_ORIGIN_ALLOWLIST:
            return {
                "Access-Control-Allow-Origin": origin,
                "Access-Control-Allow-Methods": "GET, OPTIONS",
                "Access-Control-Allow-Headers": "Content-Type",
                "Vary": "Origin",
            }
        return {}

    @staticmethod
    async def _write_response(
        writer: asyncio.StreamWriter,
        *,
        status: int,
        headers: dict[str, str],
        body: bytes,
    ) -> None:
        reason = {200: "OK", 204: "No Content", 400: "Bad Request", 404: "Not Found"}.get(
            status, "OK"
        )
        lines = [f"HTTP/1.1 {status} {reason}"]
        headers = dict(headers)
        if body:
            headers.setdefault("Content-Type", "text/plain; charset=utf-8")
            headers["Content-Length"] = str(len(body))
        elif status == 204:
            headers.pop("Content-Type", None)
        for key, value in headers.items():
            lines.append(f"{key}: {value}")
        lines.append("")
        writer.write("\r\n".join(lines).encode("utf-8") + body)
        await writer.drain()


_loopback = LoopbackOAuthServer()


def get_loopback() -> LoopbackOAuthServer:
    return _loopback


__all__ = ["LoopbackOAuthServer", "get_loopback"]