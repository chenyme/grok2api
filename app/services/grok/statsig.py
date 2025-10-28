"""Grok 请求头管理模块"""

import uuid
from typing import Dict

from app.core.config import setting


def get_dynamic_headers(pathname: str = "/rest/app-chat/conversations/new") -> Dict[str, str]:
    """获取请求头

    Args:
        pathname: 请求路径

    Returns:
        请求头字典
    """
    # 获取配置的 x-statsig-id
    statsig_id = setting.grok_config.get("x_statsig_id")
    if not statsig_id:
        raise ValueError("配置文件中未设置 x_statsig_id")

    # 构建基础请求头
    headers = {
        "Accept": "*/*",
        "Accept-Language": "zh-CN,zh;q=0.9",
        "Accept-Encoding": "gzip, deflate, br, zstd",
        "Content-Type": "application/json" if "upload-file" not in pathname else "text/plain;charset=UTF-8",
        "Connection": "keep-alive",
        "Origin": "https://grok.com",
        "Priority": "u=1, i",
        "User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36",
        "Sec-Ch-Ua": '"Not(A:Brand";v="99", "Google Chrome";v="133", "Chromium";v="133"',
        "Sec-Ch-Ua-Mobile": "?0",
        "Sec-Ch-Ua-Platform": '"macOS"',
        "Sec-Fetch-Dest": "empty",
        "Sec-Fetch-Mode": "cors",
        "Sec-Fetch-Site": "same-origin",
        "Baggage": "sentry-environment=production,sentry-public_key=b311e0f2690c81f25e2c4cf6d4f7ce1c",
        "x-statsig-id": statsig_id,
        "x-xai-request-id": str(uuid.uuid4())
    }

    return headers