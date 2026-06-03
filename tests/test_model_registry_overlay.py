import unittest
from unittest.mock import patch

from app.control.account.models import AccountQuotaSet
from app.control.account.quota_defaults import (
    default_quota_set,
    normalize_quota_set,
    supports_mode,
)
from app.control.model import registry as model_registry
from app.control.model.enums import Capability, ModeId, Tier
from app.dataplane.reverse.protocol.xai_chat import build_chat_payload


class ModelRegistryOverlayTests(unittest.TestCase):
    def test_manual_model_is_executable_as_direct_upstream_model(self):
        registry_data = {
            "enabled": True,
            "manual_models": [{"id": "grok-4.3", "name": "Grok 4.3"}],
            "aliases": {},
            "remote_model_ids": ["grok-4.3"],
        }

        with patch(
            "app.plugins.model_registry.service.registry_config",
            return_value=registry_data,
        ):
            spec = model_registry.get("grok-4.3")
            self.assertIsNotNone(spec)
            self.assertEqual(spec.model_name, "grok-4.3")
            self.assertEqual(spec.mode_id, ModeId.FAST)
            self.assertEqual(spec.tier, Tier.BASIC)
            self.assertEqual(spec.capability, Capability.CHAT)
            self.assertEqual(spec.upstream_model_name, "grok-4.3")

            desc = model_registry.describe("grok-4.3")
            self.assertTrue(desc["manual"])
            self.assertTrue(desc["executable"])
            self.assertIsNone(desc["mapped_to"])

        self.assertTrue(supports_mode("basic", int(ModeId.GROK_4_3)))
        self.assertIsNotNone(default_quota_set("basic").grok_4_3)

        old_basic = default_quota_set("basic")
        old_basic_without_grok_43 = AccountQuotaSet(
            auto=old_basic.auto,
            fast=old_basic.fast,
            expert=old_basic.expert,
        )
        normalized = normalize_quota_set("basic", old_basic_without_grok_43)
        self.assertIsNotNone(normalized.grok_4_3)

    def test_manual_model_payload_uses_actual_model_id(self):
        payload = build_chat_payload(
            message="ping",
            mode_id=ModeId.FAST,
            upstream_model_name="grok-4.3",
        )

        self.assertEqual(payload["modeId"], "fast")
        self.assertEqual(payload["modelName"], "grok-4.3")
        self.assertEqual(
            payload["responseMetadata"]["requestModelDetails"]["modelId"],
            "grok-4.3",
        )

        builtin_payload = build_chat_payload(
            message="ping",
            mode_id=ModeId.FAST,
        )
        self.assertNotIn("modelName", builtin_payload)
        self.assertNotIn("requestModelDetails", builtin_payload["responseMetadata"])


if __name__ == "__main__":
    unittest.main()
