from __future__ import annotations

import json
import os
from collections import Counter
from collections.abc import Iterator
from datetime import datetime
from pathlib import Path
from typing import Any

from app.core.logger import LOG_DIR

ALLOWED_SUFFIXES = {".log", ".txt", ".jsonl"}
DEFAULT_LIMIT = 200
MAX_LIMIT = 1000


class LogViewerError(ValueError):
    """Raised when log viewer input is invalid."""


def list_log_files() -> list[dict[str, Any]]:
    files: list[dict[str, Any]] = []
    for path in sorted(_allowed_log_paths().values(), key=_sort_key, reverse=True):
        stat = path.stat()
        files.append(
            {
                "name": path.name,
                "size": stat.st_size,
                "updated_at": datetime.fromtimestamp(stat.st_mtime).isoformat(),
            }
        )
    return files


def read_log_entries(
    file_name: str,
    *,
    limit: int = DEFAULT_LIMIT,
    level: str | None = None,
    keyword: str | None = None,
    exclude_prefixes: list[str] | None = None,
) -> dict[str, Any]:
    path = _resolve_log_path(file_name)
    if not path.exists() or not path.is_file():
        raise FileNotFoundError(file_name)

    limit = max(1, min(int(limit or DEFAULT_LIMIT), MAX_LIMIT))
    level_normalized = (level or "").strip().lower()
    keyword_normalized = (keyword or "").strip().lower()
    prefixes = [prefix.strip() for prefix in (exclude_prefixes or []) if prefix.strip()]

    entries: list[dict[str, Any]] = []
    counts: Counter[str] = Counter()
    search_count = 0

    for line in _iter_lines_reverse(path):
        parsed = _parse_log_line(line)
        if not _matches(parsed, level_normalized, keyword_normalized, prefixes):
            continue
        search_count += 1
        log_level = str(parsed.get("level") or "unknown").lower()
        counts[log_level] += 1
        entries.append(parsed)
        if len(entries) >= limit:
            break

    stat = path.stat()
    return {
        "file": {
            "name": path.name,
            "size": stat.st_size,
            "updated_at": datetime.fromtimestamp(stat.st_mtime).isoformat(),
        },
        "entries": entries,
        "stats": {
            "matched": search_count,
            "levels": dict(counts),
        },
    }


def delete_log_files(file_names: list[str]) -> dict[str, Any]:
    deleted: list[str] = []
    missing: list[str] = []
    failed: list[str] = []

    for file_name in file_names:
        path = _resolve_log_path(file_name)
        if not path.exists() or not path.is_file():
            missing.append(path.name)
            continue
        try:
            path.unlink()
            deleted.append(path.name)
        except OSError:
            failed.append(path.name)

    return {
        "deleted": deleted,
        "missing": missing,
        "failed": failed,
    }


def _allowed_log_paths() -> dict[str, Path]:
    if not LOG_DIR.exists():
        return {}

    base_dir = LOG_DIR.resolve()
    allowed: dict[str, Path] = {}
    for path in LOG_DIR.iterdir():
        if not path.is_file() or path.suffix.lower() not in ALLOWED_SUFFIXES:
            continue
        try:
            resolved = path.resolve()
        except OSError:
            continue
        if resolved.parent != base_dir:
            continue
        allowed[path.name] = resolved
    return allowed


def _validate_log_file_name(file_name: str) -> str:
    candidate = Path(file_name)
    if candidate.name != file_name:
        raise LogViewerError("Invalid log file name")
    if candidate.suffix.lower() not in ALLOWED_SUFFIXES:
        raise LogViewerError("Unsupported log file")
    return candidate.name


def _resolve_log_path(file_name: str) -> Path:
    safe_name = _validate_log_file_name(file_name)
    path = _allowed_log_paths().get(safe_name)
    if path is None:
        raise FileNotFoundError(safe_name)
    return path


def _sort_key(path: Path) -> tuple[float, str]:
    try:
        return (path.stat().st_mtime, path.name)
    except OSError:
        return (0, path.name)


def _iter_lines_reverse(path: Path, chunk_size: int = 8192) -> Iterator[str]:
    with path.open("rb") as handle:
        handle.seek(0, os.SEEK_END)
        position = handle.tell()
        buffer = b""

        while position > 0:
            read_size = min(chunk_size, position)
            position -= read_size
            handle.seek(position)
            chunk = handle.read(read_size)
            buffer = chunk + buffer
            lines = buffer.split(b"\n")
            buffer = lines[0]
            for raw_line in reversed(lines[1:]):
                line = raw_line.decode("utf-8", errors="replace").strip()
                if line:
                    yield line

        if buffer:
            line = buffer.decode("utf-8", errors="replace").strip()
            if line:
                yield line


def _parse_log_line(line: str) -> dict[str, Any]:
    try:
        data = json.loads(line)
        if isinstance(data, dict):
            parsed = data
        else:
            parsed = {"msg": line}
    except json.JSONDecodeError:
        parsed = {"msg": line}

    extra = parsed.get("extra")
    if isinstance(extra, dict):
        for key, value in extra.items():
            parsed.setdefault(key, value)

    timestamp = parsed.get("time")
    parsed["time_display"] = _format_time(timestamp)
    parsed["level"] = str(parsed.get("level") or "unknown").lower()
    parsed["msg"] = str(parsed.get("msg") or "")
    parsed["caller"] = str(parsed.get("caller") or "-")
    parsed["path"] = str(parsed.get("path") or "")
    parsed["raw"] = line
    return parsed


def _format_time(value: Any) -> str:
    if not value:
        return "-"
    try:
        normalized = str(value).replace("Z", "+00:00")
        dt = datetime.fromisoformat(normalized)
        return dt.strftime("%Y-%m-%d %H:%M:%S")
    except ValueError:
        return str(value)


def _matches(
    entry: dict[str, Any],
    level: str,
    keyword: str,
    exclude_prefixes: list[str],
) -> bool:
    if level and entry.get("level") != level:
        return False

    path = str(entry.get("path") or "")
    if path and any(path.startswith(prefix) for prefix in exclude_prefixes):
        return False

    if keyword:
        haystack = json.dumps(entry, ensure_ascii=False).lower()
        if keyword not in haystack:
            return False
    return True
