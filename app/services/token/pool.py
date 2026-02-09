"""Token 池管理"""

import random
from typing import Dict, List, Optional, Iterator

from app.services.token.models import TokenInfo, TokenStatus, TokenPoolStats
from app.core.config import get_config


class TokenPool:
    """Token 池（管理一组 Token）"""

    def __init__(self, name: str):
        self.name = name
        self._tokens: Dict[str, TokenInfo] = {}

    def add(self, token: TokenInfo):
        """添加 Token"""
        self._tokens[token.token] = token

    def remove(self, token_str: str) -> bool:
        """删除 Token"""
        if token_str in self._tokens:
            del self._tokens[token_str]
            return True
        return False

    def get(self, token_str: str) -> Optional[TokenInfo]:
        """获取 Token"""
        return self._tokens.get(token_str)

    def select(self) -> Optional[TokenInfo]:
        """
        选择一个可用 Token
        策略 (可配置 token.selection_strategy):
        - max_quota: 优先选择剩余额度最多的；同额度随机
        - random: 在可用 token 中随机
        - weighted_random: 按 quota 权重随机
        - lru / least_recent: 最久未使用优先
        """
        max_quota = -1
        active: List[TokenInfo] = []

        for t in self._tokens.values():
            if t.status != TokenStatus.ACTIVE or t.quota <= 0:
                continue
            active.append(t)
            if t.quota > max_quota:
                max_quota = t.quota

        if not active:
            return None

        # 高并发下多个协程可能同时进入 select，先 shuffle 降低选中同一 token 的概率
        random.shuffle(active)

        strategy = str(get_config("token.selection_strategy", "max_quota")).lower()

        if strategy == "random":
            return active[0]

        if strategy in ("lru", "least_recent"):
            oldest = min((t.last_used_at or 0) for t in active)
            candidates = [t for t in active if (t.last_used_at or 0) == oldest]
            return candidates[0]

        if strategy == "weighted_random":
            weights = [max(1, int(t.quota)) for t in active]
            return random.choices(active, weights=weights, k=1)[0]

        # 默认 max_quota：已 shuffle，直接取第一个最大额度的即可
        candidates = [t for t in active if t.quota == max_quota]
        return candidates[0]

    def count(self) -> int:
        """Token 数量"""
        return len(self._tokens)

    def list(self) -> List[TokenInfo]:
        """获取所有 Token"""
        return list(self._tokens.values())

    def get_stats(self) -> TokenPoolStats:
        """获取池统计信息（单次遍历）"""
        total = len(self._tokens)
        total_quota = 0
        active = 0
        disabled = 0
        expired = 0
        cooling = 0

        for token in self._tokens.values():
            total_quota += token.quota
            status = token.status
            if status == TokenStatus.ACTIVE:
                active += 1
            elif status == TokenStatus.DISABLED:
                disabled += 1
            elif status == TokenStatus.EXPIRED:
                expired += 1
            elif status == TokenStatus.COOLING:
                cooling += 1

        return TokenPoolStats(
            total=total,
            active=active,
            disabled=disabled,
            expired=expired,
            cooling=cooling,
            total_quota=total_quota,
            avg_quota=total_quota / total if total > 0 else 0.0,
        )

    def _rebuild_index(self):
        """重建索引（预留接口，用于加载时调用）"""
        pass

    def __iter__(self) -> Iterator[TokenInfo]:
        return iter(self._tokens.values())


__all__ = ["TokenPool"]
