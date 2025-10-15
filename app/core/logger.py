"""全局日志模块"""

import sys
import logging
from pathlib import Path
from logging.handlers import RotatingFileHandler
from app.core.config import setting


class LoggerManager:
    """日志管理器"""

    _initialized = False

    def __init__(self):
        """初始化日志"""
        if LoggerManager._initialized:
            return

        # 日志配置
        log_dir = Path(__file__).parents[2] / "logs"
        log_dir.mkdir(exist_ok=True)
        log_level = setting.global_config.get("log_level", "INFO").upper()
        log_format = "%(asctime)s - %(name)s - %(levelname)s - %(message)s"
        log_file = log_dir / "app.log"

        # 配置根日志器
        self.logger = logging.getLogger()
        self.logger.setLevel(log_level)

        # 避免重复添加处理器
        if self.logger.handlers:
            return

        # 创建格式器
        formatter = logging.Formatter(log_format)

        # 控制台处理器
        console_handler = logging.StreamHandler(sys.stdout)
        console_handler.setLevel(log_level)
        console_handler.setFormatter(formatter)

        # 文件处理器
        file_handler = RotatingFileHandler(
            log_file, maxBytes=10 * 1024 * 1024, backupCount=5, encoding="utf-8"
        )
        file_handler.setLevel(log_level)
        file_handler.setFormatter(formatter)

        # 添加处理器到根日志器
        self.logger.addHandler(console_handler)
        self.logger.addHandler(file_handler)

        # 配置第三方库日志级别，避免过多调试信息
        logging.getLogger("asyncio").setLevel(logging.WARNING)
        logging.getLogger("uvicorn").setLevel(logging.INFO)
        logging.getLogger("fastapi").setLevel(logging.INFO)
        logging.getLogger("aiomysql").setLevel(logging.WARNING)

        LoggerManager._initialized = True

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


# 全局日志器实例
logger = LoggerManager()
