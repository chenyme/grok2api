"""Account refresh service — mode-aware usage synchronisation."""

import asyncio
from dataclasses import dataclass
from typing import TYPE_CHECKING

from app.platform.errors import UpstreamError
from app.platform.config.snapshot import get_config
from app.platform.logging.logger import logger
from app.platform.runtime.clock import now_ms
from app.platform.runtime.batch import run_batch
from app.control.model.enums import ALL_MODES_FULL
from .enums import AccountStatus, QuotaSource
from .models import AccountRecord, QuotaWindow
from .quota_defaults import (
    default_quota_window,
    infer_pool,
    normalize_quota_window,
    supported_mode_ids,
    supports_mode,
)
from .state_machine import is_manageable

if TYPE_CHECKING:
    from .repository import AccountRepository


@dataclass
class RefreshResult:
    checked: int = 0
    refreshed: int = 0
    recovered: int = 0
    expired: int = 0
    disabled: int = 0
    rate_limited: int = 0
    failed: int = 0

    def merge(self, other: "RefreshResult") -> None:
        self.checked += other.checked
        self.refreshed += other.refreshed
        self.recovered += other.recovered
        self.expired += other.expired
        self.disabled += other.disabled
        self.rate_limited += other.rate_limited
        self.failed += other.failed


_MODE_KEYS = {
    0: "quota_auto",
    1: "quota_fast",
    2: "quota_expert",
    3: "quota_heavy",
    4: "quota_grok_4_3",
}

_ALL_MODE_IDS = tuple(int(mode) for mode in ALL_MODES_FULL)


