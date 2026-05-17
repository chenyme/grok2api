"""Account lifecycle state machine.

Applies feedback events to an AccountRecord, advancing its status
and updating quota / usage fields accordingly.
"""

from dataclasses import dataclass, field

from app.platform.runtime.clock import now_ms
from .enums import AccountStatus, FeedbackKind
from .models import AccountRecord, QuotaWindow
from .quota_defaults import default_quota_set

# ---------------------------------------------------------------------------
# Policy
# ---------------------------------------------------------------------------


@dataclass(slots=True)
class StatePolicy:
    """Configurable thresholds for automatic state transitions."""

    fail_threshold: int = 5  # consecutive failures → COOLING
    forbidden_strikes: int = 1  # 403 count → DISABLED
    default_cooling_ms: int = 15 * 60 * 1000  # 15 min


_DEFAULT_POLICY = StatePolicy()

# ---------------------------------------------------------------------------
# Feedback
# ---------------------------------------------------------------------------


@dataclass(slots=True)
class AccountFeedback:
    """Outcome of a single upstream request.

    ``mode_id``     — which quota window to update (0=auto, 1=fast, 2=expert).
    ``quota_window`` — real quota window from the usage API (optional).
    ``apply_usage`` — whether to increment usage_use_count on SUCCESS.
    """

    kind: FeedbackKind
    mode_id: int = 0
    at: int = field(default_factory=now_ms)
    status_code: int | None = None
    reason: str = ""
    quota_window: QuotaWindow | None = None  # real value from usage API
    retry_after_ms: int | None = None
    confirm_expired: bool = False
    apply_usage: bool = True

    @classmethod
    def from_status_code(
        cls,
        status_code: int,
        mode_id: int = 0,
        *,
        reason: str = "",
        retry_after_ms: int | None = None,
        confirm_expired: bool = False,
    ) -> "AccountFeedback":
        if status_code == 401:
            kind = FeedbackKind.UNAUTHORIZED
        elif status_code == 403:
            kind = FeedbackKind.FORBIDDEN
        elif status_code == 429 or status_code == 402:
            # 402 from console.x.ai = account credits exhausted; treat as a
            # rate-limited token so the pool routes around it.
            kind = FeedbackKind.RATE_LIMITED
        elif status_code >= 500:
            kind = FeedbackKind.SERVER_ERROR
        elif 200 <= status_code < 300:
            kind = FeedbackKind.SUCCESS
        else:
            kind = FeedbackKind.SERVER_ERROR
        return cls(
            kind=kind,
            mode_id=mode_id,
            status_code=status_code,
            reason=reason,
            retry_after_ms=retry_after_ms,
            confirm_expired=confirm_expired,
            apply_usage=False,
        )


# ---------------------------------------------------------------------------
# State helpers
# ---------------------------------------------------------------------------

_COOLDOWN_UNTIL_KEY = "cooldown_until"
_COOLDOWN_REASON_KEY = "cooldown_reason"
_DISABLED_AT_KEY = "disabled_at"
_DISABLED_REASON_KEY = "disabled_reason"
_EXPIRED_AT_KEY = "expired_at"
_EXPIRED_REASON_KEY = "expired_reason"
_FORBIDDEN_STRIKE_KEY = "forbidden_strikes"


def derive_status(record: AccountRecord, *, now: int | None = None) -> AccountStatus:
    """Compute the effective status, considering cooldown expiry."""
    if record.status != AccountStatus.COOLING:
        return record.status
    cooldown_until = record.ext.get(_COOLDOWN_UNTIL_KEY)
    if cooldown_until is None:
        return AccountStatus.COOLING
    ts = now if now is not None else now_ms()
    if ts >= int(cooldown_until):
        return AccountStatus.ACTIVE
    return AccountStatus.COOLING


def is_selectable(record: AccountRecord, mode_id: int, *, now: int | None = None) -> bool:
    """Return True if the account can be selected for *mode_id*."""
    if record.is_deleted():
        return False
    status = derive_status(record, now=now)
    if status != AccountStatus.ACTIVE:
        return False
    qs = record.quota_set()
    win = qs.get(mode_id)
    return win is not None and not win.is_exhausted()


def is_manageable(record: AccountRecord, *, now: int | None = None) -> bool:
    """Return True if the account should participate in maintenance flows."""
    if record.is_deleted():
        return False
    status = derive_status(record, now=now)
    return status in (AccountStatus.ACTIVE, AccountStatus.COOLING)


# ---------------------------------------------------------------------------
# State transition
# ---------------------------------------------------------------------------


