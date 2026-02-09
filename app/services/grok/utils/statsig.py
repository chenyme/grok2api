"""
Statsig ID 生成服务
"""

import base64
import random
import string

from app.core.config import get_config

# 性能优化：预生成字符集
_ALPHA_CHARS = string.ascii_lowercase
_ALPHANUM_CHARS = string.ascii_lowercase + string.digits

# 性能优化：缓存静态 ID
_STATIC_ID = "ZTpUeXBlRXJyb3I6IENhbm5vdCByZWFkIHByb3BlcnRpZXMgb2YgdW5kZWZpbmVkIChyZWFkaW5nICdjaGlsZE5vZGVzJyk="


class StatsigService:
    """Statsig ID 生成服务"""

    # 性能优化：缓存 dynamic 配置
    _dynamic_cached: bool = None

    @staticmethod
    def _rand(length: int, alphanumeric: bool = False) -> str:
        """生成随机字符串"""
        chars = _ALPHANUM_CHARS if alphanumeric else _ALPHA_CHARS
        return "".join(random.choices(chars, k=length))

    @staticmethod
    def gen_id() -> str:
        """
        生成 Statsig ID

        Returns:
            Base64 编码的 ID
        """
        # 性能优化：缓存 dynamic 配置（首次读取后缓存）
        if StatsigService._dynamic_cached is None:
            StatsigService._dynamic_cached = get_config("chat.dynamic_statsig", True)

        if not StatsigService._dynamic_cached:
            return _STATIC_ID

        # 随机格式
        if random.choice([True, False]):
            rand = StatsigService._rand(5, alphanumeric=True)
            message = f"e:TypeError: Cannot read properties of null (reading 'children['{rand}']')"
        else:
            rand = StatsigService._rand(10)
            message = f"e:TypeError: Cannot read properties of undefined (reading '{rand}')"

        return base64.b64encode(message.encode()).decode()


__all__ = ["StatsigService"]