class AccountRefreshService:
    """Fetches real quota data from the upstream usage API and persists it.

    Triggers:
      1. Import   — fetch all modes supported by the account's pool.
      2. Call     — fetch the called mode only (async, non-blocking).
      3. Schedule — refresh one pool per loop using that pool's supported modes.
    """

    def __init__(self, repository: "AccountRepository") -> None:
        self._repo = repository
        self._lock = asyncio.Lock()
        self._od_lock = asyncio.Lock()
        self._od_last = 0.0

    # ------------------------------------------------------------------
    # Usage API fetch (delegates to dataplane reverse protocol)
    # ------------------------------------------------------------------

    async def _fetch_all_quotas(
        self,
        token: str,
        pool: str,
        mode_ids: tuple[int, ...] | None = None,
    ) -> dict[int, QuotaWindow] | None:
        """Fetch quota windows for every mode supported by *pool*.

        Examples:
          - basic -> fast
          - super -> auto / fast / expert / grok_4_3
          - heavy -> auto / fast / expert / heavy / grok_4_3
        """
        try:
            from app.dataplane.reverse.protocol.xai_usage import fetch_all_quotas

            requested = mode_ids if mode_ids is not None else supported_mode_ids(pool)
            return await fetch_all_quotas(token, requested)
        except UpstreamError:
            raise
        except Exception as exc:
            logger.debug(
                "account quota fetch failed: token={}... pool={} error={}",
                token[:10],
                pool,
                exc,
            )
            return None

    async def _fetch_mode_quota(
        self, token: str, pool: str, mode_id: int
    ) -> QuotaWindow | None:
        """Fetch a single mode quota window."""
        if not supports_mode(pool, mode_id):
            logger.debug(
                "account mode quota fetch skipped: token={}... pool={} mode_id={} reason=unsupported_mode",
                token[:10],
                pool,
                mode_id,
            )
            return None
        try:
            from app.dataplane.reverse.protocol.xai_usage import fetch_mode_quota

            return await fetch_mode_quota(token, mode_id)
        except UpstreamError:
            raise
        except Exception as exc:
            logger.debug(
                "account mode quota fetch failed: token={}... pool={} mode_id={} error={}",
                token[:10],
                pool,
                mode_id,
                exc,
            )
            return None

    # ------------------------------------------------------------------
    # Core refresh logic
    # ------------------------------------------------------------------

    async def refresh_on_import(self, tokens: list[str]) -> RefreshResult:
        """Called after bulk import — sync real quotas for all accounts."""
        records = await self._repo.get_accounts(tokens)
        active = [r for r in records if is_manageable(r)]
        if not active:
            return RefreshResult(checked=len(records))

        concurrency = get_config("account.refresh.usage_concurrency", 50)
        results = await run_batch(
            active,
            lambda r: self._refresh_one(
                r, apply_fallback=True, probe_all_modes=True
            ),
            concurrency=concurrency,
        )
        agg = RefreshResult(checked=len(records))
        for r in results:
            agg.merge(r)
        return agg

    async def refresh_call_async(self, token: str, mode_id: int) -> None:
        """Fire-and-forget single-mode quota sync after a successful call."""
        record = (await self._repo.get_accounts([token]) or [None])[0]
        if record is None or record.is_deleted():
            return
        try:
            window = await self._fetch_mode_quota(token, record.pool, mode_id)
        except UpstreamError as exc:
            if await self._expire_invalid_credentials(record, exc):
                return
            raise
        await self._apply_single_mode(
            record, mode_id, window, is_use=True, use_at_ms=now_ms()
        )

    async def refresh_scheduled(self, pool: str | None = None) -> RefreshResult:
        """Periodic refresh — fetch real quotas for all (or one pool's) accounts.

        Args:
            pool: When set, only refreshes accounts belonging to that pool.
                  When ``None``, refreshes all pools.
        """
        snapshot = await self._repo.runtime_snapshot()
        records = [r for r in snapshot.items if is_manageable(r)]
        if pool is not None:
            records = [r for r in records if r.pool == pool]

        concurrency = get_config("account.refresh.usage_concurrency", 50)
        results = await run_batch(
            records,
            lambda r: self._refresh_one(r, apply_fallback=True),
            concurrency=concurrency,
        )
        agg = RefreshResult()
        for r in results:
            agg.merge(r)
        return agg

    async def refresh_on_demand(self) -> RefreshResult:
        """Throttled on-demand refresh triggered by request path."""
        min_interval = float(
            get_config("account.refresh.on_demand_min_interval_sec", 300)
        )
        import time

        now = time.monotonic()
        if now - self._od_last < min_interval:
            return RefreshResult()
        if self._od_lock.locked():
            return RefreshResult()
        async with self._od_lock:
            now = time.monotonic()
            if now - self._od_last < min_interval:
                return RefreshResult()
            result = await self.refresh_scheduled()
            self._od_last = time.monotonic()
            return result

    async def refresh_tokens(self, tokens: list[str]) -> RefreshResult:
        """Explicit refresh for a list of tokens (admin / manual trigger)."""
        records = [r for r in await self._repo.get_accounts(tokens) if is_manageable(r)]
        concurrency = get_config("account.refresh.usage_concurrency", 50)
        results = await run_batch(
            records,
            lambda r: self._refresh_one(r, probe_all_modes=True),
            concurrency=concurrency,
        )
        agg = RefreshResult()
        for r in results:
            agg.merge(r)
        return agg

    # ------------------------------------------------------------------
    # Per-account refresh
    # ------------------------------------------------------------------

    async def _refresh_one(
        self,
        record: AccountRecord,
        *,
        apply_fallback: bool = False,
        probe_all_modes: bool = False,
    ) -> RefreshResult:
        """Fetch all pool-supported modes from the usage API and persist them.

        apply_fallback=True  — used by scheduled/import paths: when API fails,
                               decrement REAL quotas or reset expired DEFAULT windows.
        apply_fallback=False — used by manual/on-demand paths: if API fails, return
                               failed=1 immediately without touching stored data.
        probe_all_modes=True — used by import/manual refresh paths to recover paid
                               accounts that were previously misclassified as basic.
        """
        if record.is_deleted():
            return RefreshResult()

        try:
            windows = await self._fetch_all_quotas(
                record.token,
                record.pool,
                _ALL_MODE_IDS if probe_all_modes else None,
            )
        except UpstreamError as exc:
            if await self._expire_invalid_credentials(record, exc):
                return RefreshResult(checked=1, expired=1, failed=0)
            raise

        # API call completely failed — no real data available.
        if windows is None:
            if not apply_fallback:
                return RefreshResult(checked=1, failed=1)
            # Scheduled/import path: apply conservative fallback.
            return await self._apply_fallback(record)

        # We got at least a response — infer the effective pool before
        # normalising quotas. Auto-detect/manual refresh can probe all modes
        # even when the stored pool is stale, so using record.pool here would
        # drop paid-only windows and keep the account stuck in the basic pool.
        effective_pool = infer_pool(
            windows, fallback=record.pool
        )  # type: ignore[arg-type]

        qs = record.quota_set()
        now = now_ms()
        patches: dict[str, dict] = {}
        refreshed = False

        for mode in ALL_MODES_FULL:
            mode_id = int(mode)
            if mode_id in windows:
                window = normalize_quota_window(
                    effective_pool, mode_id, windows[mode_id]
                )
                if window is None:
                    continue
                patches[_MODE_KEYS[mode_id]] = window.to_dict()
                refreshed = True
            elif apply_fallback:
                existing = qs.get(mode_id)
                if existing is None:
                    continue
                if existing.source == QuotaSource.REAL:
                    patches[_MODE_KEYS[mode_id]] = QuotaWindow(
                        remaining=max(0, existing.remaining - 1),
                        total=existing.total,
                        window_seconds=existing.window_seconds,
                        reset_at=existing.reset_at,
                        synced_at=existing.synced_at,
                        source=QuotaSource.ESTIMATED,
                    ).to_dict()
                elif existing.is_window_expired(now):
                    default = default_quota_window(effective_pool, mode_id)
                    if default is None:
                        continue
                    patches[_MODE_KEYS[mode_id]] = QuotaWindow(
                        remaining=default.total,
                        total=default.total,
                        window_seconds=default.window_seconds,
                        reset_at=now + default.window_seconds * 1000,
                        synced_at=now,
                        source=QuotaSource.DEFAULT,
                    ).to_dict()

        if not patches:
            return RefreshResult(checked=1, failed=0 if refreshed else 1)

        # Patch pool type only when live data gives a confident replacement.
        pool_patch = effective_pool if effective_pool != record.pool else None
        if pool_patch:
            logger.info(
                "account pool updated from live quota: token={}... previous_pool={} current_pool={}",
                record.token[:10],
                record.pool,
                effective_pool,
            )

        from .commands import AccountPatch

        await self._repo.patch_accounts(
            [
                AccountPatch(
                    token=record.token,
                    pool=pool_patch,
                    last_sync_at=now_ms() if refreshed else None,
                    usage_sync_delta=1 if refreshed else None,
                    **patches,  # type: ignore[arg-type]
                )
            ]
        )
        was_cooling = record.status == AccountStatus.COOLING
        return RefreshResult(
            checked=1,
            refreshed=1 if refreshed else 0,
            failed=0 if refreshed else 1,
            recovered=1 if (was_cooling and refreshed) else 0,
        )

    async def _apply_fallback(self, record: AccountRecord) -> RefreshResult:
        """Conservative fallback when API is unreachable (scheduled/import path only)."""
        qs = record.quota_set()
        now = now_ms()
        patches: dict[str, dict] = {}

        for mode in ALL_MODES_FULL:
            mode_id = int(mode)
            existing = qs.get(mode_id)
            if existing is None:
                continue
            if existing.source == QuotaSource.REAL:
                patches[_MODE_KEYS[mode_id]] = QuotaWindow(
                    remaining=max(0, existing.remaining - 1),
                    total=existing.total,
                    window_seconds=existing.window_seconds,
                    reset_at=existing.reset_at,
                    synced_at=existing.synced_at,
                    source=QuotaSource.ESTIMATED,
                ).to_dict()
            elif existing.is_window_expired(now):
                default = default_quota_window(record.pool, mode_id)
                if default is None:
                    continue
                patches[_MODE_KEYS[mode_id]] = QuotaWindow(
                    remaining=default.total,
                    total=default.total,
                    window_seconds=default.window_seconds,
                    reset_at=now + default.window_seconds * 1000,
                    synced_at=now,
                    source=QuotaSource.DEFAULT,
                ).to_dict()

        if patches:
            from .commands import AccountPatch

            await self._repo.patch_accounts(
                [AccountPatch(token=record.token, **patches)]
            )  # type: ignore[arg-type]

        return RefreshResult(checked=1, failed=1)

    async def record_failure_async(
        self, token: str, mode_id: int, exc: BaseException | None = None
    ) -> None:
        """Fire-and-forget: persist failure counter and timestamp after a failed call."""
        from .commands import AccountPatch

        try:
            if exc is not None:
                record = next(iter(await self._repo.get_accounts([token])), None)
                if record is not None and await self._expire_invalid_credentials(
                    record, exc
                ):
                    return
                if (
                    record is not None
                    and getattr(exc, "status", None) == 429
                    and mode_id in _MODE_KEYS
                ):
                    now = now_ms()
                    quota_patch: dict[str, dict] = {}
                    window = record.quota_set().get(mode_id)
                    if window is not None:
                        reset_at = (
                            window.reset_at
                            if window.reset_at is not None and window.reset_at > now
                            else now + max(window.window_seconds, 1) * 1000
                        )
                        quota_patch[_MODE_KEYS[mode_id]] = QuotaWindow(
                            remaining=0,
                            total=window.total,
                            window_seconds=window.window_seconds,
                            reset_at=reset_at,
                            synced_at=window.synced_at,
                            source=QuotaSource.ESTIMATED,
                        ).to_dict()
                    await self._repo.patch_accounts(
                        [
                            AccountPatch(
                                token=token,
                                usage_fail_delta=1,
                                last_fail_at=now,
                                last_fail_reason="rate_limited",
                                **quota_patch,
                            )
                        ]
                    )
                    return
            await self._repo.patch_accounts(
                [
                    AccountPatch(
                        token=token,
                        usage_fail_delta=1,
                        last_fail_at=now_ms(),
                    )
                ]
            )
        except Exception as exc:
            logger.debug(
                "account failure record update failed: token={}... error={}",
                token[:10],
                exc,
            )

    async def _apply_single_mode(
        self,
        record: AccountRecord,
        mode_id: int,
        window: QuotaWindow | None,
        *,
        is_use: bool = False,
        use_at_ms: int | None = None,
    ) -> None:
        qs = record.quota_set()
        mode_key = _MODE_KEYS.get(mode_id)
        if mode_key is None:
            logger.warning(
                "account single-mode sync skipped: token={}... pool={} mode_id={} reason=unknown_mode",
                record.token[:10],
                record.pool,
                mode_id,
            )
            return

        quota_patch: dict[str, dict] = {}
        if window is not None:
            normalized = normalize_quota_window(record.pool, mode_id, window)
            if normalized is None:
                logger.debug(
                    "account single-mode quota patch skipped: token={}... pool={} mode_id={} reason=unsupported_mode",
                    record.token[:10],
                    record.pool,
                    mode_id,
                )
                return
            quota_patch[mode_key] = normalized.to_dict()
        else:
            existing = qs.get(mode_id)
            if existing is not None:
                quota_patch[mode_key] = QuotaWindow(
                    remaining=max(0, existing.remaining - 1),
                    total=existing.total,
                    window_seconds=existing.window_seconds,
                    reset_at=existing.reset_at,
                    synced_at=existing.synced_at,
                    source=QuotaSource.ESTIMATED,
                ).to_dict()
            else:
                logger.debug(
                    "account single-mode quota patch skipped: token={}... pool={} mode_id={} reason=unsupported_mode",
                    record.token[:10],
                    record.pool,
                    mode_id,
                )

        from .commands import AccountPatch

        await self._repo.patch_accounts(
            [
                AccountPatch(
                    token=record.token,
                    last_sync_at=now_ms() if window is not None else None,
                    usage_sync_delta=1 if window is not None else None,
                    usage_use_delta=1 if is_use else None,
                    last_use_at=use_at_ms if is_use else None,
                    **quota_patch,  # type: ignore[arg-type]
                )
            ]
        )

    async def _expire_invalid_credentials(
        self, record: AccountRecord, exc: UpstreamError
    ) -> bool:
        from .invalid_credentials import mark_account_invalid_credentials

        return await mark_account_invalid_credentials(
            self._repo,
            record.token,
            exc,
            source="usage refresh",
        )


__all__ = ["AccountRefreshService", "RefreshResult"]
