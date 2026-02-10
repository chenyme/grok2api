"""
SSRF 防护 — 校验 URL 解析后的 IP 是否为私网地址

提取自 proxy_pool.py，供所有外发请求复用。
"""

import asyncio
import ipaddress
import socket
from urllib.parse import urlparse

from app.core.logger import logger


async def is_safe_url(url: str) -> bool:
    """校验 URL 的 DNS 解析结果是否全部为公网地址。

    Args:
        url: 待检查的完整 URL

    Returns:
        True — 所有解析 IP 均为公网地址
        False — 空 URL / 解析失败 / 存在私网 IP
    """
    if not url:
        return False

    try:
        parsed = urlparse(url)
        hostname = parsed.hostname
        if not hostname:
            return False

        loop = asyncio.get_running_loop()
        addr_infos = await loop.getaddrinfo(hostname, parsed.port or 80, proto=socket.IPPROTO_TCP)
        for _family, _type, _proto, _canonname, sockaddr in addr_infos:
            ip_str = sockaddr[0]
            ip = ipaddress.ip_address(ip_str)
            if ip.is_private or ip.is_loopback or ip.is_link_local or ip.is_reserved:
                logger.warning(f"[SSRF] {hostname} resolves to private address {ip_str}")
                return False
        return True
    except (socket.gaierror, ValueError, OSError) as e:
        logger.warning(f"[SSRF] DNS resolution failed for {url}: {e}")
        return False
