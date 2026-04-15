"""Helpers for writing local media cache files with size-based eviction."""

from pathlib import Path
from threading import Lock
from typing import Literal

from app.platform.config.snapshot import get_config
from app.platform.logging.logger import logger

from .media_paths import image_files_dir, video_files_dir

MediaType = Literal["image", "video"]

_CACHE_LOCK = Lock()
_MB = 1024 * 1024


def _media_dirs() -> dict[MediaType, Path]:
    return {
        "image": image_files_dir(),
        "video": video_files_dir(),
    }


def _read_limit_mb(key: str) -> float:
    value = get_config().get_float(key, 0.0)
    return value if value > 0 else 0.0


def _specific_limit_bytes(media_type: MediaType) -> int:
    limit_mb = _read_limit_mb(f"storage.{media_type}_max_mb")
    return int(limit_mb * _MB) if limit_mb > 0 else 0


def _total_limit_bytes() -> int:
    limit_mb = _read_limit_mb("storage.media_max_mb")
    return int(limit_mb * _MB) if limit_mb > 0 else 0


def _list_files(media_type: MediaType | None = None) -> list[Path]:
    dirs = _media_dirs()
    selected = [dirs[media_type]] if media_type else list(dirs.values())
    files: list[Path] = []
    for directory in selected:
        files.extend(path for path in directory.glob("*") if path.is_file())
    return files


def _prune_paths(paths: list[Path], limit_bytes: int) -> list[Path]:
    if limit_bytes <= 0:
        return []

    file_rows: list[tuple[float, str, int, Path]] = []
    total_size = 0
    for path in paths:
        try:
            stat = path.stat()
        except FileNotFoundError:
            continue
        file_rows.append((stat.st_mtime, path.name, stat.st_size, path))
        total_size += stat.st_size

    if total_size <= limit_bytes:
        return []

    removed: list[Path] = []
    for _mtime, _name, size, path in sorted(file_rows):
        try:
            path.unlink(missing_ok=True)
        except OSError as exc:
            logger.warning("media cache eviction failed: path={} error={}", path, exc)
            continue
        removed.append(path)
        total_size -= size
        if total_size <= limit_bytes:
            break
    return removed


def _enforce_limits_locked(media_type: MediaType) -> None:
    removed = _prune_paths(_list_files(media_type), _specific_limit_bytes(media_type))
    if removed:
        logger.info(
            "media cache pruned: scope={} removed_count={}",
            media_type,
            len(removed),
        )

    removed = _prune_paths(_list_files(), _total_limit_bytes())
    if removed:
        logger.info("media cache pruned: scope=all removed_count={}", len(removed))


def save_media_bytes(raw: bytes, path: Path, *, media_type: MediaType) -> Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    with _CACHE_LOCK:
        if not path.exists():
            path.write_bytes(raw)
        _enforce_limits_locked(media_type)
    return path


__all__ = ["MediaType", "save_media_bytes"]
