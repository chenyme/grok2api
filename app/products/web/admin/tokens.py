"""Admin token CRUD — list, import, delete, replace pool.

Performance notes:
  - DI-injected repo (no try/except per call)
  - orjson direct output (bypasses stdlib json)
  - Quota dict: zero deserialization — reads r.quota directly
  - Import refresh: reuses app.state.refresh_service singleton
"""

import asyncio
import re
from typing import TYPE_CHECKING

import orjson
from fastapi import APIRouter, Body, Depends, Query
from fastapi.responses import Response
from pydantic import BaseModel, RootModel

from app.platform.errors import AppError, ErrorKind, ValidationError
from app.platform.logging.logger import logger
from app.platform.runtime.clock import now_ms
from app.control.account.commands import (
    AccountPatch,
    AccountUpsert,
    BulkReplacePoolCommand,
    ListAccountsQuery,
)
from app.control.account.enums import AccountStatus

if TYPE_CHECKING:
    from app.control.account.refresh import AccountRefreshService
    from app.control.account.repository import AccountRepository

from . import get_refresh_svc, get_repo

router = APIRouter(tags=["Admin - Tokens"])

# ---------------------------------------------------------------------------
# Token sanitisation
# ---------------------------------------------------------------------------

_TOKEN_TRANS = str.maketrans({
    "\u2010": "-", "\u2011": "-", "\u2012": "-",
    "\u2013": "-", "\u2014": "-", "\u2212": "-",
    "\u00a0": " ", "\u2007": " ", "\u202f": " ",
    "\u200b": "", "\u200c": "", "\u200d": "", "\ufeff": "",
})
_STRIP_RE = re.compile(r"\s+")


def _sanitize(value: str) -> str:
    tok = str(value or "").translate(_TOKEN_TRANS)
    tok = _STRIP_RE.sub("", tok)
    if tok.startswith("sso="):
        tok = tok[4:]
    return tok.encode("ascii", errors="ignore").decode("ascii")


def _mask(token: str) -> str:
    return f"{token[:8]}...{token[-8:]}" if len(token) > 20 else token


# ---------------------------------------------------------------------------
# Request models
# ---------------------------------------------------------------------------

class ReplacePoolRequest(BaseModel):
    pool: str
    tokens: list[str]
    tags: list[str] = []


class AddTokensRequest(BaseModel):
    tokens: list[str]
    pool: str = "basic"
    tags: list[str] = []


class EditTokenRequest(BaseModel):
    old_token: str
    token: str
    pool: str = "basic"


class ToggleTokenDisabledRequest(BaseModel):
    token: str
    disabled: bool


class ToggleTokensDisabledRequest(BaseModel):
    tokens: list[str]
    disabled: bool


class TokenImportItem(BaseModel):
    token: str
    tags: list[str] = []


class SaveTokensRequest(RootModel[dict[str, list[str | TokenImportItem]]]):
    """Bulk-save payload keyed by pool name."""


# ---------------------------------------------------------------------------
# Serialisation — zero-copy quota extraction
# ---------------------------------------------------------------------------

def _quota_brief(q: dict) -> dict:
    """Extract {auto, fast, expert, heavy} with only remaining/total from stored quota dict."""
    out = {}
    for mode in ("auto", "fast", "expert", "heavy"):
        v = q.get(mode)
        if isinstance(v, dict):
            out[mode] = {
                "remaining": int(v.get("remaining", 0) or 0),
                "total": int(v.get("total", 0) or 0),
            }
    return out


def _serialize_record(r) -> dict:
    return {
        "token":       r.token,
        "pool":        r.pool or "basic",
        "status":      r.status,
        "quota":       _quota_brief(r.quota) if isinstance(r.quota, dict) else {},
        "use_count":   r.usage_use_count or 0,
        "fail_count":  r.usage_fail_count or 0,
        "last_used_at": r.last_use_at,
        "tags":        r.tags or [],
    }


def _json(data) -> Response:
    """orjson fast-path response."""
    return Response(content=orjson.dumps(data), media_type="application/json")


# ---------------------------------------------------------------------------
# Endpoints
# ---------------------------------------------------------------------------

