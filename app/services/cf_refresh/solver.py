"""
通过 FlareSolverr 自动获取 cf_clearance

FlareSolverr 是一个 Docker 服务，内部运行 Chrome 浏览器，
自动处理 Cloudflare 挑战（包括 Turnstile），无需 GUI。
"""

import asyncio
import json
from typing import Optional, Dict
from urllib import request as urllib_request
from urllib.error import HTTPError, URLError

from loguru import logger

from .config import GROK_URL, get_timeout, get_proxy, get_flaresolverr_url


def _extract_all_cookies(cookies: list[dict]) -> str:
    """将 FlareSolverr 返回 of cookie 列表转换为字符串格式"""
    return "; ".join([f"{c.get('name')}={c.get('value')}" for c in cookies])


def _extract_cookie_value(cookies: list[dict], name: str) -> str:
    for cookie in cookies:
        if cookie.get("name") == name:
            return cookie.get("value") or ""
    return ""


def _extract_user_agent(solution: dict) -> str:
    """从 FlareSolverr 的 solution 中提取 User-Agent"""
    return solution.get("userAgent", "")


def _extract_browser_profile(user_agent: str) -> str:
    """从 User-Agent 提取 chromeXXX 格式的指纹识别号"""
    import re
    match = re.search(r"Chrome/(\d+)", user_agent)
    if match:
        return f"chrome{match.group(1)}"
    return "chrome120"


async def solve_cf_challenge() -> Optional[Dict[str, str]]:
    """
    通过 FlareSolverr 访问 grok.com，自动过 CF 挑战，提取 cf_clearance。

    Returns:
        成功时返回 {"cookies": "...", "user_agent": "..."}，失败返回 None
    """
    flaresolverr_url = get_flaresolverr_url()
    cf_timeout = get_timeout()
    proxy = get_proxy()

    if not flaresolverr_url:
        logger.error("FlareSolverr 地址未配置，无法刷新 cf_clearance")
        return None

    url = f"{flaresolverr_url.rstrip('/')}/v1"

    payload = {
        "cmd": "request.get",
        "url": GROK_URL,
        "maxTimeout": cf_timeout * 1000,
    }

    if proxy:
        payload["proxy"] = {"url": proxy}

    body = json.dumps(payload).encode("utf-8")
    headers = {"Content-Type": "application/json"}

    logger.info(f"正在通过 FlareSolverr 访问 {GROK_URL} ...")
    logger.debug(f"FlareSolverr 地址: {url}")

    req = urllib_request.Request(url, data=body, method="POST", headers=headers)

    try:
        def _post():
            with urllib_request.urlopen(req, timeout=cf_timeout + 30) as resp:
                return json.loads(resp.read().decode("utf-8"))

        result = await asyncio.to_thread(_post)

        status = result.get("status", "")
        if status != "ok":
            message = result.get("message", "unknown error")
            logger.error(f"FlareSolverr 返回错误: {status} - {message}")
            return None

        solution = result.get("solution", {})
        cookies = solution.get("cookies", [])

        if not cookies:
            logger.error("FlareSolverr 成功访问但没有返回 cookies")
            return None

        cookie_str = _extract_all_cookies(cookies)
        clearance = _extract_cookie_value(cookies, "cf_clearance")
        ua = _extract_user_agent(solution)
        browser = _extract_browser_profile(ua)
        logger.info(f"成功获取 cookies (数量: {len(cookies)}), 指纹: {browser}")

        return {
            "cookies": cookie_str,
            "cf_clearance": clearance,
            "user_agent": ua,
            "browser": browser,
        }

    except HTTPError as e:
        body_text = e.read().decode("utf-8", "replace")[:300]
        logger.error(f"FlareSolverr 请求失败: {e.code} - {body_text}")
        return None
    except URLError as e:
        logger.error(f"无法连接 FlareSolverr ({flaresolverr_url}): {e.reason}")
        logger.info("请确认 FlareSolverr 服务已启动: docker run -p 8191:8191 ghcr.io/flaresolverr/flaresolverr:latest")
        return None
    except Exception as e:
        logger.error(f"请求异常: {e}")
        return None
