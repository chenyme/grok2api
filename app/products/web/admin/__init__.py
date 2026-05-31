"""Admin API — router aggregator, shared DI, lightweight endpoints.

All admin endpoints live under ``/admin/api`` with ``verify_admin_key`` guard.
Heavy handlers are split into ``tokens`` and ``batch`` sub-modules.
"""

import re
from typing import TYPE_CHECKING, Any

import orjson
from fastapi import APIRouter, Depends, Request
from fastapi.responses import Response
from pydantic import RootModel

from app.control.account.backends.factory import get_repository_backend
from app.platform.auth.middleware import verify_admin_key
from app.platform.config.snapshot import config
from app.platform.errors import AppError, ErrorKind, ValidationError
from app.platform.logging.logger import logger, reload_file_logging
from app.platform.storage import reconcile_local_media_cache_async

if TYPE_CHECKING:
    from app.control.account.refresh import AccountRefreshService
    from app.control.account.repository import AccountRepository

# ---------------------------------------------------------------------------
# Shared DI dependencies — inject via Depends, no try/except per call
# ---------------------------------------------------------------------------

_CFG_CHAR_REPLACEMENTS = str.maketrans(
    {
        "\u2010": "-",
        "\u2011": "-",
        "\u2012": "-",
        "\u2013": "-",
        "\u2014": "-",
        "\u2212": "-",
        "\u2018": "'",
        "\u2019": "'",
        "\u201c": '"',
        "\u201d": '"',
        "\u00a0": " ",
        "\u2007": " ",
        "\u202f": " ",
        "\u200b": "",
        "\u200c": "",
        "\u200d": "",
        "\ufeff": "",
    }
)

_STARTUP_ONLY_CONFIG_PREFIXES = (
    "account.storage",
    "account.local",
    "account.redis",
    "account.mysql",
    "account.postgresql",
)


class ConfigPatchRequest(RootModel[dict[str, Any]]):
    """Loose config patch payload with explicit root typing."""


def _sanitize_text(value: Any, *, remove_all_spaces: bool = False) -> str:
    text = "" if value is None else str(value)
    text = text.translate(_CFG_CHAR_REPLACEMENTS)
    if remove_all_spaces:
        text = re.sub(r"\s+", "", text)
    else:
        text = text.strip()
    return text.encode("latin-1", errors="ignore").decode("latin-1")


def _sanitize_proxy_config(payload: dict[str, Any]) -> dict[str, Any]:
    proxy = payload.get("proxy")
    if not isinstance(proxy, dict):
        return dict(payload)

    sanitized = dict(proxy)
    changed = False

    def _sanitize_fields(target: dict[str, Any]) -> tuple[dict[str, Any], bool]:
        normalized = dict(target)
        local_changed = False
        for key, strip_spaces in [
            ("user_agent", False),
            ("cf_cookies", False),
            ("cf_clearance", True),
        ]:
            if key not in normalized:
                continue
            raw = normalized[key]
            val = _sanitize_text(raw, remove_all_spaces=strip_spaces)
            if val != raw:
                normalized[key] = val
                local_changed = True
        return normalized, local_changed

    sanitized, changed = _sanitize_fields(sanitized)

    clearance = sanitized.get("clearance")
    if isinstance(clearance, dict):
        sanitized_clearance, clearance_changed = _sanitize_fields(clearance)
        if clearance_changed:
            sanitized["clearance"] = sanitized_clearance
            changed = True

    if not changed:
        return dict(payload)

    logger.warning("admin config payload sanitized before save: section=proxy")
    result = dict(payload)
    result["proxy"] = sanitized
    return result


def _iter_patch_paths(value: Any, prefix: str = ""):
    if isinstance(value, dict):
        for key, child in value.items():
            path = f"{prefix}.{key}" if prefix else str(key)
            if isinstance(child, dict):
                yield from _iter_patch_paths(child, path)
            else:
                yield path


def _ensure_runtime_patch_allowed(payload: dict[str, Any]) -> None:
    for path in _iter_patch_paths(payload):
        for blocked in _STARTUP_ONLY_CONFIG_PREFIXES:
            if path == blocked or path.startswith(f"{blocked}."):
                raise ValidationError(
                    "Storage config is startup-only and must be set via env",
                    param=path,
                    code="startup_only_config",
                )


