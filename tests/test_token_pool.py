import copy

from app.core.config import config
from app.services.token.models import TokenInfo
from app.services.token.pool import TokenPool


def test_token_pool_lru_strategy_prefers_oldest():
    original = copy.deepcopy(config._config)
    try:
        config._config = {"token": {"selection_strategy": "lru"}}
        pool = TokenPool("ssoBasic")
        t1 = TokenInfo(token="a", quota=10, last_used_at=1000)
        t2 = TokenInfo(token="b", quota=10, last_used_at=2000)
        t3 = TokenInfo(token="c", quota=10, last_used_at=None)
        pool.add(t1)
        pool.add(t2)
        pool.add(t3)

        selected = pool.select()
        assert selected is not None
        assert selected.token == "c"
    finally:
        config._config = original
