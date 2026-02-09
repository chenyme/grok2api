"""Token 池选择策略与操作单元测试"""

import copy

from app.core.config import config
from app.services.token.models import TokenInfo, TokenStatus, TokenPoolStats
from app.services.token.pool import TokenPool


def _with_strategy(strategy: str):
    """设置选择策略的上下文管理"""
    return {"token": {"selection_strategy": strategy}}


# ==================== 基础操作 ====================


def test_pool_add_and_get():
    pool = TokenPool("test")
    t = TokenInfo(token="tok1", quota=10)
    pool.add(t)
    assert pool.get("tok1") is t
    assert pool.count() == 1


def test_pool_get_missing_returns_none():
    pool = TokenPool("test")
    assert pool.get("nonexistent") is None


def test_pool_remove_existing():
    pool = TokenPool("test")
    pool.add(TokenInfo(token="tok1"))
    assert pool.remove("tok1") is True
    assert pool.count() == 0


def test_pool_remove_missing_returns_false():
    pool = TokenPool("test")
    assert pool.remove("nonexistent") is False


def test_pool_list():
    pool = TokenPool("test")
    pool.add(TokenInfo(token="a"))
    pool.add(TokenInfo(token="b"))
    tokens = pool.list()
    assert len(tokens) == 2
    assert {t.token for t in tokens} == {"a", "b"}


def test_pool_iter():
    pool = TokenPool("test")
    pool.add(TokenInfo(token="x"))
    pool.add(TokenInfo(token="y"))
    result = [t.token for t in pool]
    assert set(result) == {"x", "y"}


# ==================== select: 空池 / 无可用 ====================


def test_select_empty_pool():
    pool = TokenPool("test")
    assert pool.select() is None


def test_select_all_cooling():
    pool = TokenPool("test")
    pool.add(TokenInfo(token="a", status=TokenStatus.COOLING, quota=0))
    pool.add(TokenInfo(token="b", status=TokenStatus.EXPIRED, quota=0))
    assert pool.select() is None


def test_select_skips_zero_quota():
    pool = TokenPool("test")
    pool.add(TokenInfo(token="a", status=TokenStatus.ACTIVE, quota=0))
    assert pool.select() is None


# ==================== select: max_quota ====================


def test_select_max_quota_picks_highest():
    original = copy.deepcopy(config._config)
    try:
        config._config = _with_strategy("max_quota")
        pool = TokenPool("test")
        pool.add(TokenInfo(token="low", quota=5))
        pool.add(TokenInfo(token="high", quota=50))
        pool.add(TokenInfo(token="mid", quota=20))

        selected = pool.select()
        assert selected is not None
        assert selected.token == "high"
    finally:
        config._config = original


def test_select_max_quota_is_default():
    """无配置时默认 max_quota"""
    original = copy.deepcopy(config._config)
    try:
        config._config = {}
        pool = TokenPool("test")
        pool.add(TokenInfo(token="a", quota=1))
        pool.add(TokenInfo(token="b", quota=100))
        selected = pool.select()
        assert selected is not None
        assert selected.token == "b"
    finally:
        config._config = original


# ==================== select: random ====================


def test_select_random_returns_active_token():
    original = copy.deepcopy(config._config)
    try:
        config._config = _with_strategy("random")
        pool = TokenPool("test")
        pool.add(TokenInfo(token="only", quota=10))
        pool.add(TokenInfo(token="dead", status=TokenStatus.EXPIRED, quota=0))

        selected = pool.select()
        assert selected is not None
        assert selected.token == "only"
    finally:
        config._config = original


def test_select_random_distributes():
    """random 策略应该能选到多个不同 token"""
    original = copy.deepcopy(config._config)
    try:
        config._config = _with_strategy("random")
        pool = TokenPool("test")
        pool.add(TokenInfo(token="a", quota=10))
        pool.add(TokenInfo(token="b", quota=10))
        pool.add(TokenInfo(token="c", quota=10))

        seen = set()
        for _ in range(50):
            t = pool.select()
            if t:
                seen.add(t.token)
        assert len(seen) >= 2  # 极大概率选到至少 2 个
    finally:
        config._config = original


# ==================== select: weighted_random ====================


def test_select_weighted_random_biases_toward_high_quota():
    """高配额 token 被选中概率更大"""
    original = copy.deepcopy(config._config)
    try:
        config._config = _with_strategy("weighted_random")
        pool = TokenPool("test")
        pool.add(TokenInfo(token="tiny", quota=1))
        pool.add(TokenInfo(token="huge", quota=1000))

        counts = {"tiny": 0, "huge": 0}
        for _ in range(200):
            t = pool.select()
            if t:
                counts[t.token] += 1

        # 1000:1 权重，huge 应占绝对多数
        assert counts["huge"] > counts["tiny"] * 5
    finally:
        config._config = original


# ==================== select: lru ====================


def test_select_lru_prefers_never_used():
    """LRU 策略优先选从未使用的 (last_used_at=None)"""
    original = copy.deepcopy(config._config)
    try:
        config._config = _with_strategy("lru")
        pool = TokenPool("test")
        pool.add(TokenInfo(token="used", quota=10, last_used_at=9999))
        pool.add(TokenInfo(token="fresh", quota=10, last_used_at=None))

        selected = pool.select()
        assert selected is not None
        assert selected.token == "fresh"
    finally:
        config._config = original


def test_select_lru_alias_least_recent():
    """least_recent 是 lru 的别名"""
    original = copy.deepcopy(config._config)
    try:
        config._config = _with_strategy("least_recent")
        pool = TokenPool("test")
        pool.add(TokenInfo(token="old", quota=10, last_used_at=100))
        pool.add(TokenInfo(token="new", quota=10, last_used_at=9000))

        selected = pool.select()
        assert selected is not None
        assert selected.token == "old"
    finally:
        config._config = original


# ==================== get_stats ====================


def test_get_stats_counts_correctly():
    pool = TokenPool("test")
    pool.add(TokenInfo(token="a1", status=TokenStatus.ACTIVE, quota=10))
    pool.add(TokenInfo(token="a2", status=TokenStatus.ACTIVE, quota=20))
    pool.add(TokenInfo(token="c1", status=TokenStatus.COOLING, quota=0))
    pool.add(TokenInfo(token="e1", status=TokenStatus.EXPIRED, quota=0))
    pool.add(TokenInfo(token="d1", status=TokenStatus.DISABLED, quota=0))

    stats = pool.get_stats()
    assert isinstance(stats, TokenPoolStats)
    assert stats.total == 5
    assert stats.active == 2
    assert stats.cooling == 1
    assert stats.expired == 1
    assert stats.disabled == 1
    assert stats.total_quota == 30
    assert stats.avg_quota == 6.0


def test_get_stats_empty_pool():
    pool = TokenPool("test")
    stats = pool.get_stats()
    assert stats.total == 0
    assert stats.avg_quota == 0.0
