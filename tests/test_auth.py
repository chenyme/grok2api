import unittest
from unittest.mock import patch

from fastapi import HTTPException
from fastapi.security import HTTPAuthorizationCredentials

from app.core import auth


class AuthTokenTests(unittest.IsolatedAsyncioTestCase):
    async def test_verify_api_key_rejects_unicode_token_without_typeerror(self):
        credentials = HTTPAuthorizationCredentials(
            scheme="Bearer",
            credentials="测试令牌",
        )

        with patch.object(auth, "get_config", return_value="202020hgx"):
            with self.assertRaises(HTTPException) as ctx:
                await auth.verify_api_key(credentials)

        self.assertEqual(ctx.exception.status_code, 401)
        self.assertEqual(ctx.exception.detail, "Invalid authentication token")

    async def test_verify_api_key_accepts_unicode_token_when_config_matches(self):
        credentials = HTTPAuthorizationCredentials(
            scheme="Bearer",
            credentials="测试令牌",
        )

        with patch.object(auth, "get_config", return_value="测试令牌"):
            token = await auth.verify_api_key(credentials)

        self.assertEqual(token, "测试令牌")

    async def test_verify_function_key_accepts_unicode_token_when_enabled(self):
        credentials = HTTPAuthorizationCredentials(
            scheme="Bearer",
            credentials="功能密钥",
        )

        def fake_get_config(key, default=None):
            values = {
                "app.function_key": "功能密钥",
                "app.function_enabled": True,
            }
            return values.get(key, default)

        with patch.object(auth, "get_config", side_effect=fake_get_config):
            token = await auth.verify_function_key(credentials)

        self.assertEqual(token, "功能密钥")


if __name__ == "__main__":
    unittest.main()
