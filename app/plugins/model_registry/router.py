"""Admin API for the dynamic model registry overlay."""

from __future__ import annotations

import re
import time
from typing import Any
from urllib.parse import urlparse

from curl_cffi.requests import AsyncSession
from fastapi import APIRouter

from app.control.model import registry as model_registry
from app.platform.config.snapshot import config
from app.platform.errors import ValidationError
from app.platform.logging.logger import logger

from . import service


router = APIRouter(prefix="/models/registry", tags=["Admin - Model Registry"])

_PUBLIC_MODELS_SOURCES = (
    "https://docs.x.ai/developers/models",
    "https://x.ai/api",
)
_MODEL_ID_RE = re.compile(r"\b(grok-[a-z0-9][a-z0-9._-]*)\b", re.I)


def _extract_model_ids_from_text(text: str) -> list[str]:
    deny = {"grok-api", "grok-login", "grok-docs"}
    ids: list[str] = []
    seen: set[str] = set()
    for match in _MODEL_ID_RE.finditer(text or ""):
        mid = service.normalize_model_id(match.group(1))
        if not mid or mid in seen or mid in deny:
            continue
        seen.add(mid)
        ids.append(mid)
    return ids


async def _fetch_public_models() -> list[dict[str, Any]]:
    timeout = config.get_int("chat.timeout", 60) or 60
    proxy_cfg = (config.raw() or {}).get("proxy", {}) or {}
    clearance = proxy_cfg.get("clearance", {}) or {}
    user_agent = clearance.get("user_agent", "Mozilla/5.0")
    browser = clearance.get("browser", "chrome136")

    egress = proxy_cfg.get("egress", {}) or {}
    proxy_url = str(egress.get("proxy_url", "") or "").strip()
    session_kwargs: dict[str, Any] = {}
    if proxy_url:
        scheme = urlparse(proxy_url).scheme.lower()
        if scheme.startswith("socks"):
            session_kwargs["proxy"] = proxy_url
        else:
            session_kwargs["proxies"] = {"http": proxy_url, "https": proxy_url}

    collected: list[str] = []
    errors: list[str] = []
    for url in _PUBLIC_MODELS_SOURCES:
        try:
            async with AsyncSession(timeout=timeout, **session_kwargs) as session:
                res = await session.get(
                    url,
                    headers={"User-Agent": user_agent},
                    impersonate=browser,
                )
            if res.status_code != 200:
                errors.append(f"{url} returned HTTP {res.status_code}")
                continue
            ids = _extract_model_ids_from_text(res.text or "")
            if not ids:
                errors.append(f"{url} returned no model IDs")
                continue
            for mid in ids:
                if mid not in collected:
                    collected.append(mid)
        except Exception as exc:
            errors.append(f"{url} failed: {str(exc).replace(chr(10), ' ')[:180]}")
            logger.warning("public model discovery failed from {}: {}", url, exc)

    if not collected:
        detail = "Failed to discover public model IDs"
        if errors:
            detail += ": " + "; ".join(errors)
        raise ValidationError(detail, code="model_discovery_failed")

    now = int(time.time())
    return [
        {
            "id": mid,
            "object": "model",
            "created": now,
            "owned_by": "xai_public_docs",
        }
        for mid in collected
    ]


def _remote_models(registry: dict[str, Any]) -> list[dict[str, Any]]:
    created = int(registry.get("last_sync_at", 0) or 0)
    return [
        {
            "id": mid,
            "object": "model",
            "created": created,
            "owned_by": "xai_public_docs",
        }
        for mid in service.remote_model_ids(registry)
    ]


def _builtin_ids() -> set[str]:
    return {m.model_name for m in model_registry.list_builtin_enabled()}


@router.get("")
async def get_model_registry():
    registry = service.registry_config()
    alias_map = service.aliases(registry)
    manual = service.manual_models(registry)
    remote = _remote_models(registry)
    builtin_ids = _builtin_ids()

    items: list[dict[str, Any]] = []
    for item in remote:
        mid = str(item.get("id") or "")
        desc = model_registry.describe(mid)
        items.append(
            {
                **item,
                "supported": mid in builtin_ids,
                "executable": bool(desc.get("executable")),
                "mapped_to": desc.get("mapped_to"),
                "source": desc.get("source"),
            }
        )

    return {
        "status": "success",
        "enabled": service.registry_enabled(registry),
        "source": registry.get("source", "xai_public_docs"),
        "last_sync_at": int(registry.get("last_sync_at", 0) or 0),
        "remote_count": len(remote),
        "supported_count": sum(1 for item in remote if item["id"] in builtin_ids),
        "aliases": alias_map,
        "manual_models": manual,
        "local_models": sorted(builtin_ids),
        "models": items,
    }