def apply_feedback(
    record: AccountRecord,
    feedback: AccountFeedback,
    *,
    policy: StatePolicy = _DEFAULT_POLICY,
) -> AccountRecord:
    """Apply *feedback* to *record* and return an updated copy.

    The returned object is a **new** record; the original is not mutated.
    """
    ts = feedback.at
    ext = dict(record.ext)
    qs = record.quota_set()

    status = record.status
    last_fail_at = record.last_fail_at
    last_fail_reason = record.last_fail_reason
    last_use_at = record.last_use_at
    last_sync_at = record.last_sync_at
    use_count = record.usage_use_count
    fail_count = record.usage_fail_count
    sync_count = record.usage_sync_count
    state_reason = record.state_reason

    # Update quota window from real API data if provided.
    if feedback.quota_window is not None:
        qs.set(feedback.mode_id, feedback.quota_window)
        last_sync_at = ts
        sync_count += 1
    elif feedback.kind == FeedbackKind.SUCCESS:
        # Decrement locally when no real data is available.
        win = qs.get(feedback.mode_id)
        if win is not None:
            updated_win = QuotaWindow(
                remaining=max(0, win.remaining - 1),
                total=win.total,
                window_seconds=win.window_seconds,
                reset_at=win.reset_at,
                synced_at=win.synced_at,
                source=win.source,
            )
            qs.set(feedback.mode_id, updated_win)
    elif feedback.kind == FeedbackKind.RATE_LIMITED:
        # Mark quota as zero; set reset_at from retry_after if available.
        win = qs.get(feedback.mode_id)
        if win is not None:
            reset_at = (
                ts + feedback.retry_after_ms if feedback.retry_after_ms else (ts + win.window_seconds * 1000))
            qs.set(
                feedback.mode_id,
                QuotaWindow(
                    remaining=0,
                    total=win.total,
                    window_seconds=win.window_seconds,
                    reset_at=reset_at,
                    synced_at=win.synced_at,
                    source=win.source,
                ),
            )

    # Update usage counters.
    if feedback.kind == FeedbackKind.SUCCESS and feedback.apply_usage:
        use_count += 1
        last_use_at = ts
    elif feedback.kind not in (
            FeedbackKind.SUCCESS,
            FeedbackKind.RESTORE,
            FeedbackKind.DISABLE,
            FeedbackKind.DELETE,
    ):
        fail_count += 1
        last_fail_at = ts
        last_fail_reason = feedback.reason or str(feedback.status_code or "")

    # Status transitions.
    if feedback.kind == FeedbackKind.UNAUTHORIZED:
        if feedback.confirm_expired:
            status = AccountStatus.EXPIRED
            state_reason = feedback.reason or "token_expired"
            ext[_EXPIRED_AT_KEY] = ts
            ext[_EXPIRED_REASON_KEY] = state_reason
        else:
            # Unconfirmed 401 — only note failure; do not expire.
            pass

    elif feedback.kind == FeedbackKind.FORBIDDEN:
        strikes = int(ext.get(_FORBIDDEN_STRIKE_KEY, 0)) + 1
        ext[_FORBIDDEN_STRIKE_KEY] = strikes
        if strikes >= policy.forbidden_strikes:
            status = AccountStatus.DISABLED
            state_reason = feedback.reason or "forbidden"
            ext[_DISABLED_AT_KEY] = ts
            ext[_DISABLED_REASON_KEY] = state_reason

    elif feedback.kind == FeedbackKind.RATE_LIMITED:
        cooldown_ms = (feedback.retry_after_ms if feedback.retry_after_ms else policy.default_cooling_ms)
        status = AccountStatus.COOLING
        state_reason = feedback.reason or "rate_limited"
        ext[_COOLDOWN_UNTIL_KEY] = ts + cooldown_ms
        ext[_COOLDOWN_REASON_KEY] = state_reason

    elif feedback.kind == FeedbackKind.SUCCESS:
        if status == AccountStatus.COOLING:
            cooldown_until = ext.get(_COOLDOWN_UNTIL_KEY)
            if cooldown_until is not None and ts >= int(cooldown_until):
                status = AccountStatus.ACTIVE
                state_reason = None
                ext.pop(_COOLDOWN_UNTIL_KEY, None)
                ext.pop(_COOLDOWN_REASON_KEY, None)
                ext.pop(_FORBIDDEN_STRIKE_KEY, None)

    elif feedback.kind == FeedbackKind.DISABLE:
        status = AccountStatus.DISABLED
        state_reason = feedback.reason or "operator_disabled"
        ext[_DISABLED_AT_KEY] = ts
        ext[_DISABLED_REASON_KEY] = state_reason

    elif feedback.kind == FeedbackKind.DELETE:
        pass  # Caller sets deleted_at.

    elif feedback.kind == FeedbackKind.RESTORE:
        status = AccountStatus.ACTIVE
        state_reason = None
        ext.pop(_COOLDOWN_UNTIL_KEY, None)
        ext.pop(_COOLDOWN_REASON_KEY, None)
        ext.pop(_DISABLED_AT_KEY, None)
        ext.pop(_DISABLED_REASON_KEY, None)
        ext.pop(_EXPIRED_AT_KEY, None)
        ext.pop(_EXPIRED_REASON_KEY, None)
        ext.pop(_FORBIDDEN_STRIKE_KEY, None)
        # Reset quota to defaults.
        qs = default_quota_set(record.pool)

    return record.model_copy(
        update={
            "status": status,
            "quota": qs.to_dict(),
            "usage_use_count": use_count,
            "usage_fail_count": fail_count,
            "usage_sync_count": sync_count,
            "last_use_at": last_use_at,
            "last_fail_at": last_fail_at,
            "last_fail_reason": last_fail_reason,
            "last_sync_at": last_sync_at,
            "state_reason": state_reason,
            "ext": ext,
            "updated_at": ts,
        })


def clear_failures(record: AccountRecord) -> AccountRecord:
    """Reset failure counters and restore ACTIVE status."""
    ext = dict(record.ext)
    for k in (
            _COOLDOWN_UNTIL_KEY,
            _COOLDOWN_REASON_KEY,
            _DISABLED_AT_KEY,
            _DISABLED_REASON_KEY,
            _EXPIRED_AT_KEY,
            _EXPIRED_REASON_KEY,
            _FORBIDDEN_STRIKE_KEY,
    ):
        ext.pop(k, None)
    return record.model_copy(
        update={
            "status": AccountStatus.ACTIVE,
            "usage_fail_count": 0,
            "last_fail_at": None,
            "last_fail_reason": None,
            "state_reason": None,
            "ext": ext,
            "updated_at": now_ms(),
        })


__all__ = [
    "StatePolicy",
    "AccountFeedback",
    "derive_status",
    "is_selectable",
    "is_manageable",
    "apply_feedback",
    "clear_failures",
]
