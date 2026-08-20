#!/usr/bin/env python3
"""Cap grok2api account cooldowns that exceed routing.cooldownMax.

Official images apply upstream Retry-After after the local cooldownMax clamp,
so a 504 with a 24h Retry-After can freeze a Build account for a day. This
sidecar rewrites overdue cooldown_until values and PATCHes the account so the
in-memory selector overlay drops.

The script uses only the Python standard library. It reads bootstrapAdmin from
config.yaml and never prints the password.
"""

from __future__ import annotations

import argparse
import json
import re
import sqlite3
import sys
import traceback
import urllib.request
from datetime import datetime, timedelta, timezone
from pathlib import Path

NANO_RE = re.compile(r"\.(\d+)(?=[+-]|Z|$)")
BOOTSTRAP_BLOCK_RE = re.compile(r"bootstrapAdmin:\n((?:  .*\n)+)")
BOOTSTRAP_FIELD_RE = re.compile(
    r"\s+(username|password):\s*(?:\"(.*)\"|'(.*)'|(\S+))\s*$"
)


def parse_ts(value: object) -> datetime | None:
    """Parse SQLite/Go timestamps, including 9-digit nanosecond fractions."""
    if not value:
        return None
    text = str(value).strip().replace(" ", "T")
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"

    def _trim(match: re.Match[str]) -> str:
        return "." + (match.group(1) + "000000")[:6]

    text = NANO_RE.sub(_trim, text, count=1)
    try:
        parsed = datetime.fromisoformat(text)
    except ValueError:
        return None
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed.astimezone(timezone.utc)


def bootstrap_admin(config_path: Path) -> tuple[str, str]:
    text = config_path.read_text()
    block = BOOTSTRAP_BLOCK_RE.search(text)
    if not block:
        raise RuntimeError("config.yaml is missing bootstrapAdmin")
    user = password = None
    for line in block.group(1).splitlines():
        match = BOOTSTRAP_FIELD_RE.match(line)
        if not match:
            continue
        value = next(item for item in match.group(2, 3, 4) if item is not None)
        if match.group(1) == "username":
            user = value
        else:
            password = value
    if not user or not password:
        raise RuntimeError("bootstrapAdmin is incomplete")
    return user, password


def request(base: str, method: str, path: str, body: dict | None = None, token: str | None = None) -> dict:
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(base.rstrip("/") + path, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    with urllib.request.urlopen(req, timeout=15) as resp:
        raw = resp.read().decode()
        return json.loads(raw) if raw else {}


def clamp_rows(conn: sqlite3.Connection, cap: datetime, now: datetime) -> list[int]:
    conn.row_factory = sqlite3.Row
    rows = list(conn.execute("SELECT id, cooldown_until FROM provider_accounts WHERE cooldown_until IS NOT NULL"))
    ids: list[int] = []
    for row in rows:
        until = parse_ts(row["cooldown_until"])
        if until is None or until <= cap:
            continue
        conn.execute(
            "UPDATE provider_accounts SET cooldown_until=?, failure_count=0, last_error='', updated_at=? WHERE id=?",
            (cap.isoformat(), now.isoformat(), row["id"]),
        )
        ids.append(int(row["id"]))
    conn.commit()
    return ids


def run(db_path: Path, config_path: Path, api_base: str, max_remaining: timedelta) -> list[int]:
    now = datetime.now(timezone.utc)
    cap = now + max_remaining
    conn = sqlite3.connect(db_path)
    try:
        ids = clamp_rows(conn, cap, now)
    finally:
        conn.close()
    if not ids:
        return ids
    user, password = bootstrap_admin(config_path)
    login = request(api_base, "POST", "/api/admin/v1/auth/login", {"username": user, "password": password})
    payload = login.get("data", login)
    token = (payload.get("tokens") or {}).get("accessToken") or payload.get("accessToken")
    if not token:
        raise RuntimeError("admin login failed")
    for account_id in ids:
        request(api_base, "PATCH", f"/api/admin/v1/accounts/{account_id}", {"enabled": True}, token)
    return ids


def main() -> int:
    parser = argparse.ArgumentParser(description="Cap grok2api account cooldowns to a short retry window")
    parser.add_argument("--db", default="/var/lib/docker/volumes/grok2api_grok2api-data/_data/backend.db")
    parser.add_argument("--config", default="/home/ubuntu/grok2api/config.yaml")
    parser.add_argument("--api-base", default="http://127.0.0.1:32124")
    parser.add_argument("--max-remaining", default="1m", help="max remaining cooldown, for example 1m or 60s")
    args = parser.parse_args()
    try:
        ids = run(Path(args.db), Path(args.config), args.api_base, parse_duration(args.max_remaining))
    except Exception:
        traceback.print_exc()
        return 1
    if ids:
        print(f"{datetime.now(timezone.utc).isoformat()} clamped {ids}", flush=True)
    return 0


def parse_duration(value: str) -> timedelta:
    text = value.strip().lower()
    if text.endswith("ms"):
        return timedelta(milliseconds=int(text[:-2]))
    if text.endswith("s"):
        return timedelta(seconds=int(text[:-1]))
    if text.endswith("m"):
        return timedelta(minutes=int(text[:-1]))
    if text.endswith("h"):
        return timedelta(hours=int(text[:-1]))
    raise ValueError(f"unsupported duration {value}")


if __name__ == "__main__":
    sys.exit(main())
