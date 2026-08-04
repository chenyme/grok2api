#!/usr/bin/env python3
"""Rotate one 1024Proxy sticky session and hot-reload Mihomo.

The service is intentionally provider-specific. It only exposes a loopback
HTTP endpoint and never logs proxy credentials or session identifiers.
"""

from __future__ import annotations

import dataclasses
import hmac
import json
import os
import random
import re
import ssl
import string
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any


SESSION_PATTERN = re.compile(r"^(?P<prefix>.+-sid-)(?P<sid>[A-Za-z0-9]+)-t-(?P<ttl>\d+)$")
PROXY_NAME_PREFIX = "1024Proxy-Grok-Sticky-"


def env_int(name: str, default: int, minimum: int = 0) -> int:
    try:
        value = int(os.getenv(name, str(default)).strip())
    except ValueError as exc:
        raise ValueError(f"{name} must be an integer") from exc
    if value < minimum:
        raise ValueError(f"{name} must be >= {minimum}")
    return value


@dataclasses.dataclass(frozen=True)
class Config:
    listen: str
    port: int
    token: str
    credentials_file: Path
    mihomo_config_file: Path
    mihomo_controller_url: str
    mihomo_reload_path: str
    node_id_base: int
    listener_port_base: int
    sticky_minutes: int
    max_attempts: int
    verify_timeout_seconds: int

    @classmethod
    def from_env(cls) -> "Config":
        value = cls(
            listen=os.getenv("ROTATOR_LISTEN", "127.0.0.1").strip(),
            port=env_int("ROTATOR_PORT", 19099, 1),
            token=os.getenv("ROTATOR_TOKEN", "").strip(),
            credentials_file=Path(os.getenv("ROTATOR_CREDENTIALS_FILE", "/config/1024proxy-sticky.list")),
            mihomo_config_file=Path(os.getenv("ROTATOR_MIHOMO_CONFIG_FILE", "/config/mihomo-1024.yaml")),
            mihomo_controller_url=os.getenv("ROTATOR_MIHOMO_CONTROLLER_URL", "http://127.0.0.1:9099").strip().rstrip("/"),
            mihomo_reload_path=os.getenv("ROTATOR_MIHOMO_RELOAD_PATH", "/root/.config/mihomo/config.yaml").strip(),
            node_id_base=env_int("ROTATOR_NODE_ID_BASE", 8, 1),
            listener_port_base=env_int("ROTATOR_LISTENER_PORT_BASE", 7951, 1),
            sticky_minutes=env_int("ROTATOR_STICKY_MINUTES", 10, 1),
            max_attempts=env_int("ROTATOR_MAX_ATTEMPTS", 4, 1),
            verify_timeout_seconds=env_int("ROTATOR_VERIFY_TIMEOUT_SECONDS", 30, 5),
        )
        if not value.listen or not value.mihomo_reload_path:
            raise ValueError("rotator listen address and Mihomo reload path are required")
        return value


def log_event(event: str, **fields: Any) -> None:
    print(json.dumps({"event": event, **fields}, ensure_ascii=True, sort_keys=True, separators=(",", ":")), flush=True)


def read_credentials(path: Path) -> tuple[list[str], list[tuple[int, str, str, str, str]]]:
    lines = path.read_text(encoding="utf-8").splitlines()
    entries: list[tuple[int, str, str, str, str]] = []
    for line_number, raw in enumerate(lines):
        value = raw.strip()
        if not value or value.startswith("#"):
            continue
        parts = value.split(":", 3)
        if len(parts) != 4 or not all(parts):
            raise RuntimeError(f"invalid credential entry at line {line_number + 1}")
        entries.append((line_number, *parts))
    if not entries:
        raise RuntimeError("credential list is empty")
    return lines, entries


