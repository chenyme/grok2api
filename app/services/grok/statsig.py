"""Grok 请求头管理 - 生成动态请求头和Statsig ID"""

import base64
import random
import string
import uuid
from typing import Dict

from app.core.logger import logger
from app.core.config import setting


# 基础请求头
BASE_HEADERS = {
    "Accept": "*/*",
    "Accept-Language": "zh-CN,zh;q=0.9",
    "Accept-Encoding": "gzip, deflate, br, zstd",
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
}


def _random_string(length: int, letters_only: bool = True) -> str:
    """生成随机字符串"""
    chars = string.ascii_lowercase if letters_only else string.ascii_lowercase + string.digits
    return ''.join(random.choices(chars, k=length))


def _generate_statsig_id() -> str:
    """生成x-statsig-id
    
    随机选择两种格式：
    1. e:TypeError: Cannot read properties of null (reading 'children['xxxxx']')
    2. e:TypeError: Cannot read properties of undefined (reading 'xxxxxxxxxx')
    """
    if random.choice([True, False]):
        rand = _random_string(5, letters_only=False)
        msg = f"e:TypeError: Cannot read properties of null (reading 'children['{rand}']')"
    else:
        rand = _random_string(10)
        msg = f"e:TypeError: Cannot read properties of undefined (reading '{rand}')"
    
    return base64.b64encode(msg.encode()).decode()


def get_dynamic_headers(pathname: str = "/rest/app-chat/conversations/new") -> Dict[str, str]:
    """获取请求头
    
    Args:
        pathname: 请求路径
        
    Returns:
        完整的请求头字典
    """
    # 获取或生成statsig-id
    if setting.grok_config.get("dynamic_statsig", False):
        statsig_id = _generate_statsig_id()
        logger.debug(f"[Statsig] 动态生成: {statsig_id}")
    else:
        statsig_id = setting.grok_config.get("x_statsig_id")
        if not statsig_id:
            raise ValueError("配置文件中未设置 x_statsig_id")
        logger.debug(f"[Statsig] 使用固定值: {statsig_id}")

    # 构建请求头
    headers = BASE_HEADERS.copy()
    headers["x-statsig-id"] = statsig_id
    headers["x-xai-request-id"] = str(uuid.uuid4())
    headers["Content-Type"] = "text/plain;charset=UTF-8" if "upload-file" in pathname else "application/json"

    return headers