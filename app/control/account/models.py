"""Control-plane account models — persistent record shape."""

from dataclasses import dataclass
from typing import Any

from pydantic import BaseModel, Field, field_validator

from app.platform.runtime.clock import now_ms
from .enums import AccountStatus, QuotaSource


# ---------------------------------------------------------------------------
# QuotaWindow
# ---------------------------------------------------------------------------


@dataclass(slots=True)
class QuotaWindow:
    """Rate-limit window for one mode (auto / fast / expert).

    ``remaining``      — queries remaining in the current window.
    ``total``          — total queries per window.
    ``window_seconds`` — window duration in seconds.
    ``reset_at``       — ms timestamp when the window resets; None = unknown.
    ``synced_at``      — ms timestamp of last upstream sync; None = never.
    ``source``         — reliability of this value.
    """

    remaining: int
    total: int
    window_seconds: int
    reset_at: int | None
    synced_at: int | None
    source: QuotaSource

    def is_exhausted(self) -> bool:
        return self.remaining <= 0

    def is_window_expired(self, now: int) -> bool:
        """Return True if the reset timestamp has passed."""
        return self.reset_at is not None and now >= self.reset_at

    def to_dict(self) -> dict[str, Any]:
        return {
            "remaining": self.remaining,
            "total": self.total,
            "window_seconds": self.window_seconds,
            "reset_at": self.reset_at,
            "synced_at": self.synced_at,
            "source": int(self.source),
        }

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> "QuotaWindow":
        return cls(
            remaining=int(d.get("remaining", 0)),
            total=int(d.get("total", 0)),
            window_seconds=int(d.get("window_seconds", 0)),
            reset_at=d.get("reset_at"),
            synced_at=d.get("synced_at"),
            source=QuotaSource(int(d.get("source", 0))),
        )


# ---------------------------------------------------------------------------
# AccountQuotaSet
# ---------------------------------------------------------------------------


@dataclass(slots=True)
class AccountQuotaSet:
    """Quota set — one window per mode (auto / fast / expert / heavy / grok_4_3).

    ``heavy``    is ``None`` for basic and super accounts.
    ``grok_4_3`` is available on basic/super/heavy accounts.
    """

    auto: QuotaWindow
    fast: QuotaWindow
    expert: QuotaWindow
    heavy: QuotaWindow | None = None  # heavy-pool accounts only
    grok_4_3: QuotaWindow | None = None

    def get(self, mode_id: int) -> QuotaWindow | None:
        """Return the quota window for *mode_id* (0=auto, 1=fast, 2=expert, 3=heavy, 4=grok_4_3)."""
        if mode_id == 0:
            return self.auto
        if mode_id == 1:
            return self.fast
        if mode_id == 2:
            return self.expert
        if mode_id == 3:
            return self.heavy
        if mode_id == 4:
            return self.grok_4_3
        return None

    def set(self, mode_id: int, window: QuotaWindow) -> None:
        """Replace the quota window for *mode_id*."""
        if mode_id == 0:
            self.auto = window
        elif mode_id == 1:
            self.fast = window
        elif mode_id == 2:
            self.expert = window
        elif mode_id == 3:
            self.heavy = window
        else:
            self.grok_4_3 = window  # mode_id == 4

    def to_dict(self) -> dict[str, dict[str, Any]]:
        d: dict[str, dict[str, Any]] = {
            "auto": self.auto.to_dict(),
            "fast": self.fast.to_dict(),
            "expert": self.expert.to_dict(),
        }
        if self.heavy is not None:
            d["heavy"] = self.heavy.to_dict()
        if self.grok_4_3 is not None:
            d["grok_4_3"] = self.grok_4_3.to_dict()
        return d

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> "AccountQuotaSet":
        heavy_d = d.get("heavy")
        grok_4_3_d = d.get("grok_4_3")
        return cls(
            auto=QuotaWindow.from_dict(d.get("auto", {})),
            fast=QuotaWindow.from_dict(d.get("fast", {})),
            expert=QuotaWindow.from_dict(d.get("expert", {})),
            heavy=QuotaWindow.from_dict(heavy_d) if heavy_d else None,
            grok_4_3=QuotaWindow.from_dict(grok_4_3_d) if grok_4_3_d else None,
        )


# ---------------------------------------------------------------------------
# AccountUsageStats
# ---------------------------------------------------------------------------


@dataclass(slots=True)
class AccountUsageStats:
    """Cumulative usage counters for an account."""

    use_count: int = 0  # Successful calls (SUCCESS feedback)
    fail_count: int = 0  # Failed calls (non-success feedback)
    sync_count: int = 0  # Usage API sync calls

    def to_dict(self) -> dict[str, int]:
        return {
            "use_count": self.use_count,
            "fail_count": self.fail_count,
            "sync_count": self.sync_count,
        }

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> "AccountUsageStats":
        return cls(
            use_count=int(d.get("use_count", 0)),
            fail_count=int(d.get("fail_count", 0)),
            sync_count=int(d.get("sync_count", 0)),
        )


