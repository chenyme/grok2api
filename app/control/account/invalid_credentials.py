"""Shared handling for upstream invalid-credential failures."""

from typing import TYPE_CHECKING

from app.platform.errors import UpstreamError
from app.platform.logging.logger import logger
from app.platform.runtime.clock import now_ms

from .commands import AccountPatch
from .enums import AccountStatus, FeedbackKind

if TYPE_CHECKING:
    from .repository import AccountRepository


async def mark_account_invalid_credentials(
    repo: "AccountRepository",
    token: str,
    exc: BaseException,
    *,
    source: str,
) -> bool:
    """Mark *token* as invalid when *exc* matches Grok invalid credentials."""
    from app.dataplane.reverse.protocol.xai_usage import is_invalid_credentials_error

    if not is_invalid_credentials_error(exc):
        return False

    record = next(iter(await repo.get_accounts([token])), None)
    reason = "invalid_credentials"
    ts = now_ms()
    ext = record.ext if record is not None else {}

    await repo.patch_accounts(
        [
            AccountPatch(
                token=token,
                status=AccountStatus.EXPIRED,
                last_fail_at=ts,
                last_fail_reason=reason,
                state_reason=reason,
                ext_merge={
                    **ext,
                    "expired_at": ts,
                    "expired_reason": reason,
                },
            )
        ])
    logger.info(
        "account expired from {}: token={}... status={} upstream_status={}",
        source,
        token[:10],
        AccountStatus.EXPIRED,
        getattr(exc, "status", None) if isinstance(exc, UpstreamError) else None,
    )
    return True


def feedback_kind_for_error(exc: BaseException | None) -> FeedbackKind:
    """Map an upstream exception to the appropriate account feedback kind."""
    if exc is None:
        return FeedbackKind.SERVER_ERROR
    # Check for known blocked/invalid body markers first — these override
    # the generic status-code mapping so that e.g. a 403 with "blocked-user"
    # body is treated as an account-level credential failure, not a generic
    # FORBIDDEN that only lowers health.
    from app.dataplane.reverse.protocol.xai_usage import is_invalid_credentials_error

    if is_invalid_credentials_error(exc):
        return FeedbackKind.UNAUTHORIZED
    status = getattr(exc, "status", 0)
    if status == 429:
        return FeedbackKind.RATE_LIMITED
    if status == 402:
        # console.x.ai returns 402 when the account has exhausted its
        # prepaid web_search/credit balance. Treat it like a rate limit
        # so the account pool routes around this token until credits
        # refresh or the operator tops up the balance.
        return FeedbackKind.RATE_LIMITED
    if status == 401:
        return FeedbackKind.UNAUTHORIZED
    if status == 403:
        return FeedbackKind.FORBIDDEN
    return FeedbackKind.SERVER_ERROR


__all__ = ["mark_account_invalid_credentials", "feedback_kind_for_error"]
