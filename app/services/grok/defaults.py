"""
Grok 服务默认配置

此文件定义所有 Grok 相关服务的默认值，会在应用启动时注册到配置系统中。
"""

# Grok 服务默认配置
GROK_DEFAULTS = {
    "app": {
        "app_url": "",
        "app_key": "grok2api",
        "api_key": "",
        "image_format": "url",
        "video_format": "html",
    },
    "network": {
        "timeout": 120,
        "base_proxy_url": "",
        "asset_proxy_url": "",
    },
    "security": {
        "cf_clearance": "",
        "browser": "chrome136",
        "user_agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36",
    },
    "chat": {
        "temporary": True,
        "disable_memory": True,
        "stream": True,
        "thinking": False,
        "dynamic_statsig": True,
        "filter_tags": ["grok:render", "xaiartifact", "xai:tool_usage_card"],
    },
    "retry": {
        "max_retry": 3,
        "retry_status_codes": [401, 429, 403],
        "retry_backoff_base": 0.5,
        "retry_backoff_factor": 2.0,
        "retry_backoff_max": 30.0,
        "retry_budget": 90.0,
    },
    "timeout": {
        "stream_idle_timeout": 45.0,
        "video_idle_timeout": 90.0,
    },
    "image": {
        "image_ws": True,
        "image_ws_nsfw": True,
        "image_ws_blocked_seconds": 15,
        "image_ws_final_min_bytes": 100000,
        "image_ws_medium_min_bytes": 30000,
    },
    "token": {
        "auto_refresh": True,
        "refresh_interval_hours": 8,
        "super_refresh_interval_hours": 2,
        "fail_threshold": 5,
        "save_delay_ms": 500,
        "reload_interval_sec": 30,
    },
    "cache": {
        "enable_auto_clean": True,
        "limit_mb": 1024,
    },
    "performance": {
        "assets_max_concurrent": 25,
        "assets_delete_batch_size": 10,
        "assets_batch_size": 10,
        "assets_max_tokens": 1000,
        "media_max_concurrent": 50,
        "usage_max_concurrent": 25,
        "usage_batch_size": 50,
        "usage_max_tokens": 1000,
        "nsfw_max_concurrent": 10,
        "nsfw_batch_size": 50,
        "nsfw_max_tokens": 1000,
    },
}


def get_grok_defaults():
    """获取 Grok 默认配置"""
    return GROK_DEFAULTS


__all__ = ["GROK_DEFAULTS", "get_grok_defaults"]
