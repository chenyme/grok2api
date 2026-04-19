"""Shared account selection helpers for products-layer request handlers."""

from app.control.model.enums import ModeId
from app.control.model.spec import ModelSpec
from app.control.account.runtime import get_refresh_service
from app.platform.config.snapshot import get_config


def mode_candidates(spec: ModelSpec) -> tuple[int, ...]:
    """Return mode IDs to try for *spec* in priority order.

    Chat models using ``AUTO`` can optionally fall back to ``FAST`` and then
    ``EXPERT`` when the upstream ``auto`` quota window is exhausted but the
    account still has usable quota in the other chat windows.
    """
    primary = int(spec.mode_id)
    if (
        spec.is_chat()
        and spec.mode_id == ModeId.AUTO
        and get_config("features.auto_chat_mode_fallback", True)
    ):
        return (primary, int(ModeId.FAST), int(ModeId.EXPERT))
    return (primary,)


async def reserve_account(
    directory,
    spec: ModelSpec,
    *,
    exclude_tokens: list[str] | None = None,
    now_s_override: int | None = None,
):
    """Reserve an account and return ``(lease, selected_mode_id)``.

    Returns ``(None, original_mode_id)`` when no account is available.
    """
    original_mode_id = int(spec.mode_id)

    async def _try_reserve():
        for candidate_mode_id in mode_candidates(spec):
            lease = await directory.reserve(
                pool_candidates=spec.pool_candidates(),
                mode_id=candidate_mode_id,
                now_s_override=now_s_override,
                exclude_tokens=exclude_tokens,
            )
            if lease is not None:
                return lease, candidate_mode_id
        return None, original_mode_id

    lease, selected_mode_id = await _try_reserve()
    if lease is not None:
        return lease, selected_mode_id

    if get_config("account.refresh.on_empty_retry_enabled", True):
        refresh_svc = get_refresh_service()
        if refresh_svc is not None:
            await refresh_svc.refresh_on_demand()
            lease, selected_mode_id = await _try_reserve()
            if lease is not None:
                return lease, selected_mode_id

    return None, original_mode_id
