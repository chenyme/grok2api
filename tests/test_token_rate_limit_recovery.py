import asyncio
import time
from unittest.mock import AsyncMock

from app.services.grok.utils.retry import pick_token
from app.services.token.manager import TokenManager, BASIC_POOL_NAME
from app.services.token.models import TokenInfo, TokenStatus
from app.services.token.pool import TokenPool


async def _noop_save(*_args, **_kwargs):
    return None


def _build_manager(token: TokenInfo) -> TokenManager:
    manager = TokenManager()
    pool = TokenPool(BASIC_POOL_NAME)
    pool.add(token)
    manager.pools = {BASIC_POOL_NAME: pool}
    manager._schedule_save = lambda: None
    manager._save = _noop_save
    return manager


def test_mark_rate_limited_uses_short_backoff(monkeypatch):
    monkeypatch.setattr(
        "app.services.token.manager.get_config",
        lambda key, default=None: 300 if key == "token.rate_limit_backoff_seconds" else default,
    )

    token = TokenInfo(token="tok-1", quota=40)
    manager = _build_manager(token)

    asyncio.run(manager.mark_rate_limited("tok-1"))

    assert token.status == TokenStatus.COOLING
    assert token.quota == 40
    assert token.cooldown_until is not None
    remaining_ms = token.cooldown_until - int(time.time() * 1000)
    assert 0 < remaining_ms <= 300_000


def test_refresh_cooling_tokens_recovers_after_backoff(monkeypatch):
    monkeypatch.setattr(
        "app.services.token.manager.get_config",
        lambda key, default=None: 60 if key == "token.refresh_interval_hours" else default,
    )
    class DummyRetryContext:
        def __init__(self):
            self.attempt = 0
            self.max_retry = 0
            self.total_delay = 0.0
            self.retry_budget = 0.0

        def record_error(self, status, error):
            self.attempt += 1

        def should_retry(self, status, error):
            return False

        def calculate_delay(self, status, retry_after):
            return 0.0

        def record_delay(self, delay):
            self.total_delay += delay

    monkeypatch.setattr(
        "app.services.token.manager.RetryContext",
        DummyRetryContext,
    )

    token = TokenInfo(
        token="tok-2",
        status=TokenStatus.COOLING,
        quota=0,
        cooldown_until=int(time.time() * 1000) - 1,
    )
    manager = _build_manager(token)

    async def fake_get(_token: str):
        return {"remainingQueries": 40, "windowSizeSeconds": 7200}

    monkeypatch.setattr(
        "app.services.token.manager.UsageService.get",
        AsyncMock(side_effect=fake_get),
    )

    result = asyncio.run(manager.refresh_cooling_tokens(trigger="test"))

    assert result == {"checked": 1, "refreshed": 1, "recovered": 1, "expired": 0}
    assert token.status == TokenStatus.ACTIVE
    assert token.quota == 40
    assert token.cooldown_until is None


def test_pick_token_does_not_trigger_on_demand_refresh_when_empty():
    class DummyManager:
        def __init__(self):
            self.calls = 0

        def get_token(self, pool_name, exclude=None, prefer_tags=None):
            self.calls += 1
            return None

        async def refresh_cooling_tokens_on_demand(self):
            raise AssertionError("should not be called")

    manager = DummyManager()

    token = asyncio.run(pick_token(manager, "grok-4", set()))

    assert token is None
    assert manager.calls >= 1
