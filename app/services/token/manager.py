"""Token 管理服务"""

import asyncio
import time
from datetime import datetime
from typing import Dict, List, Optional

from app.core.logger import logger
from app.services.token.models import (
    TokenInfo,
    EffortType,
    FAIL_THRESHOLD,
    TokenStatus,
    BASIC__DEFAULT_QUOTA,
    SUPER_DEFAULT_QUOTA,
)
from app.core.storage import get_storage
from app.core.config import get_config
from app.services.token.pool import TokenPool


DEFAULT_REFRESH_BATCH_SIZE = 10
DEFAULT_REFRESH_CONCURRENCY = 5
DEFAULT_SUPER_REFRESH_INTERVAL_HOURS = 2
DEFAULT_REFRESH_INTERVAL_HOURS = 8
DEFAULT_RELOAD_INTERVAL_SEC = 30
DEFAULT_SAVE_DELAY_MS = 500

SUPER_POOL_NAME = "ssoSuper"
BASIC_POOL_NAME = "ssoBasic"


def _default_quota_for_pool(pool_name: str) -> int:
    if pool_name == SUPER_POOL_NAME:
        return SUPER_DEFAULT_QUOTA
    return BASIC__DEFAULT_QUOTA


class TokenManager:
    """管理 Token 的增删改查和配额同步"""

    _instance: Optional["TokenManager"] = None
    _lock = asyncio.Lock()

    def __init__(self):
        self.pools: Dict[str, TokenPool] = {}
        self.initialized = False
        self._save_lock = asyncio.Lock()
        self._dirty = False
        self._save_task: Optional[asyncio.Task] = None
        self._save_delay = DEFAULT_SAVE_DELAY_MS / 1000.0
        self._last_reload_at = 0.0

    @classmethod
    async def get_instance(cls) -> "TokenManager":
        """获取单例实例"""
        if cls._instance is None:
            async with cls._lock:
                if cls._instance is None:
                    cls._instance = cls()
                    await cls._instance._load()
        return cls._instance

    async def _load(self):
        """初始化加载"""
        if not self.initialized:
            try:
                storage = get_storage()
                data = await storage.load_tokens()

                # 如果后端返回 None 或空数据，尝试从本地 data/token.json 初始化后端
                if not data:
                    from app.core.storage import LocalStorage

                    local_storage = LocalStorage()
                    local_data = await local_storage.load_tokens()
                    if local_data:
                        data = local_data
                        await storage.save_tokens(local_data)
                        logger.info(
                            f"Initialized remote token storage ({storage.__class__.__name__}) with local tokens."
                        )
                    else:
                        data = {}

                self.pools = {}
                for pool_name, tokens in data.items():
                    pool = TokenPool(pool_name)
                    for token_data in tokens:
                        quota_missing = not (
                            isinstance(token_data, dict) and "quota" in token_data
                        )
                        try:
                            # 统一存储裸 token
                            if isinstance(token_data, dict):
                                raw_token = token_data.get("token")
                                if isinstance(raw_token, str) and raw_token.startswith(
                                    "sso="
                                ):
                                    token_data["token"] = raw_token[4:]
                            token_info = TokenInfo(**token_data)
                            if quota_missing and pool_name == SUPER_POOL_NAME:
                                token_info.quota = SUPER_DEFAULT_QUOTA
                            pool.add(token_info)
                        except Exception as e:
                            logger.warning(
                                f"Failed to load token in pool '{pool_name}': {e}"
                            )
                            continue
                    pool._rebuild_index()
                    self.pools[pool_name] = pool

                self.initialized = True
                self._last_reload_at = time.monotonic()
                total = sum(p.count() for p in self.pools.values())
                logger.info(
                    f"TokenManager initialized: {len(self.pools)} pools with {total} tokens"
                )
            except Exception as e:
                logger.error(f"Failed to initialize TokenManager: {e}")
                self.pools = {}
                self.initialized = True

    async def reload(self):
        """重新加载 Token 池数据"""
        async with self.__class__._lock:
            self.initialized = False
            await self._load()

    async def reload_if_stale(self):
        """在多 worker 场景下保持短周期一致性"""
        interval = get_config("token.reload_interval_sec", DEFAULT_RELOAD_INTERVAL_SEC)
        try:
            interval = float(interval)
        except Exception:
            interval = float(DEFAULT_RELOAD_INTERVAL_SEC)
        if interval <= 0:
            return
        if time.monotonic() - self._last_reload_at < interval:
            return
        await self.reload()

    async def _save(self):
        """保存变更"""
        async with self._save_lock:
            try:
                data = {}
                for pool_name, pool in self.pools.items():
                    data[pool_name] = [info.model_dump() for info in pool.list()]

                storage = get_storage()
                async with storage.acquire_lock("tokens_save", timeout=10):
                    await storage.save_tokens(data)
            except Exception as e:
                logger.error(f"Failed to save tokens: {e}")

    def _schedule_save(self):
        """合并高频保存请求，减少写入开销"""
        delay_ms = get_config("token.save_delay_ms", DEFAULT_SAVE_DELAY_MS)
        try:
            delay_ms = float(delay_ms)
        except Exception:
            delay_ms = float(DEFAULT_SAVE_DELAY_MS)
        self._save_delay = max(0.0, delay_ms / 1000.0)
        self._dirty = True
        if self._save_delay == 0:
            if self._save_task and not self._save_task.done():
                return
            self._save_task = asyncio.create_task(self._save())
            return
        if self._save_task and not self._save_task.done():
            return
        self._save_task = asyncio.create_task(self._flush_loop())

    async def _flush_loop(self):
        try:
            while True:
                await asyncio.sleep(self._save_delay)
                if not self._dirty:
                    break
                self._dirty = False
                await self._save()
        finally:
            self._save_task = None
            if self._dirty:
                self._schedule_save()

    def get_token(self, pool_name: str = "ssoBasic", exclude: set = None) -> Optional[str]:
        """
        获取可用 Token

        Args:
            pool_name: Token 池名称
            exclude: 需要排除的 token 字符串集合

        Returns:
            Token 字符串或 None
        """
        pool = self.pools.get(pool_name)
        if not pool:
            logger.warning(f"Pool '{pool_name}' not found")
            return None

        token_info = pool.select(exclude=exclude)
        if not token_info:
            logger.warning(f"No available token in pool '{pool_name}'")
            return None

        token = token_info.token
        if token.startswith("sso="):
            return token[4:]
        return token

    def get_token_info(self, pool_name: str = "ssoBasic") -> Optional["TokenInfo"]:
        """
        获取可用 Token 的完整信息

        Args:
            pool_name: Token 池名称

        Returns:
            TokenInfo 对象或 None
        """
        pool = self.pools.get(pool_name)
        if not pool:
            logger.warning(f"Pool '{pool_name}' not found")
            return None

        token_info = pool.select()
        if not token_info:
            logger.warning(f"No available token in pool '{pool_name}'")
            return None

        return token_info

    def get_token_for_video(
        self,
        resolution: str = "480p",
        video_length: int = 6,
        pool_candidates: Optional[List[str]] = None,
    ) -> Optional["TokenInfo"]:
        """
        根据视频需求智能选择 Token 池

        路由策略:
        - 如果 resolution 是 "720p" 或 video_length > 6: 优先使用 "ssoSuper" 池
        - 否则优先使用 "ssoBasic" 池
        - 当提供 pool_candidates 时，按候选池顺序回退

        Args:
            resolution: 视频分辨率 ("480p" 或 "720p")
            video_length: 视频时长(秒)
            pool_candidates: 候选 Token 池（按优先级）

        Returns:
            TokenInfo 对象或 None（无可用 token）
        """
        # 确定首选池
        requires_super = resolution == "720p" or video_length > 6
        primary_pool = SUPER_POOL_NAME if requires_super else BASIC_POOL_NAME

        if pool_candidates:
            ordered_pools = list(pool_candidates)
            if primary_pool in ordered_pools:
                ordered_pools.remove(primary_pool)
                ordered_pools.insert(0, primary_pool)
        else:
            fallback_pool = BASIC_POOL_NAME if requires_super else SUPER_POOL_NAME
            ordered_pools = [primary_pool, fallback_pool]

        for idx, pool_name in enumerate(ordered_pools):
            token_info = self.get_token_info(pool_name)
            if token_info:
                if idx == 0:
                    logger.info(
                        f"Video token routing: resolution={resolution}, length={video_length}s -> "
                        f"pool={pool_name} (token={token_info.token[:10]}...)"
                    )
                else:
                    logger.info(
                        f"Video token routing: fallback from {ordered_pools[0]} -> {pool_name} "
                        f"(token={token_info.token[:10]}...)"
                    )
                return token_info

            if idx == 0 and requires_super and pool_name == primary_pool:
                next_pool = ordered_pools[1] if len(ordered_pools) > 1 else None
                if next_pool:
                    logger.warning(
                        f"Video token routing: {primary_pool} pool has no available token for "
                        f"resolution={resolution}, length={video_length}s. "
                        f"Falling back to {next_pool} pool."
                    )

        # 两个池都没有可用 token
        logger.warning(
            f"Video token routing: no available token in any pool "
            f"(resolution={resolution}, length={video_length}s)"
        )
        return None

    async def consume(
        self, token_str: str, effort: EffortType = EffortType.LOW
    ) -> bool:
        """
        消耗配额（本地预估）

        Args:
            token_str: Token 字符串
            effort: 消耗力度

        Returns:
            是否成功
        """
        raw_token = token_str.replace("sso=", "")

        for pool in self.pools.values():
            token = pool.get(raw_token)
            if token:
                consumed = token.consume(effort)
                logger.debug(
                    f"Token {raw_token[:10]}...: consumed {consumed} quota, use_count={token.use_count}"
                )
                self._schedule_save()
                return True

        logger.warning(f"Token {raw_token[:10]}...: not found for consumption")
        return False

    async def sync_usage(
        self,
        token_str: str,
        model_name: str,
        fallback_effort: EffortType = EffortType.LOW,
        consume_on_fail: bool = True,
        is_usage: bool = True,
    ) -> bool:
        """
        同步 Token 用量

        优先从 API 获取最新配额，失败则降级到本地预估

        Args:
            token_str: Token 字符串（可带 sso= 前缀）
            model_name: 模型名称（用于 API 查询）
            fallback_effort: 降级时的消耗力度
            consume_on_fail: 失败时是否降级扣费
            is_usage: 是否记录为一次使用（影响 use_count）

        Returns:
            是否成功
        """
        raw_token = token_str.replace("sso=", "")

        # 查找 Token 对象
        target_token: Optional[TokenInfo] = None
        for pool in self.pools.values():
            target_token = pool.get(raw_token)
            if target_token:
                break

        if not target_token:
            logger.warning(f"Token {raw_token[:10]}...: not found for sync")
            return False

        # 尝试 API 同步
        try:
            from app.services.grok.services.usage import UsageService

            usage_service = UsageService()
            result = await usage_service.get(token_str, model_name=model_name)

            if result and "remainingTokens" in result:
                old_quota = target_token.quota
                new_quota = result["remainingTokens"]

                target_token.update_quota(new_quota)
                target_token.record_success(is_usage=is_usage)

                consumed = max(0, old_quota - new_quota)
                logger.info(
                    f"Token {raw_token[:10]}...: synced quota "
                    f"{old_quota} -> {new_quota} (consumed: {consumed}, use_count: {target_token.use_count})"
                )

                self._schedule_save()
                return True

        except Exception as e:
            logger.warning(
                f"Token {raw_token[:10]}...: API sync failed, fallback to local ({e})"
            )

        # 降级：本地预估扣费
        if consume_on_fail:
            logger.debug(f"Token {raw_token[:10]}...: using local consumption")
            return await self.consume(token_str, fallback_effort)
        else:
            logger.debug(
                f"Token {raw_token[:10]}...: sync failed, skipping local consumption"
            )
            return False

    async def record_fail(
        self, token_str: str, status_code: int = 401, reason: str = ""
    ) -> bool:
        """
        记录 Token 失败

        Args:
            token_str: Token 字符串
            status_code: HTTP 状态码
            reason: 失败原因

        Returns:
            是否成功
        """
        raw_token = token_str.replace("sso=", "")

        for pool in self.pools.values():
            token = pool.get(raw_token)
            if token:
                if status_code in (401, 403):
                    token.record_fail(status_code, reason)
                    logger.warning(
                        f"Token {raw_token[:10]}...: recorded {status_code} failure "
                        f"({token.fail_count}/{FAIL_THRESHOLD}) - {reason}"
                    )
                else:
                    logger.info(
                        f"Token {raw_token[:10]}...: non-auth error ({status_code}) - {reason} (not counted)"
                    )
                self._schedule_save()
                return True

        logger.warning(f"Token {raw_token[:10]}...: not found for failure record")
        return False

    async def mark_rate_limited(self, token_str: str) -> bool:
        """
        将 Token 标记为配额耗尽（COOLING）

        当 Grok API 返回 429 时调用，将 quota 设为 0 并标记 COOLING，
        使该 Token 不再被选中，等待下次 Scheduler 刷新恢复。

        Args:
            token_str: Token 字符串

        Returns:
            是否成功
        """
        raw_token = token_str.removeprefix("sso=")

        for pool in self.pools.values():
            token = pool.get(raw_token)
            if token:
                old_quota = token.quota
                token.quota = 0
                token.status = TokenStatus.COOLING
                logger.warning(
                    f"Token {raw_token[:10]}...: marked as rate limited "
                    f"(quota {old_quota} -> 0, status -> cooling)"
                )
                self._schedule_save()
                return True

        logger.warning(f"Token {raw_token[:10]}...: not found for rate limit marking")
        return False

    # ========== 管理功能 ==========

    async def add(self, token: str, pool_name: str = "ssoBasic") -> bool:
        """
        添加 Token

        Args:
            token: Token 字符串（不含 sso= 前缀）
            pool_name: 池名称

        Returns:
            是否成功
        """
        if pool_name not in self.pools:
            self.pools[pool_name] = TokenPool(pool_name)
            logger.info(f"Pool '{pool_name}': created")

        pool = self.pools[pool_name]

        token = token[4:] if token.startswith("sso=") else token
        if pool.get(token):
            logger.warning(f"Pool '{pool_name}': token already exists")
            return False

        pool.add(TokenInfo(token=token, quota=_default_quota_for_pool(pool_name)))
        await self._save()
        logger.info(f"Pool '{pool_name}': token added")
        return True

    async def mark_asset_clear(self, token: str) -> bool:
        """记录在线资产清理时间"""
        raw_token = token[4:] if token.startswith("sso=") else token
        for pool in self.pools.values():
            info = pool.get(raw_token)
            if info:
                info.last_asset_clear_at = int(datetime.now().timestamp() * 1000)
                self._schedule_save()
                return True
        return False

    async def add_tag(self, token: str, tag: str) -> bool:
        """
        给 Token 添加标签

        Args:
            token: Token 字符串
            tag: 标签名称

        Returns:
            是否成功
        """
        raw_token = token[4:] if token.startswith("sso=") else token
        for pool in self.pools.values():
            info = pool.get(raw_token)
            if info:
                if tag not in info.tags:
                    info.tags.append(tag)
                    self._schedule_save()
                    logger.debug(f"Token {raw_token[:10]}...: added tag '{tag}'")
                return True
        return False

    async def remove_tag(self, token: str, tag: str) -> bool:
        """
        移除 Token 标签

        Args:
            token: Token 字符串
            tag: 标签名称

        Returns:
            是否成功
        """
        raw_token = token[4:] if token.startswith("sso=") else token
        for pool in self.pools.values():
            info = pool.get(raw_token)
            if info:
                if tag in info.tags:
                    info.tags.remove(tag)
                    self._schedule_save()
                    logger.debug(f"Token {raw_token[:10]}...: removed tag '{tag}'")
                return True
        return False

    async def remove(self, token: str) -> bool:
        """
        删除 Token

        Args:
            token: Token 字符串

        Returns:
            是否成功
        """
        for pool_name, pool in self.pools.items():
            if pool.remove(token):
                await self._save()
                logger.info(f"Pool '{pool_name}': token removed")
                return True

        logger.warning("Token not found for removal")
        return False

    async def reset_all(self):
        """重置所有 Token 配额"""
        count = 0
        for pool_name, pool in self.pools.items():
            default_quota = _default_quota_for_pool(pool_name)
            for token in pool:
                token.reset(default_quota)
                count += 1

        await self._save()
        logger.info(f"Reset all: {count} tokens updated")

    async def reset_token(self, token_str: str) -> bool:
        """
        重置单个 Token

        Args:
            token_str: Token 字符串

        Returns:
            是否成功
        """
        raw_token = token_str.replace("sso=", "")

        for pool in self.pools.values():
            token = pool.get(raw_token)
            if token:
                default_quota = _default_quota_for_pool(pool.name)
                token.reset(default_quota)
                await self._save()
                logger.info(f"Token {raw_token[:10]}...: reset completed")
                return True

        logger.warning(f"Token {raw_token[:10]}...: not found for reset")
        return False

    def get_stats(self) -> Dict[str, dict]:
        """获取统计信息"""
        stats = {}
        for name, pool in self.pools.items():
            pool_stats = pool.get_stats()
            stats[name] = pool_stats.model_dump()
        return stats

    def get_pool_tokens(self, pool_name: str = "ssoBasic") -> List[TokenInfo]:
        """
        获取指定池的所有 Token

        Args:
            pool_name: 池名称

        Returns:
            Token 列表
        """
        pool = self.pools.get(pool_name)
        if not pool:
            return []
        return pool.list()

    async def refresh_cooling_tokens(self) -> Dict[str, int]:
        """
        批量刷新 cooling 状态的 Token 配额

        Returns:
            {"checked": int, "refreshed": int, "recovered": int, "expired": int}
        """
        from app.services.grok.services.usage import UsageService

        # 收集需要刷新的 token
        to_refresh: List[TokenInfo] = []
        for pool in self.pools.values():
            if pool.name == SUPER_POOL_NAME:
                interval_hours = get_config(
                    "token.super_refresh_interval_hours",
                    DEFAULT_SUPER_REFRESH_INTERVAL_HOURS,
                )
            else:
                interval_hours = get_config(
                    "token.refresh_interval_hours",
                    DEFAULT_REFRESH_INTERVAL_HOURS,
                )
            for token in pool:
                if token.need_refresh(interval_hours):
                    to_refresh.append(token)

        if not to_refresh:
            logger.debug("Refresh check: no tokens need refresh")
            return {"checked": 0, "refreshed": 0, "recovered": 0, "expired": 0}

        logger.info(f"Refresh check: found {len(to_refresh)} cooling tokens to refresh")

        # 批量并发刷新
        semaphore = asyncio.Semaphore(DEFAULT_REFRESH_CONCURRENCY)
        usage_service = UsageService()
        refreshed = 0
        recovered = 0
        expired = 0

        async def _refresh_one(token_info: TokenInfo) -> dict:
            """刷新单个 token"""
            async with semaphore:
                token_str = token_info.token
                if token_str.startswith("sso="):
                    token_str = token_str[4:]

                # 重试逻辑：最多 2 次重试
                for retry in range(3):  # 0, 1, 2
                    try:
                        result = await usage_service.get(token_str, model_name="grok-3")

                        if result and "remainingTokens" in result:
                            new_quota = result["remainingTokens"]
                            old_quota = token_info.quota
                            old_status = token_info.status

                            token_info.update_quota(new_quota)
                            token_info.mark_synced()

                            logger.info(
                                f"Token {token_info.token[:10]}...: refreshed "
                                f"{old_quota} -> {new_quota}, status: {old_status} -> {token_info.status}"
                            )

                            return {
                                "recovered": new_quota > 0 and old_quota == 0,
                                "expired": False,
                            }

                        return {"recovered": False, "expired": False}

                    except Exception as e:
                        error_str = str(e)

                        # 检查是否为 401 错误
                        if "401" in error_str or "Unauthorized" in error_str:
                            if retry < 2:
                                logger.warning(
                                    f"Token {token_info.token[:10]}...: 401 error, "
                                    f"retry {retry + 1}/2..."
                                )
                                await asyncio.sleep(0.5)
                                continue
                            else:
                                # 重试 2 次后仍然 401，标记为 expired
                                logger.error(
                                    f"Token {token_info.token[:10]}...: 401 after 2 retries, "
                                    f"marking as expired"
                                )
                                token_info.status = TokenStatus.EXPIRED
                                return {"recovered": False, "expired": True}
                        else:
                            logger.warning(
                                f"Token {token_info.token[:10]}...: refresh failed ({e})"
                            )
                            return {"recovered": False, "expired": False}

                return {"recovered": False, "expired": False}

        # 批量处理
        for i in range(0, len(to_refresh), DEFAULT_REFRESH_BATCH_SIZE):
            batch = to_refresh[i : i + DEFAULT_REFRESH_BATCH_SIZE]
            results = await asyncio.gather(*[_refresh_one(t) for t in batch])
            refreshed += len(batch)
            recovered += sum(r["recovered"] for r in results)
            expired += sum(r["expired"] for r in results)

            # 批次间延迟
            if i + DEFAULT_REFRESH_BATCH_SIZE < len(to_refresh):
                await asyncio.sleep(1)

        await self._save()

        logger.info(
            f"Refresh completed: "
            f"checked={len(to_refresh)}, refreshed={refreshed}, "
            f"recovered={recovered}, expired={expired}"
        )

        return {
            "checked": len(to_refresh),
            "refreshed": refreshed,
            "recovered": recovered,
            "expired": expired,
        }


# 便捷函数
async def get_token_manager() -> TokenManager:
    """获取 TokenManager 单例"""
    return await TokenManager.get_instance()


__all__ = ["TokenManager", "get_token_manager"]