# ---------------------------------------------------------------------------
# AccountRecord  (Pydantic — control plane only)
# ---------------------------------------------------------------------------


class AccountRecord(BaseModel):
    """Persistent account record — single source of truth for all backends.

    ``pool``  values: ``"basic"`` | ``"super"`` | ``"heavy"``
    All timestamps are milliseconds since epoch.
    ``ext`` carries non-core extension data only.
    """

    token: str
    pool: str = "basic"
    status: AccountStatus = AccountStatus.ACTIVE
    created_at: int = Field(default_factory=now_ms)
    updated_at: int = Field(default_factory=now_ms)
    tags: list[str] = Field(default_factory=list)
    # Serialised form of AccountQuotaSet.  Stored as dict for Pydantic
    # round-tripping; use .quota_set() to deserialise, .with_quota_set()
    # to produce an updated copy.
    quota: dict[str, Any] = Field(default_factory=dict)
    usage_use_count: int = 0
    usage_fail_count: int = 0
    usage_sync_count: int = 0
    last_use_at: int | None = None
    last_fail_at: int | None = None
    last_fail_reason: str | None = None
    last_sync_at: int | None = None
    last_clear_at: int | None = None
    state_reason: str | None = None
    deleted_at: int | None = None
    ext: dict[str, Any] = Field(default_factory=dict)
    revision: int = 0

    # --- computed properties ---

    @property
    def is_nsfw(self) -> bool:
        return "nsfw" in self.tags

    @property
    def is_super(self) -> bool:
        return self.pool == "super"

    @property
    def is_heavy(self) -> bool:
        return self.pool == "heavy"

    def is_deleted(self) -> bool:
        return self.deleted_at is not None

    def quota_set(self) -> AccountQuotaSet:
        """Deserialise the stored quota dict into an AccountQuotaSet."""
        return AccountQuotaSet.from_dict(self.quota)

    def with_quota_set(self, qs: AccountQuotaSet) -> "AccountRecord":
        """Return a copy of this record with the given quota set serialised."""
        return self.model_copy(update={"quota": qs.to_dict()})

    # --- validators ---

    @field_validator("token", mode="before")
    @classmethod
    def _normalize_token(cls, v: Any) -> str:
        if v is None:
            raise ValueError("token cannot be None")
        token = str(v)
        # Normalise Unicode dash / space variants and strip zero-width chars.
        token = token.translate(
            str.maketrans(
                {
                    "\u2010": "-",
                    "\u2011": "-",
                    "\u2012": "-",
                    "\u2013": "-",
                    "\u2014": "-",
                    "\u2212": "-",
                    "\u00a0": " ",
                    "\u2007": " ",
                    "\u202f": " ",
                    "\u200b": "",
                    "\u200c": "",
                    "\u200d": "",
                    "\ufeff": "",
                }
            )
        )
        token = "".join(token.split())
        if token.startswith("sso="):
            token = token[4:]
        token = token.encode("ascii", errors="ignore").decode("ascii")
        if not token:
            raise ValueError("token is empty after normalisation")
        return token

    @field_validator("pool", mode="before")
    @classmethod
    def _normalize_pool(cls, v: Any) -> str:
        val = str(v or "").strip().lower()
        if val in ("super"):
            return "super"
        if val in ("heavy",):
            return "heavy"
        if val in ("ssobasic", "basic", "", "auto"):
            # "auto" is a UI alias meaning "let quota sync decide";
            # save as basic for now — refresh will correct to the real type.
            return "basic"
        raise ValueError(f"Unknown pool: {v!r}")

    @field_validator("tags", mode="before")
    @classmethod
    def _normalize_tags(cls, v: Any) -> list[str]:
        if not v:
            return []
        if isinstance(v, str):
            v = [p.strip() for p in v.split(",")]
        seen: list[str] = []
        for item in v:
            tag = str(item).strip()
            if tag and tag not in seen:
                seen.append(tag)
        return seen


# ---------------------------------------------------------------------------
# Lightweight result types
# ---------------------------------------------------------------------------


class AccountMutationResult(BaseModel):
    upserted: int = 0
    patched: int = 0
    deleted: int = 0
    revision: int = 0


class AccountPage(BaseModel):
    items: list[AccountRecord] = Field(default_factory=list)
    total: int = 0
    page: int = 1
    page_size: int = 50
    total_pages: int = 1
    revision: int = 0


class AccountChangeSet(BaseModel):
    revision: int = 0
    items: list[AccountRecord] = Field(default_factory=list)
    deleted_tokens: list[str] = Field(default_factory=list)
    has_more: bool = False


class RuntimeSnapshot(BaseModel):
    revision: int = 0
    items: list[AccountRecord] = Field(default_factory=list)


__all__ = [
    "QuotaWindow",
    "AccountQuotaSet",
    "AccountUsageStats",
    "AccountRecord",
    "AccountMutationResult",
    "AccountPage",
    "AccountChangeSet",
    "RuntimeSnapshot",
]
