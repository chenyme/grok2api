#!/usr/bin/env python3
"""Active and passive quality guard for grok2api egress nodes.

The guard calls a scoped internal API, quarantines suspect nodes without
deleting bindings, and restores only nodes it disabled. Configuration and the
scoped internal credential are provided by grok2api through a private
bootstrap file. The implementation uses only Python's standard library.
"""

from __future__ import annotations

import argparse
import dataclasses
import fcntl
import json
import os
import random
import signal
import ssl
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any


RUNTIME_CONFIG_FIELDS = {
    "mode",
    "active_interval_seconds",
    "passive_poll_seconds",
    "soft_tps",
    "hard_tps",
    "consecutive_soft",
    "consecutive_errors",
    "quarantine_seconds",
    "min_healthy_nodes",
}


BOOTSTRAP_VERSION = 1
BOOTSTRAP_FILE = Path("/var/lib/grok2api-quality-guard/bootstrap.json")
INTERNAL_API_PREFIX = "/api/internal/v1/quality-guard"
QUALITY_MARKER_PROFILE_ID = "quality-marker"
THROUGHPUT_PROFILE_ID = "throughput"
THINKING_GUARD_MIN_OUTPUT_TOKENS = 64


class GuardDisabled(RuntimeError):
    pass


@dataclasses.dataclass(frozen=True)
class Config:
    base_url: str
    internal_token: str
    model: str
    node_ids: tuple[str, ...]
    mode: str
    active_interval_seconds: int
    passive_poll_seconds: int
    passive_page_size: int
    passive_max_pages: int
    jitter_seconds: int
    request_timeout_seconds: int
    soft_tps: float
    hard_tps: float
    consecutive_soft: int
    consecutive_errors: int
    quarantine_seconds: int
    no_account_backoff_seconds: int
    min_healthy_nodes: int
    max_output_tokens: int
    fail_closed: bool
    min_generation_ms: int
    rotation_url: str
    rotation_token: str
    rotation_timeout_seconds: int
    rotatable_node_ids: tuple[str, ...]
    prompt: str
    expected: str
    state_file: Path
    lock_file: Path
    runtime_config_file: Path
    profiles_file: Path = dataclasses.field(default_factory=lambda: Path("/var/lib/grok2api-quality-guard/profiles.json"))

    @classmethod
    def from_bootstrap(cls, path: Path = BOOTSTRAP_FILE) -> "Config":
        try:
            with path.open("r", encoding="utf-8") as handle:
                payload = json.load(handle)
        except FileNotFoundError as exc:
            raise ValueError("quality guard bootstrap file is missing; restart grok2api") from exc
        except (OSError, ValueError) as exc:
            raise ValueError(f"cannot read quality guard bootstrap: {type(exc).__name__}") from exc
        if not isinstance(payload, dict) or payload.get("version") != BOOTSTRAP_VERSION:
            raise ValueError("unsupported quality guard bootstrap")
        if not payload.get("enabled"):
            raise GuardDisabled("qualityGuard.enabled is false in config.yaml")
        values = payload.get("config")
        if not isinstance(values, dict):
            raise ValueError("quality guard bootstrap config is missing")
        token = str(payload.get("internal_token") or "").strip()
        node_ids = tuple(dict.fromkeys(str(value).strip() for value in values.get("node_ids", []) if str(value).strip()))
        rotatable_node_ids = tuple(dict.fromkeys(str(value).strip() for value in values.get("rotatable_node_ids", []) if str(value).strip()))
        config = cls(
            base_url="http://grok2api:8000",
            internal_token=token,
            model=str(values.get("model") or "").strip(),
            node_ids=node_ids,
            mode=str(values.get("mode") or "").strip().lower(),
            active_interval_seconds=int(values.get("active_interval_seconds") or 0),
            passive_poll_seconds=int(values.get("passive_poll_seconds") or 0),
            passive_page_size=200,
            passive_max_pages=10,
            jitter_seconds=30,
            request_timeout_seconds=120,
            soft_tps=float(values.get("soft_tps") or 0),
            hard_tps=float(values.get("hard_tps") or 0),
            consecutive_soft=int(values.get("consecutive_soft") or 0),
            consecutive_errors=int(values.get("consecutive_errors") or 0),
            quarantine_seconds=int(values.get("quarantine_seconds") or 0),
            no_account_backoff_seconds=int(values.get("no_account_backoff_seconds") or 0),
            min_healthy_nodes=int(values.get("min_healthy_nodes") or 0),
            max_output_tokens=int(values.get("max_output_tokens") or 0),
            fail_closed=bool(values.get("fail_closed")),
            min_generation_ms=int(values.get("min_generation_ms") or 0),
            rotation_url=str(values.get("rotation_url") or "").strip(),
            rotation_token=str(values.get("rotation_token") or ""),
            rotation_timeout_seconds=int(values.get("rotation_timeout_seconds") or 0),
            rotatable_node_ids=rotatable_node_ids,
            prompt=str(values.get("prompt") or "").strip(),
            expected=str(values.get("expected") or "").strip(),
            state_file=Path("/var/lib/grok2api-quality-guard/state.json"),
            lock_file=Path("/var/lib/grok2api-quality-guard/guard.lock"),
            runtime_config_file=Path("/var/lib/grok2api-quality-guard/runtime-config.json"),
            profiles_file=Path("/var/lib/grok2api-quality-guard/profiles.json"),
        )
        config.validate()
        return config

    def validate(self) -> None:
        parsed = urllib.parse.urlparse(self.base_url)
        if parsed.scheme not in {"http", "https"} or not parsed.netloc:
            raise ValueError("GROK2API_BASE_URL must be an absolute HTTP(S) URL")
        if not self.internal_token:
            raise ValueError("quality guard bootstrap internal token is missing")
        if not self.model or not self.prompt or not self.expected:
            raise ValueError("model, prompt, and expected marker must not be empty")
        if self.mode not in {"active", "passive", "hybrid"}:
            raise ValueError("qualityGuard.mode must be active, passive, or hybrid")
        if self.soft_tps >= self.hard_tps:
            raise ValueError("qualityGuard.softTPS must be lower than qualityGuard.hardTPS")
        if self.active_interval_seconds > 86400:
            raise ValueError("qualityGuard.activeInterval must not exceed 24 hours")
        if self.passive_poll_seconds > 300:
            raise ValueError("qualityGuard.passivePollInterval must not exceed 5 minutes")
        if self.soft_tps > 10000 or self.hard_tps > 10000:
            raise ValueError("quality guard Token/s thresholds must not exceed 10000")
        if self.consecutive_soft > 20 or self.consecutive_errors > 20:
            raise ValueError("quality guard consecutive strike limits must not exceed 20")
        if self.quarantine_seconds > 86400:
            raise ValueError("qualityGuard.quarantineDuration must not exceed 24 hours")
        if self.no_account_backoff_seconds > 86400:
            raise ValueError("qualityGuard.noAccountBackoff must not exceed 24 hours")
        if self.min_healthy_nodes < 1 or (self.node_ids and self.min_healthy_nodes > len(self.node_ids)):
            raise ValueError("qualityGuard.minimumHealthyNodes must fit the configured node count")
        if self.min_generation_ms > self.request_timeout_seconds * 1000:
            raise ValueError("qualityGuard.minimumGenerationWindow must fit the request timeout")
        if self.rotation_url:
            rotation_url = urllib.parse.urlparse(self.rotation_url)
            if rotation_url.scheme not in {"http", "https"} or not rotation_url.netloc:
                raise ValueError("qualityGuard.rotationURL must be an absolute HTTP(S) URL")
        elif self.rotatable_node_ids:
            raise ValueError("qualityGuard.rotationURL is required when rotatableNodeIDs are configured")
        if any(not value.isdigit() or int(value) < 1 for value in self.rotatable_node_ids):
            raise ValueError("qualityGuard.rotatableNodeIDs must contain positive integers")
        if self.passive_page_size > 2000:
            raise ValueError("internal passive page size must not exceed 2000")


