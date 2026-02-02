"""
结构化 JSON 日志 - 极简格式
"""

import sys
import json
import traceback
from pathlib import Path
from loguru import logger

# 日志目录
LOG_DIR = Path(__file__).parent.parent.parent / "logs"
LOG_DIR.mkdir(parents=True, exist_ok=True)


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
        log_entry["stacktrace"] = "".join(traceback.format_exception(
            record["exception"].type,
            record["exception"].value,
            record["exception"].traceback
        ))
    
    return json.dumps(log_entry, ensure_ascii=False)


def _make_json_sink(output):
    """创建 JSON sink"""
    def sink(message):
        json_str = _format_json(message.record)
        print(json_str, file=output, flush=True)
    return sink


def _file_json_sink(message):
    """写入日志文件"""
    record = message.record
    json_str = _format_json(record)
    log_file = LOG_DIR / f"app_{record['time'].strftime('%Y-%m-%d')}.log"
    with open(log_file, "a", encoding="utf-8") as f:
        f.write(json_str + "\n")


def setup_logging(
    level: str = "DEBUG",
    json_console: bool = True,
    file_logging: bool = True,
):
    """设置日志配置"""
    logger.remove()
    
    # 控制台输出
    if json_console:
        logger.add(
            _make_json_sink(sys.stdout),
            level=level,
            format="{message}",
            colorize=False,
        )
    else:
        logger.add(
            sys.stdout,
            level=level,
            format="<green>{time:YYYY-MM-DD HH:mm:ss}</green> | <level>{level: <8}</level> | <cyan>{file.name}:{line}</cyan> - <level>{message}</level>",
            colorize=True,
        )
    
    # 文件输出
    if file_logging:
        logger.add(
            _file_json_sink,
            level=level,
            format="{message}",
            enqueue=True,
        )
    
    return logger


def get_logger(trace_id: str = "", span_id: str = ""):
    """获取绑定了 trace 上下文的 logger"""
    bound = {}
    if trace_id:
        bound["traceID"] = trace_id
    if span_id:
        bound["spanID"] = span_id
    return logger.bind(**bound) if bound else logger


__all__ = ["logger", "setup_logging", "get_logger", "LOG_DIR"]
