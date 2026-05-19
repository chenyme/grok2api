"""Command / query objects for the account control plane."""

from typing import Any

from pydantic import BaseModel, Field

from .enums import AccountStatus


class AccountUpsert(BaseModel):
    """Create-or-replace command for a single account.

    Only ``token`` is required; all other fields use safe defaults.
    """

    token: str
    pool:  str = "basic"
    tags:  list[str] = Field(default_factory=list)
    ext:   dict[str, Any] = Field(default_factory=dict)


class AccountPatch(BaseModel):
    """Partial update — only supplied fields are written.

    Fields left as ``None`` are **not** modified in the backend.
    """

    token:            str
    pool:             str | None             = None  # update pool type when inferred
    status:           AccountStatus | None   = None
    tags:             list[str] | None       = None
    add_tags:         list[str] | None       = None
    remove_tags:      list[str] | None       = None
    # Per-mode quota patches (raw dicts matching QuotaWindow.to_dict()).
    quota_auto:       dict[str, Any] | None  = None
    quota_fast:       dict[str, Any] | None  = None
    quota_expert:     dict[str, Any] | None  = None
    quota_heavy:      dict[str, Any] | None  = None
    quota_grok_4_3:   dict[str, Any] | None  = None
    # Usage counters — delta values (added to existing counts).
    usage_use_delta:  int | None             = None
    usage_fail_delta: int | None             = None
    usage_sync_delta: int | None             = None
    # Timestamp overrides (set absolute values).
    last_use_at:      int | None             = None
    last_fail_at:     int | None             = None
    last_fail_reason: str | None             = None
    last_sync_at:     int | None             = None
    last_clear_at:    int | None             = None
    state_reason:     str | None             = None
    ext_merge:        dict[str, Any] | None  = None
    clear_failures:   bool                   = False


class ListAccountsQuery(BaseModel):
    """Query parameters for paginated account listing."""

    page:            int            = Field(default=1, ge=1)
    page_size:       int            = Field(default=50, ge=1, le=2000)
    pool:            str | None     = None
    status:          AccountStatus | None = None
    exclude_statuses: list[AccountStatus] = Field(default_factory=list)
    tags:            list[str]      = Field(default_factory=list)
    include_deleted: bool           = False
    sort_by:         str            = "updated_at"   # field name
    sort_desc:       bool           = True


class BulkReplacePoolCommand(BaseModel):
    """Replace all accounts in a pool atomically."""

    pool:    str
    upserts: list[AccountUpsert] = Field(default_factory=list)


__all__ = [
    "AccountUpsert",
    "AccountPatch",
    "ListAccountsQuery",
    "BulkReplacePoolCommand",
]
