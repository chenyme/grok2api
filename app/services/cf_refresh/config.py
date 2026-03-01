"""配置管理 — 从 app config 的 proxy.* 读取，支持面板修改实时生效"""

GROK_URL = "https://grok.com"


def _get(key: str, default=None):
    """从 app config 读取 proxy.* 配置"""
    from app.core.config import get_config
    return get_config(f"proxy.{key}", default)


def get_flaresolverr_url() -> str:
    return _get("flaresolverr_url", "") or ""


def get_refresh_interval() -> int:
    return int(_get("refresh_interval", 600))


def get_timeout() -> int:
    return int(_get("timeout", 60))


def get_proxy() -> str:
    """使用基础代理 URL，保证出口 IP 一致"""
    return _get("base_proxy_url", "") or ""


def is_enabled() -> bool:
    return bool(_get("enabled", False))
