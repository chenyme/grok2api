"""Streaming keepalive 单元测试"""

import asyncio
from typing import AsyncGenerator

from app.core.streaming import with_keepalive


async def _items_gen(*items, delay: float = 0) -> AsyncGenerator:
    """辅助: 逐个 yield items，可选延迟"""
    for item in items:
        if delay > 0:
            await asyncio.sleep(delay)
        yield item


async def _collect(gen) -> list:
    """收集异步生成器的所有结果"""
    return [item async for item in gen]


# ==================== 基础功能 ====================


def test_keepalive_passthrough_without_delay():
    """无延迟时直接透传所有元素"""

    async def _run():
        gen = _items_gen("a", "b", "c")
        result = await _collect(with_keepalive(gen, interval=1.0, ping_message="PING"))
        assert result == ["a", "b", "c"]

    asyncio.run(_run())


def test_keepalive_disabled_when_interval_zero():
    """interval <= 0 时禁用 keepalive，直接透传"""

    async def _run():
        gen = _items_gen("x", "y")
        result = await _collect(with_keepalive(gen, interval=0, ping_message="PING"))
        assert result == ["x", "y"]

    asyncio.run(_run())


def test_keepalive_disabled_when_interval_negative():

    async def _run():
        gen = _items_gen("a")
        result = await _collect(with_keepalive(gen, interval=-1, ping_message="PING"))
        assert result == ["a"]

    asyncio.run(_run())


# ==================== Ping 行为 ====================


def test_keepalive_inserts_ping_on_idle():
    """源生成器空闲时插入 ping

    wait_for 超时取消 __anext__ 协程 → CancelledError 终止内部 sleep →
    生成器被销毁 → StopAsyncIteration → 循环结束。
    因此预期结果为 ["first", "PING"]，不含后续元素。
    """

    async def _run():
        async def slow_gen():
            yield "first"
            await asyncio.sleep(10)  # 远超 interval，必然超时
            yield "second"  # 不会到达

        result = await _collect(with_keepalive(slow_gen(), interval=0.05, ping_message="PING"))
        assert result[0] == "first"
        assert "PING" in result
        assert result.index("first") < result.index("PING")

    asyncio.run(_run())


def test_keepalive_no_ping_when_fast():
    """元素产出快于 interval 时不插入 ping"""

    async def _run():
        gen = _items_gen("a", "b", "c", delay=0.01)
        result = await _collect(with_keepalive(gen, interval=1.0, ping_message="PING"))
        assert result == ["a", "b", "c"]

    asyncio.run(_run())


# ==================== 空生成器 ====================


def test_keepalive_empty_generator():

    async def _run():
        async def empty():
            return
            yield  # noqa: unreachable — 使其成为 AsyncGenerator

        result = await _collect(with_keepalive(empty(), interval=0.05, ping_message="PING"))
        assert result == []

    asyncio.run(_run())


# ==================== 自定义 ping 消息 ====================


def test_keepalive_custom_ping_message():
    """ping_message 可以是任意类型"""

    async def _run():
        async def slow():
            yield 1
            await asyncio.sleep(10)
            yield 2

        ping = {"type": "keepalive", "data": ""}
        result = await _collect(with_keepalive(slow(), interval=0.05, ping_message=ping))
        assert 1 in result
        assert ping in result

    asyncio.run(_run())
