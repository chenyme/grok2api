"""Typed configuration snapshot — built once at startup, reloaded on change."""

import asyncio
import os
import re
from pathlib import Path
from typing import Any

from .loader import _deep_merge, get_nested, load_toml
from .backends import ConfigBackend, create_config_backend

# List-valued config can arrive as a single string from the WebUI textarea
# (each item on a new line) or a comma-separated string from env-var overrides.
# Split on either delimiter so operators can use whichever feels natural.
_LIST_SEPARATOR_RE = re.compile(r"[,\r\n]+")

_BASE_DIR = Path(__file__).resolve().parents[3]  # project root


def _resolve_defaults_path() -> Path:
    return _BASE_DIR / "config.defaults.toml"


def _mtime(path: Path) -> float:
    try:
        return os.stat(path).st_mtime
    except OSError:
        return 0.0


class ConfigSnapshot:
    """Immutable view over the loaded configuration dict.

    Loading strategy (lowest → highest priority):
      1. ``config.defaults.toml``  — shipped defaults (read-only, ConfigMap)
      2. Backend user overrides    — toml file or Redis (admin hot-update target)
      3. ``GROK_*`` env vars       — always win

    Change detection is cheap: one stat() (toml) or one Redis GET (redis) per
    request on the fast path.
    """

    def __init__(self, backend: ConfigBackend | None = None) -> None:
        self._data: dict[str, Any] = {}
        self._loaded = False
        self._lock = asyncio.Lock()
        self._mtime_defaults: float = 0.0
        self._version: object = None
        self._backend: ConfigBackend | None = backend

    def _get_backend(self) -> ConfigBackend:
        if self._backend is None:
            self._backend = create_config_backend()
        return self._backend

    async def load(self, defaults_path: Path | None = None) -> None:
        """Reload config if defaults or backend overrides changed.

        Safe to call on every request — skips I/O when nothing changed.
        Pass *defaults_path* only during testing.
        """
        dp = defaults_path or _resolve_defaults_path()
        backend = self._get_backend()

        mt_dp = _mtime(dp)
        ver = await backend.version()

        # Fast path: nothing changed.
        if self._loaded and mt_dp == self._mtime_defaults and ver == self._version:
            return

        async with self._lock:
            mt_dp = _mtime(dp)
            ver = await backend.version()
            if self._loaded and mt_dp == self._mtime_defaults and ver == self._version:
                return

            if not dp.exists():
                raise RuntimeError(f"Missing required defaults config: {dp}")

            defaults = await asyncio.to_thread(load_toml, dp)
            user_overrides = await backend.load()
            self._data = _deep_merge(defaults, user_overrides)
            self._data = _apply_env(self._data)

            self._loaded = True
            self._mtime_defaults = mt_dp
            self._version = ver

    async def ensure_loaded(self) -> None:
        if not self._loaded:
            await self.load()

    def get(self, key: str, default: Any = None) -> Any:
        return get_nested(self._data, key, default)

    def get_int(self, key: str, default: int = 0) -> int:
        val = self.get(key, default)
        try:
            return int(val)
        except (TypeError, ValueError):
            return default

    def get_float(self, key: str, default: float = 0.0) -> float:
        val = self.get(key, default)
        try:
            return float(val)
        except (TypeError, ValueError):
            return default

    def get_bool(self, key: str, default: bool = False) -> bool:
        val = self.get(key, default)
        if isinstance(val, bool):
            return val
        if isinstance(val, str):
            return val.strip().lower() in {"1", "true", "yes", "on"}
        return bool(val)

    def get_str(self, key: str, default: str = "") -> str:
        val = self.get(key, default)
        return str(val) if val is not None else default

    def get_list(self, key: str, default: list | None = None) -> list:
        val = self.get(key, default)
        if val is None:
            return [] if default is None else default
        if isinstance(val, list):
            return val
        if isinstance(val, str):
            return [p.strip() for p in _LIST_SEPARATOR_RE.split(val) if p.strip()]
        return [val]

    async def update(self, patch: dict[str, Any]) -> None:
        """Persist only the changed keys in *patch* via backend."""
        backend = self._get_backend()
        async with self._lock:
            await backend.apply_patch(patch)
            # Invalidate so next load() call pulls the new version.
            self._version = None

    def raw(self) -> dict[str, Any]:
        return dict(self._data)


# ---------------------------------------------------------------------------
# Env-var override layer (GROK_SECTION_KEY → section.key)
# ---------------------------------------------------------------------------

def _apply_env(data: dict[str, Any], prefix: str = "GROK_") -> dict[str, Any]:
    prefix_len = len(prefix)
    for env_key, env_val in os.environ.items():
        if not env_key.startswith(prefix):
            continue
        parts = env_key[prefix_len:].lower().split("_", 1)
        if len(parts) == 2:
            section, key = parts
            data.setdefault(section, {})[key] = env_val
    return data


# Module-level singleton — imported everywhere.
config = ConfigSnapshot()


def get_config(key: str | None = None, default: Any = None) -> Any:
    if key is None:
        return config
    return config.get(key, default)


__all__ = ["ConfigSnapshot", "config", "get_config"]