@router.get("/tokens")
async def list_tokens(
    page: int = Query(1, ge=1),
    page_size: int = Query(50, ge=1, le=2000),
    pool: str | None = Query(None),
    status: str | None = Query(None),
    exclude_statuses: str | None = Query(None, description="Comma-separated statuses to exclude"),
    repo: "AccountRepository" = Depends(get_repo),
):
    """Return paginated token list."""
    import time as _time
    _t0 = _time.monotonic()

    query = ListAccountsQuery(page=page, page_size=page_size)
    if pool:
        query.pool = pool
    if status:
        try:
            query.status = AccountStatus(status)
        except ValueError:
            raise ValidationError(f"Invalid status: {status}", param="status")
    if exclude_statuses:
        for s in exclude_statuses.split(","):
            s = s.strip()
            if s:
                try:
                    query.exclude_statuses.append(AccountStatus(s))
                except ValueError:
                    pass

    result = await repo.list_accounts(query)
    elapsed = _time.monotonic() - _t0
    logger.info(
        "admin list_tokens: page={} page_size={} total={} elapsed_ms={:.1f}",
        page, page_size, result.total, elapsed * 1000,
    )
    return _json({
        "tokens": [_serialize_record(r) for r in result.items],
        "total": result.total,
        "page": result.page,
        "page_size": result.page_size,
        "total_pages": result.total_pages,
    })


@router.get("/tokens/stats")
async def token_stats(repo: "AccountRepository" = Depends(get_repo)):
    """Return aggregated stats across all tokens (SQL-optimised)."""
    import time as _time
    _t0 = _time.monotonic()
    raw = await repo.get_stats()
    elapsed = _time.monotonic() - _t0
    logger.info("admin token_stats: total={} elapsed_ms={:.1f}", raw.get("total", 0), elapsed * 1000)
    sc = raw.get("status_counts", {})
    usage = raw.get("usage", {})
    nsfw = raw.get("nsfw", {})
    return _json({
        "stats": {
            "total":    raw.get("total", 0),
            "active":   sc.get("active", 0),
            "cooling":  sc.get("cooling", 0),
            "expired":  sc.get("expired", 0),
            "disabled": sc.get("disabled", 0),
            "calls":    usage.get("calls", 0),
            "success":  usage.get("success", 0),
            "fail":     usage.get("fail", 0),
        },
        "quota_sums":   raw.get("quota_sums", {}),
        "pool_counts":  raw.get("pool_counts", {}),
        "pool_status":  raw.get("pool_status", {}),
        "nsfw_counts":  {"enabled": nsfw.get("enabled", 0), "disabled": nsfw.get("disabled", 0)},
    })


@router.post("/tokens")
async def save_tokens(
    req: SaveTokensRequest,
    repo: "AccountRepository" = Depends(get_repo),
    refresh_svc: "AccountRefreshService" = Depends(get_refresh_svc),
):
    """Full pool replace — accepts {pool_name: [token_objects]} dict."""
    total_upserted = 0
    all_tokens: list[str] = []

    for pool_name, items in req.root.items():
        upserts = []
        for item in items:
            td = {"token": item} if isinstance(item, str) else item.model_dump()
            token_val = _sanitize(td.get("token", ""))
            if not token_val:
                continue
            upserts.append(AccountUpsert(token=token_val, pool=pool_name, tags=td.get("tags") or []))
        if upserts:
            await repo.replace_pool(BulkReplacePoolCommand(pool=pool_name, upserts=upserts))
            all_tokens.extend(u.token for u in upserts)
            total_upserted += len(upserts)

    logger.info("admin tokens saved across pools: saved_count={}", total_upserted)
    if all_tokens:
        asyncio.create_task(_refresh_imported(refresh_svc, all_tokens))
    return _json({"status": "success", "count": total_upserted})


