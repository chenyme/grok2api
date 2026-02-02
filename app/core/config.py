"""
配置管理

- config.toml: 运行时配置
"""

from typing import Any
from app.core.storage import get_storage


class Config:
    """配置管理器"""
    
    _instance = None
    _config = {}
    
    def __init__(self):
        self._config = {}

    async def load(self):
        """显式加载配置"""
        try:
            from app.core.storage import get_storage, LocalStorage
            import asyncio
            
            storage = get_storage()
            config_data = await storage.load_config()
            
            # 从本地 data/config.toml 初始化后端
            if config_data is None:
                local_storage = LocalStorage()
                try:
                    # 尝试读取本地配置
                    config_data = await local_storage.load_config()
                    # 初始化后端
                    await storage.save_config(config_data)
                    logger.info(f"Initialized remote storage ({storage.__class__.__name__}) with local config.")
                except Exception as e:
                    logger.info(f"Failed to auto-init config from local: {e}")
                    config_data = {}
            
            self._config = config_data or {}
        except Exception as e:
            print(f"Error loading config: {e}")
            self._config = {}

    def get(self, key: str, default: Any = None) -> Any:
        """
        获取配置值
        
        Args:
            key: 配置键，格式 "section.key"
            default: 默认值
        """
        if "." in key:
            try:
                section, attr = key.split(".", 1)
                return self._config.get(section, {}).get(attr, default)
            except (ValueError, AttributeError):
                return default
        
        return self._config.get(key, default)

    async def update(self, new_config: dict):
        """更新配置"""
        from app.core.storage import get_storage
        storage = get_storage()
        async with storage.acquire_lock("config_save", timeout=10):
            await storage.save_config(new_config)
            await self.load()


# 全局配置实例
config = Config()


def get_config(key: str, default: Any = None) -> Any:
    """获取配置"""
    return config.get(key, default)


__all__ = ["Config", "config", "get_config"]
