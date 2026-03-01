"""配置管理 — 从 app config 的 proxy.* 读取，支持面板修改实时生效"""

GROK_URL = "https://grok.com"


def _get(key: str, default=None):
    """从 app config 读取 proxy.* 配置"""
    from app.core.config import get_config
    return get_config(f"proxy.{key}", default)


def get_flaresolverr_url() -> str:
    return _get("flaresolverr_url", "") or ""


def _get_int(key: str, default: int, min_value: int) -> int:
    raw = _get(key, default)
    try:
        value = int(raw)
    except (TypeError, ValueError):
        return max(default, min_value)
    if value < min_value:
        return min_value
    return value


def get_refresh_interval() -> int:
    return _get_int("refresh_interval", 600, 60)


def get_timeout() -> int:
    return _get_int("timeout", 60, 60)


def get_proxy() -> str:
    """使用基础代理 URL，保证出口 IP 一致"""
    return _get("base_proxy_url", "") or ""


def is_enabled() -> bool:
    return bool(_get("enabled", False))
