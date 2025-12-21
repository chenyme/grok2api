"""配置管理器 - 管理应用配置的读写"""

import toml
from pathlib import Path
from typing import Dict, Any, Optional, Literal


# 默认配置
DEFAULT_GROK = {
    "api_key": "",
    "proxy_url": "",
    "proxy_pool_url": "",
    "proxy_pool_interval": 300,
    "cache_proxy_url": "",
    "cf_clearance": "",
    "x_statsig_id": "",
    "dynamic_statsig": True,
    "filtered_tags": "xaiartifact,xai:tool_usage_card",
    "show_thinking": True,
    "temporary": False,
    "max_upload_concurrency": 20,
    "max_request_concurrency": 100,
    "stream_first_response_timeout": 30,
    "stream_chunk_timeout": 120,
    "stream_total_timeout": 600,
    "retry_status_codes": [401, 429],  # 可重试的HTTP状态码
}

DEFAULT_GLOBAL = {
    "base_url": "http://localhost:8000",
    "log_level": "INFO",
    "image_mode": "url",
    "admin_password": "admin",
    "admin_username": "admin",
    "image_cache_max_size_mb": 512,
    "video_cache_max_size_mb": 1024,
    "max_upload_concurrency": 20,  # 最大并发上传数
    "max_request_concurrency": 50,  # 最大并发请求数
    "batch_save_interval": 1.0,  # 批量保存间隔（秒）
    "batch_save_threshold": 10  # 触发批量保存的变更数阈值
}


class ConfigManager:
    """配置管理器"""

    def __init__(self) -> None:
        """初始化配置"""
        self.config_path: Path = Path(__file__).parents[2] / "data" / "setting.toml"
        self._storage: Optional[Any] = None
        self._ensure_exists()
        self.global_config: Dict[str, Any] = self.load("global")
        self.grok_config: Dict[str, Any] = self.load("grok")
    
    def _ensure_exists(self) -> None:
        """确保配置存在"""
        if not self.config_path.exists():
            self.config_path.parent.mkdir(parents=True, exist_ok=True)
            self._create_default()
    
    def _create_default(self) -> None:
        """创建默认配置"""
        default = {"grok": DEFAULT_GROK.copy(), "global": DEFAULT_GLOBAL.copy()}
        with open(self.config_path, "w", encoding="utf-8") as f:
            toml.dump(default, f)
    
    def _normalize_proxy(self, proxy: str) -> str:
        """标准化代理URL（sock5/socks5 → socks5h://）"""
        if not proxy:
            return proxy

        proxy = proxy.strip()
        if proxy.startswith("sock5h://"):
            proxy = proxy.replace("sock5h://", "socks5h://", 1)
        if proxy.startswith("sock5://"):
            proxy = proxy.replace("sock5://", "socks5://", 1)
        if proxy.startswith("socks5://"):
            return proxy.replace("socks5://", "socks5h://", 1)
        return proxy
    
    def _normalize_cf(self, cf: str) -> str:
        """标准化CF Clearance（自动添加前缀）"""
        if cf and not cf.startswith("cf_clearance="):
            return f"cf_clearance={cf}"
        return cf

    def set_storage(self, storage: Any) -> None:
        """设置存储实例"""
        self._storage = storage

    def load(self, section: Literal["global", "grok"]) -> Dict[str, Any]:
        """加载配置节"""
        try:
            with open(self.config_path, "r", encoding="utf-8") as f:
                config = toml.load(f)[section]

            # 标准化Grok配置
            if section == "grok":
                if "proxy_url" in config:
                    config["proxy_url"] = self._normalize_proxy(config["proxy_url"])
                if "cache_proxy_url" in config:
                    config["cache_proxy_url"] = self._normalize_proxy(config["cache_proxy_url"])
                if "cf_clearance" in config:
                    config["cf_clearance"] = self._normalize_cf(config["cf_clearance"])

            return config
        except Exception as e:
            raise Exception(f"[Setting] 配置加载失败: {e}") from e
    
    async def reload(self) -> None:
        """重新加载配置"""
        self.global_config = self.load("global")
        self.grok_config = self.load("grok")
    
    async def _save_file(self, updates: Dict[str, Dict[str, Any]]) -> None:
        """保存到文件"""
        import aiofiles
        
        async with aiofiles.open(self.config_path, "r", encoding="utf-8") as f:
            config = toml.loads(await f.read())
        
        for section, data in updates.items():
            if section in config:
                config[section].update(data)
        
        async with aiofiles.open(self.config_path, "w", encoding="utf-8") as f:
            await f.write(toml.dumps(config))
    
    async def _save_storage(self, updates: Dict[str, Dict[str, Any]]) -> None:
        """保存到存储"""
        config = await self._storage.load_config()
        
        for section, data in updates.items():
            if section in config:
                config[section].update(data)
        
        await self._storage.save_config(config)
    
    def _prepare_grok(self, grok: Dict[str, Any]) -> Dict[str, Any]:
        """准备Grok配置（移除前缀）"""
        processed = grok.copy()
        if "cf_clearance" in processed:
            cf = processed["cf_clearance"]
            if cf and cf.startswith("cf_clearance="):
                processed["cf_clearance"] = cf.replace("cf_clearance=", "", 1)
        return processed

    async def save(self, global_config: Optional[Dict[str, Any]] = None, grok_config: Optional[Dict[str, Any]] = None) -> None:
        """保存配置"""
        updates = {}
        
        if global_config:
            updates["global"] = global_config
        if grok_config:
            updates["grok"] = self._prepare_grok(grok_config)
        
        # 选择存储方式
        if self._storage:
            await self._save_storage(updates)
        else:
            await self._save_file(updates)
        
        await self.reload()
    
    async def get_proxy_async(self, proxy_type: Literal["service", "cache"] = "service") -> str:
        """异步获取代理URL（支持代理池）
        
        Args:
            proxy_type: 代理类型
                - service: 服务代理（client/upload）
                - cache: 缓存代理（cache）
        """
        from app.core.proxy_pool import proxy_pool
        
        if proxy_type == "cache":
            cache_proxy = self.grok_config.get("cache_proxy_url", "")
            if cache_proxy:
                return cache_proxy
        
        # 从代理池获取
        return await proxy_pool.get_proxy() or ""
    
    def get_proxy(self, proxy_type: Literal["service", "cache"] = "service") -> str:
        """获取代理URL（同步方法，用于向后兼容）
        
        Args:
            proxy_type: 代理类型
                - service: 服务代理（client/upload）
                - cache: 缓存代理（cache）
        """
        from app.core.proxy_pool import proxy_pool
        
        if proxy_type == "cache":
            cache_proxy = self.grok_config.get("cache_proxy_url", "")
            if cache_proxy:
                return cache_proxy
        
        # 返回当前代理（如果是代理池，返回最后一次获取的）
        return proxy_pool.get_current_proxy() or self.grok_config.get("proxy_url", "")


# 全局实例
setting = ConfigManager()
