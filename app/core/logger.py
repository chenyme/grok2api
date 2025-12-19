"""全局日志模块 - 单例模式的日志管理器"""

import sys
import logging
from pathlib import Path
from logging.handlers import RotatingFileHandler

from app.core.config import setting


# 过滤模式
FILTER_PATTERNS = [
    "chunk: b'",  # SSE原始字节
    "Got event:",  # SSE事件
    "Closing",  # SSE关闭
]


class MCPLogFilter(logging.Filter):
    """MCP日志过滤器 - 过滤大量数据的DEBUG日志"""

    def filter(self, record: logging.LogRecord) -> bool:
        """过滤日志"""
        # 过滤SSE的DEBUG日志
        if record.name == "sse_starlette.sse" and record.levelno == logging.DEBUG:
            msg = record.getMessage()
            return not any(p in msg for p in FILTER_PATTERNS)

        # 过滤MCP streamable_http的DEBUG日志
        if "mcp.server.streamable_http" in record.name and record.levelno == logging.DEBUG:
            return False

        return True


class LoggerManager:
    """日志管理器（单例）"""

    _instance = None
    _initialized = False

    def __new__(cls):
        if cls._instance is None:
            cls._instance = super().__new__(cls)
        return cls._instance

    def __init__(self):
        """初始化日志系统"""
        if LoggerManager._initialized:
            return

        # 配置
        log_dir = Path(__file__).parents[2] / "logs"
        log_dir.mkdir(exist_ok=True)
        log_level = setting.global_config.get("log_level", "INFO").upper()
        log_format = "%(asctime)s - %(name)s - %(levelname)s - %(message)s"
        log_file = log_dir / "app.log"

        # 根日志器
        self.logger = logging.getLogger()
        self.logger.setLevel(log_level)

        # 避免重复添加
        if self.logger.handlers:
            return

        # 格式器和过滤器
        formatter = logging.Formatter(log_format)
        mcp_filter = MCPLogFilter()

        # 控制台处理器
        console = logging.StreamHandler(sys.stdout)
        console.setLevel(log_level)
        console.setFormatter(formatter)
        console.addFilter(mcp_filter)

        # 文件处理器（10MB，5个备份）
        file_handler = RotatingFileHandler(
            log_file, maxBytes=10*1024*1024, backupCount=5, encoding="utf-8"
        )
        file_handler.setLevel(log_level)
        file_handler.setFormatter(formatter)
        file_handler.addFilter(mcp_filter)

        # 添加处理器
        self.logger.addHandler(console)
        self.logger.addHandler(file_handler)

        # 配置第三方库
        self._configure_third_party()

        LoggerManager._initialized = True
    
    def _configure_third_party(self):
        """配置第三方库日志级别"""
        config = {
            "asyncio": logging.WARNING,
            "uvicorn": logging.INFO,
            "fastapi": logging.INFO,
            "aiomysql": logging.WARNING,
            "mcp": logging.CRITICAL,
            "fastmcp": logging.CRITICAL,
        }
        
        for name, level in config.items():
            logging.getLogger(name).setLevel(level)

    def debug(self, msg: str) -> None:
        """调试日志"""
        self.logger.debug(msg)

    def info(self, msg: str) -> None:
        """信息日志"""
        self.logger.info(msg)

    def warning(self, msg: str) -> None:
        """警告日志"""
        self.logger.warning(msg)

    def error(self, msg: str) -> None:
        """错误日志"""
        self.logger.error(msg)

    def critical(self, msg: str) -> None:
        """严重错误日志"""
        self.logger.critical(msg)


# 全局实例
logger = LoggerManager()