def load_runtime_config(base: Config, path: Path) -> Config:
    try:
        with path.open("r", encoding="utf-8") as handle:
            value = json.load(handle)
    except FileNotFoundError:
        return base
    except (OSError, ValueError) as exc:
        raise ValueError(f"cannot read runtime quality guard config: {type(exc).__name__}") from exc
    if not isinstance(value, dict) or value.get("version") != 1 or not isinstance(value.get("settings"), dict):
        raise ValueError("unsupported runtime quality guard config")
    settings = value["settings"]
    unknown = set(settings) - RUNTIME_CONFIG_FIELDS
    if unknown:
        raise ValueError("runtime quality guard config contains unknown fields")
    if set(settings) != RUNTIME_CONFIG_FIELDS:
        raise ValueError("runtime quality guard config is incomplete")
    if not isinstance(settings["mode"], str):
        raise ValueError("runtime quality guard mode must be a string")
    integer_fields = RUNTIME_CONFIG_FIELDS - {"mode", "soft_tps", "hard_tps"}
    if any(isinstance(settings[name], bool) or not isinstance(settings[name], int) for name in integer_fields):
        raise ValueError("runtime quality guard integer field is invalid")
    if any(isinstance(settings[name], bool) or not isinstance(settings[name], (int, float)) for name in {"soft_tps", "hard_tps"}):
        raise ValueError("runtime quality guard threshold is invalid")
    config = dataclasses.replace(base, **settings)
    config.validate()
    return config


class RuntimeConfigReloader:
    def __init__(self, base: Config):
        self.base = base
        self.current = base
        self.signature: tuple[int, int] | None = None
        self.missing = False

    def reload(self, force: bool = False) -> tuple[Config, bool, Exception | None]:
        try:
            stat_result = self.base.runtime_config_file.stat()
            signature = (stat_result.st_mtime_ns, stat_result.st_size)
            missing = False
        except FileNotFoundError:
            signature = None
            missing = True
        except OSError as exc:
            return self.current, False, exc
        if not force and signature == self.signature and missing == self.missing:
            return self.current, False, None
        self.signature = signature
        self.missing = missing
        try:
            candidate = self.base if missing else load_runtime_config(self.base, self.base.runtime_config_file)
        except ValueError as exc:
            return self.current, True, exc
        changed = candidate != self.current
        self.current = candidate
        return candidate, changed or force, None


class ApiError(RuntimeError):
    def __init__(self, status: int, code: str, message: str):
        super().__init__(f"HTTP {status} {code}: {message}")
        self.status = status
        self.code = code


class ApiClient:
    def __init__(self, config: Config):
        self.config = config
        self.ssl_context = ssl.create_default_context()

    def _request(self, method: str, path: str, body: dict[str, Any] | None = None) -> Any:
        data = None if body is None else json.dumps(body, separators=(",", ":")).encode()
        headers = {"Accept": "application/json"}
        if data is not None:
            headers["Content-Type"] = "application/json"
        headers["Authorization"] = f"Bearer {self.config.internal_token}"
        request = urllib.request.Request(self.config.base_url + path, data=data, headers=headers, method=method)
        try:
            with urllib.request.urlopen(request, timeout=self.config.request_timeout_seconds, context=self.ssl_context) as response:
                payload = json.load(response)
        except urllib.error.HTTPError as exc:
            try:
                payload = json.loads(exc.read().decode("utf-8", "replace"))
            except (ValueError, OSError):
                payload = {}
            error = payload.get("error") or {}
            raise ApiError(exc.code, str(error.get("code", "request_failed")), str(error.get("message", "request failed"))) from exc
        except (urllib.error.URLError, TimeoutError, OSError) as exc:
            raise RuntimeError(f"request failed: {type(exc).__name__}") from exc
        return payload.get("data", payload)

    def list_nodes(self) -> list[dict[str, Any]]:
        page_size = 2000
        items: list[dict[str, Any]] = []
        seen_ids: set[str] = set()
        page = 1
        while True:
            query = urllib.parse.urlencode({"page": page, "pageSize": page_size, "scope": "grok_build"})
            payload = self._request("GET", f"{INTERNAL_API_PREFIX}/egress-nodes?{query}")
            batch = list(payload.get("items") or [])
            total = max(0, int(payload.get("total") or 0))
            added = 0
            for node in batch:
                node_id = str(node.get("id") or "")
                if not node_id or node_id in seen_ids:
                    continue
                seen_ids.add(node_id)
                items.append(node)
                added += 1
            if len(items) >= total or (total == 0 and len(batch) < page_size):
                return items
            if not batch or added == 0:
                raise RuntimeError(f"egress node pagination stopped at {len(items)} of {total}")
            page += 1

    def fixed_fallback_node_ids(self) -> set[str]:
        payload = self._request("GET", f"{INTERNAL_API_PREFIX}/egress-operations")
        result: set[str] = set()
        for fallback in (payload.get("fallbacks") or {}).values():
            if not isinstance(fallback, dict) or fallback.get("mode") != "fixed":
                continue
            node_id = str(fallback.get("nodeId") or "")
            if node_id:
                result.add(node_id)
        return result

    def quality_test(self, node_id: str, profile_id: str = "") -> dict[str, Any]:
        body = {"profileId": profile_id} if profile_id else {}
        return self._request("POST", f"{INTERNAL_API_PREFIX}/egress-nodes/{node_id}/quality-test", body or None)

    def connectivity_test(self, node_id: str) -> dict[str, Any]:
        return self._request("POST", f"{INTERNAL_API_PREFIX}/egress-nodes/{node_id}/test")

    def list_audits(self, cursor: str = "") -> dict[str, Any]:
        query = {
            "pagination": "cursor",
            "pageSize": self.config.passive_page_size,
            "period": "24h",
        }
        if cursor:
            query["cursor"] = cursor
        return self._request("GET", f"{INTERNAL_API_PREFIX}/request-audits?{urllib.parse.urlencode(query)}")

    def set_enabled(self, node_id: str, enabled: bool) -> int:
        result = self._request("PATCH", f"{INTERNAL_API_PREFIX}/egress-nodes/batch", {"ids": [node_id], "enabled": enabled})
        return int(result.get("updated") or 0)

    def rotate_node(self, node_id: str, old_exit_ip: str = "") -> dict[str, Any]:
        if not self.config.rotation_url:
            raise RuntimeError("rotation endpoint is not configured")
        data = json.dumps({"nodeId": node_id, "oldExitIp": old_exit_ip}, separators=(",", ":")).encode()
        headers = {"Accept": "application/json", "Content-Type": "application/json"}
        if self.config.rotation_token:
            headers["Authorization"] = f"Bearer {self.config.rotation_token}"
        request = urllib.request.Request(self.config.rotation_url, data=data, headers=headers, method="POST")
        try:
            with urllib.request.urlopen(
                request,
                timeout=self.config.rotation_timeout_seconds,
                context=self.ssl_context,
            ) as response:
                payload = json.load(response)
        except urllib.error.HTTPError as exc:
            try:
                payload = json.loads(exc.read().decode("utf-8", "replace"))
            except (ValueError, OSError):
                payload = {}
            raise RuntimeError(f"rotation failed: HTTP {exc.code} {payload.get('error', 'request failed')}") from exc
        except (urllib.error.URLError, TimeoutError, OSError, ValueError) as exc:
            raise RuntimeError(f"rotation failed: {type(exc).__name__}") from exc
        if not isinstance(payload, dict) or not bool(payload.get("changed")):
            raise RuntimeError("rotation did not confirm an exit IP change")
        return payload


