"""
安全响应头中间件

纯 ASGI 实现（不使用 BaseHTTPMiddleware），避免破坏 SSE 流式响应。
"""


class SecurityHeadersMiddleware:
    """在 HTTP 响应中注入标准安全头。"""

    _HEADERS = [
        (b"x-content-type-options", b"nosniff"),
        (b"x-frame-options", b"DENY"),
        (b"referrer-policy", b"strict-origin-when-cross-origin"),
    ]

    def __init__(self, app):
        self.app = app

    async def __call__(self, scope, receive, send):
        if scope.get("type") != "http":
            return await self.app(scope, receive, send)

        async def send_with_headers(message):
            if message.get("type") == "http.response.start":
                headers = list(message.get("headers") or [])
                headers.extend(self._HEADERS)
                message = {**message, "headers": headers}
            await send(message)

        return await self.app(scope, receive, send_with_headers)
