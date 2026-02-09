"""Token 模型单元测试 — 状态机、消耗、失败、刷新"""

from app.services.token.models import (
    TokenInfo,
    TokenStatus,
    EffortType,
    EFFORT_COST,
    BASIC_DEFAULT_QUOTA,
    SUPER_DEFAULT_QUOTA,
    FAIL_THRESHOLD,
)

# ==================== 基础状态 ====================


def test_token_defaults():
    t = TokenInfo(token="sso_abc")
    assert t.status == TokenStatus.ACTIVE
    assert t.quota == BASIC_DEFAULT_QUOTA
    assert t.fail_count == 0
    assert t.is_available()


def test_is_available_respects_status_and_quota():
    t = TokenInfo(token="t1", status=TokenStatus.COOLING, quota=10)
    assert not t.is_available()

    t2 = TokenInfo(token="t2", status=TokenStatus.ACTIVE, quota=0)
    assert not t2.is_available()

    t3 = TokenInfo(token="t3", status=TokenStatus.ACTIVE, quota=1)
    assert t3.is_available()


# ==================== consume ====================


def test_consume_low_effort():
    t = TokenInfo(token="t", quota=10)
    cost = t.consume(EffortType.LOW)
    assert cost == EFFORT_COST[EffortType.LOW]
    assert t.quota == 10 - cost
    assert t.use_count == cost
    assert t.last_used_at is not None


def test_consume_high_effort():
    t = TokenInfo(token="t", quota=10)
    cost = t.consume(EffortType.HIGH)
    assert cost == EFFORT_COST[EffortType.HIGH]
    assert t.quota == 10 - cost


def test_consume_transitions_to_cooling_on_zero_quota():
    t = TokenInfo(token="t", quota=1)
    t.consume(EffortType.LOW)
    assert t.quota == 0
    assert t.status == TokenStatus.COOLING


def test_consume_caps_at_remaining_quota():
    t = TokenInfo(token="t", quota=2)
    cost = t.consume(EffortType.HIGH)  # HIGH = 4, 但只剩2
    assert cost == 2
    assert t.quota == 0
    assert t.status == TokenStatus.COOLING


def test_consume_does_not_clear_fail_count():
    t = TokenInfo(token="t", quota=10, fail_count=3)
    t.consume(EffortType.LOW)
    assert t.fail_count == 3  # consume 不清零 fail_count


# ==================== record_fail ====================


def test_record_fail_increments_on_401():
    t = TokenInfo(token="t")
    t.record_fail(401, "Unauthorized")
    assert t.fail_count == 1
    assert t.last_fail_reason == "Unauthorized"
    assert t.status == TokenStatus.ACTIVE  # 未达阈值


def test_record_fail_increments_on_403():
    t = TokenInfo(token="t")
    t.record_fail(403, "Forbidden")
    assert t.fail_count == 1


def test_record_fail_ignores_non_auth_status():
    t = TokenInfo(token="t")
    t.record_fail(500, "Server Error")
    assert t.fail_count == 0  # 500 不计入


def test_record_fail_expires_at_threshold():
    t = TokenInfo(token="t")
    for i in range(FAIL_THRESHOLD):
        t.record_fail(401, f"fail-{i}")
    assert t.status == TokenStatus.EXPIRED
    assert t.fail_count == FAIL_THRESHOLD


# ==================== record_success ====================


def test_record_success_clears_fail_state():
    t = TokenInfo(token="t", fail_count=3, last_fail_reason="err")
    t.record_success()
    assert t.fail_count == 0
    assert t.last_fail_at is None
    assert t.last_fail_reason is None


def test_record_success_restores_active_from_expired():
    """record_success 通过 update_quota 间接恢复"""
    t = TokenInfo(token="t", status=TokenStatus.EXPIRED, quota=10)
    t.record_success()
    assert t.status == TokenStatus.ACTIVE


def test_record_success_keeps_cooling_if_zero_quota():
    t = TokenInfo(token="t", status=TokenStatus.COOLING, quota=0)
    t.record_success()
    assert t.status == TokenStatus.COOLING


# ==================== update_quota ====================


def test_update_quota_restores_from_cooling():
    t = TokenInfo(token="t", status=TokenStatus.COOLING, quota=0)
    t.update_quota(50)
    assert t.quota == 50
    assert t.status == TokenStatus.ACTIVE


def test_update_quota_restores_from_expired():
    t = TokenInfo(token="t", status=TokenStatus.EXPIRED, quota=0)
    t.update_quota(10)
    assert t.status == TokenStatus.ACTIVE


def test_update_quota_to_zero_sets_cooling():
    t = TokenInfo(token="t", quota=50)
    t.update_quota(0)
    assert t.status == TokenStatus.COOLING


def test_update_quota_negative_clamps_to_zero():
    t = TokenInfo(token="t", quota=50)
    t.update_quota(-10)
    assert t.quota == 0


# ==================== reset ====================


def test_reset_restores_defaults():
    t = TokenInfo(token="t", quota=0, status=TokenStatus.EXPIRED, fail_count=5)
    t.reset()
    assert t.quota == BASIC_DEFAULT_QUOTA
    assert t.status == TokenStatus.ACTIVE
    assert t.fail_count == 0


def test_reset_with_custom_quota():
    t = TokenInfo(token="t", quota=0, status=TokenStatus.COOLING)
    t.reset(default_quota=SUPER_DEFAULT_QUOTA)
    assert t.quota == SUPER_DEFAULT_QUOTA


# ==================== need_refresh ====================


def test_need_refresh_only_when_cooling():
    t = TokenInfo(token="t", status=TokenStatus.ACTIVE, quota=10)
    assert not t.need_refresh(8)


def test_need_refresh_true_when_never_synced():
    t = TokenInfo(token="t", status=TokenStatus.COOLING, quota=0, last_sync_at=None)
    assert t.need_refresh(8)


def test_need_refresh_false_when_recently_synced():
    import time

    t = TokenInfo(token="t", status=TokenStatus.COOLING, quota=0)
    t.last_sync_at = int(time.time() * 1000)  # 刚同步
    assert not t.need_refresh(8)
