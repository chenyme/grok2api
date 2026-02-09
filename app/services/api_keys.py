"""API Key 管理器 - 多用户密钥管理"""

import asyncio
import secrets
import time
from pathlib import Path
from typing import Dict, List, Optional

import orjson

from app.core.logger import logger


class ApiKeyManager:
    """API Key 管理服务"""

    _instance = None

    def __new__(cls):
        if cls._instance is None:
            cls._instance = super().__new__(cls)
        return cls._instance

    def __init__(self):
        if hasattr(self, "_initialized"):
            return

        self.file_path = Path(__file__).parents[2] / "data" / "api_keys.json"
        self._keys: List[Dict] = []
        self._lock = asyncio.Lock()
        self._loaded = False

        self._initialized = True
        logger.debug(f"[ApiKey] 初始化完成: {self.file_path}")

    async def init(self):
        """初始化加载数据"""
        if not self._loaded:
            await self._load_data()

    async def _load_data(self):
        """加载 API Keys"""
        if self._loaded:
            return

        if not self.file_path.exists():
            self._keys = []
            self._loaded = True
            return

        try:
            async with self._lock:
                if self.file_path.exists():
                    content = await asyncio.to_thread(self.file_path.read_bytes)
                    if content:
                        self._keys = orjson.loads(content)
                        self._loaded = True
                        logger.debug(f"[ApiKey] 加载了 {len(self._keys)} 个 API Key")
        except Exception as e:
            logger.error(f"[ApiKey] 加载失败: {e}")
            self._keys = []
            self._loaded = True

    async def _save_data(self):
        """保存 API Keys"""
        if not self._loaded:
            logger.warning("[ApiKey] 数据未加载，取消保存")
            return

        try:
            self.file_path.parent.mkdir(parents=True, exist_ok=True)

            async with self._lock:
                content = orjson.dumps(self._keys, option=orjson.OPT_INDENT_2)
                await asyncio.to_thread(self.file_path.write_bytes, content)
        except Exception as e:
            logger.error(f"[ApiKey] 保存失败: {e}")

    def generate_key(self) -> str:
        """生成 sk- 开头的 key"""
        return f"sk-{secrets.token_urlsafe(24)}"

    async def add_key(self, name: str = "") -> Dict:
        """添加 API Key"""
        key_id = secrets.token_hex(8)
        new_key = {
            "id": key_id,
            "key": self.generate_key(),
            "name": name or f"Key-{key_id[:6]}",
            "created_at": time.strftime("%Y-%m-%d %H:%M:%S"),
            "enabled": True,
        }
        self._keys.append(new_key)
        await self._save_data()
        logger.info(f"[ApiKey] 添加新 Key: {new_key['name']}")
        return new_key

    async def batch_add_keys(self, prefix: str, count: int) -> List[Dict]:
        """批量添加 API Key"""
        new_keys = []
        for i in range(1, count + 1):
            key_id = secrets.token_hex(8)
            name = f"{prefix}-{i}" if prefix else f"Key-{key_id[:6]}"
            new_keys.append(
                {
                    "id": key_id,
                    "key": self.generate_key(),
                    "name": name,
                    "created_at": time.strftime("%Y-%m-%d %H:%M:%S"),
                    "enabled": True,
                }
            )

        self._keys.extend(new_keys)
        await self._save_data()
        logger.info(f"[ApiKey] 批量添加 {count} 个 Key")
        return new_keys

    async def delete_key(self, key_id: str) -> bool:
        """删除 API Key"""
        initial_len = len(self._keys)
        self._keys = [k for k in self._keys if k["id"] != key_id]

        if len(self._keys) != initial_len:
            await self._save_data()
            logger.info(f"[ApiKey] 删除 Key: {key_id}")
            return True
        return False

    async def update_key(self, key_id: str, name: str = None, enabled: bool = None) -> bool:
        """更新 Key"""
        for k in self._keys:
            if k["id"] == key_id:
                if name is not None:
                    k["name"] = name
                if enabled is not None:
                    k["enabled"] = enabled
                await self._save_data()
                return True
        return False

    def validate_key(self, key: str) -> Optional[Dict]:
        """验证 Key"""
        from app.core.config import get_config

        # 检查全局配置的 Key
        global_key = get_config("app.api_key", "")
        if global_key and key == global_key:
            return {
                "key": global_key,
                "name": "Admin",
                "enabled": True,
                "is_admin": True,
            }

        # 检查多 Key 列表
        for k in self._keys:
            if k["key"] == key and k.get("enabled", True):
                return {**k, "is_admin": False}

        return None

    def get_all_keys(self) -> List[Dict]:
        """获取所有 Keys"""
        return self._keys


# 全局实例
api_key_manager = ApiKeyManager()