def _patch_touches_prefix(payload: dict[str, Any], prefix: str) -> bool:
    return any(
        path == prefix or path.startswith(f"{prefix}.")
        for path in _iter_patch_paths(payload)
    )


def get_repo(request: Request) -> "AccountRepository":
    """Resolve the singleton AccountRepository from app state."""
    return request.app.state.repository


def get_refresh_svc(request: Request) -> "AccountRefreshService":
    """Resolve the singleton AccountRefreshService from app state."""
    return request.app.state.refresh_service


# ---------------------------------------------------------------------------
# Router
# ---------------------------------------------------------------------------

router = APIRouter(prefix="/admin/api", dependencies=[Depends(verify_admin_key)])
_TAG_ADMIN_SYSTEM = "Admin - System"

# Mount sub-modules
from .tokens import router as _tokens_router  # noqa: E402
from .batch import router as _batch_router  # noqa: E402
from .assets import router as _assets_router  # noqa: E402
from .cache import router as _cache_router  # noqa: E402

router.include_router(_tokens_router)
router.include_router(_batch_router)
router.include_router(_assets_router)
router.include_router(_cache_router)


# ---------------------------------------------------------------------------
# Lightweight inline endpoints (no separate file needed)
# ---------------------------------------------------------------------------


@router.get("/verify", tags=[_TAG_ADMIN_SYSTEM])
async def admin_verify():
    return {"status": "success"}


@router.get("/config", tags=[_TAG_ADMIN_SYSTEM])
async def get_config_endpoint():
    return Response(
        content=orjson.dumps(config.raw()),
        media_type="application/json",
    )


@router.post("/config", tags=[_TAG_ADMIN_SYSTEM])
async def update_config(req: ConfigPatchRequest):
    from app.control.account.runtime import reconcile_refresh_runtime

    patch = _sanitize_proxy_config(req.root)
    _ensure_runtime_patch_allowed(patch)
    cache_local_changed = (
        _patch_touches_prefix(patch, "cache.local")
        or _patch_touches_prefix(patch, "storage")
    )
    await config.update(patch)
    # config.update() only writes to the backend and invalidates the in-memory
    # snapshot (_version = None); it does not refresh the data.  load() is
    # required here so that get_str/get_int calls below return the new values.
    await config.load()
    reload_file_logging(
        file_level=config.get_str("logging.file_level", "") or None,
        max_files=config.get_int("logging.max_files", 7),
    )
    if cache_local_changed:
        await reconcile_local_media_cache_async()
    strategy_name = reconcile_refresh_runtime()
    return {
        "status": "success",
        "message": "配置已更新",
        "selection_strategy": strategy_name,
    }


@router.get("/storage", tags=[_TAG_ADMIN_SYSTEM])
async def get_storage_mode():
    return {"type": get_repository_backend()}


@router.get("/status", tags=[_TAG_ADMIN_SYSTEM])
async def runtime_status():
    from app.control.account.runtime import reconcile_refresh_runtime
    from app.dataplane.account import _directory

    if _directory is None:
        raise AppError(
            "Account directory not initialised",
            kind=ErrorKind.SERVER,
            code="directory_not_initialised",
            status=503,
        )
    strategy_name = reconcile_refresh_runtime()
    return Response(
        content=orjson.dumps(
            {
                "status": "ok",
                "size": _directory.size,
                "revision": _directory.revision,
                "selection_strategy": strategy_name,
            }
        ),
        media_type="application/json",
    )


@router.post("/sync", tags=[_TAG_ADMIN_SYSTEM])
async def force_sync():
    from app.dataplane.account import _directory

    if _directory is None:
        raise AppError(
            "Account directory not initialised",
            kind=ErrorKind.SERVER,
            code="directory_not_initialised",
            status=503,
        )
    changed = await _directory.sync_if_changed()
    return Response(
        content=orjson.dumps({"changed": changed, "revision": _directory.revision}),
        media_type="application/json",
    )


__all__ = ["router", "get_repo", "get_refresh_svc"]
