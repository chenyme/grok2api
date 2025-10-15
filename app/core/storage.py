"""存储抽象层 - 支持文件和MySQL两种存储模式"""

import os
import json
import toml
import asyncio
import aiofiles
from pathlib import Path
from typing import Dict, Any, Optional, Literal
from abc import ABC, abstractmethod

from app.core.logger import logger


StorageMode = Literal["file", "mysql"]


class BaseStorage(ABC):
    """存储基类"""

    @abstractmethod
    async def init_db(self) -> None:
        """初始化数据库"""
        pass

    @abstractmethod
    async def load_tokens(self) -> Dict[str, Any]:
        """加载token数据"""
        pass

    @abstractmethod
    async def save_tokens(self, data: Dict[str, Any]) -> None:
        """保存token数据"""
        pass

    @abstractmethod
    async def load_config(self) -> Dict[str, Any]:
        """加载配置数据"""
        pass

    @abstractmethod
    async def save_config(self, data: Dict[str, Any]) -> None:
        """保存配置数据"""
        pass


class FileStorage(BaseStorage):
    """文件存储实现"""

    def __init__(self, data_dir: Path):
        self.data_dir = data_dir
        self.token_file = data_dir / "token.json"
        self.config_file = data_dir / "setting.toml"
        self._token_lock = asyncio.Lock()
        self._config_lock = asyncio.Lock()

    async def init_db(self) -> None:
        """初始化文件存储"""
        self.data_dir.mkdir(parents=True, exist_ok=True)
        
        # 初始化token文件
        if not self.token_file.exists():
            default_tokens = {"sso": {}, "ssoSuper": {}}
            async with aiofiles.open(self.token_file, "w", encoding="utf-8") as f:
                await f.write(json.dumps(default_tokens, indent=2, ensure_ascii=False))
            logger.info("[Storage] 创建新的token文件")

        # 初始化配置文件
        if not self.config_file.exists():
            default_config = {
                "global": {
                    "api_keys": [],
                    "admin_username": "admin",
                    "admin_password": "admin"
                },
                "grok": {
                    "proxy_url": "",
                    "cf_clearance": "",
                    "x_statsig_id": ""
                }
            }
            async with aiofiles.open(self.config_file, "w", encoding="utf-8") as f:
                await f.write(toml.dumps(default_config))
            logger.info("[Storage] 创建新的配置文件")

    async def load_tokens(self) -> Dict[str, Any]:
        """从文件加载token数据"""
        try:
            async with self._token_lock:
                if not self.token_file.exists():
                    return {"sso": {}, "ssoSuper": {}}
                
                async with aiofiles.open(self.token_file, "r", encoding="utf-8") as f:
                    content = await f.read()
                    return json.loads(content)
        except Exception as e:
            logger.error(f"[Storage] 加载token失败: {e}")
            return {"sso": {}, "ssoSuper": {}}

    async def save_tokens(self, data: Dict[str, Any]) -> None:
        """保存token数据到文件"""
        try:
            async with self._token_lock:
                async with aiofiles.open(self.token_file, "w", encoding="utf-8") as f:
                    await f.write(json.dumps(data, indent=2, ensure_ascii=False))
        except Exception as e:
            logger.error(f"[Storage] 保存token失败: {e}")
            raise

    async def load_config(self) -> Dict[str, Any]:
        """从文件加载配置数据"""
        try:
            async with self._config_lock:
                if not self.config_file.exists():
                    return {"global": {}, "grok": {}}
                
                async with aiofiles.open(self.config_file, "r", encoding="utf-8") as f:
                    content = await f.read()
                    return toml.loads(content)
        except Exception as e:
            logger.error(f"[Storage] 加载配置失败: {e}")
            return {"global": {}, "grok": {}}

    async def save_config(self, data: Dict[str, Any]) -> None:
        """保存配置数据到文件"""
        try:
            async with self._config_lock:
                async with aiofiles.open(self.config_file, "w", encoding="utf-8") as f:
                    await f.write(toml.dumps(data))
        except Exception as e:
            logger.error(f"[Storage] 保存配置失败: {e}")
            raise


