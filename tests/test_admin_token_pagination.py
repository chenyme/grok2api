import unittest
from dataclasses import dataclass, field

from app.api.v1.admin.token import (
    MAX_TOKEN_PAGE_SIZE,
    _build_paginated_token_payload,
    _collect_token_refs,
    _normalize_token_filter,
    _token_matches_filter,
)
from app.services.token.manager import TokenManager
from app.services.token.models import TokenInfo
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


class FakePool:
    def __init__(self, items):
        self._items = list(items)

    def list(self):
        return list(self._items)


class FakeManager:
    def __init__(self, pools):
        self.pools = pools


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


if __name__ == "__main__":
    unittest.main()
