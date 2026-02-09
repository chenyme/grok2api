"""请求统计模块 - 按小时/天统计请求数据"""

from datetime import datetime, timedelta
from typing import Dict, Any
from collections import defaultdict

from app.core.config import get_config
from .base import AsyncJsonStore


class RequestStats(AsyncJsonStore):
    """请求统计管理器（单例）"""

    _instance = None

    def __new__(cls):
        if cls._instance is None:
            cls._instance = super().__new__(cls)
        return cls._instance

    def __init__(self):
        if hasattr(self, "_initialized"):
            return

        super().__init__("stats.json")

        self._hourly: Dict[str, Dict[str, int]] = defaultdict(
            lambda: {"total": 0, "success": 0, "failed": 0}
        )
        self._daily: Dict[str, Dict[str, int]] = defaultdict(
            lambda: {"total": 0, "success": 0, "failed": 0}
        )
        self._models: Dict[str, int] = defaultdict(int)

        self._hourly_keep = 48
        self._daily_keep = 30

        self._initialized = True

    # ── AsyncJsonStore 接口 ──

    def _serialize(self) -> Any:
        return {
            "hourly": dict(self._hourly),
            "daily": dict(self._daily),
            "models": dict(self._models),
        }

    def _deserialize(self, data: Any) -> None:
        self._hourly = defaultdict(lambda: {"total": 0, "success": 0, "failed": 0})
        self._hourly.update(data.get("hourly", {}))
        self._daily = defaultdict(lambda: {"total": 0, "success": 0, "failed": 0})
        self._daily.update(data.get("daily", {}))
        self._models = defaultdict(int)
        self._models.update(data.get("models", {}))

    # ── 配置 ──

    def _apply_config(self) -> None:
        hourly_keep = get_config("stats.hourly_retention", 48)
        daily_keep = get_config("stats.daily_retention", 30)
        flush_interval = get_config("stats.flush_interval_sec", 2)
        try:
            hourly_keep = int(hourly_keep)
        except Exception:
            hourly_keep = 48
        try:
            daily_keep = int(daily_keep)
        except Exception:
            daily_keep = 30
        try:
            flush_interval = float(flush_interval)
        except Exception:
            flush_interval = 2.0

        self._hourly_keep = max(1, hourly_keep)
        self._daily_keep = max(1, daily_keep)
        self._flush_interval = max(0.0, flush_interval)

    async def init(self):
        self._apply_config()
        if not self._loaded:
            await self._load_data()

    # ── 业务逻辑 ──

    async def record_request(self, model: str, success: bool) -> None:
        if not self._loaded:
            await self.init()
        else:
            self._apply_config()

        now = datetime.now()
        hour_key = now.strftime("%Y-%m-%dT%H")
        day_key = now.strftime("%Y-%m-%d")

        self._hourly[hour_key]["total"] += 1
        if success:
            self._hourly[hour_key]["success"] += 1
        else:
            self._hourly[hour_key]["failed"] += 1

        self._daily[day_key]["total"] += 1
        if success:
            self._daily[day_key]["success"] += 1
        else:
            self._daily[day_key]["failed"] += 1

        self._models[model] += 1
        self._cleanup()
        self._schedule_save()

    def _cleanup(self) -> None:
        hour_keys = list(self._hourly.keys())
        if len(hour_keys) > self._hourly_keep:
            for key in sorted(hour_keys)[: -self._hourly_keep]:
                del self._hourly[key]

        day_keys = list(self._daily.keys())
        if len(day_keys) > self._daily_keep:
            for key in sorted(day_keys)[: -self._daily_keep]:
                del self._daily[key]

    def get_stats(self, hours: int = 24, days: int = 7) -> Dict[str, Any]:
        now = datetime.now()

        hourly_data = []
        for i in range(hours - 1, -1, -1):
            dt = now - timedelta(hours=i)
            key = dt.strftime("%Y-%m-%dT%H")
            data = self._hourly.get(key, {"total": 0, "success": 0, "failed": 0})
            hourly_data.append(
                {"hour": dt.strftime("%H:00"), "date": dt.strftime("%m-%d"), **data}
            )

        daily_data = []
        for i in range(days - 1, -1, -1):
            dt = now - timedelta(days=i)
            key = dt.strftime("%Y-%m-%d")
            data = self._daily.get(key, {"total": 0, "success": 0, "failed": 0})
            daily_data.append({"date": dt.strftime("%m-%d"), **data})

        model_data = sorted(self._models.items(), key=lambda x: x[1], reverse=True)[:10]

        total_requests = sum(d["total"] for d in self._hourly.values())
        total_success = sum(d["success"] for d in self._hourly.values())
        total_failed = sum(d["failed"] for d in self._hourly.values())

        return {
            "hourly": hourly_data,
            "daily": daily_data,
            "models": [{"model": m, "count": c} for m, c in model_data],
            "summary": {
                "total": total_requests,
                "success": total_success,
                "failed": total_failed,
                "success_rate": (
                    round(total_success / total_requests * 100, 1)
                    if total_requests > 0
                    else 0
                ),
            },
        }

    async def reset(self) -> None:
        self._hourly.clear()
        self._daily.clear()
        self._models.clear()
        await self._save_data()


# 全局实例
request_stats = RequestStats()