@router.post("/tokens/add")
async def add_tokens(
    req: AddTokensRequest,
    repo: "AccountRepository" = Depends(get_repo),
    refresh_svc: "AccountRefreshService" = Depends(get_refresh_svc),
):
    requested_pool = (req.pool or "basic").strip().lower()
    sync_auto_detect = requested_pool == "auto"

    # Deduplicate and sanitize input
    cleaned: list[str] = []
    seen: set[str] = set()
    for token in req.tokens:
        tok = _sanitize(token)
        if tok and tok not in seen:
            seen.add(tok)
            cleaned.append(tok)
    if not cleaned:
        raise ValidationError("No valid tokens provided", param="tokens")

    # Only upsert tokens that are not already active — avoids overwriting quota/status.
    # Soft-deleted tokens are treated as non-existing so they can be restored.
    existing = {r.token for r in await repo.get_accounts(cleaned) if not r.is_deleted()}
    new_tokens = [t for t in cleaned if t not in existing]

    if not new_tokens:
        return _json({"status": "success", "count": 0, "skipped": len(cleaned)})

    upserts = [AccountUpsert(token=t, pool=requested_pool, tags=req.tags) for t in new_tokens]
    result = await repo.upsert_accounts(upserts)
    logger.info(
        "admin tokens added: pool={} added_count={} skipped_count={}",
        requested_pool,
        len(new_tokens),
        len(existing),
    )

    if sync_auto_detect:
        try:
            refresh_result = await refresh_svc.refresh_on_import(new_tokens)
            logger.info(
                "admin auto-detect quota sync completed: token_count={} refreshed={} failed={}",
                len(new_tokens), refresh_result.refreshed, refresh_result.failed,
            )
        except Exception as exc:
            logger.warning("admin auto-detect quota sync failed: token_count={} error={}", len(new_tokens), exc)
    else:
        asyncio.create_task(_refresh_imported(refresh_svc, new_tokens))

    return _json({
        "status": "success",
        "count": result.upserted or len(new_tokens),
        "skipped": len(existing),
        "synced": sync_auto_detect,
    })


@router.delete("/tokens")
async def delete_tokens(
    tokens: list[str] = Body(...),
    repo: "AccountRepository" = Depends(get_repo),
):
    cleaned = [t for t in (_sanitize(t) for t in tokens) if t]
    if not cleaned:
        raise ValidationError("No valid tokens provided", param="tokens")
    await repo.delete_accounts(cleaned)
    logger.info("admin tokens deleted: deleted_count={}", len(cleaned))
    return _json({"deleted": len(cleaned)})


@router.put("/tokens/edit")
async def edit_token(
    req: EditTokenRequest,
    repo: "AccountRepository" = Depends(get_repo),
):
    old_token = _sanitize(req.old_token)
    new_token = _sanitize(req.token)
    pool = (req.pool or "basic").strip().lower()

    if not old_token or not new_token:
        raise ValidationError("Token is required", param="token")

    records = await repo.get_accounts([old_token])
    if not records:
        raise AppError(
            "Account not found",
            kind=ErrorKind.VALIDATION,
            code="account_not_found",
            status=404,
        )
    record = records[0]

    if old_token != new_token:
        existing = await repo.get_accounts([new_token])
        if existing:
            raise AppError(
                "Target token already exists",
                kind=ErrorKind.VALIDATION,
                code="token_conflict",
                status=409,
            )

    await repo.upsert_accounts([AccountUpsert(
        token=new_token,
        pool=pool,
        tags=record.tags,
        ext=record.ext,
    )])

    if old_token == new_token:
        logger.info("admin token updated: token={} pool={}", _mask(new_token), pool)
        return _json({"status": "success", "token": new_token, "pool": pool})

    qs = record.quota_set()
    await repo.patch_accounts([AccountPatch(
        token=new_token,
        status=record.status,
        tags=record.tags,
        quota_auto=qs.auto.to_dict(),
        quota_fast=qs.fast.to_dict(),
        quota_expert=qs.expert.to_dict(),
        usage_use_delta=record.usage_use_count,
        usage_fail_delta=record.usage_fail_count,
        usage_sync_delta=record.usage_sync_count,
        last_use_at=record.last_use_at,
        last_fail_at=record.last_fail_at,
        last_fail_reason=record.last_fail_reason,
        last_sync_at=record.last_sync_at,
        last_clear_at=record.last_clear_at,
        state_reason=record.state_reason,
        ext_merge=record.ext,
    )])
    await repo.delete_accounts([old_token])

    logger.info("admin token replaced: previous_token={} current_token={} pool={}", _mask(old_token), _mask(new_token), pool)
    return _json({"status": "success", "token": new_token, "pool": pool})


