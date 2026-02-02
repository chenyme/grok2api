"""
Statsig ID 生成服务
"""

import base64
import random
import string

from app.core.config import get_config


class StatsigService:
    """Statsig ID 生成服务"""
    
    @staticmethod
    def _rand(length: int, alphanumeric: bool = False) -> str:
        """生成随机字符串"""
        chars = string.ascii_lowercase + string.digits if alphanumeric else string.ascii_lowercase
        return "".join(random.choices(chars, k=length))
    
    @staticmethod
    def gen_id() -> str:
        """
        生成 Statsig ID
        
        Returns:
            Base64 编码的 ID
        """
        # 读取配置
        dynamic = get_config("grok.dynamic_statsig", True)
        
        if not dynamic:
            return "ZTpUeXBlRXJyb3I6IENhbm5vdCByZWFkIHByb3BlcnRpZXMgb2YgdW5kZWZpbmVkIChyZWFkaW5nICdjaGlsZE5vZGVzJyk="
        
        # 随机格式
        if random.choice([True, False]):
            rand = StatsigService._rand(5, alphanumeric=True)
            message = f"e:TypeError: Cannot read properties of null (reading 'children['{rand}']')"
        else:
            rand = StatsigService._rand(10)
            message = f"e:TypeError: Cannot read properties of undefined (reading '{rand}')"
        
        return base64.b64encode(message.encode()).decode()


__all__ = ["StatsigService"]
