import importlib.util
import stat
import sys
import tempfile
import unittest
from pathlib import Path


MODULE_PATH = Path(__file__).with_name("quality_guard.py")
SPEC = importlib.util.spec_from_file_location("quality_guard", MODULE_PATH)
quality_guard = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
sys.modules[SPEC.name] = quality_guard
SPEC.loader.exec_module(quality_guard)


def config(**overrides):
    values = dict(
        base_url="http://127.0.0.1:8000", username="admin", password="secret", client_key_id="1",
        model="grok-4.5", node_ids=(), mode="hybrid", active_interval_seconds=1800,
        passive_poll_seconds=5, passive_page_size=200, passive_max_pages=10, jitter_seconds=0,
        request_timeout_seconds=120, soft_tps=500.0, hard_tps=1000.0,
        consecutive_soft=2, consecutive_errors=2, quarantine_seconds=300,
        min_healthy_nodes=3, max_output_tokens=384, prompt="probe", expected="QUALITY_OK",
        state_file=Path("/tmp/state.json"), lock_file=Path("/tmp/lock"), insecure_tls=False,
        runtime_config_file=Path("/tmp/runtime-config.json"),
    )
    values.update(overrides)
    return quality_guard.Config(**values)


class ClassificationTests(unittest.TestCase):
    def test_healthy_soft_and_hard_thresholds(self):
        cfg = config()
        self.assertEqual(quality_guard.classify_result({"expectedMatched": True, "visibleTokens": 100, "visibleTokensPerSecond": 499}, cfg)[0], "healthy")
        self.assertEqual(quality_guard.classify_result({"expectedMatched": True, "visibleTokens": 100, "visibleTokensPerSecond": 500}, cfg)[0], "soft")
        self.assertEqual(quality_guard.classify_result({"expectedMatched": True, "visibleTokens": 100, "visibleTokensPerSecond": 1000}, cfg)[0], "hard")

    def test_missing_marker_and_short_response_are_soft_failures(self):
        cfg = config()
        self.assertEqual(quality_guard.classify_result({"expectedMatched": False, "visibleTokens": 100, "visibleTokensPerSecond": 10}, cfg), ("soft", "expected_marker_missing"))
        self.assertEqual(quality_guard.classify_result({"expectedMatched": True, "visibleTokens": 12, "visibleTokensPerSecond": 10}, cfg), ("soft", "insufficient_visible_tokens"))

    def test_passive_speed_excludes_reasoning_tokens(self):
        cfg = config()
        classification, reason, speed, visible = quality_guard.classify_audit({
            "provider": "grok_build", "streaming": True, "statusCode": 200,
            "firstTokenMs": 1000, "durationMs": 1100,
            "outputTokens": 1050, "reasoningTokens": 950,
        }, cfg)
        self.assertEqual((classification, reason, visible), ("hard", "hard_tps", 100))
        self.assertEqual(speed, 1000)

    def test_passive_ignores_short_and_failed_requests(self):
        cfg = config()
        short = {"provider": "grok_build", "streaming": True, "statusCode": 200, "firstTokenMs": 100, "durationMs": 110, "outputTokens": 20, "reasoningTokens": 0}
        failed = {**short, "statusCode": 502, "outputTokens": 100}
        self.assertEqual(quality_guard.classify_audit(short, cfg)[0], "ignored")
        self.assertEqual(quality_guard.classify_audit(failed, cfg)[0], "ignored")


class StateTests(unittest.TestCase):
    def test_state_write_is_atomic_and_private(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "state.json"
            state = {"version": 1, "nodes": {"8": quality_guard.default_node_state()}}
            quality_guard.save_state(path, state)
            loaded = quality_guard.load_state(path)
            self.assertEqual(loaded["nodes"], state["nodes"])
            self.assertFalse(loaded["passive_initialized"])
            self.assertEqual(loaded["seen_audit_ids"], [])
            self.assertEqual(loaded["statistics"]["active"]["total"], 0)
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)


