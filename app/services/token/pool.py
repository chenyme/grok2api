"""Token 池管理"""

import random
from typing import Dict, List, Optional, Iterator

from app.services.token.models import TokenInfo, TokenStatus, TokenPoolStats


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

    def select(self, exclude: set = None) -> Optional[TokenInfo]:
        """
        选择一个可用 Token
        策略:
        1. 选择 active 状态且有配额的 token
        2. 优先选择剩余额度最多的
        3. 如果额度相同，随机选择（避免并发冲突）
        """
        # 选择 token
        available = [
            t
            for t in self._tokens.values()
            if t.status == TokenStatus.ACTIVE and t.quota > 0
            and (not exclude or t.token not in exclude)
        ]

        if not available:
            return None

        # 找到最大额度
        max_quota = max(t.quota for t in available)

        # 筛选最大额度
        candidates = [t for t in available if t.quota == max_quota]

        # 随机选择
        return random.choice(candidates)

    def count(self) -> int:
        """Token 数量"""
        return len(self._tokens)

    def list(self) -> List[TokenInfo]:
        """获取所有 Token"""
        return list(self._tokens.values())

    def get_stats(self) -> TokenPoolStats:
        """获取池统计信息"""
        stats = TokenPoolStats(total=len(self._tokens))

        for token in self._tokens.values():
            stats.total_quota += token.quota

            if token.status == TokenStatus.ACTIVE:
                stats.active += 1
            elif token.status == TokenStatus.DISABLED:
                stats.disabled += 1
            elif token.status == TokenStatus.EXPIRED:
                stats.expired += 1
            elif token.status == TokenStatus.COOLING:
                stats.cooling += 1

        if stats.total > 0:
            stats.avg_quota = stats.total_quota / stats.total

        return stats

    def _rebuild_index(self):
        """重建索引（预留接口，用于加载时调用）"""
        pass

    def __iter__(self) -> Iterator[TokenInfo]:
        return iter(self._tokens.values())


__all__ = ["TokenPool"]
