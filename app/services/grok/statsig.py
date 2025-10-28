"""Grok 请求头管理模块"""

import base64
import random
import string
import uuid
from typing import Dict

from app.core.logger import logger
from app.core.config import setting


def _generate_random_string(length: int, use_letters: bool = True) -> str:
    """生成随机字符串
    
    Args:
        length: 字符串长度
        use_letters: 是否使用字母（True）或数字+字母（False）
        
    Returns:
        随机字符串
    """
    if use_letters:
        # 生成随机字母（小写）
        return ''.join(random.choices(string.ascii_lowercase, k=length))
    else:
        # 生成随机数字和字母组合（小写）
        return ''.join(random.choices(string.ascii_lowercase + string.digits, k=length))


def _generate_statsig_id() -> str:
    """动态生成 x-statsig-id
    
    随机选择两种格式之一：
    1. e:TypeError: Cannot read properties of null (reading 'children['xxxxx']')
       其中 xxxxx 是5位随机字符串
    2. e:TypeError: Cannot read properties of undefined (reading 'xxxxxxxxxx')
       其中 xxxxxxxxxx 是10位随机字母
       
    Returns:
        base64 编码后的字符串
    """
    # 随机选择一种格式
    format_type = random.choice([1, 2])
    
    if format_type == 1:
        # 格式1: children['xxxxx']
        random_str = _generate_random_string(5, use_letters=False)
        error_msg = f"e:TypeError: Cannot read properties of null (reading 'children['{random_str}']')"
    else:
        # 格式2: 'xxxxxxxxxx'
        random_str = _generate_random_string(10, use_letters=True)
        error_msg = f"e:TypeError: Cannot read properties of undefined (reading '{random_str}')"
    
    # base64 编码
    encoded = base64.b64encode(error_msg.encode('utf-8')).decode('utf-8')
    return encoded


def get_dynamic_headers(pathname: str = "/rest/app-chat/conversations/new") -> Dict[str, str]:
    """获取请求头

    Args:
        pathname: 请求路径

    Returns:
        请求头字典
    """
    # 检查是否启用动态生成
    dynamic_statsig = setting.grok_config.get("dynamic_statsig", False)
    
    if dynamic_statsig:
        # 动态生成 x-statsig-id
        statsig_id = _generate_statsig_id()
        logger.debug(f"[Statsig] 动态生成值 {statsig_id}")
    else:
        # 使用配置文件中的固定值
        statsig_id = setting.grok_config.get("x_statsig_id")
        logger.debug(f"[Statsig] 使用固定值 {statsig_id}")
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