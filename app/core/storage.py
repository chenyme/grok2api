"""存储抽象层 - 支持文件、MySQL和Redis存储"""

import os
import orjson
import toml
import asyncio
import warnings
import aiofiles
from pathlib import Path
from typing import Dict, Any, Optional, Literal
from abc import ABC, abstractmethod
from urllib.parse import urlparse, unquote

from app.core.logger import logger


StorageMode = Literal["file", "mysql", "redis"]


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
    """文件存储"""

    def __init__(self, data_dir: Path):
        self.data_dir = data_dir
        self.token_file = data_dir / "token.json"
        self.config_file = data_dir / "setting.toml"
        self._token_lock = asyncio.Lock()
        self._config_lock = asyncio.Lock()

    async def init_db(self) -> None:
        """初始化文件存储"""
        self.data_dir.mkdir(parents=True, exist_ok=True)

        if not self.token_file.exists():
            await self._write(self.token_file, orjson.dumps({"sso": {}, "ssoSuper": {}}, option=orjson.OPT_INDENT_2).decode())
            logger.info("[Storage] 创建token文件")

        if not self.config_file.exists():
            default = {
                "global": {"api_keys": [], "admin_username": "admin", "admin_password": "admin"},
                "grok": {"proxy_url": "", "cf_clearance": "", "x_statsig_id": ""}
            }
            await self._write(self.config_file, toml.dumps(default))
            logger.info("[Storage] 创建配置文件")

    async def _read(self, path: Path) -> str:
        """读取文件"""
        async with aiofiles.open(path, "r", encoding="utf-8") as f:
            return await f.read()

    async def _write(self, path: Path, content: str) -> None:
        """写入文件"""
        async with aiofiles.open(path, "w", encoding="utf-8") as f:
            await f.write(content)

    async def _load_json(self, path: Path, default: Dict, lock: asyncio.Lock) -> Dict[str, Any]:
        """加载JSON"""
        try:
            async with lock:
                if not path.exists():
                    return default
                return orjson.loads(await self._read(path))
        except Exception as e:
            logger.error(f"[Storage] 加载{path.name}失败: {e}")
            return default

    async def _save_json(self, path: Path, data: Dict, lock: asyncio.Lock) -> None:
        """保存JSON"""
        try:
            async with lock:
                await self._write(path, orjson.dumps(data, option=orjson.OPT_INDENT_2).decode())
        except Exception as e:
            logger.error(f"[Storage] 保存{path.name}失败: {e}")
            raise

    async def _load_toml(self, path: Path, default: Dict, lock: asyncio.Lock) -> Dict[str, Any]:
        """加载TOML"""
        try:
            async with lock:
                if not path.exists():
                    return default
                return toml.loads(await self._read(path))
        except Exception as e:
            logger.error(f"[Storage] 加载{path.name}失败: {e}")
            return default

    async def _save_toml(self, path: Path, data: Dict, lock: asyncio.Lock) -> None:
        """保存TOML"""
        try:
            async with lock:
                await self._write(path, toml.dumps(data))
        except Exception as e:
            logger.error(f"[Storage] 保存{path.name}失败: {e}")
            raise

    async def load_tokens(self) -> Dict[str, Any]:
        """加载token"""
        return await self._load_json(self.token_file, {"sso": {}, "ssoSuper": {}}, self._token_lock)

    async def save_tokens(self, data: Dict[str, Any]) -> None:
        """保存token"""
        await self._save_json(self.token_file, data, self._token_lock)

    async def load_config(self) -> Dict[str, Any]:
        """加载配置"""
        return await self._load_toml(self.config_file, {"global": {}, "grok": {}}, self._config_lock)

    async def save_config(self, data: Dict[str, Any]) -> None:
        """保存配置"""
        await self._save_toml(self.config_file, data, self._config_lock)


