"""
WebSocket helpers for reverse interfaces.
"""

import ssl
import certifi
import aiohttp
from aiohttp_socks import ProxyConnector
from typing import Mapping, Optional, Any
from urllib.parse import urlparse

from app.core.logger import logger
from app.core.config import get_config
from app.core.proxy_pool import get_current_proxy_from, rotate_proxy


def _default_ssl_context() -> ssl.SSLContext:
    context = ssl.create_default_context()
    context.load_verify_locations(certifi.where())
    return context


def _normalize_socks_proxy(proxy_url: str) -> tuple[str, Optional[bool]]:
    scheme = urlparse(proxy_url).scheme.lower()
    rdns: Optional[bool] = None
    base_scheme = scheme

    if scheme == "socks5h":
        base_scheme = "socks5"
        rdns = True
    elif scheme == "socks4a":
        base_scheme = "socks4"
        rdns = True

    if base_scheme != scheme:
        proxy_url = proxy_url.replace(f"{scheme}://", f"{base_scheme}://", 1)

    return proxy_url, rdns


def resolve_proxy(proxy_url: Optional[str] = None, ssl_context: ssl.SSLContext = _default_ssl_context()) -> tuple[aiohttp.BaseConnector, Optional[str]]:
    """Resolve proxy connector.
    
    Args:
        proxy_url: Optional[str], the proxy URL. Defaults to None.
        ssl_context: ssl.SSLContext, the SSL context. Defaults to _default_ssl_context().

    Returns:
        tuple[aiohttp.BaseConnector, Optional[str]]: The proxy connector and the proxy URL.
    """
    if not proxy_url:
        return aiohttp.TCPConnector(ssl=ssl_context), None

    scheme = urlparse(proxy_url).scheme.lower()
    if scheme.startswith("socks"):
        normalized, rdns = _normalize_socks_proxy(proxy_url)
        logger.info(f"Using SOCKS proxy: {proxy_url}")
        try:
            if rdns is not None:
                return (
                    ProxyConnector.from_url(normalized, rdns=rdns, ssl=ssl_context),
                    None,
                )
        except TypeError:
            return ProxyConnector.from_url(normalized, ssl=ssl_context), None
        return ProxyConnector.from_url(normalized, ssl=ssl_context), None

    logger.info(f"Using HTTP proxy: {proxy_url}")
    return aiohttp.TCPConnector(ssl=ssl_context), proxy_url


class WebSocketConnection:
    """WebSocket connection wrapper."""

    def __init__(self, session: aiohttp.ClientSession, ws: aiohttp.ClientWebSocketResponse) -> None:
        self.session = session
        self.ws = ws

    async def close(self) -> None:
        if not self.ws.closed:
            await self.ws.close()
        await self.session.close()

    async def __aenter__(self) -> aiohttp.ClientWebSocketResponse:
        return self.ws

    async def __aexit__(self, exc_type, exc, tb) -> None:
        await self.close()


class WebSocketClient:
    """WebSocket client with proxy support."""

    def __init__(self, proxy: Optional[str] = None) -> None:
        self._proxy_override = proxy
        self._ssl_context = _default_ssl_context()

    async def connect(
        self,
        url: str,
        headers: Optional[Mapping[str, str]] = None,
        timeout: Optional[float] = None,
        ws_kwargs: Optional[Mapping[str, object]] = None,
    ) -> WebSocketConnection:
        """Connect to the WebSocket.
        
        Args:
            url: str, the URL to connect to.
            headers: Optional[Mapping[str, str]], the headers to send. Defaults to None.
            ws_kwargs: Optional[Mapping[str, object]], extra ws_connect kwargs. Defaults to None.

        Returns:
            WebSocketConnection: The WebSocket connection.
        """
        max_retry = max(0, int(get_config("retry.max_retry") or 0))
        last_error: Optional[Exception] = None

        for attempt in range(max_retry + 1):
            active_proxy_key = None
            proxy_url = self._proxy_override
            if not proxy_url:
                active_proxy_key, proxy_url = get_current_proxy_from("proxy.base_proxy_url")
            connector, resolved_proxy = resolve_proxy(proxy_url, self._ssl_context)
            logger.debug(
                f"WebSocket connect: proxy_url={proxy_url}, resolved_proxy={resolved_proxy}, connector={type(connector).__name__}"
            )

            total_timeout = (
                float(timeout)
                if timeout is not None
                else float(get_config("voice.timeout") or 120)
            )
            client_timeout = aiohttp.ClientTimeout(total=total_timeout)
            session = aiohttp.ClientSession(connector=connector, timeout=client_timeout)
            try:
                # Cast to Any to avoid Pylance errors with **extra_kwargs
                extra_kwargs: dict[str, Any] = dict(ws_kwargs or {})
                skip_proxy_ssl = bool(get_config("proxy.skip_proxy_ssl_verify")) and bool(proxy_url)
                if skip_proxy_ssl and urlparse(proxy_url).scheme.lower() == "https":
                    proxy_ssl_context = ssl.create_default_context()
                    proxy_ssl_context.check_hostname = False
                    proxy_ssl_context.verify_mode = ssl.CERT_NONE
                    try:
                        ws = await session.ws_connect(
                            url,
                            headers=headers,
                            proxy=resolved_proxy,
                            ssl=self._ssl_context,
                            proxy_ssl=proxy_ssl_context,
                            **extra_kwargs,
                        )
                    except TypeError:
                        logger.warning(
                            "proxy.skip_proxy_ssl_verify is enabled, but aiohttp does not support proxy_ssl; keeping proxy SSL verification enabled"
                        )
                        ws = await session.ws_connect(
                            url,
                            headers=headers,
                            proxy=resolved_proxy,
                            ssl=self._ssl_context,
                            **extra_kwargs,
                        )
                else:
                    ws = await session.ws_connect(
                        url,
                        headers=headers,
                        proxy=resolved_proxy,
                        ssl=self._ssl_context,
                        **extra_kwargs,
                    )
                return WebSocketConnection(session, ws)
            except Exception as exc:
                last_error = exc
                await session.close()
                if self._proxy_override or not active_proxy_key or attempt >= max_retry:
                    raise
                rotate_proxy(active_proxy_key)
                logger.warning(
                    f"WebSocket connect failed via {active_proxy_key}, rotating proxy and retrying ({attempt + 1}/{max_retry})"
                )

        if last_error is not None:
            raise last_error
        raise RuntimeError("WebSocket connect failed without error")


__all__ = ["WebSocketClient", "WebSocketConnection", "resolve_proxy"]
