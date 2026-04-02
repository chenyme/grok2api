import asyncio
import unittest
from unittest.mock import patch

from app.services.cf_refresh import scheduler


class CfRefreshSchedulerTests(unittest.IsolatedAsyncioTestCase):
    async def test_scheduler_loop_continues_after_refresh_exception(self):
        calls = {"refresh": 0, "sleep": 0}

        async def fake_refresh_once():
            calls["refresh"] += 1
            if calls["refresh"] == 1:
                raise RuntimeError("boom")
            raise asyncio.CancelledError()

        async def fake_sleep(_seconds):
            calls["sleep"] += 1

        with (
            patch.object(scheduler, "is_enabled", return_value=True),
            patch.object(scheduler, "get_refresh_interval", return_value=0),
            patch.object(scheduler, "refresh_once", new=fake_refresh_once),
            patch.object(scheduler.asyncio, "sleep", new=fake_sleep),
        ):
            with self.assertRaises(asyncio.CancelledError):
                await scheduler._scheduler_loop()

        self.assertEqual(calls["refresh"], 2)
        self.assertEqual(calls["sleep"], 1)

    async def test_refresh_once_triggers_token_refresh_after_cf_update(self):
        calls = []

        async def fake_solve():
            return {
                "cookies": "cf_clearance=test-cookie",
                "cf_clearance": "test-cookie",
                "user_agent": "UA",
                "browser": "chrome142",
            }

        async def fake_update(**kwargs):
            calls.append(("update", kwargs))
            return True

        async def fake_refresh_tokens():
            calls.append(("refresh_tokens", None))

        with (
            patch.object(scheduler, "solve_cf_challenge", new=fake_solve),
            patch.object(scheduler, "_update_app_config", new=fake_update),
            patch.object(
                scheduler,
                "_refresh_cooling_tokens_after_cf_update",
                new=fake_refresh_tokens,
                create=True,
            ),
        ):
            ok = await scheduler.refresh_once()

        self.assertTrue(ok)
        self.assertEqual(
            calls,
            [
                (
                    "update",
                    {
                        "cf_cookies": "cf_clearance=test-cookie",
                        "cf_clearance": "test-cookie",
                        "user_agent": "UA",
                        "browser": "chrome142",
                    },
                ),
                ("refresh_tokens", None),
            ],
        )


if __name__ == "__main__":
    unittest.main()