class MysqlStorage(BaseStorage):
    """MySQL存储"""

    def __init__(self, database_url: str, data_dir: Path):
        self.database_url = database_url
        self.data_dir = data_dir
        self._pool = None
        self._file = FileStorage(data_dir)

    async def init_db(self) -> None:
        """初始化MySQL"""
        try:
            import aiomysql
            parsed = self._parse_url(self.database_url)
            logger.info(f"[Storage] MySQL: {parsed['user']}@{parsed['host']}:{parsed['port']}/{parsed['db']}")

            await self._create_db(parsed)
            self._pool = await aiomysql.create_pool(
                host=parsed['host'], port=parsed['port'], user=parsed['user'],
                password=parsed['password'], db=parsed['db'], charset="utf8mb4",
                autocommit=True, maxsize=10
            )
            await self._create_tables()
            await self._file.init_db()
            await self._sync_data()

        except ImportError:
            raise Exception("aiomysql未安装")
        except Exception as e:
            logger.error(f"[Storage] MySQL初始化失败: {e}")
            raise

    def _parse_url(self, url: str) -> Dict[str, Any]:
        """解析URL"""
        p = urlparse(url)
        return {
            'user': unquote(p.username) if p.username else "",
            'password': unquote(p.password) if p.password else "",
            'host': p.hostname,
            'port': p.port or 3306,
            'db': p.path[1:] if p.path else "grok2api"
        }

    async def _create_db(self, parsed: Dict) -> None:
        """创建数据库"""
        import aiomysql
        pool = await aiomysql.create_pool(
            host=parsed['host'], port=parsed['port'], user=parsed['user'],
            password=parsed['password'], charset="utf8mb4", autocommit=True, maxsize=1
        )

        try:
            async with pool.acquire() as conn:
                async with conn.cursor() as cursor:
                    with warnings.catch_warnings():
                        warnings.filterwarnings('ignore', message='.*database exists')
                        await cursor.execute(
                            f"CREATE DATABASE IF NOT EXISTS `{parsed['db']}` "
                            f"CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"
                        )
                    logger.info(f"[Storage] 数据库 '{parsed['db']}' 就绪")
        finally:
            pool.close()
            await pool.wait_closed()

    async def _create_tables(self) -> None:
        """创建表"""
        tables = {
            "grok_tokens": """
                CREATE TABLE IF NOT EXISTS grok_tokens (
                    id INT AUTO_INCREMENT PRIMARY KEY,
                    data JSON NOT NULL,
                    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
                    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
                ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
            """,
            "grok_settings": """
                CREATE TABLE IF NOT EXISTS grok_settings (
                    id INT AUTO_INCREMENT PRIMARY KEY,
                    data JSON NOT NULL,
                    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
                    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
                ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
            """
        }

        async with self._pool.acquire() as conn:
            async with conn.cursor() as cursor:
                with warnings.catch_warnings():
                    warnings.filterwarnings('ignore', message='.*already exists')
                    for sql in tables.values():
                        await cursor.execute(sql)
                logger.info("[Storage] MySQL表就绪")

    async def _sync_data(self) -> None:
        """同步数据"""
        try:
            for table, key in [("grok_tokens", "sso"), ("grok_settings", "global")]:
                data = await self._load_db(table)
                if data:
                    if table == "grok_tokens":
                        await self._file.save_tokens(data)
                    else:
                        await self._file.save_config(data)
                    logger.info(f"[Storage] {table.split('_')[1]}数据已从DB同步")
                else:
                    file_data = await (self._file.load_tokens() if table == "grok_tokens" else self._file.load_config())
                    if file_data.get(key) or (table == "grok_tokens" and file_data.get("ssoSuper")):
                        await self._save_db(table, file_data)
                        logger.info(f"[Storage] {table.split('_')[1]}数据已初始化到DB")
        except Exception as e:
            logger.warning(f"[Storage] 同步失败: {e}")

    async def _load_db(self, table: str) -> Optional[Dict]:
        """从DB加载"""
        try:
            async with self._pool.acquire() as conn:
                async with conn.cursor() as cursor:
                    await cursor.execute(f"SELECT data FROM {table} ORDER BY id DESC LIMIT 1")
                    result = await cursor.fetchone()
                    return orjson.loads(result[0]) if result else None
        except Exception as e:
            logger.error(f"[Storage] 加载{table}失败: {e}")
            return None

    async def _save_db(self, table: str, data: Dict) -> None:
        """保存到DB"""
        try:
            async with self._pool.acquire() as conn:
                async with conn.cursor() as cursor:
                    json_data = orjson.dumps(data).decode()
                    await cursor.execute(f"SELECT id FROM {table} ORDER BY id DESC LIMIT 1")
                    result = await cursor.fetchone()

                    if result:
                        await cursor.execute(f"UPDATE {table} SET data = %s WHERE id = %s", (json_data, result[0]))
                    else:
                        await cursor.execute(f"INSERT INTO {table} (data) VALUES (%s)", (json_data,))
        except Exception as e:
            logger.error(f"[Storage] 保存{table}失败: {e}")
            raise

    async def load_tokens(self) -> Dict[str, Any]:
        """加载token"""
        return await self._file.load_tokens()

    async def save_tokens(self, data: Dict[str, Any]) -> None:
        """保存token"""
        await self._file.save_tokens(data)
        await self._save_db("grok_tokens", data)

    async def load_config(self) -> Dict[str, Any]:
        """加载配置"""
        return await self._file.load_config()

    async def save_config(self, data: Dict[str, Any]) -> None:
        """保存配置"""
        await self._file.save_config(data)
        await self._save_db("grok_settings", data)

    async def close(self) -> None:
        """关闭连接"""
        if self._pool:
            self._pool.close()
            await self._pool.wait_closed()
            logger.info("[Storage] MySQL已关闭")


