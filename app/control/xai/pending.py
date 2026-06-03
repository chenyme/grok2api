"""Transient OAuth pending-state store, shared across workers.

The start→callback round-trip must carry the PKCE ``code_verifier`` and the
``state`` nonce.  Because the server may run multiple workers (the callback can
land on a different worker than ``start``), the pending state is persisted in
the account repository under a reserved non-grok pool so it is shared and
single-use.  The reserved pool is excluded from grok selection / quota machinery
by the ``GROK_POOLS`` guard.
"""

import secrets
from typing import TYPE_CHECKING

from app.platform.runtime.clock import now_ms
from app.control.account.commands import AccountUpsert, ListAccountsQuery

if TYPE_CHECKING:
    from app.control.account.repository import AccountRepository

PENDING_POOL = "_xai_oauth_pending"
_TTL_MS = 10 * 60 * 1000  # pending states expire after 10 minutes
_TOKEN_PREFIX = "oauthpending:"


def is_internal_record(record) -> bool:
    """Return True for transient OAuth rows that must not appear in admin lists."""
    token = getattr(record, "token", "") or ""
    pool = getattr(record, "pool", "") or ""
    return pool == PENDING_POOL or token.startswith(_TOKEN_PREFIX)


def new_state() -> str:
    """Return a fresh URL-safe, ASCII-only state/nonce value."""
    return secrets.token_urlsafe(32)


def _token_for(state: str) -> str:
    return f"{_TOKEN_PREFIX}{state}"


async def save(
    repo: "AccountRepository",
    *,
    state: str,
    code_verifier: str,
    code_challenge: str,
    token_endpoint: str,
    redirect_uri: str,
) -> None:
    """Persist a pending OAuth state (single-use, TTL-bounded)."""
    await repo.upsert_accounts(
        [
            AccountUpsert(
                token=_token_for(state),
                pool=PENDING_POOL,
                ext={
                    "code_verifier": code_verifier,
                    "code_challenge": code_challenge,
                    "token_endpoint": token_endpoint,
                    "redirect_uri": redirect_uri,
                    "created_at": now_ms(),
                },
            )
        ]
    )


async def exists(repo: "AccountRepository", state: str) -> bool:
    """Return whether a non-expired pending OAuth state is still stored."""
    if not state:
        return False
    records = await repo.get_accounts([_token_for(state)])
    record = records[0] if records else None
    if record is None or record.is_deleted():
        return False
    created = int((record.ext or {}).get("created_at") or 0)
    return not created or now_ms() - created <= _TTL_MS


async def consume(repo: "AccountRepository", state: str) -> dict | None:
    """Validate and atomically consume a pending state; return its ext or None."""
    if not state:
        return None
    token = _token_for(state)
    records = await repo.get_accounts([token])
    record = records[0] if records else None
    if record is None or record.is_deleted():
        return None
    # Single-use: delete immediately regardless of validity.
    await repo.delete_accounts([token])
    ext = dict(record.ext or {})
    created = int(ext.get("created_at") or 0)
    if created and now_ms() - created > _TTL_MS:
        return None
    return ext


async def cleanup_all(repo: "AccountRepository") -> int:
    """Remove every in-flight OAuth pending row (e.g. after a successful login)."""
    page = await repo.list_accounts(
        ListAccountsQuery(pool=PENDING_POOL, page=1, page_size=200)
    )
    tokens = [r.token for r in page.items if not r.is_deleted()]
    if tokens:
        await repo.delete_accounts(tokens)
    return len(tokens)


async def cleanup_expired(repo: "AccountRepository") -> int:
    """Best-effort sweep of expired pending states. Returns count removed."""
    page = await repo.list_accounts(
        ListAccountsQuery(pool=PENDING_POOL, page=1, page_size=200)
    )
    now = now_ms()
    stale = [
        r.token
        for r in page.items
        if not r.is_deleted()
        and now - int((r.ext or {}).get("created_at") or 0) > _TTL_MS
    ]
    if stale:
        await repo.delete_accounts(stale)
    return len(stale)


__all__ = [
    "PENDING_POOL",
    "new_state",
    "save",
    "exists",
    "consume",
    "is_internal_record",
    "cleanup_all",
    "cleanup_expired",
]
