import unittest
from unittest.mock import AsyncMock, patch

from fastapi import HTTPException

from app.api.v1.admin import cache as admin_cache_module


class FakeTokenManager:
    def __init__(self, token: str | None = None):
        self._token = token

    def get_token(self):
        return self._token


class AdminCacheRouteTests(unittest.IsolatedAsyncioTestCase):
    async def test_clear_online_empty_tokens_returns_400(self):
        mgr = FakeTokenManager()

        with patch.object(admin_cache_module, "get_token_manager", AsyncMock(return_value=mgr)):
            with self.assertRaises(HTTPException) as ctx:
                await admin_cache_module.clear_online({"tokens": []})

        self.assertEqual(ctx.exception.status_code, 400)
        self.assertEqual(ctx.exception.detail, "No tokens provided")

    async def test_clear_online_without_available_token_returns_400(self):
        mgr = FakeTokenManager(token=None)

        with patch.object(admin_cache_module, "get_token_manager", AsyncMock(return_value=mgr)):
            with self.assertRaises(HTTPException) as ctx:
                await admin_cache_module.clear_online({})

        self.assertEqual(ctx.exception.status_code, 400)
        self.assertEqual(ctx.exception.detail, "No available token to perform cleanup")


if __name__ == "__main__":
    unittest.main()
