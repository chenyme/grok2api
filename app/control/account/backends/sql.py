"""Shared SQLAlchemy-based backend for MySQL and PostgreSQL.

Both dialects share the same table schema and query logic;
only the DDL fragments and upsert syntax differ.
"""

import json
import os
import ssl
from threading import Lock
from typing import Any
from urllib.parse import parse_qsl, urlencode, urlparse, urlunparse

import asyncio

import sqlalchemy as sa
from sqlalchemy.ext.asyncio import AsyncEngine, async_sessionmaker, create_async_engine

from app.platform.runtime.clock import now_ms
from ..commands import AccountPatch, AccountUpsert, BulkReplacePoolCommand, ListAccountsQuery
from ..enums import AccountStatus
from ..models import (
    AccountChangeSet,
    AccountMutationResult,
    AccountPage,
    AccountRecord,
    RuntimeSnapshot,
)
from ..quota_defaults import default_quota_set

_TBL_ACCOUNTS = "accounts"
_TBL_META     = "account_meta"

metadata = sa.MetaData()

accounts_table = sa.Table(
    _TBL_ACCOUNTS,
    metadata,
    sa.Column("token",            sa.String(512), primary_key=True),
    sa.Column("pool",             sa.Text,    nullable=False, default="basic"),
    sa.Column("status",           sa.Text,    nullable=False, default="active"),
    sa.Column("created_at",       sa.BigInteger, nullable=False),
    sa.Column("updated_at",       sa.BigInteger, nullable=False),
    sa.Column("tags",             sa.Text,    nullable=False, default="[]"),
    sa.Column("quota_auto",       sa.Text,    nullable=False, default="{}"),
    sa.Column("quota_fast",       sa.Text,    nullable=False, default="{}"),
    sa.Column("quota_expert",     sa.Text,    nullable=False, default="{}"),
    sa.Column("quota_heavy",      sa.Text,    nullable=False, default="{}"),
    sa.Column("quota_grok_4_3",   sa.Text,    nullable=False, default="{}"),
    sa.Column("usage_use_count",  sa.Integer, nullable=False, default=0),
    sa.Column("usage_fail_count", sa.Integer, nullable=False, default=0),
    sa.Column("usage_sync_count", sa.Integer, nullable=False, default=0),
    sa.Column("last_use_at",      sa.BigInteger),
    sa.Column("last_fail_at",     sa.BigInteger),
    sa.Column("last_fail_reason", sa.Text),
    sa.Column("last_sync_at",     sa.BigInteger),
    sa.Column("last_clear_at",    sa.BigInteger),
    sa.Column("state_reason",     sa.Text),
    sa.Column("deleted_at",       sa.BigInteger),
    sa.Column("ext",              sa.Text,    nullable=False, default="{}"),
    sa.Column("revision",         sa.BigInteger, nullable=False, default=0),
)

meta_table = sa.Table(
    _TBL_META,
    metadata,
    sa.Column("key",   sa.String(128), primary_key=True),
    sa.Column("value", sa.Text, nullable=False),
)

_SQL_SSL_MODE_PARAM_KEYS = ("sslmode", "ssl-mode", "ssl")
_PG_SSL_CERT_PARAM_KEYS = ("sslrootcert", "sslcert", "sslkey")
_PG_SSL_UNSUPPORTED_PARAM_KEYS = (
    "sslcrl",
    "sslpassword",
    "sslnegotiation",
    "ssl_min_protocol_version",
    "ssl_max_protocol_version",
)
_PG_SSL_QUERY_PARAM_KEYS = (
    *_SQL_SSL_MODE_PARAM_KEYS,
    *_PG_SSL_CERT_PARAM_KEYS,
    *_PG_SSL_UNSUPPORTED_PARAM_KEYS,
)
_MYSQL_SSL_CERT_PARAM_KEYS = ("ssl-ca", "ssl-capath", "ssl-cert", "ssl-key")
_MYSQL_SSL_OPTIONAL_PARAM_KEYS = ("ssl-check-hostname", "ssl-cipher")
_MYSQL_SSL_QUERY_PARAM_KEYS = (
    *_SQL_SSL_MODE_PARAM_KEYS,
    *_MYSQL_SSL_CERT_PARAM_KEYS,
    *_MYSQL_SSL_OPTIONAL_PARAM_KEYS,
)
_SSL_BOOL_TRUE = {"1", "true", "yes", "on"}
_SSL_BOOL_FALSE = {"0", "false", "no", "off"}