class RedisStorage(BaseStorage):
    """Redis存储"""

    def __init__(self, redis_url: str, data_dir: Path):
        self.redis_url = redis_url
        self.data_dir = data_dir
        self._redis = None
        self._file = FileStorage(data_dir)

    async def init_db(self) -> None:
        """初始化Redis"""
        try:
            import redis.asyncio as aioredis
            parsed = urlparse(self.redis_url)
            db = int(parsed.path.lstrip('/')) if parsed.path and parsed.path != '/' else 0
            logger.info(f"[Storage] Redis: {parsed.hostname}:{parsed.port or 6379}/{db}")

            self._redis = aioredis.Redis.from_url(
                self.redis_url, encoding="utf-8", decode_responses=True
            )

            await self._redis.ping()
            logger.info(f"[Storage] Redis连接成功")

            await self._file.init_db()
            await self._sync_data()

        except ImportError:
            raise Exception("redis未安装")
        except Exception as e:
            logger.error(f"[Storage] Redis初始化失败: {e}")
            raise

    async def _sync_data(self) -> None:
        """同步数据"""
        try:
            for key, file_func, key_name in [
                ("grok:tokens", self._file.load_tokens, "sso"),
                ("grok:settings", self._file.load_config, "global")
            ]:
                data = await self._redis.get(key)
                if data:
                    parsed = orjson.loads(data)
                    if key == "grok:tokens":
                        await self._file.save_tokens(parsed)
                    else:
                        await self._file.save_config(parsed)
                    logger.info(f"[Storage] {key.split(':')[1]}数据已从Redis同步")
                else:
                    file_data = await file_func()
                    if file_data.get(key_name) or (key == "grok:tokens" and file_data.get("ssoSuper")):
                        await self._redis.set(key, orjson.dumps(file_data).decode())
                        logger.info(f"[Storage] {key.split(':')[1]}数据已初始化到Redis")
        except Exception as e:
            logger.warning(f"[Storage] 同步失败: {e}")

    async def _save_redis(self, key: str, data: Dict) -> None:
        """保存到Redis"""
        try:
            await self._redis.set(key, orjson.dumps(data).decode())
        except Exception as e:
            logger.error(f"[Storage] 保存Redis失败: {e}")
            raise

    async def load_tokens(self) -> Dict[str, Any]:
        """加载token"""
        return await self._file.load_tokens()

    async def save_tokens(self, data: Dict[str, Any]) -> None:
        """保存token"""
        await self._file.save_tokens(data)
        await self._save_redis("grok:tokens", data)

    async def load_config(self) -> Dict[str, Any]:
        """加载配置"""
        return await self._file.load_config()

    async def save_config(self, data: Dict[str, Any]) -> None:
        """保存配置"""
        await self._file.save_config(data)
        await self._save_redis("grok:settings", data)

    async def close(self) -> None:
        """关闭连接"""
        if self._redis:
            await self._redis.close()
            logger.info("[Storage] Redis已关闭")


class StorageManager:
    """存储管理器（单例）"""

    _instance: Optional['StorageManager'] = None
    _storage: Optional[BaseStorage] = None
    _initialized: bool = False

    def __new__(cls):
        if cls._instance is None:
            cls._instance = super().__new__(cls)
        return cls._instance

    async def init(self) -> None:
        """初始化存储"""
        if self._initialized:
            return

        mode = os.getenv("STORAGE_MODE", "file").lower()
        url = os.getenv("DATABASE_URL", "")
        data_dir = Path(__file__).parents[2] / "data"

        classes = {"mysql": MysqlStorage, "redis": RedisStorage, "file": FileStorage}

        if mode in ("mysql", "redis") and not url:
            raise ValueError(f"{mode.upper()}模式需要DATABASE_URL")

        storage_class = classes.get(mode, FileStorage)
        self._storage = storage_class(url, data_dir) if mode != "file" else storage_class(data_dir)

        await self._storage.init_db()
        self._initialized = True
        logger.info(f"[Storage] 使用{mode}模式")

    def get_storage(self) -> BaseStorage:
        """获取存储实例"""
        if not self._initialized or not self._storage:
            raise RuntimeError("StorageManager未初始化")
        return self._storage

    async def close(self) -> None:
        """关闭存储"""
        if self._storage and hasattr(self._storage, 'close'):
            await self._storage.close()


# 全局实例
storage_manager = StorageManager()
