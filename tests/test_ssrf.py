"""SSRF 防护单元测试"""

import asyncio
from unittest.mock import patch, AsyncMock
import socket

import pytest

from app.core.ssrf import is_safe_url


@pytest.fixture
def event_loop():
    loop = asyncio.new_event_loop()
    yield loop
    loop.close()


def _make_addrinfo(ip: str, port: int = 80):
    """构造 getaddrinfo 返回值"""
    return [(socket.AF_INET, socket.SOCK_STREAM, socket.IPPROTO_TCP, "", (ip, port))]


@pytest.mark.asyncio
async def test_empty_url():
    assert await is_safe_url("") is False
    assert await is_safe_url(None) is False


@pytest.mark.asyncio
async def test_no_hostname():
    assert await is_safe_url("not-a-url") is False


@pytest.mark.asyncio
async def test_loopback_blocked():
    with patch("app.core.ssrf.asyncio") as mock_aio:
        mock_loop = AsyncMock()
        mock_loop.getaddrinfo = AsyncMock(return_value=_make_addrinfo("127.0.0.1"))
        mock_aio.get_running_loop.return_value = mock_loop
        assert await is_safe_url("http://localhost/secret") is False


@pytest.mark.asyncio
async def test_private_ip_blocked():
    with patch("app.core.ssrf.asyncio") as mock_aio:
        mock_loop = AsyncMock()
        mock_loop.getaddrinfo = AsyncMock(return_value=_make_addrinfo("10.0.0.1"))
        mock_aio.get_running_loop.return_value = mock_loop
        assert await is_safe_url("http://internal.corp/api") is False


@pytest.mark.asyncio
async def test_link_local_blocked():
    with patch("app.core.ssrf.asyncio") as mock_aio:
        mock_loop = AsyncMock()
        mock_loop.getaddrinfo = AsyncMock(return_value=_make_addrinfo("169.254.1.1"))
        mock_aio.get_running_loop.return_value = mock_loop
        assert await is_safe_url("http://metadata.internal/latest") is False


@pytest.mark.asyncio
async def test_public_ip_allowed():
    with patch("app.core.ssrf.asyncio") as mock_aio:
        mock_loop = AsyncMock()
        mock_loop.getaddrinfo = AsyncMock(return_value=_make_addrinfo("93.184.216.34"))
        mock_aio.get_running_loop.return_value = mock_loop
        assert await is_safe_url("http://example.com/image.png") is True


@pytest.mark.asyncio
async def test_dns_failure_blocked():
    with patch("app.core.ssrf.asyncio") as mock_aio:
        mock_loop = AsyncMock()
        mock_loop.getaddrinfo = AsyncMock(side_effect=socket.gaierror("DNS failed"))
        mock_aio.get_running_loop.return_value = mock_loop
        assert await is_safe_url("http://nonexistent.invalid/x") is False
