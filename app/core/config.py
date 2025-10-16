"""配置管理器"""

import toml
from pathlib import Path
from typing import Dict, Any


class ConfigManager:
    """配置管理器"""

    def __init__(self) -> None:
        """初始化"""

        # 加载环境变量
        self.config_path: Path = Path(__file__).parents[2] / "data" / "setting.toml"
        self.global_config: Dict[str, Any] = self.load("global")
        self.grok_config: Dict[str, Any] = self.load("grok")
        self._storage = None

    def set_storage(self, storage) -> None:
        """设置存储实例"""
        self._storage = storage

    def load(self, section: str) -> Dict[str, Any]:
        """配置加载器"""
        try:
            with open(self.config_path, "r", encoding="utf-8") as f:
                config = toml.load(f)[section]
                
                # 自动将 SOCKS5 转换为 SOCKS5H
                if section == "grok" and "proxy_url" in config:
                    proxy_url = config["proxy_url"]
                    if proxy_url and proxy_url.startswith("socks5://"):
                        config["proxy_url"] = proxy_url.replace("socks5://", "socks5h://", 1)
                
                return config
        except Exception as e:
            raise Exception(f"[Setting] 配置加载失败: {e}")
    
    async def reload(self) -> None:
        """重新加载配置（用于从存储同步后）"""
        self.global_config = self.load("global")
        self.grok_config = self.load("grok")

    async def save(self, global_config: Dict[str, Any] = None, grok_config: Dict[str, Any] = None) -> None:
        """保存配置到存储"""
        if not self._storage:
            # 如果没有设置存储，使用传统文件保存方式（向后兼容）
            import aiofiles
            async with aiofiles.open(self.config_path, "r", encoding="utf-8") as f:
                content = await f.read()
                config = toml.loads(content)
            
            if global_config:
                config["global"].update(global_config)
            if grok_config:
                config["grok"].update(grok_config)
            
            async with aiofiles.open(self.config_path, "w", encoding="utf-8") as f:
                await f.write(toml.dumps(config))
        else:
            # 使用存储抽象层
            config_data = await self._storage.load_config()
            
            if global_config:
                config_data["global"].update(global_config)
            if grok_config:
                config_data["grok"].update(grok_config)
            
            await self._storage.save_config(config_data)
        
        # 重新加载配置
        await self.reload()

# 全局设置
setting = ConfigManager()

if __name__ == "__main__":
    print(setting.global_config)
    print(setting.grok_config)