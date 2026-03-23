"""
统一存储服务 (Professional Storage Service)
支持 Local (TOML), Redis, MySQL, PostgreSQL

特性:
- 全异步 I/O (Async I/O)
- 连接池管理 (Connection Pooling)
- 分布式/本地锁 (Distributed/Local Locking)
- 内存优化 (序列化性能优化)
"""

import abc
import os
import asyncio
import hashlib
import time
import tomllib
from typing import Any, ClassVar, Dict, Optional
from pathlib import Path
from enum import Enum

try:
    import fcntl
except ImportError:  # pragma: no cover - non-posix platforms
    fcntl = None
from contextlib import asynccontextmanager

import orjson
import aiofiles
from app.core.logger import logger

# 数据目录（支持通过环境变量覆盖）
DEFAULT_DATA_DIR = Path(__file__).parent.parent.parent / "data"
DATA_DIR = Path(os.getenv("DATA_DIR", str(DEFAULT_DATA_DIR))).expanduser()

# 配置文件路径
CONFIG_FILE = DATA_DIR / "config.toml"
TOKEN_FILE = DATA_DIR / "token.json"
LOCK_DIR = DATA_DIR / ".locks"


# JSON 序列化优化助手函数
def json_dumps(obj: Any) -> str:
    return orjson.dumps(obj).decode("utf-8")


def json_loads(obj: str | bytes) -> Any:
    return orjson.loads(obj)


def json_dumps_sorted(obj: Any) -> str:
    return orjson.dumps(obj, option=orjson.OPT_SORT_KEYS).decode("utf-8")


def has_token_entries(data: Any) -> bool:
    """Return True when the payload contains at least one non-empty token."""
    if not isinstance(data, dict):
        return False

    for tokens in data.values():
        if not isinstance(tokens, list):
            continue
        for item in tokens:
            if isinstance(item, str):
                if item.strip():
                    return True
                continue
            if isinstance(item, dict):
                token = item.get("token")
                if isinstance(token, str) and token.strip():
                    return True
    return False


class StorageError(Exception):
    """存储服务基础异常"""

    pass


class BaseStorage(abc.ABC):
    """存储基类"""

    @abc.abstractmethod
    async def load_config(self) -> Dict[str, Any]:
        """加载配置"""
        pass

    @abc.abstractmethod
    async def save_config(self, data: Dict[str, Any]):
        """保存配置"""
        pass

    @abc.abstractmethod
    async def load_tokens(self) -> Dict[str, Any]:
        """加载所有 Token"""
        pass

    @abc.abstractmethod
    async def save_tokens(self, data: Dict[str, Any]):
        """保存所有 Token"""
        pass

    async def load_tokens_version(self) -> Optional[str]:
        """加载 Token 存储版本，用于多 worker 之间的快速一致性检查。"""
        return None

    async def save_tokens_delta(
        self, updated: list[Dict[str, Any]], deleted: Optional[list[str]] = None
    ):
        """增量保存 Token（默认回退到全量保存）"""
        existing = await self.load_tokens() or {}

        deleted_set = set(deleted or [])
        if deleted_set:
            for pool_name, tokens in list(existing.items()):
                if not isinstance(tokens, list):
                    continue
                filtered = []
                for item in tokens:
                    if isinstance(item, str):
                        token_str = item
                    elif isinstance(item, dict):
                        token_str = item.get("token")
                    else:
                        token_str = None
                    if token_str and token_str in deleted_set:
                        continue
                    filtered.append(item)
                existing[pool_name] = filtered

        for item in updated or []:
            if not isinstance(item, dict):
                continue
            pool_name = item.get("pool_name")
            token_str = item.get("token")
            if not pool_name or not token_str:
                continue
            pool_list = existing.setdefault(pool_name, [])
            normalized = {
                k: v
                for k, v in item.items()
                if k not in ("pool_name", "_update_kind")
            }
            replaced = False
            for idx, current in enumerate(pool_list):
                if isinstance(current, str):
                    if current == token_str:
                        pool_list[idx] = normalized
                        replaced = True
                        break
                elif isinstance(current, dict) and current.get("token") == token_str:
                    pool_list[idx] = normalized
                    replaced = True
                    break
            if not replaced:
                pool_list.append(normalized)

        await self.save_tokens(existing)

    @abc.abstractmethod
    async def close(self):
        """关闭资源"""
        pass

    @asynccontextmanager
    async def acquire_lock(self, name: str, timeout: int = 10):
        """
        获取锁 (互斥访问)
        用于读写操作的临界区保护

        Args:
            name: 锁名称
            timeout: 超时时间 (秒)
        """
        # 默认空实现，用于 fallback
        yield

    async def verify_connection(self) -> bool:
        """健康检查"""
        return True