@router.post("/discover")
async def discover_model_registry(_: dict[str, Any] | None = None):
    normalized = await _fetch_public_models()
    old_registry = service.registry_config()
    sync_at = int(time.time())
    await config.update(
        {
            "model_registry": {
                "enabled": True,
                "source": "xai_public_docs",
                "last_sync_at": sync_at,
                "remote_model_ids": [item["id"] for item in normalized],
                "remote_models": [],
                "aliases": service.aliases(old_registry),
                "manual_models": service.manual_models(old_registry),
            }
        }
    )

    builtin_ids = _builtin_ids()
    return {
        "status": "success",
        "remote_count": len(normalized),
        "supported_count": sum(1 for item in normalized if item["id"] in builtin_ids),
        "source": "xai_public_docs",
    }


@router.post("/manual/upsert")
async def upsert_manual_model(data: dict[str, Any]):
    model_id = service.normalize_model_id((data or {}).get("id"))
    model_name = str((data or {}).get("name") or "").strip() or model_id
    if not model_id:
        raise ValidationError("id is required", param="id", code="required")

    registry = service.registry_config()
    kept: list[dict[str, str]] = []
    replaced = False
    for item in service.manual_models(registry):
        if item["id"] == model_id:
            kept.append({"id": model_id, "name": model_name})
            replaced = True
        else:
            kept.append({"id": item["id"], "name": item.get("name") or item["id"]})
    if not replaced:
        kept.append({"id": model_id, "name": model_name})

    alias_map = service.aliases(registry)
    alias_map.pop(model_id, None)
    await config.update({"model_registry": {"manual_models": kept, "aliases": alias_map}})
    return {"status": "success", "manual_models": kept, "mapped_to": None}


@router.post("/manual/delete")
async def delete_manual_model(data: dict[str, Any]):
    model_id = service.normalize_model_id((data or {}).get("id"))
    if not model_id:
        raise ValidationError("id is required", param="id", code="required")

    registry = service.registry_config()
    kept = [
        {
            key: value
            for key, value in {
                "id": item["id"],
                "name": item.get("name") or item["id"],
                "mapped_to": item.get("mapped_to", ""),
            }.items()
            if value
        }
        for item in service.manual_models(registry)
        if item["id"] != model_id
    ]
    alias_map = service.aliases(registry)
    alias_map.pop(model_id, None)
    await config.update({"model_registry": {"manual_models": kept, "aliases": alias_map}})
    return {"status": "success", "manual_models": kept}


@router.post("/alias/upsert")
async def upsert_model_alias(data: dict[str, Any]):
    remote_id = service.normalize_model_id((data or {}).get("remote_id"))
    mapped_to = service.normalize_model_id((data or {}).get("mapped_to"))
    if not remote_id or not mapped_to:
        raise ValidationError(
            "remote_id and mapped_to are required",
            code="required",
        )
    if mapped_to not in _builtin_ids():
        raise ValidationError(
            f"mapped_to not supported: {mapped_to}",
            param="mapped_to",
            code="invalid_model_mapping",
        )

    alias_map = service.aliases()
    alias_map[remote_id] = mapped_to
    await config.update({"model_registry": {"aliases": alias_map}})
    return {"status": "success", "aliases": alias_map}


@router.post("/alias/delete")
async def delete_model_alias(data: dict[str, Any]):
    remote_id = service.normalize_model_id((data or {}).get("remote_id"))
    if not remote_id:
        raise ValidationError("remote_id is required", param="remote_id", code="required")

    alias_map = service.aliases()
    alias_map.pop(remote_id, None)
    await config.update({"model_registry": {"aliases": alias_map}})
    return {"status": "success", "aliases": alias_map}


@router.post("/enable")
async def enable_model_registry():
    await config.update({"model_registry": {"enabled": True}})
    return {"status": "success"}


@router.post("/disable")
async def disable_model_registry():
    await config.update({"model_registry": {"enabled": False}})
    return {"status": "success"}


__all__ = ["router"]
