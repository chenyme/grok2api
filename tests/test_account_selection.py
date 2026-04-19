import unittest
from dataclasses import dataclass
from unittest.mock import patch

from app.control.model.enums import Capability, ModeId, Tier
from app.control.model.spec import ModelSpec
from app.products._account_selection import mode_candidates, reserve_account


@dataclass
class _Lease:
    token: str


class _FakeDirectory:
    def __init__(self, available_by_mode: dict[int, str | None]) -> None:
        self.available_by_mode = available_by_mode
        self.calls: list[int] = []

    async def reserve(
        self,
        *,
        pool_candidates,
        mode_id,
        now_s_override=None,
        exclude_tokens=None,
    ):
        self.calls.append(mode_id)
        token = self.available_by_mode.get(mode_id)
        return _Lease(token) if token else None


class _RefreshingDirectory:
    def __init__(self) -> None:
        self.calls: list[int] = []
        self.after_refresh = False

    async def reserve(
        self,
        *,
        pool_candidates,
        mode_id,
        now_s_override=None,
        exclude_tokens=None,
    ):
        self.calls.append(mode_id)
        if self.after_refresh and mode_id == int(ModeId.AUTO):
            return _Lease("auto-token")
        return None


class _RefreshService:
    def __init__(self, directory: _RefreshingDirectory) -> None:
        self.directory = directory
        self.calls = 0

    async def refresh_on_demand(self):
        self.calls += 1
        self.directory.after_refresh = True


class AccountSelectionTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self) -> None:
        self.auto_chat_spec = ModelSpec(
            "grok-4.20-0309",
            ModeId.AUTO,
            Tier.BASIC,
            Capability.CHAT,
            True,
            "Grok 4.20 0309",
        )
        self.fast_chat_spec = ModelSpec(
            "grok-4.20-fast",
            ModeId.FAST,
            Tier.BASIC,
            Capability.CHAT,
            True,
            "Grok 4.20 Fast",
        )

    @patch("app.products._account_selection.get_config", return_value=True)
    def test_auto_chat_mode_candidates_include_fallbacks(self, _mock_get_config) -> None:
        self.assertEqual(
            mode_candidates(self.auto_chat_spec),
            (int(ModeId.AUTO), int(ModeId.FAST), int(ModeId.EXPERT)),
        )

    @patch("app.products._account_selection.get_config", return_value=False)
    def test_auto_chat_mode_candidates_can_disable_fallback(self, _mock_get_config) -> None:
        self.assertEqual(mode_candidates(self.auto_chat_spec), (int(ModeId.AUTO),))

    @patch("app.products._account_selection.get_config", return_value=True)
    def test_non_auto_models_do_not_change_mode_order(self, _mock_get_config) -> None:
        self.assertEqual(mode_candidates(self.fast_chat_spec), (int(ModeId.FAST),))

    @patch("app.products._account_selection.get_config", return_value=True)
    async def test_reserve_account_falls_back_to_fast(self, _mock_get_config) -> None:
        directory = _FakeDirectory(
            {
                int(ModeId.AUTO): None,
                int(ModeId.FAST): "fast-token",
                int(ModeId.EXPERT): "expert-token",
            }
        )

        lease, selected_mode_id = await reserve_account(directory, self.auto_chat_spec)

        self.assertIsNotNone(lease)
        self.assertEqual(lease.token, "fast-token")
        self.assertEqual(selected_mode_id, int(ModeId.FAST))
        self.assertEqual(
            directory.calls,
            [int(ModeId.AUTO), int(ModeId.FAST)],
        )

    @patch("app.products._account_selection.get_refresh_service")
    @patch("app.products._account_selection.get_config")
    async def test_reserve_account_retries_after_on_demand_refresh(
        self,
        mock_get_config,
        mock_get_refresh_service,
    ) -> None:
        directory = _RefreshingDirectory()
        refresh_service = _RefreshService(directory)
        mock_get_refresh_service.return_value = refresh_service
        mock_get_config.side_effect = lambda key, default=None: {
            "features.auto_chat_mode_fallback": False,
            "account.refresh.on_empty_retry_enabled": True,
        }.get(key, default)

        lease, selected_mode_id = await reserve_account(directory, self.auto_chat_spec)

        self.assertIsNotNone(lease)
        self.assertEqual(lease.token, "auto-token")
        self.assertEqual(selected_mode_id, int(ModeId.AUTO))
        self.assertEqual(refresh_service.calls, 1)
        self.assertEqual(directory.calls, [int(ModeId.AUTO), int(ModeId.AUTO)])


if __name__ == "__main__":
    unittest.main()