class MysqlStorage(BaseStorage):
    """MySQL存储实现"""

    def __init__(self, database_url: str, data_dir: Path):
        self.database_url = database_url
        self.data_dir = data_dir
        self.token_file = data_dir / "token.json"
        self.config_file = data_dir / "setting.toml"
        self._pool = None
        self._file_storage = FileStorage(data_dir)

    async def init_db(self) -> None:
        """初始化MySQL数据库和连接池"""
        try:
            import aiomysql
            from urllib.parse import urlparse, unquote

            # 使用标准库解析URL
            parsed = urlparse(self.database_url)

            # 解码用户名和密码（处理URL编码）
            user = unquote(parsed.username) if parsed.username else ""
            password = unquote(parsed.password) if parsed.password else ""
            host = parsed.hostname
            port = parsed.port if parsed.port else 3306
            database = parsed.path[1:] if parsed.path else "grok2api"  # 去掉开头的 '/'

            logger.info(f"[Storage] 解析数据库连接: {user}@{host}:{port}/{database}")

            # 创建连接池
            self._pool = await aiomysql.create_pool(
                host=host,
                port=port,
                user=user,
                password=password,
                db=database,
                charset="utf8mb4",
                autocommit=True,
                maxsize=10
            )

            logger.info(f"[Storage] MySQL连接池创建成功: {host}:{port}/{database}")

            # 创建表
            async with self._pool.acquire() as conn:
                async with conn.cursor() as cursor:
                    # 创建tokens表
                    await cursor.execute("""
                        CREATE TABLE IF NOT EXISTS grok_tokens (
                            id INT AUTO_INCREMENT PRIMARY KEY,
                            data JSON NOT NULL,
                            updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
                            created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
                        ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
                    """)
                    
                    # 创建settings表
                    await cursor.execute("""
                        CREATE TABLE IF NOT EXISTS grok_settings (
                            id INT AUTO_INCREMENT PRIMARY KEY,
                            data JSON NOT NULL,
                            updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
                            created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
                        ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
                    """)
                    
                    logger.info("[Storage] MySQL表创建/验证成功")
            
            # 确保文件存储目录存在
            await self._file_storage.init_db()
            
            # 从数据库同步数据到文件
            await self._sync_from_db()
            
        except ImportError:
            logger.error("[Storage] aiomysql未安装，请运行: pip install aiomysql")
            raise Exception("aiomysql未安装")
        except Exception as e:
            logger.error(f"[Storage] MySQL初始化失败: {e}")
            raise

    async def _sync_from_db(self) -> None:
        """从数据库同步数据到文件"""
        try:
            # 同步tokens
            tokens = await self._load_from_db("grok_tokens")
            if tokens:
                await self._file_storage.save_tokens(tokens)
                logger.info("[Storage] Token数据已从数据库同步到文件")
            
            # 同步settings
            settings = await self._load_from_db("grok_settings")
            if settings:
                await self._file_storage.save_config(settings)
                logger.info("[Storage] 配置数据已从数据库同步到文件")
                
        except Exception as e:
            logger.warning(f"[Storage] 从数据库同步数据失败: {e}")

    async def _load_from_db(self, table: str) -> Optional[Dict[str, Any]]:
        """从数据库表加载最新数据"""
        try:
            async with self._pool.acquire() as conn:
                async with conn.cursor() as cursor:
                    await cursor.execute(f"SELECT data FROM {table} ORDER BY id DESC LIMIT 1")
                    result = await cursor.fetchone()
                    if result:
                        return json.loads(result[0])
                    return None
        except Exception as e:
            logger.error(f"[Storage] 从数据库加载{table}失败: {e}")
            return None

    async def _save_to_db(self, table: str, data: Dict[str, Any]) -> None:
        """保存数据到数据库表"""
        try:
            async with self._pool.acquire() as conn:
                async with conn.cursor() as cursor:
                    json_data = json.dumps(data, ensure_ascii=False)
                    
                    # 检查是否已有数据
                    await cursor.execute(f"SELECT id FROM {table} ORDER BY id DESC LIMIT 1")
                    result = await cursor.fetchone()
                    
                    if result:
                        # 更新现有数据
                        await cursor.execute(
                            f"UPDATE {table} SET data = %s WHERE id = %s",
                            (json_data, result[0])
                        )
                    else:
                        # 插入新数据
                        await cursor.execute(
                            f"INSERT INTO {table} (data) VALUES (%s)",
                            (json_data,)
                        )
        except Exception as e:
            logger.error(f"[Storage] 保存数据到{table}失败: {e}")
            raise

    async def load_tokens(self) -> Dict[str, Any]:
        """从文件加载token数据（已从数据库同步）"""
        return await self._file_storage.load_tokens()

    async def save_tokens(self, data: Dict[str, Any]) -> None:
        """保存token数据到文件和数据库"""
        # 先保存到文件
        await self._file_storage.save_tokens(data)
        # 再保存到数据库
        await self._save_to_db("grok_tokens", data)

    async def load_config(self) -> Dict[str, Any]:
        """从文件加载配置数据（已从数据库同步）"""
        return await self._file_storage.load_config()

    async def save_config(self, data: Dict[str, Any]) -> None:
        """保存配置数据到文件和数据库"""
        # 先保存到文件
        await self._file_storage.save_config(data)
        # 再保存到数据库
        await self._save_to_db("grok_settings", data)

    async def close(self) -> None:
        """关闭数据库连接池"""
        if self._pool:
            self._pool.close()
            await self._pool.wait_closed()
            logger.info("[Storage] MySQL连接池已关闭")


class StorageManager:
    """存储管理器 - 单例模式"""

    _instance: Optional['StorageManager'] = None
    _storage: Optional[BaseStorage] = None
    _initialized: bool = False

    def __new__(cls):
        if cls._instance is None:
            cls._instance = super().__new__(cls)
        return cls._instance

    async def init(self) -> None:
        """初始化存储管理器"""
        if self._initialized:
            return

        # 读取环境变量
        storage_mode = os.getenv("STORAGE_MODE", "file").lower()
        database_url = os.getenv("DATABASE_URL", "")
        
        data_dir = Path(__file__).parents[2] / "data"
        
        if storage_mode == "mysql":
            if not database_url:
                logger.error("[Storage] MySQL模式需要DATABASE_URL环境变量")
                raise ValueError("MySQL模式需要DATABASE_URL环境变量")
            
            logger.info(f"[Storage] 使用MySQL存储模式")
            self._storage = MysqlStorage(database_url, data_dir)
        else:
            logger.info(f"[Storage] 使用文件存储模式")
            self._storage = FileStorage(data_dir)
        
        await self._storage.init_db()
        self._initialized = True
        logger.info("[Storage] 存储管理器初始化完成")

    def get_storage(self) -> BaseStorage:
        """获取存储实例"""
        if not self._initialized or not self._storage:
            raise RuntimeError("StorageManager未初始化，请先调用init()")
        return self._storage

    async def close(self) -> None:
        """关闭存储"""
        if self._storage and isinstance(self._storage, MysqlStorage):
            await self._storage.close()


# 全局存储管理器实例
storage_manager = StorageManager()

