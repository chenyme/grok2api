from app.services.stats.request_logger import request_logger
from app.services.stats.request_stats import request_stats


def test_request_logger_schedule_save_without_loop_is_safe():
    request_logger._loaded = True
    request_logger._save_task = None
    request_logger._dirty = False

    # 立即刷盘路径
    request_logger._flush_interval = 0
    request_logger._schedule_save()

    # 节流刷盘路径
    request_logger._flush_interval = 2
    request_logger._schedule_save()

    assert request_logger._save_task is None


def test_request_stats_schedule_save_without_loop_is_safe():
    request_stats._loaded = True
    request_stats._save_task = None
    request_stats._dirty = False

    # 立即刷盘路径
    request_stats._flush_interval = 0
    request_stats._schedule_save()

    # 节流刷盘路径
    request_stats._flush_interval = 2
    request_stats._schedule_save()

    assert request_stats._save_task is None