class LocalStorage(BaseStorage):
    """
    本地文件存储
    - 使用 aiofiles 进行异步 I/O
    - 使用 asyncio.Lock 进行进程内并发控制
    - 如果需要多进程安全，需要系统级文件锁 (fcntl)
    """

    def __init__(self):
        self._lock = asyncio.Lock()

    @asynccontextmanager
    async def acquire_lock(self, name: str, timeout: int = 10):
        if fcntl is None:
            try:
                async with asyncio.timeout(timeout):
                    async with self._lock:
                        yield
            except asyncio.TimeoutError:
                logger.warning(f"LocalStorage: 获取锁 '{name}' 超时 ({timeout}s)")
                raise StorageError(f"无法获取锁 '{name}'")
            return

        lock_path = LOCK_DIR / f"{name}.lock"
        lock_path.parent.mkdir(parents=True, exist_ok=True)
        fd = None
        locked = False
        start = time.monotonic()

        async with self._lock:
            try:
                fd = open(lock_path, "a+")
                while True:
                    try:
                        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                        locked = True
                        break
                    except BlockingIOError:
                        if time.monotonic() - start >= timeout:
                            raise StorageError(f"无法获取锁 '{name}'")
                        await asyncio.sleep(0.05)
                yield
            except StorageError:
                logger.warning(f"LocalStorage: 获取锁 '{name}' 超时 ({timeout}s)")
                raise
            finally:
                if fd:
                    if locked:
                        try:
                            fcntl.flock(fd, fcntl.LOCK_UN)
                        except Exception:
                            pass
                    try:
                        fd.close()
                    except Exception:
                        pass

    async def load_config(self) -> Dict[str, Any]:
        if not CONFIG_FILE.exists():
            return {}
        try:
            async with aiofiles.open(CONFIG_FILE, "rb") as f:
                content = await f.read()
                return tomllib.loads(content.decode("utf-8"))
        except Exception as e:
            logger.error(f"LocalStorage: 加载配置失败: {e}")
            return {}

    async def save_config(self, data: Dict[str, Any]):
        try:
            lines = []
            for section, items in data.items():
                if not isinstance(items, dict):
                    continue
                lines.append(f"[{section}]")
                for key, val in items.items():
                    if isinstance(val, bool):
                        val_str = "true" if val else "false"
                    elif isinstance(val, str):
                        # Use JSON string escaping to keep TOML valid for multiline/control chars.
                        val_str = json_dumps(val)
                    elif isinstance(val, (int, float)):
                        val_str = str(val)
                    elif isinstance(val, (list, dict)):
                        val_str = json_dumps(val)
                    else:
                        val_str = f'"{str(val)}"'
                    lines.append(f"{key} = {val_str}")
                lines.append("")

            content = "\n".join(lines)

            CONFIG_FILE.parent.mkdir(parents=True, exist_ok=True)
            async with aiofiles.open(CONFIG_FILE, "w", encoding="utf-8") as f:
                await f.write(content)
        except Exception as e:
            logger.error(f"LocalStorage: 保存配置失败: {e}")
            raise StorageError(f"保存配置失败: {e}")

    async def load_tokens(self) -> Dict[str, Any]:
        if not TOKEN_FILE.exists():
            return {}
        try:
            async with aiofiles.open(TOKEN_FILE, "rb") as f:
                content = await f.read()
                return json_loads(content)
        except Exception as e:
            logger.error(f"LocalStorage: 加载 Token 失败: {e}")
            return {}

    async def load_tokens_version(self) -> Optional[str]:
        if not TOKEN_FILE.exists():
            return "missing"
        try:
            stat = TOKEN_FILE.stat()
            return f"{stat.st_mtime_ns}:{stat.st_size}"
        except Exception as e:
            logger.warning(f"LocalStorage: 读取 Token 版本失败: {e}")
            return None

    async def save_tokens(self, data: Dict[str, Any]):
        try:
            if not has_token_entries(data):
                existing = await self.load_tokens() or {}
                if has_token_entries(existing):
                    logger.warning(
                        "LocalStorage: 跳过空 Token 全量保存，避免覆盖已有数据"
                    )
                    return
            TOKEN_FILE.parent.mkdir(parents=True, exist_ok=True)
            temp_path = TOKEN_FILE.with_suffix(".tmp")

            # 原子写操作: 写入临时文件 -> 重命名
            async with aiofiles.open(temp_path, "wb") as f:
                await f.write(orjson.dumps(data, option=orjson.OPT_INDENT_2))

            # 使用 os.replace 保证原子性
            os.replace(temp_path, TOKEN_FILE)

        except Exception as e:
            logger.error(f"LocalStorage: 保存 Token 失败: {e}")
            raise StorageError(f"保存 Token 失败: {e}")

    async def close(self):
        pass


