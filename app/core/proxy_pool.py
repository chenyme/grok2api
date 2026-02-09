"""代理池管理器 - 从URL动态获取代理IP"""

import asyncio
import ipaddress
import socket
import aiohttp
import time
from typing import Optional
from urllib.parse import urlparse
from app.core.logger import logger


class ProxyPool:
    """代理池管理器"""

    def __init__(self):
        self._pool_url: Optional[str] = None
        self._static_proxy: Optional[str] = None
        self._current_proxy: Optional[str] = None
        self._last_fetch_time: float = 0
        self._fetch_interval: int = 300  # 5分钟刷新一次
        self._enabled: bool = False
        self._lock = asyncio.Lock()

    def configure(self, proxy_url: str, proxy_pool_url: str = "", proxy_pool_interval: int = 300):
        """配置代理池

        Args:
            proxy_url: 静态代理URL（socks5h://xxx 或 http://xxx）
            proxy_pool_url: 代理池API URL，返回单个代理地址
            proxy_pool_interval: 代理池刷新间隔（秒）
        """
        self._static_proxy = self._normalize_proxy(proxy_url) if proxy_url else None
        pool_url = proxy_pool_url.strip() if proxy_pool_url else None
        if pool_url and self._looks_like_proxy_url(pool_url):
            normalized_proxy = self._normalize_proxy(pool_url)
            if not self._static_proxy:
                self._static_proxy = normalized_proxy
                logger.warning(
                    "[ProxyPool] proxy_pool_url看起来是代理地址，已作为静态代理使用，请改用proxy_url"
                )
            else:
                logger.warning(
                    "[ProxyPool] proxy_pool_url看起来是代理地址，已忽略（使用proxy_url）"
                )
            pool_url = None
        self._pool_url = pool_url
        self._fetch_interval = proxy_pool_interval
        self._enabled = bool(self._pool_url)

        if self._enabled:
            logger.info(
                f"[ProxyPool] 代理池已启用: {self._pool_url}, 刷新间隔: {self._fetch_interval}s"
            )
        elif self._static_proxy:
            logger.info(f"[ProxyPool] 使用静态代理: {self._static_proxy}")
            self._current_proxy = self._static_proxy
        else:
            logger.info("[ProxyPool] 未配置代理")

    async def get_proxy(self) -> Optional[str]:
        """获取代理地址

        Returns:
            代理URL或None
        """
        # 如果未启用代理池，返回静态代理
        if not self._enabled:
            return self._static_proxy

        # 检查是否需要刷新
        now = time.time()
        if not self._current_proxy or (now - self._last_fetch_time) >= self._fetch_interval:
            async with self._lock:
                # 双重检查
                if not self._current_proxy or (now - self._last_fetch_time) >= self._fetch_interval:
                    await self._fetch_proxy()

        return self._current_proxy

    async def force_refresh(self) -> Optional[str]:
        """强制刷新代理（用于403错误重试）

        Returns:
            新的代理URL或None
        """
        if not self._enabled:
            return self._static_proxy

        async with self._lock:
            await self._fetch_proxy()

        return self._current_proxy

    async def _fetch_proxy(self):
        """从代理池URL获取新的代理"""
        if not await self._is_safe_url(self._pool_url):
            logger.warning(f"[ProxyPool] SSRF 防护: 拒绝访问私网地址 {self._pool_url}")
            if not self._current_proxy:
                self._current_proxy = self._static_proxy
            return

        try:
            logger.debug(f"[ProxyPool] 正在从代理池获取新代理: {self._pool_url}")

            timeout = aiohttp.ClientTimeout(total=10)
            async with aiohttp.ClientSession(timeout=timeout) as session:
                async with session.get(self._pool_url) as response:
                    if response.status == 200:
                        proxy_text = await response.text()
                        proxy = self._normalize_proxy(proxy_text.strip())

                        # 验证代理格式
                        if self._validate_proxy(proxy):
                            self._current_proxy = proxy
                            self._last_fetch_time = time.time()
                            logger.info(f"[ProxyPool] 成功获取新代理: {proxy}")
                        else:
                            logger.error(f"[ProxyPool] 代理格式无效: {proxy}")
                            # 降级到静态代理
                            if not self._current_proxy:
                                self._current_proxy = self._static_proxy
                    else:
                        logger.error(f"[ProxyPool] 获取代理失败: HTTP {response.status}")
                        # 降级到静态代理
                        if not self._current_proxy:
                            self._current_proxy = self._static_proxy

        except asyncio.TimeoutError:
            logger.error("[ProxyPool] 获取代理超时")
            if not self._current_proxy:
                self._current_proxy = self._static_proxy

        except Exception as e:
            logger.error(f"[ProxyPool] 获取代理异常: {e}")
            # 降级到静态代理
            if not self._current_proxy:
                self._current_proxy = self._static_proxy

    async def _is_safe_url(self, url: str) -> bool:
        """SSRF 防护: 检查 URL 的解析 IP 是否为私网地址

        Args:
            url: 待检查的 URL

        Returns:
            True 表示安全（公网），False 表示私网或解析失败
        """
        if not url:
            return False

        try:
            parsed = urlparse(url)
            hostname = parsed.hostname
            if not hostname:
                return False

            # 异步 DNS 解析，避免阻塞事件循环
            import asyncio

            loop = asyncio.get_running_loop()
            addr_infos = await loop.getaddrinfo(
                hostname, parsed.port or 80, proto=socket.IPPROTO_TCP
            )
            for family, _type, _proto, _canonname, sockaddr in addr_infos:
                ip_str = sockaddr[0]
                ip = ipaddress.ip_address(ip_str)
                if ip.is_private or ip.is_loopback or ip.is_link_local or ip.is_reserved:
                    logger.warning(f"[ProxyPool] SSRF 检测: {hostname} 解析到私网地址 {ip_str}")
                    return False
            return True
        except (socket.gaierror, ValueError, OSError) as e:
            logger.warning(f"[ProxyPool] SSRF 检测: 无法解析 {url}: {e}")
            return False

    def _validate_proxy(self, proxy: str) -> bool:
        """验证代理格式

        Args:
            proxy: 代理URL

        Returns:
            是否有效
        """
        if not proxy:
            return False

        # 支持的协议
        valid_protocols = ["http://", "https://", "socks5://", "socks5h://"]

        return any(proxy.startswith(proto) for proto in valid_protocols)

    def _normalize_proxy(self, proxy: str) -> str:
        """标准化代理URL（sock5/socks5 → socks5h://）"""
        if not proxy:
            return proxy

        proxy = proxy.strip()
        if proxy.startswith("sock5h://"):
            proxy = proxy.replace("sock5h://", "socks5h://", 1)
        if proxy.startswith("sock5://"):
            proxy = proxy.replace("sock5://", "socks5://", 1)
        if proxy.startswith("socks5://"):
            return proxy.replace("socks5://", "socks5h://", 1)
        return proxy

    def _looks_like_proxy_url(self, url: str) -> bool:
        """判断URL是否像代理地址（避免误把代理池API当代理）"""
        return url.startswith(("sock5://", "sock5h://", "socks5://", "socks5h://"))

    def get_current_proxy(self) -> Optional[str]:
        """获取当前使用的代理（同步方法）

        Returns:
            当前代理URL或None
        """
        return self._current_proxy or self._static_proxy


# 全局代理池实例
proxy_pool = ProxyPool()
