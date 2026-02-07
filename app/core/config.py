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
    迁移废弃的配置节到新配置结构

    Returns:
        (迁移后的配置, 废弃的配置节集合)
    """
    # 配置映射规则：旧配置 -> 新配置
    MIGRATION_MAP = {
        # grok.* -> 对应的新配置节
        "grok.temporary": "chat.temporary",
        "grok.disable_memory": "chat.disable_memory",
        "grok.stream": "chat.stream",
        "grok.thinking": "chat.thinking",
        "grok.dynamic_statsig": "chat.dynamic_statsig",
        "grok.filter_tags": "chat.filter_tags",
        "grok.timeout": "network.timeout",
        "grok.base_proxy_url": "network.base_proxy_url",
        "grok.asset_proxy_url": "network.asset_proxy_url",
        "grok.cf_clearance": "security.cf_clearance",
        "grok.browser": "security.browser",
        "grok.user_agent": "security.user_agent",
        "grok.max_retry": "retry.max_retry",
        "grok.retry_status_codes": "retry.retry_status_codes",
        "grok.retry_backoff_base": "retry.retry_backoff_base",
        "grok.retry_backoff_factor": "retry.retry_backoff_factor",
        "grok.retry_backoff_max": "retry.retry_backoff_max",
        "grok.retry_budget": "retry.retry_budget",
        "grok.stream_idle_timeout": "timeout.stream_idle_timeout",
        "grok.video_idle_timeout": "timeout.video_idle_timeout",
        "grok.image_ws": "image.image_ws",
        "grok.image_ws_nsfw": "image.image_ws_nsfw",
        "grok.image_ws_blocked_seconds": "image.image_ws_blocked_seconds",
        "grok.image_ws_final_min_bytes": "image.image_ws_final_min_bytes",
        "grok.image_ws_medium_min_bytes": "image.image_ws_medium_min_bytes",
    }

    deprecated_sections = set(config.keys()) - valid_sections
    if not deprecated_sections:
        return config, set()

    result = {k: deepcopy(v) for k, v in config.items() if k in valid_sections}
    migrated_count = 0

    # 处理废弃配置节中的配置项
    for old_section in deprecated_sections:
        if old_section not in config or not isinstance(config[old_section], dict):
            continue

        for old_key, old_value in config[old_section].items():
            # 查找映射规则
            old_path = f"{old_section}.{old_key}"
            new_path = MIGRATION_MAP.get(old_path)

            if new_path:
                new_section, new_key = new_path.split(".", 1)
                # 确保新配置节存在
                if new_section not in result:
                    result[new_section] = {}
                # 迁移配置项（保留用户的自定义值）
                result[new_section][new_key] = old_value
                migrated_count += 1
                logger.debug(f"Migrated config: {old_path} -> {new_path} = {old_value}")

    if migrated_count > 0:
        logger.info(f"Migrated {migrated_count} config items from deprecated sections")

    return result, deprecated_sections


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

            # 检查是否有废弃的配置节
            valid_sections = set(self._defaults.keys())
            config_data, deprecated_sections = _migrate_deprecated_config(
                config_data, valid_sections
            )
            if deprecated_sections:
                logger.info(
                    f"Cleaned deprecated config sections: {deprecated_sections}"
                )

            merged = _deep_merge(self._defaults, config_data)

            # 自动回填缺失配置到存储
            # 或迁移了配置后需要更新
            should_persist = (
                (not from_remote) or (merged != config_data) or deprecated_sections
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
            base = _deep_merge(self._defaults, self._config or {})
            merged = _deep_merge(base, new_config or {})
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