@router.post("/tokens/disabled")
async def toggle_token_disabled(
    req: ToggleTokenDisabledRequest,
    repo: "AccountRepository" = Depends(get_repo),
):
    token = _sanitize(req.token)
    if not token:
        raise ValidationError("Token is required", param="token")

    records = await repo.get_accounts([token])
    if not records:
        raise AppError(
            "Account not found",
            kind=ErrorKind.VALIDATION,
            code="account_not_found",
            status=404,
        )
    record = records[0]

    if req.disabled:
        await repo.patch_accounts([AccountPatch(
            token=token,
            status=AccountStatus.DISABLED,
            state_reason="operator_disabled",
            ext_merge={
                **record.ext,
                "disabled_at": now_ms(),
                "disabled_reason": "operator_disabled",
            },
        )])
        logger.info("admin token disabled: token={}", _mask(token))
        return _json({"status": "success", "token": token, "disabled": True})

    await repo.patch_accounts([AccountPatch(
        token=token,
        status=AccountStatus.ACTIVE,
        clear_failures=True,
    )])
    logger.info("admin token restored: token={}", _mask(token))
    return _json({"status": "success", "token": token, "disabled": False})


@router.post("/tokens/disabled/batch")
async def toggle_tokens_disabled(
    req: ToggleTokensDisabledRequest,
    repo: "AccountRepository" = Depends(get_repo),
):
    cleaned: list[str] = []
    seen: set[str] = set()
    for raw in req.tokens:
        token = _sanitize(raw)
        if token and token not in seen:
            seen.add(token)
            cleaned.append(token)
    if not cleaned:
        raise ValidationError("No valid tokens provided", param="tokens")

    records = await repo.get_accounts(cleaned)
    if not records:
        raise AppError(
            "No matching accounts found",
            kind=ErrorKind.VALIDATION,
            code="account_not_found",
            status=404,
        )

    ts = now_ms()
    patches: list[AccountPatch] = []
    for record in records:
        if req.disabled:
            patches.append(AccountPatch(
                token=record.token,
                status=AccountStatus.DISABLED,
                state_reason="operator_disabled",
                ext_merge={
                    **record.ext,
                    "disabled_at": ts,
                    "disabled_reason": "operator_disabled",
                },
            ))
        else:
            patches.append(AccountPatch(
                token=record.token,
                status=AccountStatus.ACTIVE,
                clear_failures=True,
            ))

    result = await repo.patch_accounts(patches)
    logger.info(
        "admin tokens disabled batch updated: disabled={} requested_count={} patched_count={}",
        req.disabled,
        len(cleaned),
        result.patched,
    )
    return _json({
        "status": "success",
        "disabled": req.disabled,
        "summary": {
            "total": len(cleaned),
            "ok": result.patched,
            "fail": max(0, len(cleaned) - result.patched),
        },
    })


@router.put("/tokens/pool")
async def replace_pool(
    req: ReplacePoolRequest,
    repo: "AccountRepository" = Depends(get_repo),
    refresh_svc: "AccountRefreshService" = Depends(get_refresh_svc),
):
    cleaned = [t for t in (_sanitize(t) for t in req.tokens) if t]
    upserts = [AccountUpsert(token=t, pool=req.pool, tags=req.tags) for t in cleaned]
    await repo.replace_pool(BulkReplacePoolCommand(pool=req.pool, upserts=upserts))
    logger.info("admin pool replaced: pool={} token_count={}", req.pool, len(cleaned))
    if cleaned:
        asyncio.create_task(_refresh_imported(refresh_svc, cleaned))
    return _json({"pool": req.pool, "count": len(cleaned)})


# ---------------------------------------------------------------------------
# Fire-and-forget import refresh
# ---------------------------------------------------------------------------

async def _refresh_imported(svc: "AccountRefreshService", tokens: list[str]) -> None:
    try:
        await svc.refresh_on_import(tokens)
        logger.info("admin import quota sync completed: token_count={}", len(tokens))
    except Exception as exc:
        logger.warning("admin import quota sync failed: token_count={} error={}", len(tokens), exc)
