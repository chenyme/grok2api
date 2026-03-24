import unittest

from app.services.grok.services.model import ModelService


class ModelAliasTests(unittest.TestCase):
    def test_canonical_models_use_working_upstream_pairs(self):
        cases = {
            "grok-4-heavy": ("grok-4", "MODEL_MODE_EXPERT"),
            "grok-4.1-mini": ("grok-4-1-thinking-1129", "MODEL_MODE_FAST"),
            "grok-4.1-thinking": ("grok-4-1-thinking-1129", "MODEL_MODE_EXPERT"),
            "grok-4.20-beta": ("grok-420", "MODEL_MODE_FAST"),
        }

        for model_id, (grok_model, model_mode) in cases.items():
            with self.subTest(model_id=model_id):
                info = ModelService.get(model_id)
                self.assertIsNotNone(info)
                self.assertEqual(info.grok_model, grok_model)
                self.assertEqual(info.model_mode, model_mode)

    def test_aliases_resolve_to_same_grok_target(self):
        cases = {
            "grok-4-fast": "grok-4.1-fast",
            "grok-code-fast-1": "grok-4.1-fast",
            "grok-4.20-beta-latest-non-reasoning": "grok-4.20-beta",
            "grok-4-20-beta-latest-non-reasoning": "grok-4.20-beta",
        }

        for alias_id, canonical_id in cases.items():
            with self.subTest(alias_id=alias_id):
                alias = ModelService.get(alias_id)
                canonical = ModelService.get(canonical_id)

                self.assertIsNotNone(alias)
                self.assertIsNotNone(canonical)
                self.assertEqual(alias.grok_model, canonical.grok_model)
                self.assertEqual(alias.model_mode, canonical.model_mode)
                self.assertEqual(alias.tier, canonical.tier)
                self.assertEqual(alias.cost, canonical.cost)

    def test_models_list_includes_aliases(self):
        model_ids = {model.model_id for model in ModelService.list()}

        self.assertIn("grok-4-fast", model_ids)
        self.assertIn("grok-code-fast-1", model_ids)
        self.assertIn("grok-4.20-beta-latest-non-reasoning", model_ids)
        self.assertIn("grok-4-20-beta-latest-non-reasoning", model_ids)


if __name__ == "__main__":
    unittest.main()
