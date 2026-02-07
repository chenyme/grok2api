"""
Grok 服务默认配置

此文件定义所有 Grok 相关服务的默认值，会在应用启动时注册到配置系统中。
"""

# Grok 服务默认配置
GROK_DEFAULTS = {
    "grok": {
        # 网络配置
        "browser": "chrome136",
        "timeout": 120,
        "user_agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36",
        "base_proxy_url": "",
        "asset_proxy_url": "",
        "cf_clearance": "",
        
        # 对话配置
        "temporary": True,
        "disable_memory": True,
        "stream": True,
        "thinking": False,
        "filter_tags": ["grok:render", "xaiartifact", "xai:tool_usage_card"],
        
        # 重试配置
        "max_retry": 3,
        "retry_status_codes": [401, 429, 403],
        "retry_backoff_base": 0.5,
        "retry_backoff_factor": 2.0,
        "retry_backoff_max": 30.0,
        "retry_budget": 90.0,
        
        # 超时配置
        "stream_idle_timeout": 45.0,
        "video_idle_timeout": 90.0,
        
        # 图片配置
        "image_ws_blocked_seconds": 15,
        "image_ws_final_min_bytes": 100000,
        "image_ws_medium_min_bytes": 30000,
        
        # Statsig
        "dynamic_statsig": True,
    },
    
    "app": {
        "app_url": "",
        "image_format": "url",
        "video_format": "html",
    },
    
    "performance": {
        "assets_max_concurrent": 25,
        "assets_delete_batch_size": 10,
        "media_max_concurrent": 50,
        "usage_max_concurrent": 25,
    },
    
    "cache": {
        "enable_auto_clean": True,
        "limit_mb": 1024,
    },
}


def get_grok_defaults():
    """获取 Grok 默认配置"""
    return GROK_DEFAULTS


__all__ = ["GROK_DEFAULTS", "get_grok_defaults"]
