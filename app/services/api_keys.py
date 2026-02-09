"""API Key 管理器 - 多用户密钥管理"""

import asyncio
import hashlib
import secrets
import time
from pathlib import Path
from typing import Dict, List, Optional

import orjson

from app.core.logger import logger


def _safe_str_eq(a: str, b: str) -> bool:
    """时序安全的字符串比较"""
    return secrets.compare_digest(a, b)


def _hash_key(key: str) -> str:
    """计算 key 的 SHA-256 hash"""
    return hashlib.sha256(key.encode()).hexdigest()


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

        self.file_path = Path(__file__).parents[1] / "data" / "api_keys.json"
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

                # 迁移明文 key → hash 存储
                migrated = 0
                for k in self._keys:
                    if "key_hash" not in k and "key" in k:
                        k["key_hash"] = _hash_key(k["key"])
                        k["key_prefix"] = k["key"][:12]
                        del k["key"]
                        migrated += 1
                if migrated:
                    logger.info(f"[ApiKey] 迁移 {migrated} 个明文 key 到 hash 存储")
                    await self._save_data_inner()
        except Exception as e:
            logger.error(f"[ApiKey] 加载失败: {e}")
            self._keys = []
            self._loaded = True

    async def _save_data_inner(self):
        """保存 API Keys（调用方必须持有锁）"""
        self.file_path.parent.mkdir(parents=True, exist_ok=True)
        content = orjson.dumps(self._keys, option=orjson.OPT_INDENT_2)
        await asyncio.to_thread(self.file_path.write_bytes, content)

    async def _save_data(self):
        """保存 API Keys"""
        if not self._loaded:
            logger.warning("[ApiKey] 数据未加载，取消保存")
            return

        try:
            async with self._lock:
                await self._save_data_inner()
        except Exception as e:
            logger.error(f"[ApiKey] 保存失败: {e}")

    def generate_key(self) -> str:
        """生成 sk- 开头的 key"""
        return f"sk-{secrets.token_urlsafe(24)}"

    async def add_key(self, name: str = "") -> Dict:
        """添加 API Key，返回含明文 key 的字典（仅创建时可见）"""
        key_id = secrets.token_hex(8)
        plaintext_key = self.generate_key()
        new_key = {
            "id": key_id,
            "key_hash": _hash_key(plaintext_key),
            "key_prefix": plaintext_key[:12],
            "name": name or f"Key-{key_id[:6]}",
            "created_at": time.strftime("%Y-%m-%d %H:%M:%S"),
            "enabled": True,
        }
        self._keys.append(new_key)
        await self._save_data()
        logger.info(f"[ApiKey] 添加新 Key: {new_key['name']}")
        # 返回时附带明文 key 供用户保存（不持久化）
        return {**new_key, "key": plaintext_key}

    async def batch_add_keys(self, prefix: str, count: int) -> List[Dict]:
        """批量添加 API Key"""
        new_keys = []
        results = []
        for i in range(1, count + 1):
            key_id = secrets.token_hex(8)
            plaintext_key = self.generate_key()
            name = f"{prefix}-{i}" if prefix else f"Key-{key_id[:6]}"
            stored = {
                "id": key_id,
                "key_hash": _hash_key(plaintext_key),
                "key_prefix": plaintext_key[:12],
                "name": name,
                "created_at": time.strftime("%Y-%m-%d %H:%M:%S"),
                "enabled": True,
            }
            new_keys.append(stored)
            # 返回时附带明文 key
            results.append({**stored, "key": plaintext_key})

        self._keys.extend(new_keys)
        await self._save_data()
        logger.info(f"[ApiKey] 批量添加 {count} 个 Key")
        return results

    async def delete_key(self, key_id: str) -> bool:
        """删除 API Key"""
        initial_len = len(self._keys)
        self._keys = [k for k in self._keys if k["id"] != key_id]

        if len(self._keys) != initial_len:
            await self._save_data()
            logger.info(f"[ApiKey] 删除 Key: {key_id}")
            return True
        return False

    async def update_key(
        self, key_id: str, name: str = None, enabled: bool = None
    ) -> bool:
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
        """验证 Key（hash 比对）"""
        from app.core.config import get_config

        # 检查全局配置的 Key
        global_key = get_config("app.api_key", "")
        if global_key and _safe_str_eq(key, global_key):
            return {
                "key": global_key,
                "name": "Admin",
                "enabled": True,
                "is_admin": True,
            }

        # 检查多 Key 列表（hash 比对）
        key_hash = _hash_key(key)
        for k in self._keys:
            stored_hash = k.get("key_hash", "")
            if (
                stored_hash
                and _safe_str_eq(key_hash, stored_hash)
                and k.get("enabled", True)
            ):
                return {**k, "is_admin": False}

        return None

    def get_all_keys(self) -> List[Dict]:
        """获取所有 Keys（不含明文 key，仅 key_prefix 供展示）"""
        return self._keys


# 全局实例
api_key_manager = ApiKeyManager()