class RedisStorage(BaseStorage):
    """
    Redis 存储
    - 使用 redis-py 异步客户端 (自带连接池)
    - 支持分布式锁 (redis.lock)
    - 扁平化数据结构优化性能
    """

    def __init__(self, url: str):
        try:
            from redis import asyncio as aioredis
        except ImportError:
            raise ImportError("需要安装 redis 包: pip install redis")

        # 显式配置连接池
        # 使用 decode_responses=True 简化字符串处理，但在处理复杂对象时使用 orjson
        self.redis = aioredis.from_url(
            url, decode_responses=True, health_check_interval=30
        )
        self.config_key = "grok2api:config"  # Hash: section.key -> value_json
        self.key_pools = "grok2api:pools"  # Set: pool_names
        self.prefix_pool_set = "grok2api:pool:"  # Set: pool -> token_ids
        self.prefix_token_hash = "grok2api:token:"  # Hash: token_id -> token_data
        self.tokens_version_key = "grok2api:tokens_version"
        self.lock_prefix = "grok2api:lock:"

    @asynccontextmanager
    async def acquire_lock(self, name: str, timeout: int = 10):
        # 使用 Redis 分布式锁
        lock_key = f"{self.lock_prefix}{name}"
        lock = self.redis.lock(lock_key, timeout=timeout, blocking_timeout=5)
        acquired = False
        try:
            acquired = await lock.acquire()
            if not acquired:
                raise StorageError(f"RedisStorage: 无法获取锁 '{name}'")
            yield
        finally:
            if acquired:
                try:
                    await lock.release()
                except Exception:
                    # 锁可能已过期或被意外释放，忽略异常
                    pass

    async def verify_connection(self) -> bool:
        try:
            return await self.redis.ping()
        except Exception:
            return False

    async def load_config(self) -> Dict[str, Any]:
        """从 Redis Hash 加载配置"""
        try:
            raw_data = await self.redis.hgetall(self.config_key)
            if not raw_data:
                return None

            config = {}
            for composite_key, val_str in raw_data.items():
                if "." not in composite_key:
                    continue
                section, key = composite_key.split(".", 1)

                if section not in config:
                    config[section] = {}

                try:
                    val = json_loads(val_str)
                except Exception:
                    val = val_str
                config[section][key] = val
            return config
        except Exception as e:
            logger.error(f"RedisStorage: 加载配置失败: {e}")
            return None

    async def save_config(self, data: Dict[str, Any]):
        """保存配置到 Redis Hash"""
        try:
            mapping = {}
            for section, items in data.items():
                if not isinstance(items, dict):
                    continue
                for key, val in items.items():
                    composite_key = f"{section}.{key}"
                    mapping[composite_key] = json_dumps(val)

            await self.redis.delete(self.config_key)
            if mapping:
                await self.redis.hset(self.config_key, mapping=mapping)
        except Exception as e:
            logger.error(f"RedisStorage: 保存配置失败: {e}")
            raise

    async def load_tokens(self) -> Dict[str, Any]:
        """加载所有 Token"""
        try:
            pool_names = await self.redis.smembers(self.key_pools)
            if not pool_names:
                return None

            pools = {}
            async with self.redis.pipeline() as pipe:
                for pool_name in pool_names:
                    # 获取该池下所有 Token ID
                    pipe.smembers(f"{self.prefix_pool_set}{pool_name}")
                pool_tokens_res = await pipe.execute()

            # 收集所有 Token ID 以便批量查询
            all_token_ids = []
            pool_map = {}  # pool_name -> list[token_id]

            for i, pool_name in enumerate(pool_names):
                tids = list(pool_tokens_res[i])
                pool_map[pool_name] = tids
                all_token_ids.extend(tids)

            if not all_token_ids:
                return {name: [] for name in pool_names}

            # 批量获取 Token 详情 (Hash)
            async with self.redis.pipeline() as pipe:
                for tid in all_token_ids:
                    pipe.hgetall(f"{self.prefix_token_hash}{tid}")
                token_data_list = await pipe.execute()

            # 重组数据结构
            token_lookup = {}
            for i, tid in enumerate(all_token_ids):
                t_data = token_data_list[i]
                if not t_data:
                    continue

                # 恢复 tags (JSON -> List)
                if "tags" in t_data:
                    try:
                        t_data["tags"] = json_loads(t_data["tags"])
                    except Exception:
                        t_data["tags"] = []

                # 类型转换 (Redis 返回全 string)
                for int_field in [
                    "quota",
                    "created_at",
                    "use_count",
                    "fail_count",
                    "last_used_at",
                    "last_fail_at",
                    "last_sync_at",
                ]:
                    if t_data.get(int_field) and t_data[int_field] != "None":
                        try:
                            t_data[int_field] = int(t_data[int_field])
                        except Exception:
                            pass

                token_lookup[tid] = t_data

            # 按 Pool 分组返回
            for pool_name in pool_names:
                pools[pool_name] = []
                for tid in pool_map[pool_name]:
                    if tid in token_lookup:
                        pools[pool_name].append(token_lookup[tid])

            return pools

        except Exception as e:
            logger.error(f"RedisStorage: 加载 Token 失败: {e}")
            return None

    async def load_tokens_version(self) -> Optional[str]:
        try:
            version = await self.redis.get(self.tokens_version_key)
            return str(version) if version is not None else None
        except Exception as e:
            logger.warning(f"RedisStorage: 读取 Token 版本失败: {e}")
            return None

    async def save_tokens(self, data: Dict[str, Any]):
        """保存所有 Token"""
        if data is None:
            return
        try:
            new_pools = set(data.keys()) if isinstance(data, dict) else set()
            pool_tokens_map = {}
            new_token_ids = set()

            for pool_name, tokens in (data or {}).items():
                tids_in_pool = []
                for t in tokens:
                    token_str = t.get("token")
                    if not token_str:
                        continue
                    tids_in_pool.append(token_str)
                    new_token_ids.add(token_str)
                pool_tokens_map[pool_name] = tids_in_pool

            existing_pools = await self.redis.smembers(self.key_pools)
            existing_pools = set(existing_pools) if existing_pools else set()

            existing_token_ids = set()
            if existing_pools:
                async with self.redis.pipeline() as pipe:
                    for pool_name in existing_pools:
                        pipe.smembers(f"{self.prefix_pool_set}{pool_name}")
                    pool_tokens_res = await pipe.execute()
                for tokens in pool_tokens_res:
                    existing_token_ids.update(list(tokens or []))

            if not new_token_ids:
                if existing_token_ids:
                    logger.warning(
                        "RedisStorage: 跳过空 Token 全量保存，避免删除已有数据"
                    )
                return

            tokens_to_delete = existing_token_ids - new_token_ids
            all_pools = existing_pools.union(new_pools)

            async with self.redis.pipeline() as pipe:
                # Reset pool index
                pipe.delete(self.key_pools)
                if new_pools:
                    pipe.sadd(self.key_pools, *new_pools)

                # Reset pool sets
                for pool_name in all_pools:
                    pipe.delete(f"{self.prefix_pool_set}{pool_name}")
                for pool_name, tids_in_pool in pool_tokens_map.items():
                    if tids_in_pool:
                        pipe.sadd(f"{self.prefix_pool_set}{pool_name}", *tids_in_pool)

                # Remove deleted token hashes
                for token_str in tokens_to_delete:
                    pipe.delete(f"{self.prefix_token_hash}{token_str}")

                # Upsert token hashes
                for pool_name, tokens in (data or {}).items():
                    for t in tokens:
                        token_str = t.get("token")
                        if not token_str:
                            continue
                        t_flat = t.copy()
                        if "tags" in t_flat:
                            t_flat["tags"] = json_dumps(t_flat["tags"])
                        status = t_flat.get("status")
                        if isinstance(status, str) and status.startswith(
                            "TokenStatus."
                        ):
                            t_flat["status"] = status.split(".", 1)[1].lower()
                        elif isinstance(status, Enum):
                            t_flat["status"] = status.value
                        t_flat = {k: str(v) for k, v in t_flat.items() if v is not None}
                        pipe.hset(
                            f"{self.prefix_token_hash}{token_str}", mapping=t_flat
                        )

                pipe.set(self.tokens_version_key, str(time.time_ns()))

                await pipe.execute()

        except Exception as e:
            logger.error(f"RedisStorage: 保存 Token 失败: {e}")
            raise

    async def close(self):
        try:
            await self.redis.close()
        except (RuntimeError, asyncio.CancelledError, Exception):
            # 忽略关闭时的 Event loop is closed 错误
            pass


