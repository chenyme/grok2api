"""Per-operation timeout / retry profiles for the reverse pipeline.

Pre-built profiles eliminate per-request config lookups in the hot path.
Each profile specifies timeout, max retries, and retry-eligible status codes.
"""

from dataclasses import dataclass


@dataclass(slots=True, frozen=True)
class OperationProfile:
    """Timeout and retry configuration for one operation type."""
    timeout_s:       float        = 30.0
    max_retries:     int          = 0
    retry_codes:     frozenset[int] = frozenset()
    retry_delay_s:   float        = 1.0
    idle_timeout_s:  float        = 0.0   # 0 = no idle timeout


# ---------------------------------------------------------------------------
# Pre-built profiles
# ---------------------------------------------------------------------------

CHAT = OperationProfile(
    timeout_s      = 120.0,
    max_retries    = 1,
    retry_codes    = frozenset({502, 503}),
    retry_delay_s  = 2.0,
    idle_timeout_s = 30.0,
)

IMAGE = OperationProfile(
    timeout_s      = 300.0,
    max_retries    = 0,
    idle_timeout_s = 60.0,
)

IMAGE_EDIT = OperationProfile(
    timeout_s      = 120.0,
    max_retries    = 1,
    retry_codes    = frozenset({502, 503}),
    retry_delay_s  = 2.0,
    idle_timeout_s = 30.0,
)

VIDEO = OperationProfile(
    timeout_s      = 60.0,
    max_retries    = 1,
    retry_codes    = frozenset({502, 503, 429}),
    retry_delay_s  = 5.0,
)

VOICE = OperationProfile(
    timeout_s      = 120.0,
    max_retries    = 0,
    idle_timeout_s = 15.0,
)

ASSET = OperationProfile(
    timeout_s      = 60.0,
    max_retries    = 2,
    retry_codes    = frozenset({502, 503}),
    retry_delay_s  = 1.0,
)

GRPC = OperationProfile(
    timeout_s      = 15.0,
    max_retries    = 1,
    retry_codes    = frozenset({503}),
    retry_delay_s  = 0.5,
)

# Lookup by name for programmatic access.
PROFILES: dict[str, OperationProfile] = {
    "chat":       CHAT,
    "image":      IMAGE,
    "image_edit": IMAGE_EDIT,
    "video":      VIDEO,
    "voice":      VOICE,
    "asset":      ASSET,
    "grpc":       GRPC,
}


__all__ = ["OperationProfile", "PROFILES", "CHAT", "IMAGE", "IMAGE_EDIT", "VIDEO", "VOICE", "ASSET", "GRPC"]
