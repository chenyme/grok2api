"""重试机制单元测试"""

import asyncio

from app.core.config import config
from app.core.exceptions import UpstreamException
from app.services.grok.retry import RetryContext, retry_on_status, extract_retry_after


def _setup_retry_config():
    """设置测试用重试配置"""
    config._config["retry"] = {
        "max_retry": 3,
        "retry_status_codes": [401, 429, 500, 502, 503],
        "retry_backoff_base": 0.01,
        "retry_backoff_factor": 2.0,
        "retry_backoff_max": 0.1,
        "retry_budget": 5.0,
    }


# ==================== RetryContext ====================


def test_retry_context_should_retry_for_allowed_status():
    _setup_retry_config()
    ctx = RetryContext()
    assert ctx.should_retry(429)
    assert ctx.should_retry(500)


def test_retry_context_should_not_retry_for_disallowed_status():
    _setup_retry_config()
    ctx = RetryContext()
    assert not ctx.should_retry(404)  # 不在retry_status_codes中


def test_retry_context_respects_max_retry():
    _setup_retry_config()
    ctx = RetryContext()
    ctx.attempt = 3  # 已达最大
    assert not ctx.should_retry(429)


def test_retry_context_respects_budget():
    _setup_retry_config()
    ctx = RetryContext()
    ctx.total_delay = 5.0  # 预算耗尽
    assert not ctx.should_retry(429)


def test_retry_context_calculate_delay_uses_retry_after():
    _setup_retry_config()
    ctx = RetryContext()
    delay = ctx.calculate_delay(429, retry_after=2.0)
    assert delay == min(2.0, ctx.backoff_max)


def test_retry_context_calculate_delay_429_decorrelated_jitter():
    _setup_retry_config()
    ctx = RetryContext()
    delays = [ctx.calculate_delay(429) for _ in range(10)]
    # 所有延迟应在 [backoff_base, backoff_max] 范围内
    for d in delays:
        assert 0 <= d <= ctx.backoff_max + 0.001


def test_retry_context_calculate_delay_exponential_for_5xx():
    _setup_retry_config()
    ctx = RetryContext()
    ctx.attempt = 1
    delay = ctx.calculate_delay(500)
    assert 0 <= delay <= ctx.backoff_max


def test_retry_context_record_error():
    _setup_retry_config()
    ctx = RetryContext()
    err = Exception("test")
    ctx.record_error(500, err)
    assert ctx.attempt == 1
    assert ctx.last_status == 500
    assert ctx.last_error is err


# ==================== extract_retry_after ====================


def test_extract_retry_after_from_details():
    e = UpstreamException("err", details={"retry_after": 10})
    assert extract_retry_after(e) == 10.0


def test_extract_retry_after_from_headers():
    e = UpstreamException("err", details={"headers": {"Retry-After": "5"}})
    assert extract_retry_after(e) == 5.0


def test_extract_retry_after_none_for_non_upstream():
    assert extract_retry_after(ValueError("test")) is None


def test_extract_retry_after_none_when_missing():
    e = UpstreamException("err", details={})
    assert extract_retry_after(e) is None


# ==================== retry_on_status ====================


def test_retry_succeeds_on_first_try():
    _setup_retry_config()

    call_count = 0

    async def succeed():
        nonlocal call_count
        call_count += 1
        return "ok"

    result = asyncio.run(retry_on_status(succeed))
    assert result == "ok"
    assert call_count == 1


def test_retry_retries_on_retryable_status():
    _setup_retry_config()

    call_count = 0

    async def fail_then_succeed():
        nonlocal call_count
        call_count += 1
        if call_count < 3:
            raise UpstreamException("err", details={"status": 429})
        return "recovered"

    result = asyncio.run(retry_on_status(fail_then_succeed))
    assert result == "recovered"
    assert call_count == 3


def test_retry_raises_on_non_retryable_status():
    _setup_retry_config()

    async def fail_404():
        raise UpstreamException("not found", details={"status": 404})

    try:
        asyncio.run(retry_on_status(fail_404))
        assert False, "Should raise"
    except UpstreamException:
        pass


def test_retry_raises_after_max_retries():
    _setup_retry_config()

    async def always_fail():
        raise UpstreamException("err", details={"status": 500})

    try:
        asyncio.run(retry_on_status(always_fail))
        assert False, "Should raise"
    except UpstreamException:
        pass


def test_retry_calls_on_retry_callback():
    _setup_retry_config()

    retry_log = []

    async def fail_twice():
        if len(retry_log) < 2:
            raise UpstreamException("err", details={"status": 429})
        return "ok"

    def on_retry(attempt, status, error, delay):
        retry_log.append((attempt, status))

    asyncio.run(retry_on_status(fail_twice, on_retry=on_retry))
    assert len(retry_log) == 2
    assert retry_log[0][1] == 429
