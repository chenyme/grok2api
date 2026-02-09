"""
配置管理

- config.toml: 运行时配置
- config.defaults.toml: 默认配置基线
"""

from copy import deepcopy
from pathlib import Path
from typing import Any, Dict
import tomllib

from app.core.logger import logger

DEFAULT_CONFIG_FILE = Path(__file__).parent.parent.parent / "config.defaults.toml"


def _deep_merge(base: Dict[str, Any], override: Dict[str, Any]) -> Dict[str, Any]:
    """深度合并字典: override 覆盖 base."""
    if not isinstance(base, dict):
        return deepcopy(override) if isinstance(override, dict) else deepcopy(base)

    result = deepcopy(base)
    if not isinstance(override, dict):
        return result

    for key, val in override.items():
        if isinstance(val, dict) and isinstance(result.get(key), dict):
            result[key] = _deep_merge(result[key], val)
        else:
            result[key] = val
    return result


def _migrate_deprecated_config(
    config: Dict[str, Any], valid_sections: set
) -> tuple[Dict[str, Any], set]:
    """
    过滤废弃配置节，仅保留当前有效配置结构。

    Returns:
        (过滤后的配置, 被移除的废弃配置节集合)
    """
    deprecated_sections = set(config.keys()) - valid_sections
    if not deprecated_sections:
        return config, set()

    result = {k: deepcopy(v) for k, v in config.items() if k in valid_sections}
    return result, deprecated_sections


# 字段重命名映射: (section, old_key) -> new_key
_FIELD_RENAMES = {
    ("app", "app_key"): "app_password",
}


def _migrate_renamed_fields(config: Dict[str, Any]) -> bool:
    """迁移已重命名的配置字段。返回 True 表示有迁移发生。"""
    migrated = False
    for (section, old_key), new_key in _FIELD_RENAMES.items():
        sec = config.get(section)
        if not isinstance(sec, dict):
            continue
        if old_key in sec and new_key not in sec:
            sec[new_key] = sec.pop(old_key)
            migrated = True
            logger.info(f"Migrated config: [{section}].{old_key} → {new_key}")
        elif old_key in sec and new_key in sec:
            # 新旧都存在，删除旧的
            del sec[old_key]
            migrated = True
    return migrated


def _strip_empty_numeric_overrides(
    data: Dict[str, Any], defaults: Dict[str, Any]
) -> Dict[str, Any]:
    """清理数值类字段的空字符串覆盖，避免 '' 覆盖默认数值"""
    if not isinstance(data, dict) or not isinstance(defaults, dict):
        return data

    result = deepcopy(data)
    for section, items in result.items():
        if not isinstance(items, dict):
            continue
        default_section = defaults.get(section, {})
        if not isinstance(default_section, dict):
            continue
        for key in list(items.keys()):
            if items[key] == "" and isinstance(default_section.get(key), (int, float)):
                del items[key]
    return result


def _load_defaults() -> Dict[str, Any]:
    """加载默认配置文件"""
    if not DEFAULT_CONFIG_FILE.exists():
        return {}
    try:
        with DEFAULT_CONFIG_FILE.open("rb") as f:
            return tomllib.load(f)
    except Exception as e:
        logger.warning(f"Failed to load defaults from {DEFAULT_CONFIG_FILE}: {e}")
        return {}


class Config:
    """配置管理器"""

    _instance = None
    _config = {}

    def __init__(self):
        self._config = {}
        self._defaults = {}
        self._code_defaults = {}
        self._defaults_loaded = False

    def register_defaults(self, defaults: Dict[str, Any]):
        """注册代码中定义的默认值"""
        self._code_defaults = _deep_merge(self._code_defaults, defaults)

    def _ensure_defaults(self):
        if self._defaults_loaded:
            return
        file_defaults = _load_defaults()
        # 合并文件默认值和代码默认值（代码默认值优先级更低）
        self._defaults = _deep_merge(self._code_defaults, file_defaults)
        self._defaults_loaded = True

    async def load(self):
        """显式加载配置"""
        try:
            from app.core.storage import get_storage, LocalStorage

            self._ensure_defaults()

            storage = get_storage()
            config_data = await storage.load_config()
            from_remote = True

            # 从本地 data/config.toml 初始化后端
            if config_data is None:
                local_storage = LocalStorage()
                from_remote = False
                try:
                    # 尝试读取本地配置
                    config_data = await local_storage.load_config()
                except Exception as e:
                    logger.info(f"Failed to auto-init config from local: {e}")
                    config_data = {}

            config_data = config_data or {}

            # 迁移已重命名的字段（如 app_key → app_password）
            fields_migrated = _migrate_renamed_fields(config_data)

            # 检查是否有废弃的配置节
            valid_sections = set(self._defaults.keys())
            config_data, deprecated_sections = _migrate_deprecated_config(
                config_data, valid_sections
            )
            if deprecated_sections:
                logger.info(f"Cleaned deprecated config sections: {deprecated_sections}")

            # 清理空字符串对数值默认值的覆盖
            config_data = _strip_empty_numeric_overrides(config_data, self._defaults)

            merged = _deep_merge(self._defaults, config_data)

            # 自动回填缺失配置到存储
            # 或迁移了配置后需要更新
            should_persist = (
                (not from_remote)
                or (merged != config_data)
                or deprecated_sections
                or fields_migrated
            )
            if should_persist:
                async with storage.acquire_lock("config_save", timeout=10):
                    await storage.save_config(merged)
                if not from_remote:
                    logger.info(
                        f"Initialized remote storage ({storage.__class__.__name__}) with config baseline."
                    )
                if deprecated_sections:
                    logger.info("Configuration automatically migrated and cleaned.")

            self._config = merged
        except Exception as e:
            logger.error(f"Error loading config: {e}")
            self._config = {}

    def get(self, key: str, default: Any = None) -> Any:
        """
        获取配置值

        Args:
            key: 配置键，格式 "section.key"
            default: 默认值
        """
        if "." in key:
            try:
                section, attr = key.split(".", 1)
                return self._config.get(section, {}).get(attr, default)
            except (ValueError, AttributeError):
                return default

        return self._config.get(key, default)

    async def update(self, new_config: dict):
        """更新配置"""
        from app.core.storage import get_storage

        storage = get_storage()
        async with storage.acquire_lock("config_save", timeout=10):
            self._ensure_defaults()
            cleaned = _strip_empty_numeric_overrides(new_config or {}, self._defaults)
            base = _deep_merge(self._defaults, self._config or {})
            merged = _deep_merge(base, cleaned)
            await storage.save_config(merged)
            self._config = merged


# 全局配置实例
config = Config()


def get_config(key: str, default: Any = None) -> Any:
    """获取配置"""
    return config.get(key, default)


def register_defaults(defaults: Dict[str, Any]):
    """注册默认配置"""
    config.register_defaults(defaults)


__all__ = ["Config", "config", "get_config", "register_defaults"]