def classify_result(result: dict[str, Any], config: Config, profile: dict[str, Any] | None = None) -> tuple[str, str]:
    expected = ""
    if profile:
        expected = str(profile.get("expected_text") or profile.get("expected") or "").strip()
    if expected and not bool(result.get("expectedMatched")):
        return "hard", "expected_marker_missing"
    if profile is None and not bool(result.get("expectedMatched")):
        return "soft", "expected_marker_missing"
    output_tokens = int(result.get("outputTokens") or result.get("visibleTokens") or 0)
    speed_value = result.get("outputTokensPerSecond")
    if speed_value is None:
        # Rolling upgrades may still expose panel-equivalent TPS under the legacy name.
        speed_value = result.get("visibleTokensPerSecond")
    speed = float(speed_value or 0.0)
    generation_ms = int(result.get("generationMs") or 0)
    if generation_ms <= 0:
        generation_ms = max(0, int(result.get("durationMs") or 0) - int(result.get("firstTokenMs") or 0))
    # QUALITY_OK is a content marker, not a quality proof. Apply the same
    # token / window / TPS rules used for user-traffic audits.
    if output_tokens < 32:
        return "soft", "insufficient_output_tokens"
    reasoning_tokens = max(0, int(result.get("reasoningTokens") or result.get("reasoning_tokens") or 0))
    if "thinkingRequired" in result:
        require_thinking = bool(result.get("thinkingRequired"))
    else:
        # Rolling-upgrade compatibility with an older main service.
        require_thinking = bool(profile and profile.get("require_thinking"))
    if require_thinking and output_tokens >= THINKING_GUARD_MIN_OUTPUT_TOKENS and reasoning_tokens <= 0:
        return "hard", "missing_thinking"
    if config.fail_closed and generation_ms < config.min_generation_ms and speed >= config.soft_tps:
        return "hard", "buffered_burst"
    if config.fail_closed and generation_ms < config.min_generation_ms:
        return "soft", "insufficient_generation_window"
    if speed >= config.hard_tps:
        return "hard", "hard_tps"
    if speed >= config.soft_tps:
        return "soft", "soft_tps"
    return "healthy", "within_threshold"


def builtin_probe_profiles() -> dict[str, dict[str, Any]]:
    return {
        QUALITY_MARKER_PROFILE_ID: {
            "id": QUALITY_MARKER_PROFILE_ID,
            "built_in": True,
            "expected_text": "QUALITY_OK",
            "match_mode": "last_line",
            "require_thinking": True,
        },
        THROUGHPUT_PROFILE_ID: {
            "id": THROUGHPUT_PROFILE_ID,
            "built_in": True,
            "expected_text": "",
            "match_mode": "contains",
            "require_thinking": False,
        },
    }


def load_probe_profiles(path: Path) -> dict[str, Any]:
    data = {"version": 1, "active_profile_id": QUALITY_MARKER_PROFILE_ID, "profiles": {}}
    try:
        with path.open("r", encoding="utf-8") as handle:
            loaded = json.load(handle)
        if isinstance(loaded, dict):
            data.update(loaded)
    except FileNotFoundError:
        pass
    except (OSError, ValueError):
        pass
    profiles = data.get("profiles")
    if not isinstance(profiles, dict):
        profiles = {}
        data["profiles"] = profiles
    for profile_id, builtin in builtin_probe_profiles().items():
        current = profiles.get(profile_id)
        if not isinstance(current, dict):
            profiles[profile_id] = builtin
            continue
        # Reserved built-in IDs are part of the recovery safety boundary.
        # Canonicalize them even when an older writer or a manual edit cleared
        # built_in, otherwise a forged quality-marker could bypass recovery.
        current.update(builtin)
    active_profile_id = str(data.get("active_profile_id") or QUALITY_MARKER_PROFILE_ID)
    if active_profile_id not in profiles:
        data["active_profile_id"] = QUALITY_MARKER_PROFILE_ID
    return data


def resolve_probe_profile(path: Path, profile_id: str = "") -> tuple[str, dict[str, Any] | None]:
    data = load_probe_profiles(path)
    profiles = data.get("profiles") if isinstance(data.get("profiles"), dict) else {}
    active = str(data.get("active_profile_id") or QUALITY_MARKER_PROFILE_ID)
    chosen = str(profile_id or active)
    profile = profiles.get(chosen)
    if isinstance(profile, dict):
        return chosen, profile
    return chosen, None