_PG_SSL_MODE_ALIASES: dict[str, str] = {
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

_MYSQL_SSL_MODE_ALIASES: dict[str, str] = {
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

_ENGINE_CACHE_LOCK = Lock()
_ENGINE_CACHE: dict[tuple[str, str, str], AsyncEngine] = {}
_ENGINE_KEYS_BY_ID: dict[int, set[tuple[str, str, str]]] = {}


def _normalize_sql_url(dialect: str, url: str) -> str:
    """Rewrite SQL URLs to the async SQLAlchemy dialect form."""
    if not url or "://" not in url:
        return url
    if dialect == "mysql":
        if url.startswith("mysql://"):
            return f"mysql+aiomysql://{url[len('mysql://') :]}"
        if url.startswith("mariadb://"):
            return f"mysql+aiomysql://{url[len('mariadb://') :]}"
        if url.startswith("mariadb+aiomysql://"):
            return f"mysql+aiomysql://{url[len('mariadb+aiomysql://') :]}"
        return url
    if url.startswith("postgres://"):
        return f"postgresql+asyncpg://{url[len('postgres://') :]}"
    if url.startswith("postgresql://"):
        return f"postgresql+asyncpg://{url[len('postgresql://') :]}"
    if url.startswith("pgsql://"):
        return f"postgresql+asyncpg://{url[len('pgsql://') :]}"
    return url


def _get_env_int(name: str, default: int, *, minimum: int = 0) -> int:
    raw = os.getenv(name, "").strip()
    if not raw:
        return default
    try:
        return max(minimum, int(raw))
    except ValueError:
        return default


def _normalize_ssl_mode(dialect: str, raw_mode: str) -> str:
    if not raw_mode:
        raise ValueError("SSL mode cannot be empty")

    mode = raw_mode.strip().lower().replace(" ", "")
    if dialect == "mysql":
        canonical = _MYSQL_SSL_MODE_ALIASES.get(mode)
    else:
        canonical = _PG_SSL_MODE_ALIASES.get(mode)
    if not canonical:
        raise ValueError(f"Unsupported SSL mode {raw_mode!r} for SQL dialect {dialect!r}")
    return canonical


def _has_ssl_options(options: dict[str, str], keys: tuple[str, ...]) -> bool:
    return any(options.get(key) for key in keys)


def _parse_ssl_bool(name: str, raw_value: str | None) -> bool | None:
    if raw_value is None:
        return None

    value = raw_value.strip().lower()
    if not value:
        return None
    if value in _SSL_BOOL_TRUE:
        return True
    if value in _SSL_BOOL_FALSE:
        return False
    raise ValueError(f"Unsupported boolean value {raw_value!r} for SQL SSL option {name!r}")


def _extract_sql_ssl_options(
    dialect: str,
    url: str,
) -> tuple[str, dict[str, str]]:
    parsed = urlparse(url)
    ssl_query_keys = _PG_SSL_QUERY_PARAM_KEYS if dialect == "postgresql" else _MYSQL_SSL_QUERY_PARAM_KEYS
    ssl_options: dict[str, str] = {}
    filtered_query_items: list[tuple[str, str]] = []
    ssl_param_keys = {key.lower() for key in ssl_query_keys}
    for key, value in parse_qsl(parsed.query, keep_blank_values=True):
        key_lower = key.lower()
        if key_lower in ssl_param_keys:
            normalized_value = value.strip()
            if key_lower not in ssl_options:
                ssl_options[key_lower] = normalized_value
            continue
        filtered_query_items.append((key, value))

    cleaned_url = urlunparse(
        parsed._replace(query=urlencode(filtered_query_items, doseq=True))
    )
    return cleaned_url, ssl_options


def _resolve_ssl_mode(dialect: str, ssl_options: dict[str, str]) -> str | None:
    raw_ssl_mode = next(
        (ssl_options.get(key) for key in _SQL_SSL_MODE_PARAM_KEYS if ssl_options.get(key)),
        None,
    )
    if raw_ssl_mode:
        return _normalize_ssl_mode(dialect, raw_ssl_mode)

    if ssl_options:
        if dialect == "postgresql":
            raise ValueError("PostgreSQL SSL URL parameters require sslmode to be set explicitly")
        raise ValueError("MySQL SSL URL parameters require ssl-mode to be set explicitly")
    return None


def _validate_pg_ssl_options(mode: str | None, ssl_options: dict[str, str]) -> None:
    unsupported = [
        key for key in _PG_SSL_UNSUPPORTED_PARAM_KEYS
        if ssl_options.get(key)
    ]
    if unsupported:
        joined = ", ".join(sorted(unsupported))
        raise ValueError(f"Unsupported PostgreSQL SSL URL parameter(s): {joined}")
    if mode == "disable" and _has_ssl_options(ssl_options, _PG_SSL_CERT_PARAM_KEYS):
        raise ValueError("PostgreSQL SSL certificate parameters cannot be used with sslmode=disable")
    if mode in {"allow", "prefer"} and _has_ssl_options(ssl_options, _PG_SSL_CERT_PARAM_KEYS):
        raise ValueError("PostgreSQL sslmode=allow/prefer is not supported with certificate URL parameters")


def _build_pg_ssl_context(mode: str, ssl_options: dict[str, str]) -> ssl.SSLContext:
    sslrootcert = ssl_options.get("sslrootcert") or None
    sslcert = ssl_options.get("sslcert") or None
    sslkey = ssl_options.get("sslkey") or None

    ctx = ssl.create_default_context(cafile=sslrootcert)
    if mode == "require":
        ctx.check_hostname = False
        if not sslrootcert:
            ctx.verify_mode = ssl.CERT_NONE
    elif mode == "verify-ca":
        ctx.check_hostname = False
    else:
        ctx.check_hostname = True

    if sslcert:
        ctx.load_cert_chain(certfile=sslcert, keyfile=sslkey or None)
    elif sslkey:
        raise ValueError("PostgreSQL sslkey requires sslcert")
    return ctx


def _build_mysql_ssl_context(mode: str, ssl_options: dict[str, str]) -> ssl.SSLContext | None:
    if mode == "disabled":
        if _has_ssl_options(ssl_options, _MYSQL_SSL_CERT_PARAM_KEYS):
            raise ValueError("MySQL SSL certificate parameters cannot be used with ssl-mode=disabled")
        return None
    if mode == "preferred":
        raise ValueError("MySQL ssl-mode=allow/prefer is not supported by aiomysql")

    ssl_ca = ssl_options.get("ssl-ca") or None
    ssl_capath = ssl_options.get("ssl-capath") or None
    ssl_cert = ssl_options.get("ssl-cert") or None
    ssl_key = ssl_options.get("ssl-key") or None
    ssl_check_hostname = _parse_ssl_bool("ssl-check-hostname", ssl_options.get("ssl-check-hostname"))
    ssl_cipher = ssl_options.get("ssl-cipher") or None

    ctx = ssl.create_default_context(cafile=ssl_ca, capath=ssl_capath)
    if mode in ("preferred", "required"):
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
    elif mode == "verify_ca":
        ctx.check_hostname = False
    else:
        ctx.check_hostname = True

    if ssl_check_hostname is not None:
        if mode == "required" and ssl_check_hostname:
            raise ValueError("MySQL ssl-check-hostname=true requires ssl-mode=verify_identity")
        ctx.check_hostname = ssl_check_hostname
    if ssl_cipher:
        ctx.set_ciphers(ssl_cipher)
    if ssl_cert:
        ctx.load_cert_chain(certfile=ssl_cert, keyfile=ssl_key or None)
    elif ssl_key:
        raise ValueError("MySQL ssl-key requires ssl-cert")
    return ctx


def _build_sql_connect_args(
    dialect: str,
    ssl_options: dict[str, str],
) -> dict[str, Any] | None:
    mode = _resolve_ssl_mode(dialect, ssl_options)
    if not mode:
        return None

    if dialect == "mysql":
        ctx = _build_mysql_ssl_context(mode, ssl_options)
        return {"ssl": ctx} if ctx is not None else None

    _validate_pg_ssl_options(mode, ssl_options)
    if mode == "disable":
        return None
    # asyncpg does not accept ssl= as a plain string (e.g. "require").
    # Always build a proper ssl.SSLContext so the driver can use it directly.
    return {"ssl": _build_pg_ssl_context(mode, ssl_options)}


def _prepare_sql_url_and_connect_args(
    dialect: str,
    url: str,
) -> tuple[str, dict[str, Any] | None]:
    """Strip SSL query params from the URL and translate them to connect_args."""
    normalized_url = _normalize_sql_url(dialect, url)
    if "://" not in normalized_url:
        return normalized_url, None

    cleaned_url, ssl_options = _extract_sql_ssl_options(dialect, normalized_url)
    return cleaned_url, _build_sql_connect_args(dialect, ssl_options)


def _is_serverless() -> bool:
    """Detect common serverless environments (Vercel, AWS Lambda, etc.)."""
    return bool(
        os.getenv("VERCEL")
        or os.getenv("AWS_LAMBDA_FUNCTION_NAME")
        or os.getenv("FUNCTIONS_WORKER_RUNTIME")  # Azure Functions
    )


def _sql_engine_kwargs(connect_args: dict[str, Any] | None) -> dict[str, Any]:
    # In serverless environments each function instance is short-lived and may
    # run concurrently.  Keep pools small to avoid exhausting DB connections.
    serverless = _is_serverless()
    kwargs: dict[str, Any] = {
        "pool_size":    _get_env_int("ACCOUNT_SQL_POOL_SIZE",    1 if serverless else 5,  minimum=1),
        "max_overflow": _get_env_int("ACCOUNT_SQL_MAX_OVERFLOW", 2 if serverless else 10, minimum=0),
        "pool_timeout": _get_env_int("ACCOUNT_SQL_POOL_TIMEOUT", 30, minimum=1),
        "pool_recycle": _get_env_int("ACCOUNT_SQL_POOL_RECYCLE", 1800, minimum=0),
        "pool_pre_ping": True,
        "pool_use_lifo": True,
    }
    if connect_args:
        kwargs["connect_args"] = connect_args
    return kwargs


def _get_or_create_engine(
    cache_key: tuple[str, str, str],
    normalized_url: str,
    connect_args: dict[str, Any] | None,
) -> AsyncEngine:
    with _ENGINE_CACHE_LOCK:
        engine = _ENGINE_CACHE.get(cache_key)
        if engine is not None:
            return engine

        engine = create_async_engine(normalized_url, **_sql_engine_kwargs(connect_args))
        _ENGINE_CACHE[cache_key] = engine
        _ENGINE_KEYS_BY_ID.setdefault(id(engine), set()).add(cache_key)
        return engine


def _evict_cached_engine(engine: AsyncEngine) -> None:
    with _ENGINE_CACHE_LOCK:
        for key in _ENGINE_KEYS_BY_ID.pop(id(engine), set()):
            if _ENGINE_CACHE.get(key) is engine:
                _ENGINE_CACHE.pop(key, None)


def _row_to_record(row: Any) -> AccountRecord:
    d = dict(row._mapping)
    d["tags"]  = json.loads(d.get("tags")  or "[]")
    heavy_raw     = d.pop("quota_heavy",    "{}") or "{}"
    grok_4_3_raw  = d.pop("quota_grok_4_3", "{}") or "{}"
    heavy_dict    = json.loads(heavy_raw)
    grok_4_3_dict = json.loads(grok_4_3_raw)
    d["quota"] = {
        "auto":   json.loads(d.pop("quota_auto",   "{}") or "{}"),
        "fast":   json.loads(d.pop("quota_fast",   "{}") or "{}"),
        "expert": json.loads(d.pop("quota_expert", "{}") or "{}"),
        **({"heavy":    heavy_dict}    if heavy_dict    else {}),
        **({"grok_4_3": grok_4_3_dict} if grok_4_3_dict else {}),
    }
    d["ext"] = json.loads(d.get("ext") or "{}")
    return AccountRecord.model_validate(d)


class SqlAccountRepository:
    """Async SQLAlchemy-based repository for MySQL / PostgreSQL."""

    def __init__(
        self,
        engine: AsyncEngine,
        *,
        dialect: str = "mysql",
        dispose_engine: bool = True,
    ) -> None:
        self._engine       = engine
        self._dialect      = dialect   # "mysql" | "postgresql"
        self._session      = async_sessionmaker(engine, expire_on_commit=False)
        self._dispose_engine = dispose_engine
        self._initialized  = False
        self._init_lock    = asyncio.Lock()

    # ------------------------------------------------------------------
    # Revision helpers (run inside a transaction)
    # ------------------------------------------------------------------

    async def _bump_revision(self, conn: Any) -> int:
        await conn.execute(
            meta_table.update()
            .where(meta_table.c.key == "revision")
            .values(value=sa.cast(
                sa.cast(meta_table.c.value, sa.BigInteger) + 1, sa.Text
            ))
        )
        row = await conn.execute(
            sa.select(meta_table.c.value).where(meta_table.c.key == "revision")
        )
        return int(row.scalar())

    async def _get_revision(self, conn: Any) -> int:
        row = await conn.execute(
            sa.select(meta_table.c.value).where(meta_table.c.key == "revision")
        )
        v = row.scalar()
        return int(v) if v else 0

    # ------------------------------------------------------------------
    # Upsert — dialect-specific
    # ------------------------------------------------------------------

    def _build_upsert(self, row: dict[str, Any]):
        if self._dialect == "postgresql":
            from sqlalchemy.dialects.postgresql import insert
            stmt = insert(accounts_table).values(**row)
            # On conflict, update all columns except token and created_at.
            update_cols = {k: stmt.excluded[k] for k in row if k not in ("token", "created_at")}
            return stmt.on_conflict_do_update(index_elements=["token"], set_=update_cols)
        else:
            # MySQL
            from sqlalchemy.dialects.mysql import insert
            stmt = insert(accounts_table).values(**row)
            update_cols = {k: stmt.inserted[k] for k in row if k not in ("token", "created_at")}
            return stmt.on_duplicate_key_update(**update_cols)

    # ------------------------------------------------------------------
    # Public API
    # ------------------------------------------------------------------

    async def _ensure_initialized(self) -> None:
        """Idempotent: create tables + seed revision row if not already done.

        Safe to call on every request — short-circuits after first success so
        repeated calls cost only an asyncio lock check.  This allows the
        repository to self-initialise even when the ASGI lifespan is not
        executed (e.g. Vercel serverless cold-starts).
        """
        if self._initialized:
            return
        async with self._init_lock:
            if self._initialized:
                return
            await self._do_initialize()
            self._initialized = True

    async def _do_initialize(self) -> None:
        async with self._engine.begin() as conn:
            await conn.run_sync(metadata.create_all)
            await self._ensure_columns(conn)
            # Seed revision row.
            if self._dialect == "postgresql":
                from sqlalchemy.dialects.postgresql import insert as pg_insert
                await conn.execute(
                    pg_insert(meta_table)
                    .values(key="revision", value="0")
                    .on_conflict_do_nothing()
                )
            else:
                from sqlalchemy.dialects.mysql import insert as my_insert
                await conn.execute(
                    my_insert(meta_table)
                    .values(key="revision", value="0")
                    .on_duplicate_key_update(value="0")
                )

    async def _ensure_columns(self, conn: Any) -> None:
        """Idempotent ALTER TABLE migrations for columns added after the initial schema."""
        existing = await self._table_columns(conn, _TBL_ACCOUNTS)
        if "quota_grok_4_3" not in existing:
            if self._dialect == "mysql":
                # MySQL forbids DEFAULT values on TEXT/BLOB columns;
                # add as nullable, backfill, then promote to NOT NULL.
                await conn.exec_driver_sql(
                    f"ALTER TABLE {_TBL_ACCOUNTS} "
                    f"ADD COLUMN quota_grok_4_3 TEXT"
                )
                await conn.exec_driver_sql(
                    f"UPDATE {_TBL_ACCOUNTS} "
                    f"SET quota_grok_4_3 = '{{}}' "
                    f"WHERE quota_grok_4_3 IS NULL"
                )
                await conn.exec_driver_sql(
                    f"ALTER TABLE {_TBL_ACCOUNTS} "
                    f"MODIFY COLUMN quota_grok_4_3 TEXT NOT NULL"
                )
            else:
                await conn.exec_driver_sql(
                    f"ALTER TABLE {_TBL_ACCOUNTS} "
                    f"ADD COLUMN quota_grok_4_3 TEXT NOT NULL DEFAULT '{{}}'"
                )

    async def _table_columns(self, conn: Any, table: str) -> set[str]:
        if self._dialect == "postgresql":
            rows = await conn.execute(
                sa.text(
                    "SELECT column_name FROM information_schema.columns "
                    "WHERE table_name = :t"
                ),
                {"t": table},
            )
        else:
            rows = await conn.execute(
                sa.text(
                    "SELECT COLUMN_NAME FROM information_schema.columns "
                    "WHERE table_schema = DATABASE() AND table_name = :t"
                ),
                {"t": table},
            )
        return {str(r[0]).lower() for r in rows.fetchall()}

    async def initialize(self) -> None:
        await self._ensure_initialized()

    async def get_revision(self) -> int:
        await self._ensure_initialized()
        async with self._engine.connect() as conn:
            return await self._get_revision(conn)

    async def runtime_snapshot(self) -> RuntimeSnapshot:
        await self._ensure_initialized()
        async with self._engine.connect() as conn:
            rev = await self._get_revision(conn)
            rows = (await conn.execute(
                sa.select(accounts_table).where(accounts_table.c.deleted_at.is_(None))
            )).fetchall()
            return RuntimeSnapshot(revision=rev, items=[_row_to_record(r) for r in rows])

    async def scan_changes(
        self,
        since_revision: int,
        *,
        limit: int = 5000,
    ) -> AccountChangeSet:
        await self._ensure_initialized()
        async with self._engine.connect() as conn:
            rev = await self._get_revision(conn)
            rows = (await conn.execute(
                sa.select(accounts_table)
                .where(accounts_table.c.revision > since_revision)
                .order_by(accounts_table.c.revision)
                .limit(limit)
            )).fetchall()
            items: list[AccountRecord] = []
            deleted: list[str] = []
            for row in rows:
                r = _row_to_record(row)
                if r.is_deleted():
                    deleted.append(r.token)
                else:
                    items.append(r)
            return AccountChangeSet(
                revision=rev,
                items=items,
                deleted_tokens=deleted,
                has_more=len(rows) == limit,
            )

    async def upsert_accounts(
        self,
        items: list[AccountUpsert],
    ) -> AccountMutationResult:
        if not items:
            return AccountMutationResult()
        await self._ensure_initialized()
        async with self._engine.begin() as conn:
            rev = await self._bump_revision(conn)
            ts  = now_ms()
            count = 0
            for item in items:
                try:
                    token = AccountRecord.model_validate({"token": item.token, "pool": item.pool}).token
                except Exception:
                    continue
                pool = item.pool if item.pool in ("basic", "super", "heavy") else "basic"
                qs   = default_quota_set(pool)
                row  = {
                    "token":            token,
                    "pool":             pool,
                    "status":           "active",
                    "created_at":       ts,
                    "updated_at":       ts,
                    "deleted_at":       None,   # clear soft-delete on re-import
                    "tags":             json.dumps(item.tags),
                    "quota_auto":       json.dumps(qs.auto.to_dict()),
                    "quota_fast":       json.dumps(qs.fast.to_dict()),
                    "quota_expert":     json.dumps(qs.expert.to_dict()),
                    "quota_heavy":      json.dumps(qs.heavy.to_dict())    if qs.heavy    else "{}",
                    "quota_grok_4_3":   json.dumps(qs.grok_4_3.to_dict()) if qs.grok_4_3 else "{}",
                    "usage_use_count":  0,
                    "usage_fail_count": 0,
                    "usage_sync_count": 0,
                    "ext":              json.dumps(item.ext),
                    "revision":         rev,
                }
                await conn.execute(self._build_upsert(row))
                count += 1
            return AccountMutationResult(upserted=count, revision=rev)

    async def patch_accounts(
        self,
        patches: list[AccountPatch],
    ) -> AccountMutationResult:
        if not patches:
            return AccountMutationResult()
        await self._ensure_initialized()
        async with self._engine.begin() as conn:
            rev = await self._bump_revision(conn)
            ts  = now_ms()
            count = 0
            for patch in patches:
                row = (await conn.execute(
                    sa.select(accounts_table).where(accounts_table.c.token == patch.token)
                )).fetchone()
                if row is None:
                    continue
                record = _row_to_record(row)

                updates: dict[str, Any] = {"updated_at": ts, "revision": rev}
                if patch.pool is not None:
                    updates["pool"] = patch.pool
                if patch.status is not None:
                    updates["status"] = patch.status.value
                if patch.state_reason is not None:
                    updates["state_reason"] = patch.state_reason
                if patch.last_use_at is not None:
                    updates["last_use_at"] = patch.last_use_at
                if patch.last_fail_at is not None:
                    updates["last_fail_at"] = patch.last_fail_at
                if patch.last_fail_reason is not None:
                    updates["last_fail_reason"] = patch.last_fail_reason
                if patch.last_sync_at is not None:
                    updates["last_sync_at"] = patch.last_sync_at
                if patch.last_clear_at is not None:
                    updates["last_clear_at"] = patch.last_clear_at
                if patch.quota_auto is not None:
                    updates["quota_auto"] = json.dumps(patch.quota_auto)
                if patch.quota_fast is not None:
                    updates["quota_fast"] = json.dumps(patch.quota_fast)
                if patch.quota_expert is not None:
                    updates["quota_expert"] = json.dumps(patch.quota_expert)
                if patch.quota_heavy is not None:
                    updates["quota_heavy"] = json.dumps(patch.quota_heavy)
                if patch.quota_grok_4_3 is not None:
                    updates["quota_grok_4_3"] = json.dumps(patch.quota_grok_4_3)
                if patch.usage_use_delta is not None:
                    updates["usage_use_count"] = max(0, record.usage_use_count + patch.usage_use_delta)
                if patch.usage_fail_delta is not None:
                    updates["usage_fail_count"] = max(0, record.usage_fail_count + patch.usage_fail_delta)
                if patch.usage_sync_delta is not None:
                    updates["usage_sync_count"] = max(0, record.usage_sync_count + patch.usage_sync_delta)

                tags = list(record.tags)
                if patch.tags is not None:
                    tags = patch.tags
                if patch.add_tags:
                    for t in patch.add_tags:
                        if t not in tags:
                            tags.append(t)
                if patch.remove_tags:
                    tags = [t for t in tags if t not in patch.remove_tags]
                updates["tags"] = json.dumps(tags)

                ext = dict(record.ext)
                if patch.ext_merge:
                    ext.update(patch.ext_merge)
                if patch.clear_failures:
                    for k in ("cooldown_until", "cooldown_reason", "disabled_at",
                              "disabled_reason", "expired_at", "expired_reason",
                              "forbidden_strikes"):
                        ext.pop(k, None)
                    updates["status"]           = AccountStatus.ACTIVE.value
                    updates["usage_fail_count"] = 0
                    updates["last_fail_at"]     = None
                    updates["last_fail_reason"] = None
                    updates["state_reason"]     = None
                updates["ext"] = json.dumps(ext)

                await conn.execute(
                    accounts_table.update()
                    .where(accounts_table.c.token == patch.token)
                    .values(**updates)
                )
                count += 1
            return AccountMutationResult(patched=count, revision=rev)

    async def delete_accounts(
        self,
        tokens: list[str],
    ) -> AccountMutationResult:
        if not tokens:
            return AccountMutationResult()
        await self._ensure_initialized()
        async with self._engine.begin() as conn:
            rev = await self._bump_revision(conn)
            ts  = now_ms()
            result = await conn.execute(
                accounts_table.update()
                .where(
                    accounts_table.c.token.in_(tokens),
                    accounts_table.c.deleted_at.is_(None),
                )
                .values(deleted_at=ts, updated_at=ts, revision=rev)
            )
            return AccountMutationResult(deleted=result.rowcount, revision=rev)

    async def get_accounts(
        self,
        tokens: list[str],
    ) -> list[AccountRecord]:
        if not tokens:
            return []
        await self._ensure_initialized()
        async with self._engine.connect() as conn:
            rows = (await conn.execute(
                sa.select(accounts_table).where(accounts_table.c.token.in_(tokens))
            )).fetchall()
            return [_row_to_record(r) for r in rows]

    async def list_accounts(
        self,
        query: ListAccountsQuery,
    ) -> AccountPage:
        await self._ensure_initialized()
        async with self._engine.connect() as conn:
            stmt = sa.select(accounts_table)
            if not query.include_deleted:
                stmt = stmt.where(accounts_table.c.deleted_at.is_(None))
            if query.pool:
                stmt = stmt.where(accounts_table.c.pool == query.pool)
            if query.status:
                stmt = stmt.where(accounts_table.c.status == query.status.value)
            if query.exclude_statuses:
                stmt = stmt.where(accounts_table.c.status.notin_(
                    [s.value for s in query.exclude_statuses]
                ))

            total_row = (await conn.execute(
                sa.select(sa.func.count()).select_from(stmt.subquery())
            )).scalar()
            total = int(total_row or 0)

            sort_col = getattr(accounts_table.c, query.sort_by, accounts_table.c.updated_at)
            if query.sort_desc:
                stmt = stmt.order_by(sort_col.desc())
            else:
                stmt = stmt.order_by(sort_col.asc())
            offset = (query.page - 1) * query.page_size
            stmt = stmt.limit(query.page_size).offset(offset)

            rows = (await conn.execute(stmt)).fetchall()
            rev  = await self._get_revision(conn)
            return AccountPage(
                items=[_row_to_record(r) for r in rows],
                total=total,
                page=query.page,
                page_size=query.page_size,
                total_pages=max(1, (total + query.page_size - 1) // query.page_size),
                revision=rev,
            )

    async def replace_pool(
        self,
        command: BulkReplacePoolCommand,
    ) -> AccountMutationResult:
        await self._ensure_initialized()
        async with self._engine.begin() as conn:
            rev = await self._bump_revision(conn)
            ts  = now_ms()
            del_result = await conn.execute(
                accounts_table.update()
                .where(
                    accounts_table.c.pool == command.pool,
                    accounts_table.c.deleted_at.is_(None),
                )
                .values(deleted_at=ts, updated_at=ts, revision=rev)
            )
            deleted = del_result.rowcount

        upserted_result = await self.upsert_accounts(command.upserts)
        return AccountMutationResult(
            upserted=upserted_result.upserted,
            deleted=deleted,
            revision=upserted_result.revision,
        )

    async def get_stats(self) -> dict:
        """Return aggregated stats via SQL — no Python-side row iteration."""
        import time as _time
        t = accounts_table

        t0 = _time.monotonic()
        async with self._session() as session:
            # 1. Status counts
            stmt = (
                sa.select(t.c.status, sa.func.count())
                .where(t.c.deleted_at.is_(None))
                .group_by(t.c.status)
            )
            rows = (await session.execute(stmt)).all()
            status_counts = {r[0]: r[1] for r in rows}
            total = sum(status_counts.values())

            # 2. Pool counts
            stmt = (
                sa.select(t.c.pool, sa.func.count())
                .where(t.c.deleted_at.is_(None))
                .group_by(t.c.pool)
            )
            rows = (await session.execute(stmt)).all()
            pool_counts = {r[0]: r[1] for r in rows}

            # 3. Pool × status
            stmt = (
                sa.select(t.c.pool, t.c.status, sa.func.count())
                .where(t.c.deleted_at.is_(None))
                .group_by(t.c.pool, t.c.status)
            )
            rows = (await session.execute(stmt)).all()
            pool_status: dict[str, dict[str, int]] = {}
            for r in rows:
                pool_status.setdefault(r[0], {})[r[1]] = r[2]

            # 4. Usage sums
            stmt = sa.select(
                sa.func.coalesce(sa.func.sum(t.c.usage_use_count), 0),
                sa.func.coalesce(sa.func.sum(t.c.usage_fail_count), 0),
            ).where(t.c.deleted_at.is_(None))
            row = (await session.execute(stmt)).one()
            success, fail = int(row[0]), int(row[1])

            # 5. Quota sums (dialect-aware JSON extraction)
            quota_sums: dict[str, int] = {}
            for mode in ("auto", "fast", "expert", "heavy"):
                col = getattr(t.c, f"quota_{mode}")
                if self._dialect == "mysql":
                    remaining_expr = sa.cast(
                        sa.func.json_extract(col, "$.remaining"), sa.Integer
                    )
                else:  # postgresql
                    remaining_expr = sa.cast(
                        sa.func.json_extract_path_text(col, "remaining"), sa.Integer
                    )
                stmt = sa.select(
                    sa.func.coalesce(sa.func.sum(remaining_expr), 0)
                ).where(t.c.deleted_at.is_(None))
                r = (await session.execute(stmt)).scalar()
                quota_sums[mode] = int(r)

            # 6. NSFW
            stmt = sa.select(sa.func.count()).where(
                t.c.deleted_at.is_(None),
                t.c.tags.like('%"nsfw"%'),
            )
            nsfw_enabled = int((await session.execute(stmt)).scalar())
            nsfw_disabled = total - nsfw_enabled

        elapsed = _time.monotonic() - t0
        return {
            "total": total,
            "status_counts": status_counts,
            "pool_counts": pool_counts,
            "pool_status": pool_status,
            "usage": {"success": success, "fail": fail, "calls": success + fail},
            "quota_sums": quota_sums,
            "nsfw": {"enabled": nsfw_enabled, "disabled": nsfw_disabled},
            "elapsed_ms": round(elapsed * 1000, 1),
        }

    async def close(self) -> None:
        """Dispose the SQLAlchemy connection pool."""
        if self._dispose_engine:
            _evict_cached_engine(self._engine)
            await self._engine.dispose()


def _engine_cache_key(dialect: str, normalized_url: str, connect_args: dict[str, Any] | None) -> tuple[str, str, str]:
    """Build a stable cache key from the normalized URL and connect args."""
    args_key = str(sorted(connect_args.items(), key=lambda kv: kv[0])) if connect_args else ""
    return (dialect, normalized_url, args_key)


def create_mysql_engine(url: str) -> AsyncEngine:
    """Create an async SQLAlchemy engine for MySQL."""
    normalized_url, connect_args = _prepare_sql_url_and_connect_args("mysql", (url or "").strip())
    return _get_or_create_engine(_engine_cache_key("mysql", normalized_url, connect_args), normalized_url, connect_args)


def create_pgsql_engine(url: str) -> AsyncEngine:
    """Create an async SQLAlchemy engine for PostgreSQL."""
    normalized_url, connect_args = _prepare_sql_url_and_connect_args("postgresql", (url or "").strip())
    return _get_or_create_engine(_engine_cache_key("postgresql", normalized_url, connect_args), normalized_url, connect_args)


__all__ = ["SqlAccountRepository", "create_mysql_engine", "create_pgsql_engine"]
