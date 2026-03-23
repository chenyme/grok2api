import unittest
from contextlib import asynccontextmanager
from copy import deepcopy
from dataclasses import dataclass, field
from unittest.mock import AsyncMock, patch

from app.api.v1.admin import token as admin_token_module
from app.api.v1.admin.token import (
    MAX_TOKEN_PAGE_SIZE,
    _build_paginated_token_payload,
    _collect_token_refs,
    _normalize_token_filter,
    _token_matches_filter,
    update_tokens,
)
from app.services.token.manager import TokenManager, get_token_manager
from app.services.token.models import TokenInfo, TokenStatus
from app.services.token.pool import TokenPool


@dataclass
class FakeTokenInfo:
    token: str
    status: str = "active"
    quota: int = 0
    consumed: int = 0
    note: str = ""
    fail_count: int = 0
    use_count: int = 0
    tags: list[str] = field(default_factory=list)
    created_at: int = 0
    last_used_at: int | None = None


class FakePool:
    def __init__(self, items):
        self._items = list(items)

    def list(self):
        return list(self._items)


class FakeManager:
    def __init__(self, pools):
        self.pools = pools


class FakeVersionedStorage:
    def __init__(self, data, version: str):
        self.data = deepcopy(data)
        self.version = version

    async def load_tokens(self):
        return deepcopy(self.data)

    async def load_tokens_version(self):
        return self.version


class FakeAdminTokenStorage:
    def __init__(self, existing=None):
        self.existing = deepcopy(existing or {})
        self.saved_tokens = []

    @asynccontextmanager
    async def acquire_lock(self, name: str, timeout: int = 10):
        yield

    async def load_tokens(self):
        return deepcopy(self.existing)

    async def save_tokens(self, data):
        snapshot = deepcopy(data)
        self.saved_tokens.append(snapshot)
        self.existing = snapshot


class AdminTokenPaginationTests(unittest.TestCase):
    def setUp(self):
        self.manager = FakeManager(
            {
                "ssoBasic": FakePool(
                    [
                        FakeTokenInfo("token-a", status="active", quota=80, use_count=1),
                        FakeTokenInfo("token-b", status="cooling", quota=40, consumed=4),
                        FakeTokenInfo("token-c", status="disabled", tags=["nsfw"], use_count=2),
                    ]
                ),
                "ssoSuper": FakePool(
                    [
                        FakeTokenInfo("token-d", status="active", quota=140, tags=["nsfw"]),
                        FakeTokenInfo("token-e", status="active", quota=140),
                    ]
                ),
            }
        )

    def test_normalize_token_filter_falls_back_to_all(self):
        self.assertEqual(_normalize_token_filter(None), "all")
        self.assertEqual(_normalize_token_filter("ACTIVE"), "active")
        self.assertEqual(_normalize_token_filter("weird"), "all")

    def test_page_size_limit_keeps_existing_ui_options_valid(self):
        self.assertEqual(MAX_TOKEN_PAGE_SIZE, 2000)

    def test_token_matches_filter_variants(self):
        active = FakeTokenInfo("token-a", status="active")
        cooling = FakeTokenInfo("token-b", status="cooling")
        disabled_nsfw = FakeTokenInfo("token-c", status="disabled", tags=["nsfw"])

        self.assertTrue(_token_matches_filter(active, "all"))
        self.assertTrue(_token_matches_filter(active, "active"))
        self.assertTrue(_token_matches_filter(cooling, "cooling"))
        self.assertTrue(_token_matches_filter(disabled_nsfw, "expired"))
        self.assertTrue(_token_matches_filter(disabled_nsfw, "nsfw"))
        self.assertTrue(_token_matches_filter(active, "no-nsfw"))
        self.assertFalse(_token_matches_filter(cooling, "active"))

    def test_build_paginated_payload_returns_summary_counts_and_page_slice(self):
        payload = _build_paginated_token_payload(
            self.manager,
            status_filter="all",
            page=2,
            page_size=2,
            consumed_mode_enabled=False,
        )

        self.assertEqual(payload["page"], 2)
        self.assertEqual(payload["page_size"], 2)
        self.assertEqual(payload["total"], 5)
        self.assertEqual(payload["total_pages"], 3)
        self.assertEqual([item["token"] for item in payload["items"]], ["token-c", "token-d"])
        self.assertEqual(payload["counts"]["all"], 5)
        self.assertEqual(payload["counts"]["active"], 3)
        self.assertEqual(payload["counts"]["cooling"], 1)
        self.assertEqual(payload["counts"]["expired"], 1)
        self.assertEqual(payload["counts"]["nsfw"], 2)
        self.assertEqual(payload["counts"]["no-nsfw"], 3)
        self.assertEqual(payload["summary"]["chat_quota"], 360)
        self.assertEqual(payload["summary"]["image_quota"], 180)
        self.assertEqual(payload["summary"]["total_consumed"], 4)
        self.assertEqual(payload["summary"]["total_calls"], 3)

    def test_build_paginated_payload_normalizes_enum_statuses_and_orders_basic_first(self):
        manager = FakeManager(
            {
                "ssoSuper": FakePool(
                    [
                        TokenInfo(
                            token="super-token",
                            status=TokenStatus.ACTIVE,
                            quota=140,
                            created_at=10,
                        )
                    ]
                ),
                "ssoBasic": FakePool(
                    [
                        TokenInfo(
                            token="basic-new",
                            status=TokenStatus.ACTIVE,
                            quota=80,
                            created_at=30,
                        ),
                        TokenInfo(
                            token="basic-cooling",
                            status=TokenStatus.COOLING,
                            quota=0,
                            created_at=20,
                        ),
                    ]
                ),
            }
        )

        payload = _build_paginated_token_payload(
            manager,
            status_filter="all",
            page=1,
            page_size=10,
            consumed_mode_enabled=False,
        )

        self.assertEqual(
            [item["token"] for item in payload["items"]],
            ["basic-new", "basic-cooling", "super-token"],
        )
        self.assertEqual(
            [item["status"] for item in payload["items"]],
            ["active", "cooling", "active"],
        )
        self.assertEqual(payload["counts"]["active"], 2)
        self.assertEqual(payload["counts"]["cooling"], 1)
        self.assertEqual(payload["counts"]["expired"], 0)
        self.assertEqual(payload["summary"]["invalid"], 0)

    def test_keys_only_payload_honors_filter(self):
        payload = _build_paginated_token_payload(
            self.manager,
            status_filter="nsfw",
            page=1,
            page_size=50,
            consumed_mode_enabled=False,
            keys_only=True,
        )

        self.assertEqual(payload["total"], 2)
        self.assertEqual([item["token"] for item in payload["items"]], ["token-c", "token-d"])

    def test_collect_token_refs_keeps_same_token_in_different_pools(self):
        refs = _collect_token_refs(
            {
                "token": "dup-token",
                "pool": "ssoBasic",
                "tokens": [
                    {"token": "dup-token", "pool": "ssoBasic"},
                    {"token": "dup-token", "pool": "ssoSuper"},
                    {"token": "dup-token", "pool": "ssoSuper"},
                ],
            }
        )

        self.assertEqual(
            refs,
            [
                ("ssoBasic", "dup-token"),
                ("ssoSuper", "dup-token"),
            ],
        )


class TokenManagerPoolScopedTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.manager = TokenManager()
        basic_pool = TokenPool("ssoBasic")
        basic_pool.add(TokenInfo(token="dup-token", quota=80))
        super_pool = TokenPool("ssoSuper")
        super_pool.add(TokenInfo(token="dup-token", quota=140))
        self.manager.pools = {
            "ssoBasic": basic_pool,
            "ssoSuper": super_pool,
        }
        self.manager._schedule_save = lambda: None

        async def _noop_save(*args, **kwargs):
            return None

        self.manager._save = _noop_save

    async def test_consume_with_pool_name_updates_requested_pool_only(self):
        basic_token = self.manager.pools["ssoBasic"].get("dup-token")
        super_token = self.manager.pools["ssoSuper"].get("dup-token")

        await self.manager.consume("dup-token", pool_name="ssoSuper")

        self.assertEqual(basic_token.quota, 80)
        self.assertEqual(super_token.quota, 139)

    async def test_remove_with_pool_name_keeps_other_pool_copy(self):
        removed = await self.manager.remove("dup-token", pool_name="ssoSuper")

        self.assertTrue(removed)
        self.assertIsNotNone(self.manager.pools["ssoBasic"].get("dup-token"))
        self.assertIsNone(self.manager.pools["ssoSuper"].get("dup-token"))


class TokenManagerFreshnessTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        TokenManager._instance = None

    def tearDown(self):
        TokenManager._instance = None

    async def test_get_token_manager_reloads_immediately_when_storage_version_changes(self):
        storage = FakeVersionedStorage(
            {"ssoBasic": [{"token": "token-a", "quota": 80}]},
            version="v1",
        )

        with patch("app.services.token.manager.get_storage", return_value=storage):
            manager = await get_token_manager()
            self.assertEqual(manager.pools["ssoBasic"].count(), 1)

            storage.data = {
                "ssoBasic": [
                    {"token": "token-a", "quota": 80},
                    {"token": "token-b", "quota": 80},
                ]
            }
            storage.version = "v2"

            same_manager = await get_token_manager()

        self.assertIs(same_manager, manager)
        self.assertEqual(same_manager.pools["ssoBasic"].count(), 2)


class AdminTokenUpdateRouteTests(unittest.IsolatedAsyncioTestCase):
    async def test_update_tokens_dedupes_duplicate_entries_in_same_pool_payload(self):
        storage = FakeAdminTokenStorage()
        mgr = type("ReloadableManager", (), {})()
        mgr.reload = AsyncMock()

        with (
            patch.object(admin_token_module, "get_storage", return_value=storage),
            patch.object(
                admin_token_module,
                "get_token_manager",
                AsyncMock(return_value=mgr),
            ),
        ):
            payload = await update_tokens(
                {
                    "ssoBasic": [
                        {"token": "dup-token", "status": "active", "quota": 80, "note": "first"},
                        {"token": "dup-token", "status": "active", "quota": 80, "note": "second"},
                    ]
                }
            )

        self.assertEqual(payload["status"], "success")
        saved = storage.saved_tokens[-1]["ssoBasic"]
        self.assertEqual(len(saved), 1)
        self.assertEqual(saved[0]["token"], "dup-token")
        self.assertEqual(saved[0]["note"], "second")
        mgr.reload.assert_awaited_once()


if __name__ == "__main__":
    unittest.main()