def classify_audit(value: dict[str, Any], config: Config) -> tuple[str, str, float, int]:
    if value.get("provider") != "grok_build" or not bool(value.get("streaming")):
        return "ignored", "not_build_stream", 0.0, 0
    status = int(value.get("statusCode") or 0)
    if status < 200 or status >= 300 or value.get("errorCode"):
        return "ignored", "unsuccessful", 0.0, 0
    first_token_ms = value.get("firstTokenMs")
    if first_token_ms is None:
        return "ignored", "missing_first_token", 0.0, 0
    generation_ms = int(value.get("durationMs") or 0) - int(first_token_ms)
    output_tokens = max(0, int(value.get("outputTokens") or 0))
    if generation_ms <= 0 or output_tokens < 32:
        return "ignored", "insufficient_output_tokens", 0.0, output_tokens
    speed = float(output_tokens) * 1000 / float(generation_ms)
    if config.fail_closed and generation_ms < config.min_generation_ms and speed >= config.soft_tps:
        return "hard", "buffered_burst", speed, output_tokens
    if speed >= config.hard_tps:
        return "hard", "hard_tps", speed, output_tokens
    if speed >= config.soft_tps:
        return "soft", "soft_tps", speed, output_tokens
    return "healthy", "within_threshold", speed, output_tokens


def default_node_state() -> dict[str, Any]:
    return {
        "active_soft_strikes": 0,
        "passive_soft_strikes": 0,
        "error_strikes": 0,
        "quarantined_until": 0.0,
        "disabled_by_guard": False,
        "last_reason": "",
        "last_probe_at": 0.0,
        "last_observed_at": 0.0,
        "last_source": "",
        "last_classification": "",
        "last_output_tps": 0.0,
        "last_output_tokens": 0,
        "last_first_token_ms": 0,
        "last_duration_ms": 0,
        "last_rotation_at": 0.0,
        "last_rotation_exit_ip": "",
        "rotation_failures": 0,
        "last_no_account_log_at": 0.0,
        "quarantine_source": "",
        "passive_degrade_repeats": 0,
    }


def default_statistics() -> dict[str, Any]:
    return {
        "started_at": time.time(),
        "active": {"total": 0, "healthy": 0, "soft": 0, "hard": 0, "errors": 0, "output_tokens": 0},
        "passive": {"total": 0, "healthy": 0, "soft": 0, "hard": 0, "errors": 0, "output_tokens": 0},
        "actions": {"quarantined": 0, "restored": 0, "suppressed": 0},
    }


def ensure_statistics(state: dict[str, Any]) -> dict[str, Any]:
    defaults = default_statistics()
    statistics = state.setdefault("statistics", {})
    if not isinstance(statistics, dict):
        raise RuntimeError("invalid quality guard statistics")
    statistics.setdefault("started_at", defaults["started_at"])
    for group_name in ("active", "passive", "actions"):
        group = statistics.setdefault(group_name, {})
        if not isinstance(group, dict):
            raise RuntimeError("invalid quality guard statistics")
        if group_name in {"active", "passive"}:
            legacy_tokens = int(group.pop("visible_tokens", 0))
            group.setdefault("output_tokens", legacy_tokens)
        for field, default in defaults[group_name].items():
            group.setdefault(field, default)
            if isinstance(group[field], bool) or not isinstance(group[field], int) or group[field] < 0:
                raise RuntimeError("invalid quality guard statistics")
    return statistics


def load_state(path: Path) -> dict[str, Any]:
    try:
        with path.open("r", encoding="utf-8") as handle:
            value = json.load(handle)
    except FileNotFoundError:
        return {"version": 1, "nodes": {}, "passive_initialized": False, "seen_audit_ids": []}
    except (OSError, ValueError) as exc:
        raise RuntimeError(f"cannot read state file: {type(exc).__name__}") from exc
    if value.get("version") != 1 or not isinstance(value.get("nodes"), dict):
        raise RuntimeError("unsupported state file format")
    value.setdefault("passive_initialized", False)
    value.setdefault("seen_audit_ids", [])
    if "last_active_cycle_at" not in value:
        value["last_active_cycle_at"] = max(
            (float(node.get("last_probe_at", 0.0)) for node in value["nodes"].values()),
            default=0.0,
        )
    if not isinstance(value["seen_audit_ids"], list):
        raise RuntimeError("invalid passive audit state")
    ensure_statistics(value)
    return value