class ConfigTests(unittest.TestCase):
    def test_rejects_reversed_thresholds(self):
        with self.assertRaises(ValueError):
            config(soft_tps=1000, hard_tps=500).validate()

    def test_runtime_config_overrides_only_strategy_fields(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "runtime-config.json"
            path.write_text('{"version":1,"settings":{"mode":"passive","active_interval_seconds":3600,"passive_poll_seconds":10,"soft_tps":400,"hard_tps":900,"consecutive_soft":3,"consecutive_errors":4,"quarantine_seconds":600,"min_healthy_nodes":2}}', encoding="utf-8")
            base = config(runtime_config_file=path, node_ids=("1", "2", "3"))
            loaded = quality_guard.load_runtime_config(base, path)
            self.assertEqual((loaded.mode, loaded.soft_tps, loaded.quarantine_seconds), ("passive", 400, 600))
            self.assertEqual((loaded.model, loaded.client_key_id, loaded.node_ids), (base.model, base.client_key_id, base.node_ids))

    def test_runtime_config_reloader_keeps_last_valid_config(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "runtime-config.json"
            base = config(runtime_config_file=path, node_ids=("1", "2", "3"))
            reloader = quality_guard.RuntimeConfigReloader(base)
            loaded, _, error = reloader.reload(force=True)
            self.assertIsNone(error)
            self.assertEqual(loaded, base)
            path.write_text('{"version":1,"settings":{"mode":"invalid"}}', encoding="utf-8")
            loaded, changed, error = reloader.reload()
            self.assertTrue(changed)
            self.assertIsNotNone(error)
            self.assertEqual(loaded, base)


class FakeApi:
    def __init__(self, nodes, results, audit_pages=None):
        self.nodes = nodes
        self.results = list(results)
        self.audit_pages = list(audit_pages or [])
        self.enabled_calls = []

    def list_nodes(self):
        return self.nodes

    def quality_test(self, _node_id):
        value = self.results.pop(0)
        if isinstance(value, Exception):
            raise value
        return value

    def connectivity_test(self, _node_id):
        return {"status": "healthy"}

    def set_enabled(self, node_id, enabled):
        self.enabled_calls.append((node_id, enabled))
        for node in self.nodes:
            if str(node["id"]) == node_id:
                node["enabled"] = enabled
                return 1
        return 0

    def list_audits(self, _cursor=""):
        if self.audit_pages:
            return self.audit_pages.pop(0)
        return {"items": [], "hasMore": False, "nextCursor": ""}


class GuardTests(unittest.TestCase):
    @staticmethod
    def nodes(count=5):
        return [{"id": str(index), "name": f"node-{index}", "enabled": True, "proxyConfigured": True} for index in range(1, count + 1)]

    def test_hard_signal_quarantines_and_healthy_recovery_restores(self):
        with tempfile.TemporaryDirectory() as directory:
            cfg = config(
                state_file=Path(directory) / "state.json",
                lock_file=Path(directory) / "lock",
                node_ids=("1",),
            )
            bad = {"expectedMatched": True, "visibleTokens": 100, "visibleTokensPerSecond": 1200}
            good = {"expectedMatched": True, "visibleTokens": 100, "visibleTokensPerSecond": 100}
            api = FakeApi(self.nodes(), [bad, good])
            guard = quality_guard.Guard(cfg, api)
            guard.run_cycle()
            self.assertEqual(api.enabled_calls, [("1", False)])
            state = guard.state["nodes"]["1"]
            self.assertTrue(state["disabled_by_guard"])
            state["quarantined_until"] = 0
            guard.run_cycle()
            self.assertEqual(api.enabled_calls, [("1", False), ("1", True)])
            self.assertFalse(state["disabled_by_guard"])
            self.assertEqual(guard.state["statistics"]["active"], {
                "total": 2, "healthy": 1, "soft": 0, "hard": 1, "errors": 0, "visible_tokens": 200,
            })
            self.assertEqual(guard.state["statistics"]["actions"]["quarantined"], 1)
            self.assertEqual(guard.state["statistics"]["actions"]["restored"], 1)

    def test_minimum_healthy_nodes_suppresses_quarantine(self):
        with tempfile.TemporaryDirectory() as directory:
            cfg = config(
                state_file=Path(directory) / "state.json",
                lock_file=Path(directory) / "lock",
                node_ids=("1",),
                min_healthy_nodes=3,
            )
            bad = {"expectedMatched": True, "visibleTokens": 100, "visibleTokensPerSecond": 1200}
            api = FakeApi(self.nodes(3), [bad])
            guard = quality_guard.Guard(cfg, api)
            guard.run_cycle()
            self.assertEqual(api.enabled_calls, [])
            self.assertFalse(guard.state["nodes"]["1"]["disabled_by_guard"])

    def test_model_probe_can_restore_when_generic_connectivity_probe_is_unhealthy(self):
        with tempfile.TemporaryDirectory() as directory:
            cfg = config(state_file=Path(directory) / "state.json", lock_file=Path(directory) / "lock", node_ids=("1",))
            nodes = self.nodes()
            nodes[0]["enabled"] = False
            api = FakeApi(nodes, [{"expectedMatched": True, "visibleTokens": 100, "visibleTokensPerSecond": 100}])
            api.connectivity_test = lambda _node_id: {"status": "unhealthy"}
            guard = quality_guard.Guard(cfg, api)
            state = guard._state_for("1")
            state.update({"disabled_by_guard": True, "quarantined_until": 0})
            guard.run_active_cycle()
            self.assertEqual(api.enabled_calls, [("1", True)])
            self.assertEqual(len(api.results), 0)
            self.assertFalse(state["disabled_by_guard"])

    def test_passive_baseline_does_not_replay_historical_audits(self):
        with tempfile.TemporaryDirectory() as directory:
            cfg = config(state_file=Path(directory) / "state.json", lock_file=Path(directory) / "lock", mode="passive")
            audit = self.audit("old", "1", 1200)
            api = FakeApi(self.nodes(), [], [{"items": [audit], "hasMore": False, "nextCursor": ""}])
            guard = quality_guard.Guard(cfg, api)
            guard.run_passive_cycle()
            self.assertEqual(api.enabled_calls, [])
            self.assertTrue(guard.state["passive_initialized"])
            self.assertIn("old", guard.state["seen_audit_ids"])

    def test_passive_hard_signal_quarantines_but_ignores_guard_key(self):
        with tempfile.TemporaryDirectory() as directory:
            cfg = config(state_file=Path(directory) / "state.json", lock_file=Path(directory) / "lock", mode="passive")
            api = FakeApi(self.nodes(), [], [
                {"items": [], "hasMore": False, "nextCursor": ""},
                {"items": [self.audit("guard", "1", 1200, client_key_id="1"), self.audit("user", "2", 1200)], "hasMore": False, "nextCursor": ""},
            ])
            guard = quality_guard.Guard(cfg, api)
            guard.run_passive_cycle()
            guard.run_passive_cycle()
            self.assertEqual(api.enabled_calls, [("2", False)])
            self.assertFalse(guard.state["nodes"].get("1", {}).get("disabled_by_guard", False))
            self.assertTrue(guard.state["nodes"]["2"]["disabled_by_guard"])
            self.assertEqual(guard.state["statistics"]["passive"]["total"], 1)
            self.assertEqual(guard.state["statistics"]["passive"]["hard"], 1)

    @staticmethod
    def audit(audit_id, node_id, visible_tps, client_key_id="99"):
        generation_ms = 100
        visible_tokens = int(visible_tps * generation_ms / 1000)
        return {
            "id": audit_id, "requestId": f"request-{audit_id}", "clientKeyId": client_key_id,
            "clientKeyName": "user", "provider": "grok_build", "streaming": True,
            "statusCode": 200, "firstTokenMs": 1000, "durationMs": 1000 + generation_ms,
            "outputTokens": visible_tokens + 100, "reasoningTokens": 100,
            "egressNodeId": node_id, "errorCode": None,
        }


if __name__ == "__main__":
    unittest.main()
