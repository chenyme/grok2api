"""Token 池管理"""

import random
from typing import Dict, List, Optional, Iterator, Set

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

    def _is_consumed_mode(self) -> bool:
        """检查是否启用 consumed 模式"""
        try:
            return get_config("token.consumed_mode_enabled", False)
        except Exception:
            return False

    def select(
        self, exclude: set = None, prefer_tags: Optional[Set[str]] = None
    ) -> Optional[TokenInfo]:
        """
        选择一个可用 Token

        默认模式（consumed_mode_enabled=false）:
            1. 选择 active 状态且 quota > 0 的 token
            2. 优先选择剩余额度最多的
            3. 如果额度相同，随机选择

        Consumed 模式（consumed_mode_enabled=true）:
            1. 选择 active 状态的 token
            2. 优先选择消耗次数（consumed）最少的
            3. 如果 consumed 相同，随机选择

        Args:
            exclude: 需要排除的 token 字符串集合
            prefer_tags: 优先选择包含这些 tag 的 token（若存在则仅在其子集中选择）
        """
        consumed_mode = self._is_consumed_mode()

        if consumed_mode:
            # ===== Consumed 模式（新逻辑）=====
            available = [
                t
                for t in self._tokens.values()
                if t.is_available(consumed_mode=True)
                and (not exclude or t.token not in exclude)
            ]

            if not available:
                return None

            # 优先选带指定标签的 token（若存在）
            if prefer_tags:
                preferred = [
                    t for t in available if prefer_tags.issubset(set(t.tags or []))
                ]
                if preferred:
                    available = preferred

            # 找到最小消耗（优先选择消耗少的）
            min_consumed = min(t.consumed for t in available)
            candidates = [t for t in available if t.consumed == min_consumed]
            return random.choice(candidates)


        else:
            # ===== 默认模式（旧逻辑）=====
            available = [
                t
                for t in self._tokens.values()
                if t.is_available(consumed_mode=False)
                and (not exclude or t.token not in exclude)
            ]

            if not available:
                return None

            # 优先选带指定标签的 token（若存在）
            if prefer_tags:
                preferred = [
                    t for t in available if prefer_tags.issubset(set(t.tags or []))
                ]
                if preferred:
                    available = preferred

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
            stats.total_consumed += token.consumed

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
            stats.avg_consumed = stats.total_consumed / stats.total

        return stats

    def _rebuild_index(self):
        """重建索引（预留接口，用于加载时调用）"""
        pass

    def __iter__(self) -> Iterator[TokenInfo]:
        return iter(self._tokens.values())


__all__ = ["TokenPool"]