class SQLStorage(BaseStorage):
    """
    SQL 数据库存储 (MySQL/PgSQL)
    - 使用 SQLAlchemy 异步引擎
    - 自动 Schema 初始化
    - 内置连接池 (QueuePool)
    """

    def __init__(self, url: str, connect_args: dict | None = None):
        try:
            from sqlalchemy.ext.asyncio import create_async_engine, async_sessionmaker
        except ImportError:
            raise ImportError(
                "需要安装 sqlalchemy 和 async 驱动: pip install sqlalchemy[asyncio]"
            )

        self.dialect = url.split(":", 1)[0].split("+", 1)[0].lower()

        # 配置 robust 的连接池
        self.engine = create_async_engine(
            url,
            echo=False,
            pool_size=20,
            max_overflow=10,
            pool_recycle=3600,
            pool_pre_ping=True,
            **({"connect_args": connect_args} if connect_args else {}),
        )
        self.async_session = async_sessionmaker(self.engine, expire_on_commit=False)
        self._initialized = False

    async def _ensure_schema(self):
        """确保数据库表存在"""
        if self._initialized:
            return
        try:
            async with self.engine.begin() as conn:
                from sqlalchemy import text

                # Tokens 表 (通用 SQL)
                await conn.execute(
                    text("""
                    CREATE TABLE IF NOT EXISTS tokens (
                        token VARCHAR(512) PRIMARY KEY,
                        pool_name VARCHAR(64) NOT NULL,
                        status VARCHAR(16),
                        quota INT,
                        created_at BIGINT,
                        last_used_at BIGINT,
                        use_count INT,
                        fail_count INT,
                        last_fail_at BIGINT,
                        last_fail_reason TEXT,
                        last_sync_at BIGINT,
                        tags TEXT,
                        note TEXT,
                        last_asset_clear_at BIGINT,
                        data TEXT,
                        data_hash CHAR(64),
                        updated_at BIGINT
                    )
                """)
                )

                # 配置表
                await conn.execute(
                    text("""
                    CREATE TABLE IF NOT EXISTS app_config (
                        section VARCHAR(64) NOT NULL,
                        key_name VARCHAR(64) NOT NULL,
                        value TEXT,
                        PRIMARY KEY (section, key_name)
                    )
                """)
                )

                # 索引
                if self.dialect in ("postgres", "postgresql", "pgsql"):
                    await conn.execute(
                        text(
                            "CREATE INDEX IF NOT EXISTS idx_tokens_pool ON tokens (pool_name)"
                        )
                    )
                else:
                    try:
                        await conn.execute(
                            text("CREATE INDEX idx_tokens_pool ON tokens (pool_name)")
                        )
                    except Exception:
                        pass

                # 补齐旧表字段
                columns = [
                    ("status", "VARCHAR(16)"),
                    ("quota", "INT"),
                    ("created_at", "BIGINT"),
                    ("last_used_at", "BIGINT"),
                    ("use_count", "INT"),
                    ("fail_count", "INT"),
                    ("last_fail_at", "BIGINT"),
                    ("last_fail_reason", "TEXT"),
                    ("last_sync_at", "BIGINT"),
                    ("tags", "TEXT"),
                    ("note", "TEXT"),
                    ("last_asset_clear_at", "BIGINT"),
                    ("data", "TEXT"),
                    ("data_hash", "CHAR(64)"),
                    ("updated_at", "BIGINT"),
                ]
                if self.dialect in ("postgres", "postgresql", "pgsql"):
                    for col_name, col_type in columns:
                        await conn.execute(
                            text(
                                f"ALTER TABLE tokens ADD COLUMN IF NOT EXISTS {col_name} {col_type}"
                            )
                        )
                else:
                    for col_name, col_type in columns:
                        try:
                            await conn.execute(
                                text(
                                    f"ALTER TABLE tokens ADD COLUMN {col_name} {col_type}"
                                )
                            )
                        except Exception:
                            pass

                # 尝试兼容旧表结构
                try:
                    if self.dialect in ("mysql", "mariadb"):
                        await conn.execute(
                            text("ALTER TABLE tokens MODIFY token VARCHAR(512)")
                        )
                        await conn.execute(text("ALTER TABLE tokens MODIFY data TEXT"))
                    elif self.dialect in ("postgres", "postgresql", "pgsql"):
                        await conn.execute(
                            text(
                                "ALTER TABLE tokens ALTER COLUMN token TYPE VARCHAR(512)"
                            )
                        )
                        await conn.execute(
                            text("ALTER TABLE tokens ALTER COLUMN data TYPE TEXT")
                        )
                except Exception:
                    pass

            await self._migrate_legacy_tokens()
            self._initialized = True
        except Exception as e:
            logger.error(f"SQLStorage: Schema 初始化失败: {e}")
            raise

    def _normalize_status(self, status: Any) -> Any:
        if isinstance(status, str) and status.startswith("TokenStatus."):
            return status.split(".", 1)[1].lower()
        if isinstance(status, Enum):
            return status.value
        return status

    def _normalize_tags(self, tags: Any) -> Optional[str]:
        if tags is None:
            return None
        if isinstance(tags, str):
            try:
                parsed = json_loads(tags)
                if isinstance(parsed, list):
                    return tags
            except Exception:
                pass
            return json_dumps([tags])
        return json_dumps(tags)

    def _parse_tags(self, tags: Any) -> Optional[list]:
        if tags is None:
            return None
        if isinstance(tags, str):
            try:
                parsed = json_loads(tags)
                if isinstance(parsed, list):
                    return parsed
            except Exception:
                return []
        if isinstance(tags, list):
            return tags
        return []

    def _token_to_row(self, token_data: Dict[str, Any], pool_name: str) -> Dict[str, Any]:
        token_str = token_data.get("token")
        if isinstance(token_str, str) and token_str.startswith("sso="):
            token_str = token_str[4:]

        status = self._normalize_status(token_data.get("status"))
        tags_json = self._normalize_tags(token_data.get("tags"))
        data_json = json_dumps_sorted(token_data)
        data_hash = hashlib.sha256(data_json.encode("utf-8")).hexdigest()
        note = token_data.get("note")
        if note is None:
            note = ""

        return {
            "token": token_str,
            "pool_name": pool_name,
            "status": status,
            "quota": token_data.get("quota"),
            "created_at": token_data.get("created_at"),
            "last_used_at": token_data.get("last_used_at"),
            "use_count": token_data.get("use_count"),
            "fail_count": token_data.get("fail_count"),
            "last_fail_at": token_data.get("last_fail_at"),
            "last_fail_reason": token_data.get("last_fail_reason"),
            "last_sync_at": token_data.get("last_sync_at"),
            "tags": tags_json,
            "note": note,
            "last_asset_clear_at": token_data.get("last_asset_clear_at"),
            "data": data_json,
            "data_hash": data_hash,
            "updated_at": int(time.time() * 1000),
        }

    async def _migrate_legacy_tokens(self):
        """将旧版 data JSON 回填到平铺字段"""
        from sqlalchemy import text

        try:
            async with self.async_session() as session:
                try:
                    res = await session.execute(
                        text(
                            "SELECT token FROM tokens "
                            "WHERE data IS NOT NULL AND "
                            "(status IS NULL OR quota IS NULL OR created_at IS NULL) "
                            "LIMIT 1"
                        )
                    )
                    if not res.first():
                        return
                except Exception as e:
                    msg = str(e).lower()
                    if "undefinedcolumn" in msg or "undefined column" in msg:
                        return
                    raise

                res = await session.execute(
                    text(
                        "SELECT token, pool_name, data FROM tokens "
                        "WHERE data IS NOT NULL AND "
                        "(status IS NULL OR quota IS NULL OR created_at IS NULL)"
                    )
                )
                rows = res.fetchall()
                if not rows:
                    return

                params = []
                for token_str, pool_name, data_json in rows:
                    if not data_json:
                        continue
                    try:
                        if isinstance(data_json, str):
                            t_data = json_loads(data_json)
                        else:
                            t_data = data_json
                        if not isinstance(t_data, dict):
                            continue
                        t_data = dict(t_data)
                        t_data["token"] = token_str
                        row = self._token_to_row(t_data, pool_name)
                        params.append(row)
                    except Exception:
                        continue

                if not params:
                    return

                await session.execute(
                    text(
                        "UPDATE tokens SET "
                        "pool_name=:pool_name, "
                        "status=:status, "
                        "quota=:quota, "
                        "created_at=:created_at, "
                        "last_used_at=:last_used_at, "
                        "use_count=:use_count, "
                        "fail_count=:fail_count, "
                        "last_fail_at=:last_fail_at, "
                        "last_fail_reason=:last_fail_reason, "
                        "last_sync_at=:last_sync_at, "
                        "tags=:tags, "
                        "note=:note, "
                        "last_asset_clear_at=:last_asset_clear_at, "
                        "data=:data, "
                        "data_hash=:data_hash, "
                        "updated_at=:updated_at "
                        "WHERE token=:token"
                    ),
                    params,
                )
                await session.commit()
        except Exception as e:
            logger.warning(f"SQLStorage: 旧数据回填失败: {e}")

    @asynccontextmanager
    async def acquire_lock(self, name: str, timeout: int = 10):
        # SQL 分布式锁: MySQL GET_LOCK / PG advisory_lock
        from sqlalchemy import text

        lock_name = f"g2a:{hashlib.sha1(name.encode('utf-8')).hexdigest()[:24]}"
        if self.dialect in ("mysql", "mariadb"):
            async with self.async_session() as session:
                res = await session.execute(
                    text("SELECT GET_LOCK(:name, :timeout)"),
                    {"name": lock_name, "timeout": timeout},
                )
                got = res.scalar()
                if got != 1:
                    raise StorageError(f"SQLStorage: 无法获取锁 '{name}'")
                try:
                    yield
                finally:
                    try:
                        await session.execute(
                            text("SELECT RELEASE_LOCK(:name)"), {"name": lock_name}
                        )
                        await session.commit()
                    except Exception:
                        pass
        elif self.dialect in ("postgres", "postgresql", "pgsql"):
            lock_key = int.from_bytes(
                hashlib.sha256(name.encode("utf-8")).digest()[:8], "big", signed=True
            )
            async with self.async_session() as session:
                start = time.monotonic()
                while True:
                    res = await session.execute(
                        text("SELECT pg_try_advisory_lock(:key)"), {"key": lock_key}
                    )
                    if res.scalar():
                        break
                    if time.monotonic() - start >= timeout:
                        raise StorageError(f"SQLStorage: 无法获取锁 '{name}'")
                    await asyncio.sleep(0.1)
                try:
                    yield
                finally:
                    try:
                        await session.execute(
                            text("SELECT pg_advisory_unlock(:key)"), {"key": lock_key}
                        )
                        await session.commit()
                    except Exception:
                        pass
        else:
            yield

    async def load_config(self) -> Dict[str, Any]:
        await self._ensure_schema()
        from sqlalchemy import text

        try:
            async with self.async_session() as session:
                res = await session.execute(
                    text("SELECT section, key_name, value FROM app_config")
                )
                rows = res.fetchall()
                if not rows:
                    return None

                config = {}
                for section, key, val_str in rows:
                    if section not in config:
                        config[section] = {}
                    try:
                        val = json_loads(val_str)
                    except Exception:
                        val = val_str
                    config[section][key] = val
                return config
        except Exception as e:
            logger.error(f"SQLStorage: 加载配置失败: {e}")
            return None

    async def save_config(self, data: Dict[str, Any]):
        await self._ensure_schema()
        from sqlalchemy import text

        try:
            async with self.async_session() as session:
                await session.execute(text("DELETE FROM app_config"))

                params = []
                for section, items in data.items():
                    if not isinstance(items, dict):
                        continue
                    for key, val in items.items():
                        params.append(
                            {
                                "s": section,
                                "k": key,
                                "v": json_dumps(val),
                            }
                        )

                if params:
                    await session.execute(
                        text(
                            "INSERT INTO app_config (section, key_name, value) VALUES (:s, :k, :v)"
                        ),
                        params,
                    )
                await session.commit()
        except Exception as e:
            logger.error(f"SQLStorage: 保存配置失败: {e}")
            raise

    async def load_tokens(self) -> Dict[str, Any]:
        await self._ensure_schema()
        from sqlalchemy import text

        try:
            async with self.async_session() as session:
                res = await session.execute(
                    text(
                        "SELECT token, pool_name, status, quota, created_at, "
                        "last_used_at, use_count, fail_count, last_fail_at, "
                        "last_fail_reason, last_sync_at, tags, note, "
                        "last_asset_clear_at, data "
                        "FROM tokens"
                    )
                )
                rows = res.fetchall()
                if not rows:
                    return None

                pools = {}
                for (
                    token_str,
                    pool_name,
                    status,
                    quota,
                    created_at,
                    last_used_at,
                    use_count,
                    fail_count,
                    last_fail_at,
                    last_fail_reason,
                    last_sync_at,
                    tags,
                    note,
                    last_asset_clear_at,
                    data_json,
                ) in rows:
                    if pool_name not in pools:
                        pools[pool_name] = []

                    try:
                        token_data = {}
                        if token_str:
                            token_data["token"] = token_str
                        if status is not None:
                            token_data["status"] = self._normalize_status(status)
                        if quota is not None:
                            token_data["quota"] = int(quota)
                        if created_at is not None:
                            token_data["created_at"] = int(created_at)
                        if last_used_at is not None:
                            token_data["last_used_at"] = int(last_used_at)
                        if use_count is not None:
                            token_data["use_count"] = int(use_count)
                        if fail_count is not None:
                            token_data["fail_count"] = int(fail_count)
                        if last_fail_at is not None:
                            token_data["last_fail_at"] = int(last_fail_at)
                        if last_fail_reason is not None:
                            token_data["last_fail_reason"] = last_fail_reason
                        if last_sync_at is not None:
                            token_data["last_sync_at"] = int(last_sync_at)
                        if tags is not None:
                            token_data["tags"] = self._parse_tags(tags)
                        if note is not None:
                            token_data["note"] = note
                        if last_asset_clear_at is not None:
                            token_data["last_asset_clear_at"] = int(
                                last_asset_clear_at
                            )

                        legacy_data = None
                        if data_json:
                            if isinstance(data_json, str):
                                legacy_data = json_loads(data_json)
                            else:
                                legacy_data = data_json
                        if isinstance(legacy_data, dict):
                            for key, val in legacy_data.items():
                                if key not in token_data or token_data[key] is None:
                                    token_data[key] = val

                        pools[pool_name].append(token_data)
                    except Exception:
                        pass
                return pools
        except Exception as e:
            logger.error(f"SQLStorage: 加载 Token 失败: {e}")
            return None

    async def load_tokens_version(self) -> Optional[str]:
        await self._ensure_schema()
        from sqlalchemy import text

        try:
            async with self.async_session() as session:
                res = await session.execute(
                    text(
                        "SELECT COALESCE(MAX(updated_at), 0), COUNT(*) "
                        "FROM tokens"
                    )
                )
                max_updated_at, count = res.one()
                return f"{int(max_updated_at or 0)}:{int(count or 0)}"
        except Exception as e:
            logger.warning(f"SQLStorage: 读取 Token 版本失败: {e}")
            return None

    async def save_tokens(self, data: Dict[str, Any]):
        await self._ensure_schema()
        from sqlalchemy import text

        if data is None:
            return

        updates = []
        new_tokens = set()
        for pool_name, tokens in (data or {}).items():
            for t in tokens:
                if isinstance(t, dict):
                    token_data = dict(t)
                elif isinstance(t, str):
                    token_data = {"token": t}
                else:
                    continue
                token_str = token_data.get("token")
                if not token_str:
                    continue
                if token_str.startswith("sso="):
                    token_str = token_str[4:]
                token_data["token"] = token_str
                token_data["pool_name"] = pool_name
                token_data["_update_kind"] = "state"
                updates.append(token_data)
                new_tokens.add(token_str)

        try:
            existing_tokens = set()
            async with self.async_session() as session:
                res = await session.execute(text("SELECT token FROM tokens"))
                rows = res.fetchall()
                existing_tokens = {row[0] for row in rows}
            if not new_tokens:
                if existing_tokens:
                    logger.warning(
                        "SQLStorage: 跳过空 Token 全量保存，避免删除已有数据"
                    )
                return
            tokens_to_delete = list(existing_tokens - new_tokens)
            await self.save_tokens_delta(updates, tokens_to_delete)
        except Exception as e:
            logger.error(f"SQLStorage: 保存 Token 失败: {e}")
            raise

    async def save_tokens_delta(
        self, updated: list[Dict[str, Any]], deleted: Optional[list[str]] = None
    ):
        await self._ensure_schema()
        from sqlalchemy import bindparam, text

        try:
            async with self.async_session() as session:
                deleted_set = set(deleted or [])
                if deleted_set:
                    delete_stmt = text(
                        "DELETE FROM tokens WHERE token IN :tokens"
                    ).bindparams(bindparam("tokens", expanding=True))
                    chunk_size = 500
                    deleted_list = list(deleted_set)
                    for i in range(0, len(deleted_list), chunk_size):
                        chunk = deleted_list[i : i + chunk_size]
                        await session.execute(delete_stmt, {"tokens": chunk})

                updates = []
                usage_updates = []

                for item in updated or []:
                    if not isinstance(item, dict):
                        continue
                    pool_name = item.get("pool_name")
                    token_str = item.get("token")
                    if not pool_name or not token_str:
                        continue
                    if token_str in deleted_set:
                        continue
                    update_kind = item.get("_update_kind", "state")
                    token_data = {
                        k: v
                        for k, v in item.items()
                        if k not in ("pool_name", "_update_kind")
                    }
                    row = self._token_to_row(token_data, pool_name)
                    if update_kind == "usage":
                        usage_updates.append(row)
                    else:
                        updates.append(row)

                if updates:
                    if self.dialect in ("mysql", "mariadb"):
                        upsert_stmt = text(
                            "INSERT INTO tokens (token, pool_name, status, quota, created_at, "
                            "last_used_at, use_count, fail_count, last_fail_at, "
                            "last_fail_reason, last_sync_at, tags, note, "
                            "last_asset_clear_at, data, data_hash, updated_at) "
                            "VALUES (:token, :pool_name, :status, :quota, :created_at, "
                            ":last_used_at, :use_count, :fail_count, :last_fail_at, "
                            ":last_fail_reason, :last_sync_at, :tags, :note, "
                            ":last_asset_clear_at, :data, :data_hash, :updated_at) "
                            "ON DUPLICATE KEY UPDATE "
                            "pool_name=VALUES(pool_name), "
                            "status=VALUES(status), "
                            "quota=VALUES(quota), "
                            "created_at=VALUES(created_at), "
                            "last_used_at=VALUES(last_used_at), "
                            "use_count=VALUES(use_count), "
                            "fail_count=VALUES(fail_count), "
                            "last_fail_at=VALUES(last_fail_at), "
                            "last_fail_reason=VALUES(last_fail_reason), "
                            "last_sync_at=VALUES(last_sync_at), "
                            "tags=VALUES(tags), "
                            "note=VALUES(note), "
                            "last_asset_clear_at=VALUES(last_asset_clear_at), "
                            "data=VALUES(data), "
                            "data_hash=VALUES(data_hash), "
                            "updated_at=VALUES(updated_at)"
                        )
                    elif self.dialect in ("postgres", "postgresql", "pgsql"):
                        upsert_stmt = text(
                            "INSERT INTO tokens (token, pool_name, status, quota, created_at, "
                            "last_used_at, use_count, fail_count, last_fail_at, "
                            "last_fail_reason, last_sync_at, tags, note, "
                            "last_asset_clear_at, data, data_hash, updated_at) "
                            "VALUES (:token, :pool_name, :status, :quota, :created_at, "
                            ":last_used_at, :use_count, :fail_count, :last_fail_at, "
                            ":last_fail_reason, :last_sync_at, :tags, :note, "
                            ":last_asset_clear_at, :data, :data_hash, :updated_at) "
                            "ON CONFLICT (token) DO UPDATE SET "
                            "pool_name=EXCLUDED.pool_name, "
                            "status=EXCLUDED.status, "
                            "quota=EXCLUDED.quota, "
                            "created_at=EXCLUDED.created_at, "
                            "last_used_at=EXCLUDED.last_used_at, "
                            "use_count=EXCLUDED.use_count, "
                            "fail_count=EXCLUDED.fail_count, "
                            "last_fail_at=EXCLUDED.last_fail_at, "
                            "last_fail_reason=EXCLUDED.last_fail_reason, "
                            "last_sync_at=EXCLUDED.last_sync_at, "
                            "tags=EXCLUDED.tags, "
                            "note=EXCLUDED.note, "
                            "last_asset_clear_at=EXCLUDED.last_asset_clear_at, "
                            "data=EXCLUDED.data, "
                            "data_hash=EXCLUDED.data_hash, "
                            "updated_at=EXCLUDED.updated_at"
                        )
                    else:
                        upsert_stmt = text(
                            "INSERT INTO tokens (token, pool_name, status, quota, created_at, "
                            "last_used_at, use_count, fail_count, last_fail_at, "
                            "last_fail_reason, last_sync_at, tags, note, "
                            "last_asset_clear_at, data, data_hash, updated_at) "
                            "VALUES (:token, :pool_name, :status, :quota, :created_at, "
                            ":last_used_at, :use_count, :fail_count, :last_fail_at, "
                            ":last_fail_reason, :last_sync_at, :tags, :note, "
                            ":last_asset_clear_at, :data, :data_hash, :updated_at)"
                        )
                    await session.execute(upsert_stmt, updates)

                if usage_updates:
                    if self.dialect in ("mysql", "mariadb"):
                        usage_stmt = text(
                            "INSERT INTO tokens (token, pool_name, status, quota, created_at, "
                            "last_used_at, use_count, fail_count, last_fail_at, "
                            "last_fail_reason, last_sync_at, tags, note, "
                            "last_asset_clear_at, data, data_hash, updated_at) "
                            "VALUES (:token, :pool_name, :status, :quota, :created_at, "
                            ":last_used_at, :use_count, :fail_count, :last_fail_at, "
                            ":last_fail_reason, :last_sync_at, :tags, :note, "
                            ":last_asset_clear_at, :data, :data_hash, :updated_at) "
                            "ON DUPLICATE KEY UPDATE "
                            "pool_name=VALUES(pool_name), "
                            "status=VALUES(status), "
                            "quota=VALUES(quota), "
                            "last_used_at=VALUES(last_used_at), "
                            "use_count=VALUES(use_count), "
                            "fail_count=VALUES(fail_count), "
                            "last_fail_at=VALUES(last_fail_at), "
                            "last_fail_reason=VALUES(last_fail_reason), "
                            "last_sync_at=VALUES(last_sync_at), "
                            "updated_at=VALUES(updated_at)"
                        )
                    elif self.dialect in ("postgres", "postgresql", "pgsql"):
                        usage_stmt = text(
                            "INSERT INTO tokens (token, pool_name, status, quota, created_at, "
                            "last_used_at, use_count, fail_count, last_fail_at, "
                            "last_fail_reason, last_sync_at, tags, note, "
                            "last_asset_clear_at, data, data_hash, updated_at) "
                            "VALUES (:token, :pool_name, :status, :quota, :created_at, "
                            ":last_used_at, :use_count, :fail_count, :last_fail_at, "
                            ":last_fail_reason, :last_sync_at, :tags, :note, "
                            ":last_asset_clear_at, :data, :data_hash, :updated_at) "
                            "ON CONFLICT (token) DO UPDATE SET "
                            "pool_name=EXCLUDED.pool_name, "
                            "status=EXCLUDED.status, "
                            "quota=EXCLUDED.quota, "
                            "last_used_at=EXCLUDED.last_used_at, "
                            "use_count=EXCLUDED.use_count, "
                            "fail_count=EXCLUDED.fail_count, "
                            "last_fail_at=EXCLUDED.last_fail_at, "
                            "last_fail_reason=EXCLUDED.last_fail_reason, "
                            "last_sync_at=EXCLUDED.last_sync_at, "
                            "updated_at=EXCLUDED.updated_at"
                        )
                    else:
                        usage_stmt = text(
                            "INSERT INTO tokens (token, pool_name, status, quota, created_at, "
                            "last_used_at, use_count, fail_count, last_fail_at, "
                            "last_fail_reason, last_sync_at, tags, note, "
                            "last_asset_clear_at, data, data_hash, updated_at) "
                            "VALUES (:token, :pool_name, :status, :quota, :created_at, "
                            ":last_used_at, :use_count, :fail_count, :last_fail_at, "
                            ":last_fail_reason, :last_sync_at, :tags, :note, "
                            ":last_asset_clear_at, :data, :data_hash, :updated_at)"
                        )
                    await session.execute(usage_stmt, usage_updates)

                await session.commit()
        except Exception as e:
            logger.error(f"SQLStorage: 增量保存 Token 失败: {e}")
            raise

    async def close(self):
        await self.engine.dispose()


