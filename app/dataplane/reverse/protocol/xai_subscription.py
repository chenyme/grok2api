"""XAI subscription / account-type protocol — fetch subscription tier and account ID.

Provides the official account type query by calling ``GET /rest/subscriptions``.
Returns the user's subscription tier (basic / super / heavy) and unique account
ID (``xaiUserId``) for deduplication purposes.
"""

from __future__ import annotations

from dataclasses import dataclass

from app.platform.errors import UpstreamError
from app.platform.logging.logger import logger


# ---------------------------------------------------------------------------
# Tier mapping — Grok subscription tiers → Grok2API pool names
# ---------------------------------------------------------------------------

# Known subscription tier strings returned by grok.com/rest/subscriptions.
_TIER_TO_POOL: dict[str, str] = {
    "SUBSCRIPTION_TIER_UNKNOWN":          "basic",
    "SUBSCRIPTION_TIER_FREE":             "basic",
    "SUBSCRIPTION_TIER_GROK_PRO":         "super",
    "SUBSCRIPTION_TIER_SUPER_GROK":       "super",
    "SUBSCRIPTION_TIER_SUPER_GROK_PRO":   "heavy",
    "SUBSCRIPTION_TIER_GROK_PRO_HEAVY":   "heavy",
    "SUBSCRIPTION_TIER_SUPER_GROK_LITE":  "super",
    "SUBSCRIPTION_TIER_GROK_TEAMS":       "super",
}

# Active subscription statuses.
_ACTIVE_STATUSES: frozenset[str] = frozenset({
    "SUBSCRIPTION_STATUS_ACTIVE",
    "SUBSCRIPTION_STATUS_TRIAL",
    "SUBSCRIPTION_STATUS_TRIALING",
})


# ---------------------------------------------------------------------------
# Result type
# ---------------------------------------------------------------------------


@dataclass(slots=True)
class SubscriptionInfo:
    """Parsed account-type information from the subscription API.

    ``xai_user_id`` — unique account identifier (UUID); ``None`` if unavailable.
    ``pool``        — Grok2API pool name (``"basic"`` / ``"super"`` / ``"heavy"``).
    ``tier``        — raw upstream tier string (e.g. ``"SUBSCRIPTION_TIER_GROK_PRO"``).
    ``is_active``   — whether at least one subscription is currently active.
    ``raw``         — the full parsed JSON dict for debugging / extension use.
    """

    xai_user_id: str | None
    pool: str
    tier: str
    is_active: bool
    raw: dict | None


def tier_to_pool(tier: str) -> str:
    """Map an upstream subscription tier string to a Grok2API pool name."""
    return _TIER_TO_POOL.get(tier, "basic")


def is_active_status(status: str) -> bool:
    """Return True if *status* represents an active subscription."""
    return status in _ACTIVE_STATUSES


# ---------------------------------------------------------------------------
# Response parser
# ---------------------------------------------------------------------------


def parse_subscription(body: dict) -> SubscriptionInfo:
    """Parse the ``/rest/subscriptions`` response body.

    Expected format::

        {
            "subscriptions": [
                {
                    "xaiUserId": "uuid-here",
                    "tier": "SUBSCRIPTION_TIER_GROK_PRO",
                    "status": "SUBSCRIPTION_STATUS_ACTIVE",
                    ...
                },
                ...
            ]
        }

    Returns a ``SubscriptionInfo`` populated from the most recent (last in
    list) subscription entry.  If the subscriptions array is empty, returns
    a ``"basic"`` pool with no account ID.
    """
    subs: list[dict] = body.get("subscriptions", []) if body else []

    if not subs:
        return SubscriptionInfo(
            xai_user_id=None,
            pool="basic",
            tier="",
            is_active=False,
            raw=body,
        )

    # Use the *last* subscription (typically most recent).
    last = subs[-1]

    xai_user_id = last.get("xaiUserId") or None
    tier        = last.get("tier", "")
    status      = last.get("status", "")

    pool = tier_to_pool(tier)

    # If the most recent subscription is not active but a later entry is, use
    # the best active tier we can find.  Also collect account ID from any entry.
    if not xai_user_id:
        for sub in subs:
            xai_user_id = sub.get("xaiUserId") or xai_user_id
            if xai_user_id:
                break

    is_active = False
    best_active_pool = pool
    for sub in subs:
        if is_active_status(sub.get("status", "")):
            is_active = True
            active_tier = sub.get("tier", "")
            active_pool = tier_to_pool(active_tier)
            if _pool_rank(active_pool) > _pool_rank(best_active_pool):
                best_active_pool = active_pool

    if is_active:
        pool = best_active_pool

    # If all subscriptions are inactive (expired/cancelled), the account
    # reverts to basic tier on the Grok side, but we keep the historical tier
    # for information and let the rate-limits API refine it.
    if not is_active and pool != "basic":
        logger.debug(
            "subscription inactive — may have reverted to basic: tier={} status={}",
            tier, status,
        )

    return SubscriptionInfo(
        xai_user_id=xai_user_id,
        pool=pool,
        tier=tier,
        is_active=is_active,
        raw=body,
    )