def replace_username_in_mihomo(value: str, proxy_index: int, username: str) -> str:
    target_name = f"- name: {PROXY_NAME_PREFIX}{proxy_index + 1:02d}"
    lines = value.splitlines()
    in_target = False
    replaced = False
    for index, line in enumerate(lines):
        if line.startswith("- name: "):
            in_target = line.strip() == target_name
            continue
        if in_target and line.startswith("  username:"):
            lines[index] = f"  username: {username}"
            replaced = True
            break
    if not replaced:
        raise RuntimeError("target Mihomo proxy username was not found")
    return "\n".join(lines) + "\n"


def write_checked(path: Path, value: str) -> None:
    with path.open("w", encoding="utf-8") as handle:
        handle.write(value)
        handle.flush()
        os.fsync(handle.fileno())
    os.chmod(path, 0o600)


class Rotator:
    def __init__(self, config: Config):
        self.config = config
        self.lock = threading.Lock()
        self.ssl_context = ssl.create_default_context()

    def _reload_mihomo(self) -> None:
        data = json.dumps({"path": self.config.mihomo_reload_path}, separators=(",", ":")).encode()
        request = urllib.request.Request(
            self.config.mihomo_controller_url + "/configs?force=true",
            data=data,
            headers={"Content-Type": "application/json"},
            method="PUT",
        )
        try:
            with urllib.request.urlopen(request, timeout=10) as response:
                if response.status < 200 or response.status >= 300:
                    raise RuntimeError(f"Mihomo reload returned HTTP {response.status}")
        except (urllib.error.URLError, urllib.error.HTTPError, TimeoutError, OSError) as exc:
            raise RuntimeError(f"Mihomo reload failed: {type(exc).__name__}") from exc

    def _exit_ip(self, proxy_index: int) -> str:
        proxy_url = f"http://127.0.0.1:{self.config.listener_port_base + proxy_index}"
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({"http": proxy_url, "https": proxy_url}))
        request = urllib.request.Request(
            "https://1.1.1.1/cdn-cgi/trace",
            headers={"User-Agent": "grok2api-session-rotator/1", "Accept": "text/plain"},
        )
        with opener.open(request, timeout=8) as response:
            body = response.read(8192).decode("utf-8", "replace")
        for line in body.splitlines():
            if line.startswith("ip="):
                return line.removeprefix("ip=").strip()
        return ""

    def _wait_for_exit_ip(self, proxy_index: int) -> str:
        deadline = time.monotonic() + self.config.verify_timeout_seconds
        last_error: Exception | None = None
        while time.monotonic() < deadline:
            try:
                value = self._exit_ip(proxy_index)
                if value:
                    return value
            except Exception as exc:
                last_error = exc
            time.sleep(1)
        if last_error is not None:
            raise RuntimeError(f"exit IP verification failed: {type(last_error).__name__}") from last_error
        raise RuntimeError("exit IP verification returned no address")

    @staticmethod
    def _new_session_id() -> str:
        alphabet = string.ascii_letters + string.digits
        return "".join(random.SystemRandom().choice(alphabet) for _ in range(8))

    def _update_session(self, proxy_index: int, session_id: str) -> None:
        credential_lines, entries = read_credentials(self.config.credentials_file)
        if proxy_index < 0 or proxy_index >= len(entries):
            raise RuntimeError("node is not mapped to a sticky session")
        line_number, server, port, username, password = entries[proxy_index]
        match = SESSION_PATTERN.match(username)
        if match is None:
            raise RuntimeError("sticky username does not contain a supported session marker")
        new_username = f"{match.group('prefix')}{session_id}-t-{self.config.sticky_minutes}"
        credential_lines[line_number] = f"{server}:{port}:{new_username}:{password}"
        mihomo_value = self.config.mihomo_config_file.read_text(encoding="utf-8")
        new_mihomo_value = replace_username_in_mihomo(mihomo_value, proxy_index, new_username)
        write_checked(self.config.credentials_file, "\n".join(credential_lines) + "\n")
        write_checked(self.config.mihomo_config_file, new_mihomo_value)

    def rotate(self, node_id: str, expected_old_exit_ip: str = "") -> dict[str, Any]:
        try:
            numeric_node_id = int(node_id)
        except ValueError as exc:
            raise RuntimeError("node ID is invalid") from exc
        proxy_index = numeric_node_id - self.config.node_id_base
        with self.lock:
            original_credentials = self.config.credentials_file.read_text(encoding="utf-8")
            original_mihomo = self.config.mihomo_config_file.read_text(encoding="utf-8")
            # The caller's value is historical node metadata and may be stale.
            # A rotation can only be verified against an exit IP observed
            # immediately before changing the session.
            old_exit_ip = self._exit_ip(proxy_index)
            expected_old_exit_ip = expected_old_exit_ip.strip()
            if expected_old_exit_ip and expected_old_exit_ip != old_exit_ip:
                log_event(
                    "stale_expected_exit_ip",
                    node_id=node_id,
                    expected_exit_ip=expected_old_exit_ip,
                    observed_exit_ip=old_exit_ip,
                )
            try:
                for attempt in range(1, self.config.max_attempts + 1):
                    self._update_session(proxy_index, self._new_session_id())
                    self._reload_mihomo()
                    new_exit_ip = self._wait_for_exit_ip(proxy_index)
                    if not old_exit_ip or new_exit_ip != old_exit_ip:
                        log_event("session_rotated", node_id=node_id, attempt=attempt, old_exit_ip=old_exit_ip, new_exit_ip=new_exit_ip)
                        return {
                            "changed": True,
                            "nodeId": node_id,
                            "oldExitIp": old_exit_ip,
                            "newExitIp": new_exit_ip,
                            "stickyMinutes": self.config.sticky_minutes,
                        }
                raise RuntimeError("provider returned the same exit IP after all rotation attempts")
            except Exception:
                write_checked(self.config.credentials_file, original_credentials)
                write_checked(self.config.mihomo_config_file, original_mihomo)
                try:
                    self._reload_mihomo()
                except Exception:
                    pass
                raise


