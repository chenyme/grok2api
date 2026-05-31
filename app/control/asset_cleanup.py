"""Automatic Grok cloud asset cleanup — delete assets older than N days.

Runs as a background task at configurable intervals, iterating over all
manageable accounts and deleting stale assets via the Grok API.
"""

from __future__ import annotations

import asyncio
import time
from dataclasses import dataclass

from app.platform.config.snapshot import get_config
from app.platform.logging.logger import logger
from app.control.account.commands import ListAccountsQuery
from app.control.account.state_machine import is_manageable


@dataclass
class CleanupResult:
    scanned: int = 0       # accounts checked
    total_assets: int = 0  # assets found
    deleted: int = 0       # assets removed
    failed: int = 0        # accounts that errored
    skipped: int = 0       # accounts skipped (no manageable tokens)


def _max_age_seconds() -> float:
    """Return the max asset age in seconds. Default 3 days."""
    days = get_config("asset.cleanup.max_age_days", None)
    if days is not None:
        return float(days) * 86400.0
    # Fallback: check old config path.
    days = get_config("batch.asset_cleanup_max_age_days", None)
    return float(days or 3) * 86400.0


def _cleanup_interval_seconds() -> float:
    """Return the cleanup interval in seconds. Default 6 hours."""
    hours = get_config("asset.cleanup.interval_hours", None)
    if hours is not None:
        return float(hours) * 3600.0
    # Fallback: check old config path.
    hours = get_config("batch.asset_cleanup_interval_hours", None)
    return float(hours or 6) * 3600.0


def _cleanup_concurrency() -> int:
    return max(1, get_config("asset.cleanup.concurrency", 10))


def _enabled() -> bool:
    return get_config("asset.cleanup.enabled", True)


class AssetCleanupService:
    """Periodically scan all accounts and delete stale Grok cloud assets."""

    def __init__(self, repository_getter) -> None:
        self._get_repo = repository_getter
        self._task: asyncio.Task | None = None
        self._stop = asyncio.Event()

    def start(self) -> None:
        if not _enabled():
            logger.info("asset cleanup scheduler: disabled by config")
            return
        if self._task and not self._task.done():
            return
        self._stop.clear()
        self._task = asyncio.create_task(self._loop(), name="asset-cleanup")
        interval_h = _cleanup_interval_seconds() / 3600.0
        max_age_d = _max_age_seconds() / 86400.0
        logger.info(
            "asset cleanup scheduler started: interval_h={:.1f} max_age_d={:.0f} concurrency={}",
            interval_h, max_age_d, _cleanup_concurrency(),
        )

    def stop(self) -> None:
        self._stop.set()
        if self._task and not self._task.done():
            self._task.cancel()

    async def _loop(self) -> None:
        while not self._stop.is_set():
            interval = _cleanup_interval_seconds()
            try:
                await asyncio.wait_for(self._stop.wait(), timeout=float(interval))
                break
            except asyncio.TimeoutError:
                pass

            if self._stop.is_set():
                break

            try:
                result = await self._run_cleanup()
                logger.info(
                    "asset cleanup cycle completed: scanned={} assets_found={} deleted={} failed={}",
                    result.scanned, result.total_assets, result.deleted, result.failed,
                )
            except asyncio.CancelledError:
                raise
            except Exception as exc:
                logger.error(
                    "asset cleanup cycle failed: error_type={} error={}",
                    type(exc).__name__, exc,
                )

    async def _run_cleanup(self) -> CleanupResult:
        from app.dataplane.reverse.transport.assets import delete_asset, list_assets

        repo = self._get_repo()
        if repo is None:
            return CleanupResult()

        # Collect all manageable accounts.
        tokens = await self._list_manageable_tokens(repo)
        if not tokens:
            return CleanupResult()

        max_age_s = _max_age_seconds()
        now_s = time.time()
        cutoff = now_s - max_age_s
        concurrency = _cleanup_concurrency()
        sem = asyncio.Semaphore(concurrency)

        result = CleanupResult(scanned=len(tokens))

        async def _clean_one(token: str) -> tuple[int, int, bool]:
            """Return (found, deleted, failed)."""
            async with sem:
                try:
                    resp = await list_assets(token)
                except Exception:
                    return (0, 0, True)

                items = resp.get("assets", resp.get("items", []))
                found = len(items)
                deleted = 0

                for item in items:
                    created_raw = item.get("createdAt") or item.get("created_at") or ""
                    asset_id = item.get("id") or item.get("assetId") or ""
                    if not asset_id:
                        continue

                    # Parse creation time; skip if unparseable or within max age.
                    created_ts = self._parse_created_at(created_raw)
                    if created_ts is None or created_ts > cutoff:
                        continue

                    try:
                        await delete_asset(token, asset_id)
                        deleted += 1
                    except Exception:
                        pass

                return (found, deleted, False)

        tasks = [asyncio.create_task(_clean_one(t)) for t in tokens]
        for coro in asyncio.as_completed(tasks):
            try:
                found, deleted, failed = await coro
            except Exception:
                result.failed += 1
                continue
            result.total_assets += found
            result.deleted += deleted
            if failed:
                result.failed += 1

        return result

    async def _list_manageable_tokens(self, repo) -> list[str]:
        tokens: list[str] = []
        page_num = 1
        while True:
            page = await repo.list_accounts(
                ListAccountsQuery(page=page_num, page_size=2000)
            )
            for r in page.items:
                if is_manageable(r):
                    tokens.append(r.token)
            if page_num * 2000 >= page.total:
                break
            page_num += 1
        return tokens

    @staticmethod
    def _parse_created_at(raw: str) -> float | None:
        """Parse an ISO-8601 timestamp to a Unix timestamp (seconds).

        Returns None if parsing fails (asset will be skipped).
        """
        if not raw:
            return None
        raw = str(raw).strip()
        if not raw:
            return None

        # Try ISO-8601 with Z suffix.
        try:
            import datetime

            # Python >= 3.11: datetime.fromisoformat handles Z
            if raw.endswith("Z"):
                raw = raw[:-1] + "+00:00"
            dt = datetime.datetime.fromisoformat(raw)
            return dt.timestamp()
        except (ValueError, TypeError):
            pass

        # Try parsing as millisecond timestamp.
        try:
            ms = float(raw)
            if ms > 1e12:  # likely milliseconds
                return ms / 1000.0
            return ms
        except (ValueError, TypeError):
            pass

        return None


async def run_cleanup_once(repository_getter) -> CleanupResult:
    """Run one cleanup cycle immediately (for admin trigger / testing)."""
    svc = AssetCleanupService(repository_getter)
    return await svc._run_cleanup()


__all__ = ["AssetCleanupService", "CleanupResult", "run_cleanup_once"]
