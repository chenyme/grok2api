"""
Token 数据模型

额度规则:
- Basic 新号默认 80 配额
- Super 新号默认 140 配额
- 重置后恢复默认值
- lowEffort 扣 1，highEffort 扣 4
"""

from enum import Enum
from typing import Optional, List
from pydantic import BaseModel, Field
from datetime import datetime


# 默认配额
BASIC__DEFAULT_QUOTA = 80
SUPER_DEFAULT_QUOTA = 140

# 失败阈值
FAIL_THRESHOLD = 5


class TokenStatus(str, Enum):
    """Token 状态"""

    ACTIVE = "active"
    DISABLED = "disabled"
    EXPIRED = "expired"
    COOLING = "cooling"


class EffortType(str, Enum):
    """请求消耗类型"""

    LOW = "low"  # 扣 1
    HIGH = "high"  # 扣 4


EFFORT_COST = {
    EffortType.LOW: 1,
    EffortType.HIGH: 4,
}


class TokenInfo(BaseModel):
    """Token 信息"""

    token: str
    status: TokenStatus = TokenStatus.ACTIVE
    quota: int = BASIC__DEFAULT_QUOTA

    # 统计
    created_at: int = Field(
        default_factory=lambda: int(datetime.now().timestamp() * 1000)
    )
    last_used_at: Optional[int] = None
    use_count: int = 0

    # 失败追踪
    fail_count: int = 0
    last_fail_at: Optional[int] = None
    last_fail_reason: Optional[str] = None

    # 冷却管理
    last_sync_at: Optional[int] = None  # 上次同步时间

    # 扩展
    tags: List[str] = Field(default_factory=list)
    note: str = ""
    last_asset_clear_at: Optional[int] = None

    def is_available(self) -> bool:
        """检查是否可用（状态正常且配额 > 0）"""
        return self.status == TokenStatus.ACTIVE and self.quota > 0

    def consume(self, effort: EffortType = EffortType.LOW) -> int:
        """
        消耗配额

        Args:
            effort: LOW 扣 1 配额并计 1 次，HIGH 扣 4 配额并计 4 次

        Returns:
            实际扣除的配额
        """
        cost = EFFORT_COST[effort]
        actual_cost = min(cost, self.quota)

        self.last_used_at = int(datetime.now().timestamp() * 1000)
        self.use_count += actual_cost  # 使用 actual_cost 避免配额不足时过度计数
        self.quota = max(0, self.quota - actual_cost)

        # 注意：不在这里清零 fail_count，只有 record_success() 才清零
        # 这样可以避免失败后调用 consume 导致失败计数被重置

        if self.quota == 0:
            self.status = TokenStatus.COOLING
        elif self.status == TokenStatus.COOLING:
            # 只从 COOLING 恢复，不从 EXPIRED 恢复
            self.status = TokenStatus.ACTIVE

        return actual_cost

    def update_quota(self, new_quota: int):
        """
        更新配额（用于 API 同步）

        Args:
            new_quota: 新的配额值
        """
        self.quota = max(0, new_quota)

        if self.quota == 0:
            self.status = TokenStatus.COOLING
        elif self.quota > 0 and self.status in [
            TokenStatus.COOLING,
            TokenStatus.EXPIRED,
        ]:
            self.status = TokenStatus.ACTIVE

    def reset(self, default_quota: Optional[int] = None):
        """重置配额到默认值"""
        quota = BASIC__DEFAULT_QUOTA if default_quota is None else default_quota
        self.quota = max(0, int(quota))
        self.status = TokenStatus.ACTIVE
        self.fail_count = 0
        self.last_fail_reason = None

    def record_fail(self, status_code: int = 401, reason: str = ""):
        """记录失败，达到阈值后自动标记为 expired"""
        # 401/403 错误计入失败（都表示认证/授权失败）
        if status_code not in (401, 403):
            return

        self.fail_count += 1
        self.last_fail_at = int(datetime.now().timestamp() * 1000)
        self.last_fail_reason = reason

        if self.fail_count >= FAIL_THRESHOLD:
            self.status = TokenStatus.EXPIRED

    def record_success(self, is_usage: bool = True):
        """记录成功，清空失败计数并根据配额更新状态"""
        self.fail_count = 0
        self.last_fail_at = None
        self.last_fail_reason = None

        if is_usage:
            self.use_count += 1
            self.last_used_at = int(datetime.now().timestamp() * 1000)

        if self.quota == 0:
            self.status = TokenStatus.COOLING
        else:
            self.status = TokenStatus.ACTIVE

    def need_refresh(self, interval_hours: int = 8) -> bool:
        """检查是否需要刷新配额"""
        if self.status != TokenStatus.COOLING:
            return False

        if self.last_sync_at is None:
            return True

        now = int(datetime.now().timestamp() * 1000)
        interval_ms = interval_hours * 3600 * 1000
        return (now - self.last_sync_at) >= interval_ms

    def mark_synced(self):
        """标记已同步"""
        self.last_sync_at = int(datetime.now().timestamp() * 1000)


class TokenPoolStats(BaseModel):
    """Token 池统计"""

    total: int = 0
    active: int = 0
    disabled: int = 0
    expired: int = 0
    cooling: int = 0
    total_quota: int = 0
    avg_quota: float = 0.0


__all__ = [
    "TokenStatus",
    "TokenInfo",
    "TokenPoolStats",
    "EffortType",
    "EFFORT_COST",
    "BASIC__DEFAULT_QUOTA",
    "SUPER_DEFAULT_QUOTA",
    "FAIL_THRESHOLD",
]
