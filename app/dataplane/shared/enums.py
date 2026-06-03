"""Dataplane-local integer enumerations for hot-path array indexing.

These mirror the control-plane enums but use IntEnum exclusively so they
can be used directly as array indices with zero overhead.
"""

from enum import IntEnum


class ModeId(IntEnum):
    AUTO = 0
    FAST = 1
    EXPERT = 2
    HEAVY = 3
    GROK_4_3 = 4


class PoolId(IntEnum):
    BASIC = 0
    SUPER = 1
    HEAVY = 2


class StatusId(IntEnum):
    ACTIVE = 0
    COOLING = 1
    EXPIRED = 2
    DISABLED = 3
    DELETED = 4


# Map pool string → PoolId integer (used during sync from control plane).
POOL_STR_TO_ID: dict[str, int] = {
    "basic": int(PoolId.BASIC),
    "super": int(PoolId.SUPER),
    "heavy": int(PoolId.HEAVY),
}

POOL_ID_TO_STR: dict[int, str] = {v: k for k, v in POOL_STR_TO_ID.items()}

# Pools that participate in the grok.com reverse-proxy hot path (selection,
# runtime table, quota probing).  Accounts in other pools (e.g. "xai", which
# use the official api.x.ai OAuth provider) must be excluded from this machinery.
GROK_POOLS: frozenset[str] = frozenset(POOL_STR_TO_ID.keys())

STATUS_STR_TO_ID: dict[str, int] = {
    "active": int(StatusId.ACTIVE),
    "cooling": int(StatusId.COOLING),
    "expired": int(StatusId.EXPIRED),
    "disabled": int(StatusId.DISABLED),
}

ALL_MODE_IDS: tuple[int, ...] = (
    int(ModeId.AUTO),
    int(ModeId.FAST),
    int(ModeId.EXPERT),
    int(ModeId.HEAVY),
    int(ModeId.GROK_4_3),
)

__all__ = [
    "ModeId",
    "PoolId",
    "StatusId",
    "POOL_STR_TO_ID",
    "POOL_ID_TO_STR",
    "GROK_POOLS",
    "STATUS_STR_TO_ID",
    "ALL_MODE_IDS",
]
