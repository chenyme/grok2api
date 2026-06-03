"""Synchronise the AccountRuntimeTable from the control-plane repository.

Two modes:
  bootstrap  — full snapshot load at startup.
  incremental — revision-based change scan at runtime.
"""

from app.platform.logging.logger import logger
from app.platform.runtime.clock import ms_to_s
from app.control.account.models import AccountRecord
from app.control.account.quota_defaults import normalize_quota_set
from app.control.account.repository import AccountRepository
from app.control.account.state_machine import derive_status
from ..shared.enums import GROK_POOLS, POOL_STR_TO_ID, STATUS_STR_TO_ID, StatusId
from .table import AccountRuntimeTable, make_empty_table


def _is_grok_pool(record: AccountRecord) -> bool:
    """Return True if *record* belongs to the grok.com reverse-proxy machinery.

    Accounts in non-grok pools (e.g. ``"xai"`` for the official api.x.ai OAuth
    provider) are excluded from the hot-path runtime table so they are never
    selected for grok.com chat requests.
    """
    return record.pool in GROK_POOLS


def _record_to_slot_args(record: AccountRecord) -> dict:
    """Extract columnar values from a control-plane AccountRecord."""
    qs = normalize_quota_set(record.pool, record.quota_set())
    status_id = STATUS_STR_TO_ID.get(str(derive_status(record)), int(StatusId.ACTIVE))
    pool_id = POOL_STR_TO_ID.get(record.pool, 0)

    def _reset_s(window) -> int:
        if window.reset_at is None:
            return 0
        return int(ms_to_s(window.reset_at))

    def _total(window) -> int:
        return max(0, int(window.total)) if window is not None else 0

    def _window_s(window) -> int:
        return max(0, int(window.window_seconds)) if window is not None else 0

    heavy_w = qs.heavy
    grok_4_3_w = qs.grok_4_3
    # fmt: off
    return dict(
        pool_id         = pool_id,
        status_id       = status_id,
        quota_auto      = max(0, qs.auto.remaining),
        quota_fast      = max(0, qs.fast.remaining),
        quota_expert    = max(0, qs.expert.remaining),
        quota_heavy     = max(0, heavy_w.remaining)     if heavy_w    is not None else -1,
        quota_grok_4_3  = max(0, grok_4_3_w.remaining) if grok_4_3_w is not None else -1,
        total_auto      = _total(qs.auto),
        total_fast      = _total(qs.fast),
        total_expert    = _total(qs.expert),
        total_heavy     = _total(heavy_w),
        total_grok_4_3  = _total(grok_4_3_w),
        window_auto     = _window_s(qs.auto),
        window_fast     = _window_s(qs.fast),
        window_expert   = _window_s(qs.expert),
        window_heavy    = _window_s(heavy_w),
        window_grok_4_3 = _window_s(grok_4_3_w),
        reset_auto      = _reset_s(qs.auto),
        reset_fast      = _reset_s(qs.fast),
        reset_expert    = _reset_s(qs.expert),
        reset_heavy     = _reset_s(heavy_w)    if heavy_w    is not None else 0,
        reset_grok_4_3  = _reset_s(grok_4_3_w) if grok_4_3_w is not None else 0,
        health          = 1.0,
        last_use_s      = ms_to_s(record.last_use_at)  if record.last_use_at  else 0,
        last_fail_s     = ms_to_s(record.last_fail_at) if record.last_fail_at else 0,
        fail_count      = record.usage_fail_count,
        tags            = record.tags,
    )
    # fmt: on


async def bootstrap(repository: AccountRepository) -> AccountRuntimeTable:
    """Load all non-deleted accounts into a fresh AccountRuntimeTable."""
    snapshot = await repository.runtime_snapshot()
    table = make_empty_table()
    # Cache tags per token for tag_idx.
    _tags_by_token: dict[str, list[str]] = {}

    for record in snapshot.items:
        if record.is_deleted() or not _is_grok_pool(record):
            continue
        args = _record_to_slot_args(record)
        tags = args.pop("tags")
        _tags_by_token[record.token] = tags
        table._append_slot(record.token, **args, tags=tags)

    table.revision = snapshot.revision
    logger.info(
        "account runtime table bootstrapped: revision={} account_count={} pool_count={}",
        table.revision,
        table.size,
        len({k[0] for k in table.mode_available}),
    )
    return table


async def apply_changes(
    table: AccountRuntimeTable,
    repository: AccountRepository,
    *,
    batch_limit: int = 5000,
) -> bool:
    """Incrementally sync changes since ``table.revision``.

    Returns ``True`` if any changes were applied.
    """
    changed = False
    while True:
        changeset = await repository.scan_changes(table.revision, limit=batch_limit)

        for token in changeset.deleted_tokens:
            idx = table.idx_by_token.get(token)
            if idx is not None:
                table._remove_from_indexes(idx)
                # Mark as deleted in status column so it is skipped.
                table.status_by_idx[idx] = int(StatusId.DELETED)
                table.size = max(0, table.size - 1)
                changed = True

        for record in changeset.items:
            if record.is_deleted() or not _is_grok_pool(record):
                # Soft-deleted records, or records that moved to a non-grok pool
                # (e.g. "xai"), are removed from the hot-path table.
                idx = table.idx_by_token.get(record.token)
                if idx is not None:
                    table._remove_from_indexes(idx)
                    table.status_by_idx[idx] = int(StatusId.DELETED)
                    table.size = max(0, table.size - 1)
                    changed = True
                continue

            args = _record_to_slot_args(record)
            tags = args.pop("tags")
            existing = table.idx_by_token.get(record.token)

            if existing is not None:
                old_tags = []
                # Collect old tags from tag_idx (reverse lookup).
                for tag, bucket in list(table.tag_idx.items()):
                    if existing in bucket:
                        old_tags.append(tag)
                table._update_slot(existing, **args, old_tags=old_tags, new_tags=tags)
            else:
                table._append_slot(record.token, **args, tags=tags)

            changed = True

        if changeset.revision > table.revision:
            table.revision = changeset.revision

        if not changeset.has_more:
            break

    return changed


__all__ = ["bootstrap", "apply_changes"]
