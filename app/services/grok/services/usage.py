"""
Grok 用量服务
"""

import asyncio
from typing import Dict

from curl_cffi.requests import AsyncSession

from app.core.logger import logger
from app.core.config import get_config
from app.core.exceptions import UpstreamException
from app.services.grok.utils.headers import apply_statsig, build_sso_cookie
from app.services.grok.utils.retry import retry_on_status

LIMITS_API = "https://grok.com/rest/rate-limits"

_USAGE_SEMAPHORE = asyncio.Semaphore(25)
_USAGE_SEM_VALUE = 25


class UsageService:
    """用量查询服务"""

    def __init__(self, proxy: str = None):
        self.proxy = proxy or get_config("network.base_proxy_url")
        self.timeout = get_config("network.timeout")

    def _build_headers(self, token: str) -> dict:
        """构建请求头"""
        user_agent = get_config("security.user_agent")
        headers = {
            "Accept": "*/*",
            "Accept-Encoding": "gzip, deflate, br, zstd",
            "Accept-Language": "zh-CN,zh;q=0.9",
            "Baggage": "sentry-environment=production,sentry-release=d6add6fb0460641fd482d767a335ef72b9b6abb8,sentry-public_key=b311e0f2690c81f25e2c4cf6d4f7ce1c",
            "Cache-Control": "no-cache",
            "Content-Type": "application/json",
            "Origin": "https://grok.com",
            "Pragma": "no-cache",
            "Priority": "u=1, i",
            "Referer": "https://grok.com/",
            "Sec-Ch-Ua": '"Google Chrome";v="136", "Chromium";v="136", "Not(A:Brand";v="24"',
            "Sec-Ch-Ua-Arch": "arm",
            "Sec-Ch-Ua-Bitness": "64",
            "Sec-Ch-Ua-Mobile": "?0",
            "Sec-Ch-Ua-Model": "",
            "Sec-Ch-Ua-Platform": '"macOS"',
            "Sec-Fetch-Dest": "empty",
            "Sec-Fetch-Mode": "cors",
            "Sec-Fetch-Site": "same-origin",
            "User-Agent": user_agent,
        }

        apply_statsig(headers)
        headers["Cookie"] = build_sso_cookie(token)

        return headers

    def _build_proxies(self) -> dict:
        """构建代理配置"""
        return {"http": self.proxy, "https": self.proxy} if self.proxy else None

    async def get(self, token: str, model_name: str = "grok-4-1-thinking-1129") -> Dict:
        """
        获取速率限制信息

        Args:
            token: 认证 Token
            model_name: 模型名称

        Returns:
            响应数据

        Raises:
            UpstreamException: 当获取失败且重试耗尽时
        """
        value = get_config("performance.usage_max_concurrent")
        try:
            value = int(value)
        except Exception:
            value = 25
        value = max(1, value)
        global _USAGE_SEMAPHORE, _USAGE_SEM_VALUE
        if value != _USAGE_SEM_VALUE:
            _USAGE_SEM_VALUE = value
            _USAGE_SEMAPHORE = asyncio.Semaphore(value)
        async with _USAGE_SEMAPHORE:
            # 定义状态码提取器
            def extract_status(e: Exception) -> int | None:
                if isinstance(e, UpstreamException) and e.details:
                    return e.details.get("status")
                return None

            # 定义实际的请求函数
            async def do_request():
                try:
                    headers = self._build_headers(token)
                    payload = {"requestKind": "DEFAULT", "modelName": model_name}
                    browser = get_config("security.browser")

                    async with AsyncSession() as session:
                        response = await session.post(
                            LIMITS_API,
                            headers=headers,
                            json=payload,
                            impersonate=browser,
                            timeout=self.timeout,
                            proxies=self._build_proxies(),
                        )

                    if response.status_code == 200:
                        data = response.json()
                        remaining = data.get("remainingTokens", 0)
                        logger.info(
                            f"Usage sync success: remaining={remaining}, token={token[:10]}..."
                        )
                        return data

                    logger.error(
                        f"Usage sync failed: status={response.status_code}, token={token[:10]}..."
                    )

                    raise UpstreamException(
                        message=f"Failed to get usage stats: {response.status_code}",
                        details={"status": response.status_code},
                    )

                except Exception as e:
                    if isinstance(e, UpstreamException):
                        raise
                    logger.error(f"Usage error: {e}")
                    raise UpstreamException(
                        message=f"Usage service error: {str(e)}",
                        details={"error": str(e)},
                    )

            # 带重试的执行
            try:
                result = await retry_on_status(
                    do_request, extract_status=extract_status
                )
                return result

            except Exception:
                # 最后一次失败已经被记录
                raise


__all__ = ["UsageService"]