class StorageFactory:
    """存储后端工厂"""

    _instance: Optional[BaseStorage] = None

    # SSL-related query parameters that async drivers (asyncpg, aiomysql)
    # cannot accept via the URL and must be passed as connect_args instead.
    _SQL_SSL_PARAM_KEYS = ("sslmode", "ssl-mode", "ssl")

    # Canonical postgres ssl modes (asyncpg accepts libpq-style mode strings).
    _PG_SSL_MODE_ALIASES: ClassVar[dict[str, str]] = {
        "disable": "disable",
        "disabled": "disable",
        "false": "disable",
        "0": "disable",
        "no": "disable",
        "off": "disable",
        "prefer": "prefer",
        "preferred": "prefer",
        "allow": "allow",
        "require": "require",
        "required": "require",
        "true": "require",
        "1": "require",
        "yes": "require",
        "on": "require",
        "verify-ca": "verify-ca",
        "verify_ca": "verify-ca",
        "verify-full": "verify-full",
        "verify_full": "verify-full",
        "verify-identity": "verify-full",
        "verify_identity": "verify-full",
    }

    # Canonical mysql ssl modes (aiomysql accepts SSLContext, not mode strings).
    _MY_SSL_MODE_ALIASES: ClassVar[dict[str, str]] = {
        "disable": "disabled",
        "disabled": "disabled",
        "false": "disabled",
        "0": "disabled",
        "no": "disabled",
        "off": "disabled",
        "prefer": "preferred",
        "preferred": "preferred",
        "allow": "preferred",
        "require": "required",
        "required": "required",
        "true": "required",
        "1": "required",
        "yes": "required",
        "on": "required",
        "verify-ca": "verify_ca",
        "verify_ca": "verify_ca",
        "verify-full": "verify_identity",
        "verify_full": "verify_identity",
        "verify-identity": "verify_identity",
        "verify_identity": "verify_identity",
    }

    @classmethod
    def _normalize_ssl_mode(cls, storage_type: str, mode: str) -> str:
        """Normalize SSL mode aliases for the target storage backend."""
        if not mode:
            raise ValueError("SSL mode cannot be empty")

        normalized = mode.strip().lower().replace(" ", "")
        if storage_type == "pgsql":
            canonical = cls._PG_SSL_MODE_ALIASES.get(normalized)
        elif storage_type == "mysql":
            canonical = cls._MY_SSL_MODE_ALIASES.get(normalized)
        else:
            canonical = None

        if not canonical:
            raise ValueError(
                f"Unsupported SSL mode '{mode}' for storage type '{storage_type}'"
            )
        return canonical

    @classmethod
    def _build_mysql_ssl_context(cls, mode: str):
        """Build SSLContext for aiomysql according to normalized mysql mode.

        Note: aiomysql enforces SSL whenever an SSLContext is provided — there
        is no "try SSL, fall back to plaintext" behaviour.  As a result the
        ``preferred`` mode is treated identically to ``required`` (encrypted,
        no cert verification).  Connections to MySQL servers that do not
        support SSL will fail rather than degrade gracefully.
        """
        import ssl as _ssl

        if mode == "disabled":
            return None

        ctx = _ssl.create_default_context()
        if mode in ("preferred", "required"):
            ctx.check_hostname = False
            ctx.verify_mode = _ssl.CERT_NONE
        elif mode == "verify_ca":
            # verify CA, but do not enforce hostname match.
            ctx.check_hostname = False
        # verify_identity keeps defaults: verify cert + hostname.
        return ctx

    @classmethod
    def _build_sql_connect_args(
        cls, storage_type: str, raw_ssl_mode: Optional[str]
    ) -> Optional[dict]:
        """Build SQLAlchemy connect_args for SQL SSL modes."""
        if not raw_ssl_mode:
            return None

        mode = cls._normalize_ssl_mode(storage_type, raw_ssl_mode)
        if storage_type == "pgsql":
            # asyncpg accepts libpq-style ssl mode strings via ssl=...
            return {"ssl": mode}
        if storage_type == "mysql":
            ctx = cls._build_mysql_ssl_context(mode)
            if ctx is None:
                return None
            return {"ssl": ctx}
        return None

    @classmethod
    def _normalize_sql_url(cls, storage_type: str, url: str) -> str:
        """Rewrite scheme prefix to the SQLAlchemy async dialect form."""
        if not url or "://" not in url:
            return url
        if storage_type == "mysql":
            if url.startswith("mysql://"):
                url = f"mysql+aiomysql://{url[len('mysql://') :]}"
            elif url.startswith("mariadb://"):
                # Use mysql+aiomysql for both MySQL and MariaDB endpoints.
                # The mariadb dialect enforces strict MariaDB server detection.
                url = f"mysql+aiomysql://{url[len('mariadb://') :]}"
            elif url.startswith("mariadb+aiomysql://"):
                url = f"mysql+aiomysql://{url[len('mariadb+aiomysql://') :]}"
        elif storage_type == "pgsql":
            if url.startswith("postgres://"):
                url = f"postgresql+asyncpg://{url[len('postgres://') :]}"
            elif url.startswith("postgresql://"):
                url = f"postgresql+asyncpg://{url[len('postgresql://') :]}"
            elif url.startswith("pgsql://"):
                url = f"postgresql+asyncpg://{url[len('pgsql://') :]}"
        return url

    @classmethod
    def _prepare_sql_url_and_connect_args(
        cls, storage_type: str, url: str
    ) -> tuple[str, Optional[dict]]:
        """Normalize SQL URL and build connect_args from SSL query params."""
        from urllib.parse import urlparse, parse_qsl, urlencode, urlunparse

        normalized_url = cls._normalize_sql_url(storage_type, url)
        if "://" not in normalized_url:
            return normalized_url, None

        parsed = urlparse(normalized_url)
        ssl_mode: Optional[str] = None
        filtered_query_items = []
        ssl_param_keys = {k.lower() for k in cls._SQL_SSL_PARAM_KEYS}
        for key, value in parse_qsl(parsed.query, keep_blank_values=True):
            if key.lower() in ssl_param_keys:
                if ssl_mode is None and value:
                    ssl_mode = value
                continue
            filtered_query_items.append((key, value))

        cleaned_url = urlunparse(
            parsed._replace(query=urlencode(filtered_query_items, doseq=True))
        )
        connect_args = cls._build_sql_connect_args(storage_type, ssl_mode)
        return cleaned_url, connect_args

    @classmethod
    def get_storage(cls) -> BaseStorage:
        """获取全局存储实例 (单例)"""
        if cls._instance:
            return cls._instance

        storage_type = os.getenv("SERVER_STORAGE_TYPE", "local").lower()
        storage_url = os.getenv("SERVER_STORAGE_URL", "")

        logger.info(f"StorageFactory: 初始化存储后端: {storage_type}")

        if storage_type == "redis":
            if not storage_url:
                raise ValueError("Redis 存储需要设置 SERVER_STORAGE_URL")
            cls._instance = RedisStorage(storage_url)

        elif storage_type in ("mysql", "pgsql"):
            if not storage_url:
                raise ValueError("SQL 存储需要设置 SERVER_STORAGE_URL")
            # Drivers reject SSL query params in URL. Normalize URL and pass
            # backend-specific SSL handling through connect_args.
            storage_url, connect_args = cls._prepare_sql_url_and_connect_args(
                storage_type, storage_url
            )
            cls._instance = SQLStorage(storage_url, connect_args=connect_args)

        else:
            cls._instance = LocalStorage()

        return cls._instance


def get_storage() -> BaseStorage:
    return StorageFactory.get_storage()
