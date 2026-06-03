"""Model registry overlay helpers.

Built-in models keep the upstream modeId-based execution path. Manual models
use a direct path that sends their actual ID to Grok as ``modelName``.
"""

from __future__ import annotations

from typing import Any, Mapping

from app.control.model.enums import Capability, ModeId, Tier
from app.control.model.spec import ModelSpec
from app.platform.config.snapshot import config


def normalize_model_id(value: Any) -> str:
    return str(value or "").strip().lower()


def registry_config() -> dict[str, Any]:
    raw = config.raw() or {}
    section = raw.get("model_registry", {})
    return section if isinstance(section, dict) else {}


def registry_enabled(registry: Mapping[str, Any] | None = None) -> bool:
    data = registry if registry is not None else registry_config()
    return bool(data.get("enabled", False))


def aliases(registry: Mapping[str, Any] | None = None) -> dict[str, str]:
    data = registry if registry is not None else registry_config()
    raw = data.get("aliases", {})
    if not isinstance(raw, dict):
        return {}

    out: dict[str, str] = {}
    for key, value in raw.items():
        src = normalize_model_id(key)
        dst = normalize_model_id(value)
        if src and dst:
            out[src] = dst
    return out


def manual_models(registry: Mapping[str, Any] | None = None) -> list[dict[str, str]]:
    data = registry if registry is not None else registry_config()
    raw = data.get("manual_models", [])
    if not isinstance(raw, list):
        return []

    out: list[dict[str, str]] = []
    seen: set[str] = set()
    for item in raw:
        if not isinstance(item, dict):
            continue
        mid = normalize_model_id(item.get("id"))
        if not mid or mid in seen:
            continue
        seen.add(mid)
        row = {
            "id": mid,
            "name": str(item.get("name") or mid).strip() or mid,
        }
        out.append(row)
    return out


def manual_index(registry: Mapping[str, Any] | None = None) -> dict[str, dict[str, str]]:
    return {item["id"]: item for item in manual_models(registry)}


def remote_model_ids(registry: Mapping[str, Any] | None = None) -> list[str]:
    data = registry if registry is not None else registry_config()
    out: list[str] = []
    seen: set[str] = set()

    raw_ids = data.get("remote_model_ids", [])
    if isinstance(raw_ids, list):
        for raw in raw_ids:
            mid = normalize_model_id(raw)
            if mid and mid not in seen:
                seen.add(mid)
                out.append(mid)

    legacy = data.get("remote_models", [])
    if isinstance(legacy, list):
        for item in legacy:
            if not isinstance(item, dict):
                continue
            mid = normalize_model_id(item.get("id"))
            if mid and mid not in seen:
                seen.add(mid)
                out.append(mid)

    return out


def _title_from_id(model_id: str) -> str:
    return " ".join(part.capitalize() for part in model_id.replace("_", "-").split("-") if part)


def _manual_capability(model_id: str) -> Capability:
    if model_id.startswith("grok-imagine-image-edit"):
        return Capability.IMAGE_EDIT
    if model_id.startswith("grok-imagine-image"):
        return Capability.IMAGE
    return Capability.CHAT


def _manual_direct_spec(model_id: str, public_name: str | None = None) -> ModelSpec:
    return ModelSpec(
        model_id,
        ModeId.FAST,
        Tier.BASIC,
        _manual_capability(model_id),
        True,
        public_name or _title_from_id(model_id),
        upstream_model_name=model_id,
    )


def _clone_spec(base: ModelSpec, model_id: str, public_name: str | None = None) -> ModelSpec:
    return ModelSpec(
        model_id,
        base.mode_id,
        base.tier,
        base.capability,
        base.enabled,
        public_name or _title_from_id(model_id),
        prefer_best=base.prefer_best,
        upstream_model_name=base.upstream_model_name,
    )


def overlay_get(
    model_name: str,
    builtin_by_name: Mapping[str, ModelSpec],
    *,
    registry: Mapping[str, Any] | None = None,
    seen: set[str] | None = None,
) -> ModelSpec | None:
    model_id = normalize_model_id(model_name)
    if not model_id:
        return None

    data = registry if registry is not None else registry_config()
    visited = seen or set()
    if model_id in visited:
        return None
    visited.add(model_id)

    manual = manual_index(data)
    alias_map = aliases(data)
    manual_item = manual.get(model_id)
    if manual_item is not None:
        return _manual_direct_spec(model_id, manual_item.get("name"))

    mapped_to = alias_map.get(model_id) or (manual_item or {}).get("mapped_to")
    if not mapped_to:
        return None

    target = normalize_model_id(mapped_to)
    base = builtin_by_name.get(target) or overlay_get(
        target,
        builtin_by_name,
        registry=data,
        seen=visited,
    )
    if base is None:
        return None
    return _clone_spec(base, model_id, (manual_item or {}).get("name"))


def overlay_list(
    builtin_by_name: Mapping[str, ModelSpec],
    *,
    registry: Mapping[str, Any] | None = None,
) -> list[ModelSpec]:
    data = registry if registry is not None else registry_config()
    ids: list[str] = []
    seen: set[str] = set(builtin_by_name)

    def add_id(raw: Any) -> None:
        mid = normalize_model_id(raw)
        if mid and mid not in seen:
            seen.add(mid)
            ids.append(mid)

    for item in manual_models(data):
        add_id(item.get("id"))
    for mid in aliases(data):
        add_id(mid)

    specs: list[ModelSpec] = []
    for mid in ids:
        spec = overlay_get(mid, builtin_by_name, registry=data)
        if spec is not None and spec.enabled:
            specs.append(spec)
    return specs


def describe_model(
    model_name: str,
    builtin_by_name: Mapping[str, ModelSpec],
    *,
    registry: Mapping[str, Any] | None = None,
) -> dict[str, Any]:
    model_id = normalize_model_id(model_name)
    data = registry if registry is not None else registry_config()
    manual = manual_index(data)
    alias_map = aliases(data)
    remote_ids = set(remote_model_ids(data))

    if model_id in builtin_by_name:
        return {
            "source": "builtin",
            "manual": False,
            "mapped_to": None,
            "executable": True,
        }

    source = "manual" if model_id in manual else "remote" if model_id in remote_ids else "alias"
    mapped_to = None if source == "manual" else alias_map.get(model_id)
    return {
        "source": source,
        "manual": model_id in manual,
        "mapped_to": mapped_to or None,
        "executable": overlay_get(model_id, builtin_by_name, registry=data) is not None,
    }
