"""
Token 数据模型

额度规则:
- 新号默认 80 配额
- 重置后恢复 80
- lowEffort 扣 1，highEffort 扣 4
"""

from enum import Enum
from typing import Optional, List
from pydantic import BaseModel, Field
from datetime import datetime


# 默认配额
DEFAULT_QUOTA = 80

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
    LOW = "low"    # 扣 1
    HIGH = "high"  # 扣 4


EFFORT_COST = {
    EffortType.LOW: 1,
    EffortType.HIGH: 4,
}


class TokenInfo(BaseModel):
    """Token 信息"""
    
    token: str
    status: TokenStatus = TokenStatus.ACTIVE
    quota: int = DEFAULT_QUOTA
    
    # 统计
    created_at: int = Field(default_factory=lambda: int(datetime.now().timestamp() * 1000))
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
            effort: LOW 扣 1，HIGH 扣 4
            
        Returns:
            实际扣除的配额
        """
        cost = EFFORT_COST[effort]
        actual_cost = min(cost, self.quota)
        
        self.last_used_at = int(datetime.now().timestamp() * 1000)
        self.use_count += 1
        self.quota = max(0, self.quota - cost)
        
        # 成功消耗后清空失败计数
        self.fail_count = 0
        self.last_fail_reason = None
        
        if self.quota == 0:
            self.status = TokenStatus.COOLING
        elif self.status in [TokenStatus.COOLING, TokenStatus.EXPIRED]:
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
        elif self.quota > 0 and self.status in [TokenStatus.COOLING, TokenStatus.EXPIRED]:
            self.status = TokenStatus.ACTIVE
    
    def reset(self):
        """重置配额到默认值"""
        self.quota = DEFAULT_QUOTA
        self.status = TokenStatus.ACTIVE
        self.fail_count = 0
        self.last_fail_reason = None
    
    def record_fail(self, status_code: int = 401, reason: str = ""):
        """记录失败，达到阈值后自动标记为 expired"""
        # 仅 401 错误才计入失败
        if status_code != 401:
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
    "DEFAULT_QUOTA",
    "FAIL_THRESHOLD",
]
