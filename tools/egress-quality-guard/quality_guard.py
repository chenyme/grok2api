#!/usr/bin/env python3
"""Active and passive quality guard for grok2api egress nodes.

The guard calls the administrator-only quality-test endpoint, quarantines
suspect nodes without deleting bindings, and restores only nodes it disabled.
It uses only Python's standard library.
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


DEFAULT_PROMPT = (
    "Write exactly 16 numbered lines about reliable distributed systems. "
    "Each line must be one complete English sentence, with no markdown heading. "
    "The final line must end with the exact marker QUALITY_OK."
)

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


def _env_int(name: str, default: int, minimum: int = 0) -> int:
    raw = os.getenv(name, str(default)).strip()
    try:
        value = int(raw)
    except ValueError as exc:
        raise ValueError(f"{name} must be an integer") from exc
    if value < minimum:
        raise ValueError(f"{name} must be >= {minimum}")
    return value


def _env_float(name: str, default: float, minimum: float = 0.0) -> float:
    raw = os.getenv(name, str(default)).strip()
    try:
        value = float(raw)
    except ValueError as exc:
        raise ValueError(f"{name} must be a number") from exc
    if value < minimum:
        raise ValueError(f"{name} must be >= {minimum}")
    return value


def _env_bool(name: str, default: bool = False) -> bool:
    raw = os.getenv(name)
    if raw is None:
        return default
    normalized = raw.strip().lower()
    if normalized in {"1", "true", "yes", "on"}:
        return True
    if normalized in {"0", "false", "no", "off"}:
        return False
    raise ValueError(f"{name} must be true or false")


@dataclasses.dataclass(frozen=True)
class Config:
    base_url: str
    username: str
    password: str
    client_key_id: str
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
    min_healthy_nodes: int
    max_output_tokens: int
    prompt: str
    expected: str
    state_file: Path
    lock_file: Path
    runtime_config_file: Path
    insecure_tls: bool

    @classmethod
    def from_env(cls) -> "Config":
        raw_nodes = os.getenv("QUALITY_GUARD_NODE_IDS", "")
        node_ids = tuple(dict.fromkeys(value.strip() for value in raw_nodes.split(",") if value.strip()))
        password = os.getenv("GROK2API_ADMIN_PASSWORD", "")
        password_file = os.getenv("GROK2API_ADMIN_PASSWORD_FILE", "").strip()
        if password_file:
            try:
                password = Path(password_file).read_text(encoding="utf-8").rstrip("\r\n")
            except OSError as exc:
                raise ValueError("cannot read GROK2API_ADMIN_PASSWORD_FILE") from exc
        legacy_interval = os.getenv("QUALITY_GUARD_INTERVAL_SECONDS", "").strip()
        active_interval_default = int(legacy_interval) if legacy_interval else 1800
        config = cls(
            base_url=os.getenv("GROK2API_BASE_URL", "http://127.0.0.1:8000").strip().rstrip("/"),
            username=os.getenv("GROK2API_ADMIN_USERNAME", "").strip(),
            password=password,
            client_key_id=os.getenv("QUALITY_GUARD_CLIENT_KEY_ID", "").strip(),
            model=os.getenv("QUALITY_GUARD_MODEL", "grok-4.5").strip(),
            node_ids=node_ids,
            mode=os.getenv("QUALITY_GUARD_MODE", "hybrid").strip().lower(),
            active_interval_seconds=_env_int("QUALITY_GUARD_ACTIVE_INTERVAL_SECONDS", active_interval_default, 60),
            passive_poll_seconds=_env_int("QUALITY_GUARD_PASSIVE_POLL_SECONDS", 5, 1),
            passive_page_size=_env_int("QUALITY_GUARD_PASSIVE_PAGE_SIZE", 200, 20),
            passive_max_pages=_env_int("QUALITY_GUARD_PASSIVE_MAX_PAGES", 10, 1),
            jitter_seconds=_env_int("QUALITY_GUARD_JITTER_SECONDS", 30, 0),
            request_timeout_seconds=_env_int("QUALITY_GUARD_REQUEST_TIMEOUT_SECONDS", 120, 10),
            soft_tps=_env_float("QUALITY_GUARD_SOFT_TPS", 500.0, 1.0),
            hard_tps=_env_float("QUALITY_GUARD_HARD_TPS", 1000.0, 1.0),
            consecutive_soft=_env_int("QUALITY_GUARD_CONSECUTIVE_SOFT", 2, 1),
            consecutive_errors=_env_int("QUALITY_GUARD_CONSECUTIVE_ERRORS", 2, 1),
            quarantine_seconds=_env_int("QUALITY_GUARD_QUARANTINE_SECONDS", 300, 30),
            min_healthy_nodes=_env_int("QUALITY_GUARD_MIN_HEALTHY_NODES", 3, 1),
            max_output_tokens=_env_int("QUALITY_GUARD_MAX_OUTPUT_TOKENS", 384, 32),
            prompt=os.getenv("QUALITY_GUARD_PROMPT", DEFAULT_PROMPT).strip(),
            expected=os.getenv("QUALITY_GUARD_EXPECTED", "QUALITY_OK").strip(),
            state_file=Path(os.getenv("QUALITY_GUARD_STATE_FILE", "/var/lib/grok2api-quality-guard/state.json")),
            lock_file=Path(os.getenv("QUALITY_GUARD_LOCK_FILE", "/var/lib/grok2api-quality-guard/guard.lock")),
            runtime_config_file=Path(os.getenv("QUALITY_GUARD_RUNTIME_CONFIG_FILE", "/var/lib/grok2api-quality-guard/runtime-config.json")),
            insecure_tls=_env_bool("QUALITY_GUARD_INSECURE_TLS", False),
        )
        config.validate()
        return config

    def validate(self) -> None:
        parsed = urllib.parse.urlparse(self.base_url)
        if parsed.scheme not in {"http", "https"} or not parsed.netloc:
            raise ValueError("GROK2API_BASE_URL must be an absolute HTTP(S) URL")
        if not self.username or not self.password:
            raise ValueError("GROK2API_ADMIN_USERNAME and GROK2API_ADMIN_PASSWORD are required")
        if not self.client_key_id.isdigit() or int(self.client_key_id) < 1:
            raise ValueError("QUALITY_GUARD_CLIENT_KEY_ID must be a positive integer")
        if not self.model or not self.prompt or not self.expected:
            raise ValueError("model, prompt, and expected marker must not be empty")
        if self.mode not in {"active", "passive", "hybrid"}:
            raise ValueError("QUALITY_GUARD_MODE must be active, passive, or hybrid")
        if self.soft_tps >= self.hard_tps:
            raise ValueError("QUALITY_GUARD_SOFT_TPS must be lower than QUALITY_GUARD_HARD_TPS")
        if self.active_interval_seconds > 86400:
            raise ValueError("QUALITY_GUARD_ACTIVE_INTERVAL_SECONDS must not exceed 86400")
        if self.passive_poll_seconds > 300:
            raise ValueError("QUALITY_GUARD_PASSIVE_POLL_SECONDS must not exceed 300")
        if self.soft_tps > 10000 or self.hard_tps > 10000:
            raise ValueError("quality guard Token/s thresholds must not exceed 10000")
        if self.consecutive_soft > 20 or self.consecutive_errors > 20:
            raise ValueError("quality guard consecutive strike limits must not exceed 20")
        if self.quarantine_seconds > 86400:
            raise ValueError("QUALITY_GUARD_QUARANTINE_SECONDS must not exceed 86400")
        if self.min_healthy_nodes < 1 or (self.node_ids and self.min_healthy_nodes > len(self.node_ids)):
            raise ValueError("QUALITY_GUARD_MIN_HEALTHY_NODES must fit the configured node count")
        if self.passive_page_size > 2000:
            raise ValueError("QUALITY_GUARD_PASSIVE_PAGE_SIZE must not exceed 2000")


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
        self.token = ""
        self.ssl_context = ssl.create_default_context()
        if config.insecure_tls:
            self.ssl_context.check_hostname = False
            self.ssl_context.verify_mode = ssl.CERT_NONE

    def _request(self, method: str, path: str, body: dict[str, Any] | None = None, retry_auth: bool = True) -> Any:
        data = None if body is None else json.dumps(body, separators=(",", ":")).encode()
        headers = {"Accept": "application/json"}
        if data is not None:
            headers["Content-Type"] = "application/json"
        if self.token:
            headers["Authorization"] = f"Bearer {self.token}"
        request = urllib.request.Request(self.config.base_url + path, data=data, headers=headers, method=method)
        try:
            with urllib.request.urlopen(request, timeout=self.config.request_timeout_seconds, context=self.ssl_context) as response:
                payload = json.load(response)
        except urllib.error.HTTPError as exc:
            try:
                payload = json.loads(exc.read().decode("utf-8", "replace"))
            except (ValueError, OSError):
                payload = {}
            if exc.code == 401 and retry_auth and path != "/api/admin/v1/auth/login":
                self.login()
                return self._request(method, path, body, retry_auth=False)
            error = payload.get("error") or {}
            raise ApiError(exc.code, str(error.get("code", "request_failed")), str(error.get("message", "request failed"))) from exc
        except (urllib.error.URLError, TimeoutError, OSError) as exc:
            raise RuntimeError(f"request failed: {type(exc).__name__}") from exc
        return payload.get("data", payload)

    def login(self) -> None:
        data = self._request("POST", "/api/admin/v1/auth/login", {
            "username": self.config.username,
            "password": self.config.password,
        }, retry_auth=False)
        token = ((data.get("tokens") or {}).get("accessToken") or "").strip()
        if not token:
            raise RuntimeError("login response did not contain access token")
        self.token = token

    def list_nodes(self) -> list[dict[str, Any]]:
        query = urllib.parse.urlencode({"page": 1, "pageSize": 2000, "scope": "grok_build"})
        return list(self._request("GET", f"/api/admin/v1/egress-nodes?{query}").get("items") or [])

    def quality_test(self, node_id: str) -> dict[str, Any]:
        return self._request("POST", f"/api/admin/v1/egress-nodes/{node_id}/quality-test", {
            "clientKeyId": self.config.client_key_id,
            "model": self.config.model,
            "prompt": self.config.prompt,
            "expected": self.config.expected,
            "maxOutputTokens": self.config.max_output_tokens,
        })

    def connectivity_test(self, node_id: str) -> dict[str, Any]:
        return self._request("POST", f"/api/admin/v1/egress-nodes/{node_id}/test")

    def list_audits(self, cursor: str = "") -> dict[str, Any]:
        query = {
            "pagination": "cursor",
            "pageSize": self.config.passive_page_size,
            "period": "24h",
        }
        if cursor:
            query["cursor"] = cursor
        return self._request("GET", f"/api/admin/v1/request-audits?{urllib.parse.urlencode(query)}")

    def set_enabled(self, node_id: str, enabled: bool) -> int:
        result = self._request("PATCH", "/api/admin/v1/egress-nodes/batch", {"ids": [node_id], "enabled": enabled})
        return int(result.get("updated") or 0)


def classify_result(result: dict[str, Any], config: Config) -> tuple[str, str]:
    if not bool(result.get("expectedMatched")):
        return "soft", "expected_marker_missing"
    speed = float(result.get("visibleTokensPerSecond") or 0.0)
    visible_tokens = int(result.get("visibleTokens") or 0)
    if visible_tokens < 32:
        return "soft", "insufficient_visible_tokens"
    if speed >= config.hard_tps:
        return "hard", "hard_tps"
    if speed >= config.soft_tps:
        return "soft", "soft_tps"
    return "healthy", "within_threshold"


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
    visible_tokens = max(0, int(value.get("outputTokens") or 0) - int(value.get("reasoningTokens") or 0))
    if generation_ms <= 0 or visible_tokens < 32:
        return "ignored", "insufficient_visible_tokens", 0.0, visible_tokens
    speed = float(visible_tokens) * 1000 / float(generation_ms)
    if speed >= config.hard_tps:
        return "hard", "hard_tps", speed, visible_tokens
    if speed >= config.soft_tps:
        return "soft", "soft_tps", speed, visible_tokens
    return "healthy", "within_threshold", speed, visible_tokens


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
        "last_visible_tps": 0.0,
        "last_visible_tokens": 0,
        "last_first_token_ms": 0,
        "last_duration_ms": 0,
    }


def default_statistics() -> dict[str, Any]:
    return {
        "started_at": time.time(),
        "active": {"total": 0, "healthy": 0, "soft": 0, "hard": 0, "errors": 0, "visible_tokens": 0},
        "passive": {"total": 0, "healthy": 0, "soft": 0, "hard": 0, "visible_tokens": 0},
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
        self.state.setdefault("started_at", time.time())
        self.state.setdefault("recent_events", [])
        ensure_statistics(self.state)
        self._update_guard_metadata()

    def _bump_statistic(self, group: str, field: str, amount: int = 1) -> None:
        statistics = ensure_statistics(self.state)
        statistics[group][field] = int(statistics[group][field]) + amount

    def _update_guard_metadata(self) -> None:
        self.state["updated_at"] = time.time()
        self.state["guard"] = {
            "mode": self.config.mode,
            "model": self.config.model,
            "client_key_id": self.config.client_key_id,
            "node_ids": list(self.config.node_ids),
            "active_interval_seconds": self.config.active_interval_seconds,
            "passive_poll_seconds": self.config.passive_poll_seconds,
            "soft_tps": self.config.soft_tps,
            "hard_tps": self.config.hard_tps,
            "consecutive_soft": self.config.consecutive_soft,
            "consecutive_errors": self.config.consecutive_errors,
            "quarantine_seconds": self.config.quarantine_seconds,
            "min_healthy_nodes": self.config.min_healthy_nodes,
            "max_output_tokens": self.config.max_output_tokens,
            "prompt": self.config.prompt,
            "expected": self.config.expected,
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
        for key, value in default_node_state().items():
            current.setdefault(key, value)
        return current

    def _eligible_nodes(self, nodes: list[dict[str, Any]]) -> list[dict[str, Any]]:
        configured = set(self.config.node_ids)
        state_nodes = self.state.get("nodes") or {}
        result = []
        for node in nodes:
            node_id = str(node.get("id") or "")
            if not node_id or not node.get("proxyConfigured"):
                continue
            if configured and node_id not in configured:
                continue
            tracked_quarantine = bool((state_nodes.get(node_id) or {}).get("disabled_by_guard"))
            if node.get("enabled") or tracked_quarantine:
                result.append(node)
        return result

    def _can_quarantine(self, nodes: list[dict[str, Any]], node_id: str) -> bool:
        enabled = sum(1 for node in nodes if bool(node.get("enabled")))
        target_enabled = any(str(node.get("id")) == node_id and bool(node.get("enabled")) for node in nodes)
        return target_enabled and enabled - 1 >= self.config.min_healthy_nodes

    def _quarantine(self, nodes: list[dict[str, Any]], node: dict[str, Any], reason: str, now: float) -> None:
        node_id = str(node["id"])
        state = self._state_for(node_id)
        if not self._can_quarantine(nodes, node_id):
            self._bump_statistic("actions", "suppressed")
            log_event("quarantine_suppressed", node_id=node_id, node_name=node.get("name"), reason=reason, minimum_healthy=self.config.min_healthy_nodes)
            return
        updated = self.api.set_enabled(node_id, False)
        if updated != 1:
            log_event("quarantine_not_applied", node_id=node_id, node_name=node.get("name"), reason=reason, updated=updated)
            return
        state.update({
            "active_soft_strikes": 0,
            "passive_soft_strikes": 0,
            "error_strikes": 0,
            "quarantined_until": now + self.config.quarantine_seconds,
            "disabled_by_guard": True,
            "last_reason": reason,
        })
        node["enabled"] = False
        self._bump_statistic("actions", "quarantined")
        append_state_event(self.state, "node_quarantined", node_id=node_id, node_name=node.get("name"), reason=reason)
        log_event("node_quarantined", node_id=node_id, node_name=node.get("name"), reason=reason, quarantine_seconds=self.config.quarantine_seconds)

    def _record_probe(self, node: dict[str, Any], result: dict[str, Any], classification: str, reason: str, now: float) -> None:
        node_id = str(node["id"])
        state = self._state_for(node_id)
        state["last_probe_at"] = now
        state.update({
            "last_observed_at": now,
            "last_source": "active",
            "last_classification": classification,
            "last_visible_tps": round(float(result.get("visibleTokensPerSecond") or 0.0), 3),
            "last_visible_tokens": int(result.get("visibleTokens") or 0),
            "last_first_token_ms": int(result.get("firstTokenMs") or 0),
            "last_duration_ms": int(result.get("durationMs") or 0),
        })
        state["error_strikes"] = 0
        self._bump_statistic("active", classification)
        self._bump_statistic("active", "visible_tokens", int(result.get("visibleTokens") or 0))
        if classification == "healthy":
            state["active_soft_strikes"] = 0
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
            visible_tps=round(float(result.get("visibleTokensPerSecond") or 0.0), 3),
            visible_tokens=int(result.get("visibleTokens") or 0),
            first_token_ms=int(result.get("firstTokenMs") or 0),
            duration_ms=int(result.get("durationMs") or 0),
            chunk_count=int(result.get("chunkCount") or 0),
            expected_matched=bool(result.get("expectedMatched")),
        )

    def _probe_active(self, nodes: list[dict[str, Any]], node: dict[str, Any], now: float) -> None:
        node_id = str(node["id"])
        state = self._state_for(node_id)
        self._bump_statistic("active", "total")
        try:
            result = self.api.quality_test(node_id)
        except Exception as exc:
            self._bump_statistic("active", "errors")
            state["error_strikes"] = int(state.get("error_strikes", 0)) + 1
            state["last_probe_at"] = now
            log_event("quality_probe_failed", node_id=node_id, node_name=node.get("name"), error_type=type(exc).__name__, strikes=state["error_strikes"])
            if state["error_strikes"] >= self.config.consecutive_errors:
                self._quarantine(nodes, node, "probe_errors", now)
            return
        classification, reason = classify_result(result, self.config)
        self._record_probe(node, result, classification, reason, now)
        if classification == "hard" or int(state.get("active_soft_strikes", 0)) >= self.config.consecutive_soft:
            self._quarantine(nodes, node, reason, now)

    def _probe_quarantined(self, node: dict[str, Any], now: float) -> None:
        node_id = str(node["id"])
        state = self._state_for(node_id)
        if now < float(state.get("quarantined_until", 0.0)):
            return
        try:
            try:
                connectivity = self.api.connectivity_test(node_id)
                connectivity_status = str(connectivity.get("status") or "unknown")
            except Exception as exc:
                connectivity_status = "error"
                log_event("recovery_connectivity_probe_failed", node_id=node_id, node_name=node.get("name"), error_type=type(exc).__name__)
            self._bump_statistic("active", "total")
            result = self.api.quality_test(node_id)
            classification, reason = classify_result(result, self.config)
            self._record_probe(node, result, classification, reason, now)
        except Exception as exc:
            self._bump_statistic("active", "errors")
            state["quarantined_until"] = now + self.config.quarantine_seconds
            state["last_reason"] = "recovery_probe_error"
            log_event("recovery_probe_failed", node_id=node_id, node_name=node.get("name"), error_type=type(exc).__name__)
            return
        if classification != "healthy":
            state["quarantined_until"] = now + self.config.quarantine_seconds
            state["last_reason"] = reason
            log_event("quarantine_extended", node_id=node_id, node_name=node.get("name"), reason=reason)
            return
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
        })
        node["enabled"] = True
        self._bump_statistic("actions", "restored")
        append_state_event(self.state, "node_restored", node_id=node_id, node_name=node.get("name"), reason="quality_probe_healthy")
        log_event("node_restored", node_id=node_id, node_name=node.get("name"), connectivity_status=connectivity_status)

    def _prepare_nodes(self, now: float) -> tuple[list[dict[str, Any]], list[dict[str, Any]], set[str]]:
        all_nodes = self.api.list_nodes()
        nodes = self._eligible_nodes(all_nodes)
        skip_ids: set[str] = set()
        if not nodes:
            log_event("no_eligible_nodes")
            return all_nodes, [], skip_ids
        for node in nodes:
            node_id = str(node["id"])
            state = self._state_for(node_id)
            if state.get("disabled_by_guard") and node.get("enabled"):
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
        classification, reason, speed, visible_tokens = classify_audit(audit_value, self.config)
        if classification == "ignored":
            return
        self._bump_statistic("passive", "total")
        self._bump_statistic("passive", classification)
        self._bump_statistic("passive", "visible_tokens", visible_tokens)
        state.update({
            "last_observed_at": now,
            "last_source": "passive",
            "last_classification": classification,
            "last_visible_tps": round(speed, 3),
            "last_visible_tokens": visible_tokens,
            "last_first_token_ms": int(audit_value.get("firstTokenMs") or 0),
            "last_duration_ms": int(audit_value.get("durationMs") or 0),
        })
        if classification == "healthy":
            state["passive_soft_strikes"] = 0
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
            visible_tps=round(speed, 3),
        )
        log_event(
            "passive_audit_anomaly",
            request_id=audit_value.get("requestId"),
            node_id=node_id,
            node_name=node.get("name"),
            classification=classification,
            reason=reason,
            visible_tps=round(speed, 3),
            visible_tokens=visible_tokens,
            first_token_ms=int(audit_value.get("firstTokenMs") or 0),
            duration_ms=int(audit_value.get("durationMs") or 0),
            strikes=int(state.get("passive_soft_strikes", 0)),
        )
        if classification == "hard" or int(state.get("passive_soft_strikes", 0)) >= self.config.consecutive_soft:
            self._quarantine(all_nodes, node, "passive_" + reason, now)

    def run_passive_cycle(self) -> None:
        now = time.time()
        self.state["last_passive_poll_at"] = now
        all_nodes, nodes, _skip_ids = self._prepare_nodes(now)
        node_by_id = {str(node["id"]): node for node in nodes}
        audits = self._fetch_new_audits()
        for value in audits:
            if str(value.get("clientKeyId") or "") == self.config.client_key_id or value.get("clientKeyName") == "egress-quality-guard":
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
    parser.add_argument("--check-config", action="store_true", help="validate environment and exit")
    args = parser.parse_args(argv)
    try:
        base_config = Config.from_env()
        reloader = RuntimeConfigReloader(base_config)
        config, _, runtime_error = reloader.reload(force=True)
        if runtime_error is not None:
            raise ValueError(str(runtime_error))
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