class Handler(BaseHTTPRequestHandler):
    server_version = "grok2api-session-rotator/1"

    def _json(self, status: int, value: dict[str, Any]) -> None:
        payload = json.dumps(value, ensure_ascii=True, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self) -> None:
        if self.path != "/healthz":
            self._json(404, {"error": "not found"})
            return
        self._json(200, {"status": "ok"})

    def do_POST(self) -> None:
        if self.path != "/rotate":
            self._json(404, {"error": "not found"})
            return
        config: Config = self.server.config  # type: ignore[attr-defined]
        if config.token:
            expected = f"Bearer {config.token}"
            if not hmac.compare_digest(self.headers.get("Authorization", ""), expected):
                self._json(401, {"error": "unauthorized"})
                return
        try:
            length = int(self.headers.get("Content-Length", "0"))
            if length < 2 or length > 4096:
                raise ValueError("request body size is invalid")
            body = json.loads(self.rfile.read(length))
            node_id = str(body.get("nodeId") or "")
            value = self.server.rotator.rotate(node_id, str(body.get("oldExitIp") or ""))  # type: ignore[attr-defined]
        except (ValueError, RuntimeError, OSError) as exc:
            log_event("rotation_request_failed", error_type=type(exc).__name__)
            self._json(503, {"error": str(exc)})
            return
        self._json(200, value)

    def log_message(self, _format: str, *_args: Any) -> None:
        return


def main() -> int:
    try:
        config = Config.from_env()
        rotator = Rotator(config)
        _, entries = read_credentials(config.credentials_file)
        if not entries:
            raise RuntimeError("no sticky sessions configured")
    except (ValueError, RuntimeError, OSError) as exc:
        log_event("rotator_start_failed", error_type=type(exc).__name__)
        return 2
    server = ThreadingHTTPServer((config.listen, config.port), Handler)
    server.config = config  # type: ignore[attr-defined]
    server.rotator = rotator  # type: ignore[attr-defined]
    log_event("rotator_started", listen=config.listen, port=config.port, sticky_minutes=config.sticky_minutes)
    try:
        server.serve_forever(poll_interval=0.5)
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
