"""xAI account credential storage + single-account selection and refresh.

xAI accounts are stored as ordinary ``AccountRecord`` rows with ``pool="xai"``
and an ``ext`` dict carrying the OAuth payload.  They are excluded from the
grok.com hot path (see ``app.dataplane.shared.enums.GROK_POOLS``).

Per current scope only a SINGLE xAI account is used; selection just returns the
first active xAI account.  Token refresh is lazy (before each request, with a
5-minute lead) and guarded by a per-token lock to avoid duplicate refreshes.
"""

import asyncio
from typing import TYPE_CHECKING

from app.platform.logging.logger import logger
from app.platform.runtime.clock import now_ms
from app.control.account.commands import AccountPatch, AccountUpsert, ListAccountsQuery
from app.control.account.enums import AccountStatus
from app.control.account.models import AccountRecord
from app.dataplane.proxy import get_proxy_runtime
from . import oauth

if TYPE_CHECKING:
    from app.control.account.repository import AccountRepository

XAI_POOL = "xai"
XAI_TAG = "xai"

# Per-token refresh locks (avoid concurrent refresh of the same account).
_refresh_locks: dict[str, asyncio.Lock] = {}


def _lock_for(token: str) -> asyncio.Lock:
    lock = _refresh_locks.get(token)
    if lock is None:
        lock = asyncio.Lock()
        _refresh_locks[token] = lock
    return lock


def _synthetic_token(email: str | None, sub: str | None) -> str:
    """Build the stable primary-key token for an xAI account.

    Prefers ``xai:<sub>`` (stable across refreshes); falls back to email, then
    a timestamp.  Never the access_token (which rotates on refresh).
    """
    if sub:
        return f"xai:{sub}"
    if email:
        return f"xai:{email}"
    return f"xai:{now_ms()}"


def build_ext_from_token_response(
    resp: dict,
    *,
    token_endpoint: str,
    base_url: str,
    email: str | None,
    sub: str | None,
    previous: dict | None = None,
) -> dict:
    """Build/merge the ``ext`` OAuth payload from a token-endpoint response."""
    prev = dict(previous or {})
    now = now_ms()
    expires_in = int(resp.get("expires_in") or 0)
    ext = {
        **prev,
        "provider": "xai",
        "access_token": resp.get("access_token") or prev.get("access_token", ""),
        "refresh_token": resp.get("refresh_token") or prev.get("refresh_token", ""),
        "id_token": resp.get("id_token") or prev.get("id_token", ""),
        "token_type": resp.get("token_type") or prev.get("token_type", "Bearer"),
        "expires_at": now + expires_in * 1000 if expires_in else prev.get("expires_at"),
        "base_url": base_url,
        "token_endpoint": token_endpoint,
        "last_refresh": now,
    }
    if email:
        ext["email"] = email
    if sub:
        ext["sub"] = sub
    return ext


async def upsert_xai_account(
    repo: "AccountRepository",
    *,
    ext: dict,
    email: str | None,
    sub: str | None,
) -> str:
    """Create or replace the xAI account; return its primary-key token."""
    token = _synthetic_token(email, sub)
    await repo.upsert_accounts(
        [AccountUpsert(token=token, pool=XAI_POOL, tags=[XAI_TAG], ext=ext)]
    )
    logger.info("xai account saved: token={} email={}", token, email or "-")
    return token


async def list_xai_accounts(repo: "AccountRepository") -> list[AccountRecord]:
    """Return all non-deleted xAI accounts."""
    page = await repo.list_accounts(
        ListAccountsQuery(pool=XAI_POOL, page=1, page_size=200)
    )
    return [r for r in page.items if not r.is_deleted()]


async def get_xai_account(repo: "AccountRepository") -> AccountRecord | None:
    """Return the active xAI account to use (single-account scope)."""
    for record in await list_xai_accounts(repo):
        if record.status == AccountStatus.ACTIVE:
            return record
    return None


def _needs_refresh(ext: dict) -> bool:
    expires_at = ext.get("expires_at")
    if not expires_at:
        return False  # unknown expiry — assume valid, let upstream 401 drive refresh
    return now_ms() >= int(expires_at) - oauth.REFRESH_LEAD_MS


async def ensure_fresh(
    repo: "AccountRepository", account: AccountRecord
) -> str:
    """Return a valid access token, refreshing it first if near/after expiry."""
    ext = dict(account.ext or {})
    access_token = ext.get("access_token", "")

    if not _needs_refresh(ext):
        return access_token

    refresh_token = ext.get("refresh_token", "")
    token_endpoint = ext.get("token_endpoint") or oauth.DEFAULT_TOKEN_ENDPOINT
    base_url = ext.get("base_url") or oauth.DEFAULT_API_BASE
    if not refresh_token:
        return access_token  # nothing to refresh with; let upstream reject

    async with _lock_for(account.token):
        # Re-read inside the lock: another coroutine may have refreshed already.
        records = await repo.get_accounts([account.token])
        cur_ext = dict(records[0].ext) if records else ext
        if not _needs_refresh(cur_ext):
            return cur_ext.get("access_token", access_token)

        proxy = await get_proxy_runtime()
        lease = await proxy.acquire()
        resp = await oauth.refresh_tokens(
            token_endpoint, cur_ext.get("refresh_token", refresh_token), lease=lease
        )
        email = cur_ext.get("email")
        sub = cur_ext.get("sub")
        new_ext = build_ext_from_token_response(
            resp,
            token_endpoint=token_endpoint,
            base_url=base_url,
            email=email,
            sub=sub,
            previous=cur_ext,
        )
        await repo.patch_accounts(
            [AccountPatch(token=account.token, ext_merge=new_ext)]
        )
        logger.info("xai token refreshed: token={}", account.token)
        return new_ext.get("access_token", access_token)


__all__ = [
    "XAI_POOL",
    "XAI_TAG",
    "build_ext_from_token_response",
    "upsert_xai_account",
    "list_xai_accounts",
    "get_xai_account",
    "ensure_fresh",
]