def save_state(path: Path, state: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    descriptor, temporary = tempfile.mkstemp(prefix=".state-", dir=path.parent)
    try:
        os.fchmod(descriptor, 0o600)
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            json.dump(state, handle, ensure_ascii=True, sort_keys=True, separators=(",", ":"))
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
    except Exception:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass
        raise


def append_state_event(state: dict[str, Any], event: str, **fields: Any) -> None:
    events = state.setdefault("recent_events", [])
    events.append({"ts": time.time(), "event": event, **fields})
    del events[:-100]


def log_event(event: str, **fields: Any) -> None:
    payload = {"ts": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "event": event, **fields}
    print(json.dumps(payload, ensure_ascii=True, sort_keys=True, separators=(",", ":")), flush=True)


class Guard:
    def __init__(self, config: Config, api: ApiClient):
        self.config = config
        self.api = api
        self.state = load_state(config.state_file)
        self._resolved_node_ids = list(config.node_ids)
        self.state.setdefault("started_at", time.time())
        self.state.setdefault("recent_events", [])
        ensure_statistics(self.state)
        self._update_guard_metadata()
        self._save()

    def _bump_statistic(self, group: str, field: str, amount: int = 1) -> None:
        statistics = ensure_statistics(self.state)
        statistics[group][field] = int(statistics[group][field]) + amount

    def _update_guard_metadata(self) -> None:
        self.state["updated_at"] = time.time()
        self.state["guard"] = {
            "mode": self.config.mode,
            "model": self.config.model,
            "node_ids": list(self.config.node_ids) if self.config.node_ids else self._resolved_node_ids,
            "active_interval_seconds": self.config.active_interval_seconds,
            "passive_poll_seconds": self.config.passive_poll_seconds,
            "soft_tps": self.config.soft_tps,
            "hard_tps": self.config.hard_tps,
            "consecutive_soft": self.config.consecutive_soft,
            "consecutive_errors": self.config.consecutive_errors,
            "quarantine_seconds": self.config.quarantine_seconds,
            "no_account_backoff_seconds": self.config.no_account_backoff_seconds,
            "min_healthy_nodes": self.config.min_healthy_nodes,
            "max_output_tokens": self.config.max_output_tokens,
            "fail_closed": self.config.fail_closed,
            "min_generation_ms": self.config.min_generation_ms,
            "rotatable_node_ids": list(self.config.rotatable_node_ids),
            "prompt": self.config.prompt,
            "expected": self.config.expected,
            "active_profile_id": resolve_probe_profile(self.config.profiles_file)[0],
        }

    def _save(self) -> None:
        self._update_guard_metadata()
        save_state(self.config.state_file, self.state)

    def _state_for(self, node_id: str) -> dict[str, Any]:
        nodes = self.state.setdefault("nodes", {})
        current = nodes.setdefault(node_id, default_node_state())
        legacy_strikes = int(current.pop("soft_strikes", 0))
        current.setdefault("active_soft_strikes", legacy_strikes)
        current.setdefault("passive_soft_strikes", 0)
        current.setdefault("last_output_tps", float(current.pop("last_visible_tps", 0.0)))
        current.setdefault("last_output_tokens", int(current.pop("last_visible_tokens", 0)))
        for key, value in default_node_state().items():
            current.setdefault(key, value)
        return current

    def _defer_no_account(self, state: dict[str, Any], node: dict[str, Any], now: float, event: str, **fields: Any) -> None:
        state["last_probe_at"] = now
        state["last_reason"] = "probe_no_account"
        state["quarantined_until"] = max(
            float(state.get("quarantined_until", 0.0)),
            now + self.config.no_account_backoff_seconds,
        )
        last_logged = float(state.get("last_no_account_log_at", 0.0))
        if last_logged <= 0 or now - last_logged >= self.config.no_account_backoff_seconds:
            state["last_no_account_log_at"] = now
            log_event(event, node_id=str(node["id"]), node_name=node.get("name"), reason="probe_no_account", **fields)

    def _eligible_nodes(self, nodes: list[dict[str, Any]], protected_node_ids: set[str]) -> list[dict[str, Any]]:
        configured = set(self.config.node_ids)
        state_nodes = self.state.get("nodes") or {}
        result = []
        for node in nodes:
            node_id = str(node.get("id") or "")
            if not node_id or not node.get("proxyConfigured"):
                continue
            tracked_quarantine = bool((state_nodes.get(node_id) or {}).get("disabled_by_guard"))
            if node_id in protected_node_ids and not tracked_quarantine:
                continue
            # A node removed from the configured set while quarantined remains
            # managed until it has passed recovery. Otherwise configuration
            # changes could strand a guard-owned disabled node forever.
            if configured and node_id not in configured and not tracked_quarantine:
                continue
            if node.get("enabled") or tracked_quarantine:
                result.append(node)
        return result

    def _can_quarantine(self, nodes: list[dict[str, Any]], node_id: str) -> bool:
        enabled = sum(1 for node in nodes if bool(node.get("enabled")))
        target_enabled = any(str(node.get("id")) == node_id and bool(node.get("enabled")) for node in nodes)
        if self.config.fail_closed:
            return target_enabled
        return target_enabled and enabled - 1 >= self.config.min_healthy_nodes

    def _should_rotate(self, node_id: str, reason: str) -> bool:
        return (
            bool(self.config.rotation_url)
            and node_id in set(self.config.rotatable_node_ids)
            and reason in {
                "hard_tps", "soft_tps", "buffered_burst", "missing_thinking", "expected_marker_missing",
                "insufficient_output_tokens", "insufficient_generation_window", "probe_errors",
                "recovery_probe_error", "rotation_error",
            }
        )

    @staticmethod
    def _probe_account_unavailable(exc: Exception) -> bool:
        return isinstance(exc, ApiError) and exc.code == "egressQualityProbeNoAccount"

    def _quarantine(self, nodes: list[dict[str, Any]], node: dict[str, Any], reason: str, now: float, recover_now: bool = True) -> None:
        node_id = str(node["id"])
        state = self._state_for(node_id)
        if not self._can_quarantine(nodes, node_id):
            self._bump_statistic("actions", "suppressed")
            log_event("quarantine_suppressed", node_id=node_id, node_name=node.get("name"), reason=reason, minimum_healthy=self.config.min_healthy_nodes)
            return
        previous_state = dict(state)
        source = "passive" if not recover_now else "active"
        repeats = int(state.get("passive_degrade_repeats", 0))
        if source == "passive":
            repeats += 1
        scale = min(8, 2 ** max(0, repeats - 1)) if source == "passive" else 1
        state.update({
            "active_soft_strikes": 0,
            "passive_soft_strikes": 0,
            "error_strikes": 0,
            "quarantined_until": now + self.config.quarantine_seconds * scale,
            "disabled_by_guard": True,
            "last_reason": reason,
            "quarantine_source": source,
            "passive_degrade_repeats": repeats if source == "passive" else int(state.get("passive_degrade_repeats", 0)),
        })
        # Persist ownership before changing backend scheduling state. A crash
        # after the API call can then be reconciled safely on restart.
        self._save()
        try:
            updated = self.api.set_enabled(node_id, False)
        except Exception as exc:
            state.clear()
            state.update(previous_state)
            self._save()
            log_event("quarantine_failed", node_id=node_id, node_name=node.get("name"), reason=reason, error_type=type(exc).__name__)
            return
        if updated != 1:
            state.clear()
            state.update(previous_state)
            self._save()
            log_event("quarantine_not_applied", node_id=node_id, node_name=node.get("name"), reason=reason, updated=updated)
            return
        node["enabled"] = False
        self._bump_statistic("actions", "quarantined")
        append_state_event(self.state, "node_quarantined", node_id=node_id, node_name=node.get("name"), reason=reason)
        self._save()
        log_event(
            "node_quarantined",
            node_id=node_id,
            node_name=node.get("name"),
            reason=reason,
            quarantine_seconds=self.config.quarantine_seconds,
            quarantine_hold_seconds=self.config.quarantine_seconds * scale,
            quarantine_source=source,
            degrade_repeats=repeats if source == "passive" else 0,
            recover_now=recover_now,
        )
        if recover_now:
            if reason == "buffered_burst":
                self._recover_quarantined(node, time.time(), rotate=False, rotate_on_failure=True)
            elif self._should_rotate(node_id, reason):
                self._recover_quarantined(node, time.time(), rotate=True)
        elif self._should_rotate(node_id, reason):
            try:
                rotation = self.api.rotate_node(node_id, str(node.get("exitIp") or ""))
            except Exception as exc:
                state["rotation_failures"] = int(state.get("rotation_failures", 0)) + 1
                log_event("node_rotation_failed", node_id=node_id, node_name=node.get("name"), error_type=type(exc).__name__, trigger="passive_hold")
            else:
                state.update({
                    "last_rotation_at": time.time(),
                    "last_rotation_exit_ip": str(rotation.get("newExitIp") or ""),
                    "rotation_failures": 0,
                })
                append_state_event(self.state, "node_rotated", node_id=node_id, node_name=node.get("name"), exit_ip=str(rotation.get("newExitIp") or ""))
                self._save()
                log_event("node_rotated", node_id=node_id, node_name=node.get("name"), exit_ip=str(rotation.get("newExitIp") or ""), trigger="passive_hold")

    def _record_probe(self, node: dict[str, Any], result: dict[str, Any], classification: str, reason: str, now: float) -> None:
        node_id = str(node["id"])
        state = self._state_for(node_id)
        output_tokens = int(result.get("outputTokens") or result.get("visibleTokens") or 0)
        output_tps_value = result.get("outputTokensPerSecond")
        if output_tps_value is None:
            output_tps_value = result.get("visibleTokensPerSecond")
        output_tps = float(output_tps_value or 0.0)
        state["last_probe_at"] = now
        state.update({
            "last_observed_at": now,
            "last_source": "active",
            "last_classification": classification,
            "last_output_tps": round(output_tps, 3),
            "last_output_tokens": output_tokens,
            "last_reasoning_tokens": max(0, int(result.get("reasoningTokens") or result.get("reasoning_tokens") or 0)),
            "last_first_token_ms": int(result.get("firstTokenMs") or 0),
            "last_duration_ms": int(result.get("durationMs") or 0),
        })
        state["error_strikes"] = 0
        self._bump_statistic("active", classification)
        self._bump_statistic("active", "output_tokens", output_tokens)
        if classification == "healthy":
            state["active_soft_strikes"] = 0
            state["passive_soft_strikes"] = 0
        elif classification == "soft":
            state["active_soft_strikes"] = int(state.get("active_soft_strikes", 0)) + 1
        else:
            state["active_soft_strikes"] = self.config.consecutive_soft
        log_event(
            "quality_probe_completed",
            node_id=node_id,
            node_name=node.get("name"),
            classification=classification,
            reason=reason,
            output_tps=round(output_tps, 3),
            output_tokens=output_tokens,
            reasoning_tokens=max(0, int(result.get("reasoningTokens") or result.get("reasoning_tokens") or 0)),
            first_token_ms=int(result.get("firstTokenMs") or 0),
            duration_ms=int(result.get("durationMs") or 0),
            chunk_count=int(result.get("chunkCount") or 0),
            expected_matched=bool(result.get("expectedMatched")),
        )

    def _probe_active(self, nodes: list[dict[str, Any]], node: dict[str, Any], now: float, trigger: str = "scheduled") -> None:
        node_id = str(node["id"])
        state = self._state_for(node_id)
        if state.get("last_reason") == "probe_no_account" and now < float(state.get("quarantined_until", 0.0)):
            return
        self._bump_statistic("active", "total")
        profile_id, profile = resolve_probe_profile(self.config.profiles_file)
        try:
            result = self.api.quality_test(node_id, profile_id)
        except Exception as exc:
            if self._probe_account_unavailable(exc):
                self._defer_no_account(state, node, now, "quality_probe_deferred", trigger=trigger)
                return
            self._bump_statistic("active", "errors")
            state["error_strikes"] = int(state.get("error_strikes", 0)) + 1
            state["last_probe_at"] = now
            log_event("quality_probe_failed", node_id=node_id, node_name=node.get("name"), trigger=trigger, error_type=type(exc).__name__, strikes=state["error_strikes"])
            if trigger == "scheduled" and state["error_strikes"] >= self.config.consecutive_errors:
                self._quarantine(nodes, node, "probe_errors", now)
            return
        classification, reason = classify_result(result, self.config, profile)
        self._record_probe(node, result, classification, reason, now)
        if classification == "hard" or (
            classification == "soft" and self.config.fail_closed
        ) or int(state.get("active_soft_strikes", 0)) >= self.config.consecutive_soft:
            self._quarantine(nodes, node, reason, now)

    def _recover_quarantined(
        self,
        node: dict[str, Any],
        now: float,
        rotate: bool,
        rotate_on_failure: bool = False,
    ) -> None:
        node_id = str(node["id"])
        state = self._state_for(node_id)
        if rotate:
            try:
                rotation = self.api.rotate_node(node_id, str(node.get("exitIp") or ""))
            except Exception as exc:
                state["rotation_failures"] = int(state.get("rotation_failures", 0)) + 1
                state["quarantined_until"] = now + self.config.quarantine_seconds
                state["last_reason"] = "rotation_error"
                log_event("node_rotation_failed", node_id=node_id, node_name=node.get("name"), error_type=type(exc).__name__)
                return
            state.update({
                "last_rotation_at": time.time(),
                "last_rotation_exit_ip": str(rotation.get("newExitIp") or ""),
                "rotation_failures": 0,
            })
            append_state_event(
                self.state,
                "node_rotated",
                node_id=node_id,
                node_name=node.get("name"),
                exit_ip=str(rotation.get("newExitIp") or ""),
            )
            log_event("node_rotated", node_id=node_id, node_name=node.get("name"), exit_ip=str(rotation.get("newExitIp") or ""))
        try:
            try:
                connectivity = self.api.connectivity_test(node_id)
                connectivity_status = str(connectivity.get("status") or "unknown")
            except Exception as exc:
                connectivity_status = "error"
                log_event("recovery_connectivity_probe_failed", node_id=node_id, node_name=node.get("name"), error_type=type(exc).__name__)
            self._bump_statistic("active", "total")
            profile_id, profile = resolve_probe_profile(self.config.profiles_file, QUALITY_MARKER_PROFILE_ID)
            result = self.api.quality_test(node_id, profile_id)
            classification, reason = classify_result(result, self.config, profile)
            self._record_probe(node, result, classification, reason, now)
        except Exception as exc:
            if self._probe_account_unavailable(exc):
                self._defer_no_account(state, node, now, "recovery_probe_deferred")
                return
            self._bump_statistic("active", "errors")
            state["quarantined_until"] = now + self.config.quarantine_seconds
            state["last_reason"] = "recovery_probe_error"
            log_event("recovery_probe_failed", node_id=node_id, node_name=node.get("name"), error_type=type(exc).__name__)
            return
        if classification != "healthy":
            state["quarantined_until"] = now + self.config.quarantine_seconds
            state["last_reason"] = reason
            log_event("quarantine_extended", node_id=node_id, node_name=node.get("name"), reason=reason)
            if rotate_on_failure and self._should_rotate(node_id, reason):
                self._recover_quarantined(node, time.time(), rotate=True)
            return
        after_passive = self._is_passive_quarantine(state)
        updated = self.api.set_enabled(node_id, True)
        if updated != 1:
            log_event("restore_not_applied", node_id=node_id, node_name=node.get("name"), updated=updated)
            return
        state.update({
            "active_soft_strikes": 0,
            "passive_soft_strikes": 0,
            "error_strikes": 0,
            "quarantined_until": 0.0,
            "disabled_by_guard": False,
            "last_reason": "",
            "quarantine_source": "",
        })
        node["enabled"] = True
        self._bump_statistic("actions", "restored")
        append_state_event(self.state, "node_restored", node_id=node_id, node_name=node.get("name"), reason="quality_probe_healthy")
        log_event(
            "node_restored",
            node_id=node_id,
            node_name=node.get("name"),
            connectivity_status=connectivity_status,
            reason="quality_probe_healthy",
            after_passive_hold=after_passive,
            expected_matched=bool(result.get("expectedMatched")),
        )

    @staticmethod
    def _is_passive_quarantine(state: dict[str, Any]) -> bool:
        source = str(state.get("quarantine_source") or "")
        if source == "passive":
            return True
        if source == "active":
            return False
        return str(state.get("last_source") or "") == "passive"

    def _probe_quarantined(self, node: dict[str, Any], now: float) -> None:
        node_id = str(node["id"])
        state = self._state_for(node_id)
        if now < float(state.get("quarantined_until", 0.0)):
            return
        reason = str(state.get("last_reason") or "")
        passive = self._is_passive_quarantine(state)
        # Dual probe: user traffic isolates immediately; after the hold a
        # QUALITY_OK probe must come back healthy before restore. Passive
        # isolate already rotated, so only rotate again if the probe fails.
        self._recover_quarantined(
            node,
            now,
            rotate=(not passive) and self._should_rotate(node_id, reason) and reason != "buffered_burst",
            rotate_on_failure=passive or reason == "buffered_burst",
        )

    def _prepare_nodes(self, now: float) -> tuple[list[dict[str, Any]], list[dict[str, Any]], set[str]]:
        all_nodes = self.api.list_nodes()
        protected_node_ids = self.api.fixed_fallback_node_ids()
        previous_protected = set(str(value) for value in self.state.get("protected_node_ids", []))
        if protected_node_ids != previous_protected:
            self.state["protected_node_ids"] = sorted(protected_node_ids)
            for node_id in sorted(protected_node_ids - previous_protected):
                log_event("fixed_fallback_node_skipped", node_id=node_id)
        state_nodes = self.state.setdefault("nodes", {})
        # Making an enabled node a fixed fallback is an explicit operator
        # override. Relinquish stale guard ownership before eligibility checks
        # so strict mode cannot repeatedly attempt an invalid disable. A
        # protected node that is still disabled remains tracked until recovery.
        for node in all_nodes:
            node_id = str(node.get("id") or "")
            state = state_nodes.get(node_id) or {}
            if node_id not in protected_node_ids or not node.get("enabled") or not state.get("disabled_by_guard"):
                continue
            state.update({
                "active_soft_strikes": 0,
                "passive_soft_strikes": 0,
                "error_strikes": 0,
                "quarantined_until": 0.0,
                "disabled_by_guard": False,
                "last_reason": "",
            })
            log_event("fixed_fallback_guard_released", node_id=node_id, node_name=node.get("name"))
        if not self.config.node_ids:
            self._resolved_node_ids = [
                str(node["id"]) for node in all_nodes
                if node.get("id") and node.get("proxyConfigured") and str(node["id"]) not in protected_node_ids
            ]
        nodes = self._eligible_nodes(all_nodes, protected_node_ids)
        present_ids = {str(node.get("id")) for node in all_nodes if node.get("id")}
        managed_ids = {str(node.get("id")) for node in nodes if node.get("id")}
        for stale_id in list(state_nodes):
            tracked = bool((state_nodes.get(stale_id) or {}).get("disabled_by_guard"))
            if stale_id not in present_ids or (stale_id not in managed_ids and not tracked):
                del state_nodes[stale_id]
        skip_ids: set[str] = set()
        if not nodes:
            log_event("no_eligible_nodes")
            return all_nodes, [], skip_ids
        for node in nodes:
            node_id = str(node["id"])
            state = self._state_for(node_id)
            if state.get("disabled_by_guard") and node.get("enabled"):
                if self.config.fail_closed:
                    updated = self.api.set_enabled(node_id, False)
                    if updated == 1:
                        node["enabled"] = False
                        log_event("operator_reenable_requires_probe", node_id=node_id, node_name=node.get("name"))
                        if now >= float(state.get("quarantined_until", 0.0)):
                            reason = str(state.get("last_reason") or "")
                            passive = self._is_passive_quarantine(state)
                            self._recover_quarantined(
                                node,
                                now,
                                rotate=(not passive) and self._should_rotate(node_id, reason) and reason != "buffered_burst",
                                rotate_on_failure=passive or reason == "buffered_burst",
                            )
                    skip_ids.add(node_id)
                    continue
                state.update({
                    "active_soft_strikes": 0,
                    "passive_soft_strikes": 0,
                    "error_strikes": 0,
                    "quarantined_until": 0.0,
                    "disabled_by_guard": False,
                    "last_reason": "",
                })
                log_event("operator_reenabled_node", node_id=node_id, node_name=node.get("name"))
                skip_ids.add(node_id)
                continue
            if state.get("disabled_by_guard"):
                skip_ids.add(node_id)
                self._probe_quarantined(node, now)
        return all_nodes, nodes, skip_ids

    def run_active_cycle(self) -> None:
        now = time.time()
        all_nodes, nodes, skip_ids = self._prepare_nodes(now)
        for node in nodes:
            node_id = str(node["id"])
            state = self._state_for(node_id)
            if node_id not in skip_ids and node.get("enabled") and not state.get("disabled_by_guard"):
                self._probe_active(all_nodes, node, now)
            self._save()
        self.state["last_active_cycle_at"] = time.time()
        self._save()

    def _fetch_new_audits(self) -> list[dict[str, Any]]:
        known = set(str(value) for value in self.state.get("seen_audit_ids", []))
        fetched_ids: list[str] = []
        collected: list[dict[str, Any]] = []
        cursor = ""
        reached_known = False
        for _page in range(self.config.passive_max_pages):
            page = self.api.list_audits(cursor)
            items = list(page.get("items") or [])
            if not items:
                break
            for item in items:
                audit_id = str(item.get("id") or item.get("requestId") or "")
                if not audit_id:
                    continue
                fetched_ids.append(audit_id)
                if audit_id in known:
                    reached_known = True
                    break
                collected.append(item)
            if reached_known or not page.get("hasMore"):
                break
            cursor = str(page.get("nextCursor") or "")
            if not cursor:
                break

        combined = []
        seen = set()
        for audit_id in [*fetched_ids, *self.state.get("seen_audit_ids", [])]:
            audit_id = str(audit_id)
            if audit_id and audit_id not in seen:
                seen.add(audit_id)
                combined.append(audit_id)
            if len(combined) >= 2000:
                break
        self.state["seen_audit_ids"] = combined
        if not self.state.get("passive_initialized"):
            self.state["passive_initialized"] = True
            log_event("passive_baseline_initialized", audit_count=len(fetched_ids))
            return []
        if collected and not reached_known and known:
            log_event("passive_audit_gap", collected=len(collected), max_pages=self.config.passive_max_pages)
        collected.reverse()
        return collected

    def _record_passive_audit(self, all_nodes: list[dict[str, Any]], node: dict[str, Any], audit_value: dict[str, Any], now: float) -> None:
        node_id = str(node["id"])
        state = self._state_for(node_id)
        classification, reason, speed, output_tokens = classify_audit(audit_value, self.config)
        if classification == "ignored":
            return
        self._bump_statistic("passive", "total")
        self._bump_statistic("passive", classification)
        self._bump_statistic("passive", "output_tokens", output_tokens)
        state.update({
            "last_observed_at": now,
            "last_source": "passive",
            "last_classification": classification,
            "last_output_tps": round(speed, 3),
            "last_output_tokens": output_tokens,
            "last_first_token_ms": int(audit_value.get("firstTokenMs") or 0),
            "last_duration_ms": int(audit_value.get("durationMs") or 0),
        })
        if classification == "healthy":
            state["passive_soft_strikes"] = 0
            state["passive_degrade_repeats"] = 0
            return
        if classification == "soft":
            state["passive_soft_strikes"] = int(state.get("passive_soft_strikes", 0)) + 1
        else:
            state["passive_soft_strikes"] = self.config.consecutive_soft
        append_state_event(
            self.state,
            "passive_audit_anomaly",
            node_id=node_id,
            node_name=node.get("name"),
            reason=reason,
            classification=classification,
            output_tps=round(speed, 3),
        )
        log_event(
            "passive_audit_anomaly",
            request_id=audit_value.get("requestId"),
            node_id=node_id,
            node_name=node.get("name"),
            classification=classification,
            reason=reason,
            output_tps=round(speed, 3),
            output_tokens=output_tokens,
            first_token_ms=int(audit_value.get("firstTokenMs") or 0),
            duration_ms=int(audit_value.get("durationMs") or 0),
            strikes=int(state.get("passive_soft_strikes", 0)),
        )
        log_event(
            "passive_immediate_quarantine",
            node_id=node_id,
            node_name=node.get("name"),
            classification=classification,
            reason=reason,
            output_tps=round(speed, 3),
        )
        self._quarantine(all_nodes, node, reason, now, recover_now=False)

    def run_passive_cycle(self) -> None:
        now = time.time()
        self.state["last_passive_poll_at"] = now
        all_nodes, nodes, _skip_ids = self._prepare_nodes(now)
        node_by_id = {str(node["id"]): node for node in nodes}
        audits = self._fetch_new_audits()
        for value in audits:
            if bool(value.get("qualityProbe")):
                continue
            node = node_by_id.get(str(value.get("egressNodeId") or ""))
            if node is None or not node.get("enabled"):
                continue
            self._record_passive_audit(all_nodes, node, value, now)
        self._save()

    # Backward-compatible name for callers that expect one active cycle.
    def run_cycle(self) -> None:
        self.run_active_cycle()


def acquire_lock(path: Path):
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    handle = path.open("a+", encoding="utf-8")
    os.chmod(path, 0o600)
    try:
        fcntl.flock(handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        handle.close()
        raise RuntimeError("another quality guard instance is already running")
    return handle


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Active and passive quality guard for grok2api egress nodes")
    parser.add_argument("--once", action="store_true", help="run one cycle for each detector enabled by the selected mode")
    parser.add_argument("--check-config", action="store_true", help="validate config.yaml bootstrap and exit")
    args = parser.parse_args(argv)
    try:
        base_config = Config.from_bootstrap()
        reloader = RuntimeConfigReloader(base_config)
        config, _, runtime_error = reloader.reload(force=True)
        if runtime_error is not None:
            raise ValueError(str(runtime_error))
    except GuardDisabled as exc:
        print(str(exc))
        return 0
    except ValueError as exc:
        print(f"configuration error: {exc}", file=sys.stderr)
        return 2
    if args.check_config:
        print("configuration is valid")
        return 0
    try:
        lock = acquire_lock(config.lock_file)
    except RuntimeError as exc:
        print(str(exc), file=sys.stderr)
        return 1
    _ = lock
    stopping = False

    def stop(_signum, _frame):
        nonlocal stopping
        stopping = True

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    api = ApiClient(config)
    guard = Guard(config, api)
    last_active_at = float(guard.state.get("last_active_cycle_at", 0.0))
    active_delay = max(0.0, last_active_at + config.active_interval_seconds - time.time())
    next_active = 0.0 if args.once else time.monotonic() + active_delay
    next_passive = 0.0
    log_event(
        "guard_started",
        mode=config.mode,
        active_interval_seconds=config.active_interval_seconds,
        passive_poll_seconds=config.passive_poll_seconds,
        node_count=len(config.node_ids),
        model=config.model,
    )
    while not stopping:
        now = time.monotonic()
        next_config, changed, runtime_error = reloader.reload()
        if runtime_error is not None:
            log_event("runtime_config_rejected", error_type=type(runtime_error).__name__)
        elif changed:
            previous_mode = config.mode
            config = next_config
            guard.config = config
            api.config = config
            guard._save()
            last_active_at = float(guard.state.get("last_active_cycle_at", 0.0))
            next_active = now + max(0.0, last_active_at + config.active_interval_seconds - time.time())
            next_passive = now
            log_event("runtime_config_reloaded", previous_mode=previous_mode, mode=config.mode)
        active_enabled = config.mode in {"active", "hybrid"}
        passive_enabled = config.mode in {"passive", "hybrid"}
        if passive_enabled and now >= next_passive:
            try:
                guard.run_passive_cycle()
            except Exception as exc:
                log_event("passive_cycle_failed", error_type=type(exc).__name__)
            next_passive = time.monotonic() + config.passive_poll_seconds
        if active_enabled and now >= next_active:
            try:
                guard.run_active_cycle()
            except Exception as exc:
                log_event("active_cycle_failed", error_type=type(exc).__name__)
            jitter = random.uniform(-config.jitter_seconds, config.jitter_seconds)
            next_active = time.monotonic() + max(60.0, config.active_interval_seconds + jitter)
        if args.once:
            break
        deadlines = []
        if passive_enabled:
            deadlines.append(next_passive)
        if active_enabled:
            deadlines.append(next_active)
        delay = max(0.1, min(deadlines) - time.monotonic()) if deadlines else 1.0
        time.sleep(min(1.0, delay))
    log_event("guard_stopped")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
