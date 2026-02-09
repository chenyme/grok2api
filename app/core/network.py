"""
网络相关共享工具

提取自 auth.py 和 security_middleware.py 中的重复逻辑
"""

from app.core.config import get_config


def trusted_proxy_ips() -> set[str]:
    """
    获取受信代理 IP 集合

    从配置 security.trusted_proxy_ips 读取，
    支持字符串(逗号分隔)和列表格式
    """
    raw = get_config("security.trusted_proxy_ips", ["127.0.0.1", "::1"])
    if isinstance(raw, str):
        values = [item.strip() for item in raw.split(",") if item.strip()]
    elif isinstance(raw, list):
        values = [str(item).strip() for item in raw if str(item).strip()]
    else:
        values = ["127.0.0.1", "::1"]
    return set(values)


__all__ = ["trusted_proxy_ips"]