def _pool_rank(pool: str) -> int:
    """Return an ordinal rank for a pool name (higher = better)."""
    return {"basic": 0, "super": 1, "heavy": 2}.get(pool, 0)


# ---------------------------------------------------------------------------
# HTTP fetch
# ---------------------------------------------------------------------------


async def _do_fetch(token: str) -> dict:
    """GET the subscriptions endpoint and return parsed JSON body."""
    from app.dataplane.reverse.transport.http import get_json
    from app.dataplane.proxy import get_proxy_runtime
    from app.control.proxy.models import ProxyFeedback, ProxyFeedbackKind
    from app.dataplane.reverse.runtime.endpoint_table import SUBSCRIPTIONS

    proxy = await get_proxy_runtime()
    lease = await proxy.acquire()
    try:
        body = await get_json(
            SUBSCRIPTIONS,
            token,
            lease=lease,
            timeout_s=20.0,
        )
        await proxy.feedback(
            lease, ProxyFeedback(kind=ProxyFeedbackKind.SUCCESS, status_code=200)
        )
        return body
    except Exception as exc:
        status = getattr(exc, "status", None) or getattr(exc, "status_code", None)
        from app.dataplane.reverse.protocol.xai_usage import _proxy_feedback_kind_for_error
        kind = _proxy_feedback_kind_for_error(exc, status=status)
        await proxy.feedback(lease, ProxyFeedback(kind=kind, status_code=status))
        raise


async def fetch_subscription(token: str) -> SubscriptionInfo | None:
    """Fetch account-type / subscription information for *token*.

    Returns a ``SubscriptionInfo``, or ``None`` if the endpoint is unreachable
    (network timeout, 5xx, etc.).
    """
    import asyncio

    try:
        body = await asyncio.wait_for(_do_fetch(token), timeout=25.0)
    except asyncio.TimeoutError:
        logger.debug(
            "subscription fetch timed out: token={}...", token[:10]
        )
        return None
    except UpstreamError as exc:
        if getattr(exc, "status", None) in (401, 403):
            # Invalid token — let caller handle.
            raise
        logger.debug(
            "subscription fetch failed: token={}... status={}",
            token[:10], exc.status if hasattr(exc, "status") else "?",
        )
        return None
    except Exception as exc:
        logger.debug(
            "subscription fetch error: token={}... error={}",
            token[:10], exc,
        )
        return None

    return parse_subscription(body)


async def fetch_subscription_for_import(token: str) -> SubscriptionInfo:
    """Fetch subscription info during account import.

    Always returns a ``SubscriptionInfo`` — falls back to ``"basic"`` with
    no account ID when the API is unreachable.

    Raises ``UpstreamError`` for 401/403 (invalid credentials).
    """
    result = await fetch_subscription(token)
    if result is not None:
        return result

    # API unreachable — return a safe default.
    return SubscriptionInfo(
        xai_user_id=None,
        pool="basic",
        tier="",
        is_active=False,
        raw=None,
    )


__all__ = [
    "SubscriptionInfo",
    "parse_subscription",
    "fetch_subscription",
    "fetch_subscription_for_import",
    "tier_to_pool",
    "is_active_status",
]
