import unittest
from contextlib import asynccontextmanager
from copy import deepcopy
from unittest.mock import patch

from app.api.v1.admin import config as admin_config_module
from app.core.config import Config


class FakeStorage:
    def __init__(self):
        self.saved_configs = []

    @asynccontextmanager
    async def acquire_lock(self, name: str, timeout: int = 10):
        yield

    async def save_config(self, data):
        self.saved_configs.append(deepcopy(data))


def build_test_config() -> Config:
    cfg = Config()
    cfg._defaults_loaded = True
    cfg._defaults = {
        "app": {
            "app_key": "grok2api",
            "app_url": "",
        },
        "proxy": {
            "enabled": True,
            "flaresolverr_url": "http://solver.local",
            "refresh_interval": 600,
            "timeout": 60,
            "cf_cookies": "",
            "cf_clearance": "OLD-CLEAR",
            "browser": "old-browser",
            "user_agent": "OLD-UA",
        },
    }
    cfg._config = deepcopy(cfg._defaults)
    cfg._loaded = True
    return cfg


class AdminConfigRouteTests(unittest.IsolatedAsyncioTestCase):
    async def test_update_config_returns_canonical_sanitized_config(self):
        cfg = build_test_config()
        storage = FakeStorage()

        with (
            patch.object(admin_config_module, "config", cfg),
            patch("app.core.storage.get_storage", return_value=storage),
        ):
            payload = await admin_config_module.update_config(
                {
                    "proxy": {
                        "user_agent": "  TEST-UA  ",
                        "cf_clearance": " AB C ",
                    }
                }
            )

        self.assertEqual(payload["status"], "success")
        self.assertEqual(payload["config"]["proxy"]["user_agent"], "TEST-UA")
        self.assertEqual(payload["config"]["proxy"]["cf_clearance"], "ABC")
        self.assertEqual(storage.saved_configs[-1]["proxy"]["user_agent"], "TEST-UA")
        self.assertEqual(storage.saved_configs[-1]["proxy"]["cf_clearance"], "ABC")

    async def test_partial_update_preserves_unsubmitted_server_managed_fields(self):
        cfg = build_test_config()
        storage = FakeStorage()

        with (
            patch.object(admin_config_module, "config", cfg),
            patch("app.core.storage.get_storage", return_value=storage),
        ):
            payload = await admin_config_module.update_config(
                {
                    "app": {
                        "app_url": "https://example.com",
                    }
                }
            )

        self.assertEqual(payload["config"]["app"]["app_url"], "https://example.com")
        self.assertEqual(payload["config"]["proxy"]["cf_clearance"], "OLD-CLEAR")
        self.assertEqual(payload["config"]["proxy"]["browser"], "old-browser")
        self.assertEqual(payload["config"]["proxy"]["user_agent"], "OLD-UA")

    async def test_get_config_returns_snapshot_not_internal_reference(self):
        cfg = build_test_config()

        with patch.object(admin_config_module, "config", cfg):
            payload = await admin_config_module.get_config()

        payload["proxy"]["browser"] = "mutated"
        self.assertEqual(cfg._config["proxy"]["browser"], "old-browser")


if __name__ == "__main__":
    unittest.main()
