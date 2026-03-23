"""
结构化 JSON 日志 - 极简格式
"""

import json
import os
import sys
import traceback
from pathlib import Path
from typing import Any

from loguru import logger

# 日志目录
DEFAULT_LOG_DIR = Path(__file__).parent.parent.parent / "logs"
LOG_DIR = Path(os.getenv("LOG_DIR", str(DEFAULT_LOG_DIR)))
DEFAULT_LOG_MAX_FILE_SIZE_MB = 100
DEFAULT_LOG_MAX_FILES = 7
_LOG_DIR_READY = False


def _prepare_log_dir() -> bool:
    """确保日志目录可用"""
    global LOG_DIR, _LOG_DIR_READY
    if _LOG_DIR_READY:
        return True
    try:
        LOG_DIR.mkdir(parents=True, exist_ok=True)
        _LOG_DIR_READY = True
        return True
    except Exception:
        _LOG_DIR_READY = False
        return False


def _format_json(record) -> str:
    """格式化日志"""
    # ISO8601 时间
    time_str = record["time"].strftime("%Y-%m-%dT%H:%M:%S.%f")[:-3]
    tz = record["time"].strftime("%z")
    if tz:
        time_str += tz[:3] + ":" + tz[3:]

    log_entry = {
        "time": time_str,
        "level": record["level"].name.lower(),
        "msg": record["message"],
        "caller": f"{record['file'].name}:{record['line']}",
    }

    # trace 上下文
    extra = record["extra"]
    if extra.get("traceID"):
        log_entry["traceID"] = extra["traceID"]
    if extra.get("spanID"):
        log_entry["spanID"] = extra["spanID"]

    # 其他 extra 字段
    for key, value in extra.items():
        if key not in ("traceID", "spanID") and not key.startswith("_"):
            log_entry[key] = value

    # 错误及以上级别添加堆栈跟踪
    if record["level"].no >= 40 and record["exception"]:
        log_entry["stacktrace"] = "".join(
            traceback.format_exception(
                record["exception"].type,
                record["exception"].value,
                record["exception"].traceback,
            )
        )

    return json.dumps(log_entry, ensure_ascii=False, default=str)


# Provide logging.Logger compatibility for legacy calls
if not hasattr(logger, "isEnabledFor"):
    logger.isEnabledFor = lambda _level: True


def _env_flag(name: str, default: bool) -> bool:
    raw = os.getenv(name)
    if raw is None:
        return default
    return raw.strip().lower() in ("1", "true", "yes", "on", "y")


def _env_int(name: str, default: int) -> int:
    raw = os.getenv(name)
    if raw is None:
        return default
    try:
        return int(raw.strip())
    except (TypeError, ValueError):
        return default


def _patch_json_record(record) -> None:
    """为全局 Loguru 记录补充序列化后的 JSON 文本。"""
    record["extra"]["_json_line"] = _format_json(record)


def setup_logging(
    level: str = "DEBUG",
    json_console: bool = True,
    file_logging: bool = True,
    file_rotation_size_mb: int | None = None,
    file_retention_count: int | None = None,
):
    """设置日志配置"""
    logger.configure(patcher=_patch_json_record)
    logger.remove()
    file_logging = _env_flag("LOG_FILE_ENABLED", file_logging)
    rotation_size_mb = _env_int(
        "LOG_MAX_FILE_SIZE_MB",
        DEFAULT_LOG_MAX_FILE_SIZE_MB
        if file_rotation_size_mb is None
        else int(file_rotation_size_mb),
    )
    retention_count = _env_int(
        "LOG_MAX_FILES",
        DEFAULT_LOG_MAX_FILES
        if file_retention_count is None
        else int(file_retention_count),
    )

    # 控制台输出
    if json_console:
        logger.add(
            sys.stdout,
            level=level,
            format="{extra[_json_line]}",
            colorize=False,
            backtrace=False,
            diagnose=False,
        )
    else:
        logger.add(
            sys.stdout,
            level=level,
            format="<green>{time:YYYY-MM-DD HH:mm:ss}</green> | <level>{level: <8}</level> | <cyan>{file.name}:{line}</cyan> - <level>{message}</level>",
            colorize=True,
            backtrace=False,
            diagnose=False,
        )

    # 文件输出
    if file_logging:
        if _prepare_log_dir():
            file_kwargs: dict[str, Any] = {
                "level": level,
                "format": "{extra[_json_line]}",
                "colorize": False,
                "enqueue": True,
                "encoding": "utf-8",
                "backtrace": False,
                "diagnose": False,
            }
            if rotation_size_mb > 0:
                file_kwargs["rotation"] = rotation_size_mb * 1024 * 1024
            if retention_count > 0:
                file_kwargs["retention"] = retention_count
            logger.add(
                str(LOG_DIR / "app_{time:YYYY-MM-DD}.log"),
                **file_kwargs,
            )
        else:
            logger.warning("File logging disabled: no writable log directory.")

    return logger


def reload_logging_from_config(
    default_level: str = "INFO",
    json_console: bool = False,
):
    """根据运行时配置重新加载日志设置。"""
    try:
        from app.core.config import get_config

        return setup_logging(
            level=default_level,
            json_console=json_console,
            file_logging=True,
            file_rotation_size_mb=get_config(
                "log.max_file_size_mb", DEFAULT_LOG_MAX_FILE_SIZE_MB
            ),
            file_retention_count=get_config("log.max_files", DEFAULT_LOG_MAX_FILES),
        )
    except Exception as exc:
        configured = setup_logging(level=default_level, json_console=json_console)
        configured.warning("Failed to reload logging config: {}", exc)
        return configured


def get_logger(trace_id: str = "", span_id: str = ""):
    """获取绑定了 trace 上下文的 logger"""
    bound = {}
    if trace_id:
        bound["traceID"] = trace_id
    if span_id:
        bound["spanID"] = span_id
    return logger.bind(**bound) if bound else logger


__all__ = [
    "logger",
    "setup_logging",
    "reload_logging_from_config",
    "get_logger",
    "LOG_DIR",
]
