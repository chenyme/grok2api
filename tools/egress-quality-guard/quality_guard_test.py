import importlib.util
import json
import os
import stat
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock


MODULE_PATH = Path(__file__).with_name("quality_guard.py")
SPEC = importlib.util.spec_from_file_location("quality_guard", MODULE_PATH)
quality_guard = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
sys.modules[SPEC.name] = quality_guard
SPEC.loader.exec_module(quality_guard)


def config(**overrides):
    values = dict(
        base_url="http://grok2api:8000", internal_token="scoped-secret",
        model="grok-4.5", node_ids=(), mode="hybrid", active_interval_seconds=1800,
        passive_poll_seconds=5, passive_page_size=200, passive_max_pages=10, jitter_seconds=0,
        request_timeout_seconds=120, soft_tps=500.0, hard_tps=1000.0,
        consecutive_soft=2, consecutive_errors=2, quarantine_seconds=300,
        no_account_backoff_seconds=300,
        min_healthy_nodes=3, max_output_tokens=384, prompt="probe", expected="QUALITY_OK",
        fail_closed=False, min_generation_ms=1000, rotation_url="", rotation_token="",
        rotation_timeout_seconds=45, rotatable_node_ids=(),
        state_file=Path("/tmp/state.json"), lock_file=Path("/tmp/lock"),
        runtime_config_file=Path("/tmp/runtime-config.json"),
    )
    values.update(overrides)
    return quality_guard.Config(**values)


class ClassificationTests(unittest.TestCase):
    def test_healthy_soft_and_hard_thresholds(self):
        cfg = config()
        self.assertEqual(quality_guard.classify_result({"expectedMatched": True, "outputTokens": 100, "reasoningTokens": 40, "outputTokensPerSecond": 499}, cfg)[0], "healthy")
        self.assertEqual(quality_guard.classify_result({"expectedMatched": True, "outputTokens": 100, "reasoningTokens": 40, "outputTokensPerSecond": 500}, cfg)[0], "soft")
        self.assertEqual(quality_guard.classify_result({"expectedMatched": True, "outputTokens": 100, "reasoningTokens": 40, "outputTokensPerSecond": 1000}, cfg)[0], "hard")

    def test_missing_marker_is_hard_and_quality_ok_is_not_a_healthy_shortcut(self):
        cfg = config(fail_closed=True, min_generation_ms=1000)
        marker = {"expected_text": "QUALITY_OK", "match_mode": "last_line", "require_thinking": True}
        self.assertEqual(quality_guard.classify_result({"expectedMatched": False, "outputTokens": 100, "outputTokensPerSecond": 10}, cfg, marker), ("hard", "expected_marker_missing"))
        self.assertEqual(quality_guard.classify_result({"expectedMatched": True, "outputTokens": 12, "outputTokensPerSecond": 8000}, cfg, marker), ("soft", "insufficient_output_tokens"))
        self.assertEqual(quality_guard.classify_result({
            "expectedMatched": True, "outputTokens": 128, "reasoningTokens": 40, "outputTokensPerSecond": 8000, "generationMs": 50,
        }, cfg, marker), ("hard", "buffered_burst"))
        self.assertEqual(quality_guard.classify_result({
            "expectedMatched": True, "outputTokens": 128, "reasoningTokens": 40, "outputTokensPerSecond": 80, "generationMs": 1500,
        }, cfg, marker), ("healthy", "within_threshold"))
        self.assertEqual(quality_guard.classify_result({"expectedMatched": True, "outputTokens": 12, "outputTokensPerSecond": 10}, cfg, {"expected_text": ""}), ("soft", "insufficient_output_tokens"))

    def test_passive_speed_matches_panel_and_includes_reasoning_tokens(self):
        cfg = config()
        classification, reason, speed, output = quality_guard.classify_audit({
            "provider": "grok_build", "streaming": True, "statusCode": 200,
            "firstTokenMs": 200, "durationMs": 1200,
            "outputTokens": 1050, "reasoningTokens": 950,
        }, cfg)
        self.assertEqual((classification, reason, output), ("hard", "hard_tps", 1050))
        self.assertEqual(speed, 1050)

    def test_passive_late_first_token_uses_full_duration(self):
        cfg = config()
        classification, reason, speed, output = quality_guard.classify_audit({
            "provider": "grok_build", "streaming": True, "statusCode": 200,
            "firstTokenMs": 19763, "durationMs": 19827,
            "outputTokens": 1511, "reasoningTokens": 1471,
        }, cfg)
        self.assertEqual((classification, reason, output), ("healthy", "within_threshold", 1511))
        self.assertAlmostEqual(speed, 1511 * 1000 / 19827)

    def test_passive_late_first_token_without_reasoning_remains_burst(self):
        cfg = config(fail_closed=True, min_generation_ms=1000)
        classification, reason, speed, output = quality_guard.classify_audit({
            "provider": "grok_build", "streaming": True, "statusCode": 200,
            "firstTokenMs": 10000, "durationMs": 10100,
            "outputTokens": 2000, "reasoningTokens": 0,
        }, cfg)
        self.assertEqual((classification, reason, output), ("hard", "buffered_burst", 2000))
        self.assertEqual(speed, 20000)

    def test_passive_audit_does_not_infer_thinking_requirement(self):
        cfg = config(fail_closed=True, min_generation_ms=1000)
        base = {
            "provider": "grok_build", "streaming": True, "statusCode": 200,
            "firstTokenMs": 2000, "durationMs": 4000, "outputTokens": 200,
        }
        self.assertEqual(quality_guard.classify_audit({**base, "reasoningTokens": 0}, cfg)[:2], ("healthy", "within_threshold"))
        classification, reason, speed, _ = quality_guard.classify_audit({**base, "reasoningTokens": 80}, cfg)
        self.assertEqual((classification, reason), ("healthy", "within_threshold"))
        self.assertAlmostEqual(speed, 100.0)
        short = {**base, "outputTokens": 50, "reasoningTokens": 0, "durationMs": 2500}
        self.assertEqual(quality_guard.classify_audit(short, cfg)[:2], ("healthy", "within_threshold"))
        tiny = {**base, "outputTokens": 20, "reasoningTokens": 0, "durationMs": 2200}
        self.assertEqual(quality_guard.classify_audit(tiny, cfg)[0], "ignored")

    def test_probe_missing_thinking_is_hard(self):
        cfg = config()
        thinking_profile = {"expected_text": "QUALITY_OK", "require_thinking": True}
        self.assertEqual(
            quality_guard.classify_result({
                "expectedMatched": True, "outputTokens": 200, "outputTokensPerSecond": 80,
                "generationMs": 2500, "reasoningTokens": 0,
            }, cfg, thinking_profile),
            ("hard", "missing_thinking"),
        )
        self.assertEqual(
            quality_guard.classify_result({
                "expectedMatched": True, "outputTokens": 200, "outputTokensPerSecond": 80,
                "generationMs": 2500, "reasoningTokens": 90,
            }, cfg, thinking_profile)[0],
            "healthy",
        )
        self.assertEqual(
            quality_guard.classify_result({
                "expectedMatched": True, "outputTokens": 200, "outputTokensPerSecond": 80,
                "generationMs": 2500,
            }, cfg, thinking_profile),
            ("hard", "missing_thinking"),
        )
        self.assertEqual(
            quality_guard.classify_result({
                "expectedMatched": True, "outputTokens": 200, "outputTokensPerSecond": 80,
                "generationMs": 2500, "reasoningTokens": 0,
            }, cfg, {"expected_text": "", "require_thinking": False})[0],
            "healthy",
        )
        self.assertEqual(
            quality_guard.classify_result({
                "expectedMatched": True, "outputTokens": 200, "outputTokensPerSecond": 80,
                "generationMs": 2500, "reasoningTokens": 0, "thinkingRequired": False,
            }, cfg, thinking_profile)[0],
            "healthy",
        )
        self.assertEqual(
            quality_guard.classify_result({
                "expectedMatched": True, "outputTokens": 200, "outputTokensPerSecond": 80,
                "generationMs": 2500, "reasoningTokens": 0, "thinkingRequired": True,
            }, cfg, {"expected_text": "", "require_thinking": False}),
            ("hard", "missing_thinking"),
        )

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
    def test_missing_profile_file_seeds_builtin_recovery_profiles(self):
        with tempfile.TemporaryDirectory() as directory:
            profile_id, profile = quality_guard.resolve_probe_profile(Path(directory) / "missing.json")
            self.assertEqual(profile_id, quality_guard.QUALITY_MARKER_PROFILE_ID)
            self.assertEqual(profile.get("expected_text"), "QUALITY_OK")
            self.assertTrue(profile.get("require_thinking"))

    def test_reserved_profile_ids_are_canonicalized(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "profiles.json"
            path.write_text(json.dumps({
                "version": 1,
                "active_profile_id": "deleted-profile",
                "profiles": {
                    "quality-marker": {
                        "id": "quality-marker", "built_in": False,
                        "expected_text": "", "match_mode": "contains",
                        "require_thinking": False,
                    },
                    "throughput": {
                        "id": "throughput", "built_in": False,
                        "expected_text": "PASS", "match_mode": "regex",
                        "require_thinking": True,
                    },
                },
            }), encoding="utf-8")
            loaded = quality_guard.load_probe_profiles(path)["profiles"]
            profile_id, _ = quality_guard.resolve_probe_profile(path)
            self.assertEqual(profile_id, quality_guard.QUALITY_MARKER_PROFILE_ID)
            marker = loaded[quality_guard.QUALITY_MARKER_PROFILE_ID]
            self.assertTrue(marker["built_in"])
            self.assertEqual(marker["expected_text"], "QUALITY_OK")
            self.assertEqual(marker["match_mode"], "last_line")
            self.assertTrue(marker["require_thinking"])
            throughput = loaded[quality_guard.THROUGHPUT_PROFILE_ID]
            self.assertTrue(throughput["built_in"])
            self.assertEqual(throughput["expected_text"], "")
            self.assertEqual(throughput["match_mode"], "contains")
            self.assertFalse(throughput["require_thinking"])

    def test_loads_private_bootstrap_without_admin_credentials(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "bootstrap.json"
            path.write_text(json.dumps({
                "version": 1,
                "enabled": True,
                "internal_token": "scoped-secret",
                "config": {
                    "model": "grok-4.5", "node_ids": ["2", "9"], "mode": "hybrid",
                    "prompt": "probe", "expected": "QUALITY_OK",
                    "active_interval_seconds": 1800, "passive_poll_seconds": 5,
                    "soft_tps": 500, "hard_tps": 1000, "consecutive_soft": 2, "consecutive_errors": 2,
                    "quarantine_seconds": 300, "no_account_backoff_seconds": 300,
                    "min_healthy_nodes": 1, "max_output_tokens": 384, "fail_closed": False,
                    "min_generation_ms": 1000, "rotation_url": "", "rotation_token": "",
                    "rotation_timeout_seconds": 45, "rotatable_node_ids": [],
                },
            }), encoding="utf-8")
            with mock.patch.dict(os.environ, {}, clear=True):
                loaded = quality_guard.Config.from_bootstrap(path)
            self.assertEqual((loaded.node_ids, loaded.internal_token), (("2", "9"), "scoped-secret"))
            self.assertEqual(loaded.base_url, "http://grok2api:8000")

    def test_bootstrap_honors_grok2api_base_url(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "bootstrap.json"
            path.write_text(json.dumps({
                "version": 1,
                "enabled": True,
                "internal_token": "scoped-secret",
                "config": {
                    "model": "grok-4.5", "node_ids": ["2"], "mode": "hybrid",
                    "prompt": "probe", "expected": "QUALITY_OK",
                    "active_interval_seconds": 1800, "passive_poll_seconds": 5,
                    "soft_tps": 500, "hard_tps": 1000, "consecutive_soft": 2, "consecutive_errors": 2,
                    "quarantine_seconds": 300, "no_account_backoff_seconds": 300,
                    "min_healthy_nodes": 1, "max_output_tokens": 384, "fail_closed": False,
                    "min_generation_ms": 1000, "rotation_url": "", "rotation_token": "",
                    "rotation_timeout_seconds": 45, "rotatable_node_ids": [],
                },
            }), encoding="utf-8")
            with mock.patch.dict(os.environ, {"GROK2API_BASE_URL": "  http://127.0.0.1:18182/  "}, clear=True):
                loaded = quality_guard.Config.from_bootstrap(path)
            self.assertEqual(loaded.base_url, "http://127.0.0.1:18182")

            with mock.patch.dict(os.environ, {"GROK2API_BASE_URL": "  "}, clear=True):
                loaded = quality_guard.Config.from_bootstrap(path)
            self.assertEqual(loaded.base_url, "http://grok2api:8000")

    def test_rejects_unsafe_grok2api_base_urls(self):
        invalid = (
            "http://127.0.0.1:18182?x=1",
            "http://127.0.0.1:18182?",
            "http://127.0.0.1:18182#fragment",
            "http://127.0.0.1:18182#",
            "http://user:pass@127.0.0.1:18182",
            "http://127.0.0.1:invalid",
            "http://:18182",
        )
        for base_url in invalid:
            with self.subTest(base_url=base_url), self.assertRaisesRegex(ValueError, "GROK2API_BASE_URL"):
                config(base_url=base_url).validate()

    def test_disabled_bootstrap_exits_cleanly(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "bootstrap.json"
            path.write_text('{"version":1,"enabled":false,"config":{}}', encoding="utf-8")
            with self.assertRaises(quality_guard.GuardDisabled):
                quality_guard.Config.from_bootstrap(path)

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
            self.assertEqual((loaded.model, loaded.node_ids), (base.model, base.node_ids))

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


class ApiClientTests(unittest.TestCase):
    def test_list_nodes_reads_every_page(self):
        client = quality_guard.ApiClient(config())
        requested_pages = []

        def request(_method, path, _body=None):
            page = int(quality_guard.urllib.parse.parse_qs(quality_guard.urllib.parse.urlparse(path).query)["page"][0])
            requested_pages.append(page)
            if page == 1:
                return {"items": [{"id": str(index)} for index in range(1, 2001)], "total": 2001}
            return {"items": [{"id": "2001"}], "total": 2001}

        client._request = request
        nodes = client.list_nodes()
        self.assertEqual(len(nodes), 2001)
        self.assertEqual(requested_pages, [1, 2])

    def test_list_nodes_rejects_incomplete_pagination(self):
        client = quality_guard.ApiClient(config())
        client._request = lambda *_args, **_kwargs: {"items": [], "total": 1}
        with self.assertRaises(RuntimeError):
            client.list_nodes()

    def test_fixed_fallback_nodes_are_discovered_from_operations_policy(self):
        client = quality_guard.ApiClient(config())
        client._request = lambda *_args, **_kwargs: {
            "fallbacks": {
                "grok_build": {"mode": "fixed", "nodeId": "9"},
                "grok_web": {"mode": "direct"},
                "grok_console": {"mode": "fixed", "nodeId": "11"},
            },
        }
        self.assertEqual(client.fixed_fallback_node_ids(), {"9", "11"})


class FakeApi:
    def __init__(self, nodes, results, audit_pages=None, fixed_fallback_ids=None):
        self.nodes = nodes
        self.results = list(results)
        self.audit_pages = list(audit_pages or [])
        self.fixed_fallback_ids = set(fixed_fallback_ids or [])
        self.enabled_calls = []
        self.quality_calls = []
        self.quality_profile_calls = []
        self.rotation_calls = []

    def list_nodes(self):
        return self.nodes

    def fixed_fallback_node_ids(self):
        return set(self.fixed_fallback_ids)

    def quality_test(self, node_id, profile_id=""):
        self.quality_calls.append(node_id)
        self.quality_profile_calls.append(profile_id)
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

    def rotate_node(self, node_id, old_exit_ip=""):
        self.rotation_calls.append((node_id, old_exit_ip))
        return {"changed": True, "oldExitIp": old_exit_ip, "newExitIp": "203.0.113.10"}

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
            bad = {"expectedMatched": True, "outputTokens": 100, "reasoningTokens": 40, "outputTokensPerSecond": 1200}
            good = {"expectedMatched": True, "outputTokens": 100, "reasoningTokens": 40, "outputTokensPerSecond": 100}
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
                "total": 2, "healthy": 1, "soft": 0, "hard": 1, "errors": 0, "output_tokens": 200,
            })
            self.assertEqual(guard.state["statistics"]["actions"]["quarantined"], 1)
            self.assertEqual(guard.state["statistics"]["actions"]["restored"], 1)

    def test_auto_discovery_publishes_resolved_node_ids_for_status_consumers(self):
        with tempfile.TemporaryDirectory() as directory:
            cfg = config(state_file=Path(directory) / "state.json", lock_file=Path(directory) / "lock", mode="passive")
            nodes = self.nodes(3)
            nodes[1]["enabled"] = False
            nodes.append({"id": "4", "name": "direct", "enabled": True, "proxyConfigured": False})
            api = FakeApi(nodes, [], [{"items": [], "hasMore": False, "nextCursor": ""}])
            guard = quality_guard.Guard(cfg, api)
            guard.run_passive_cycle()
            self.assertEqual(guard.state["guard"]["node_ids"], ["1", "2", "3"])

    def test_fixed_fallback_node_is_excluded_without_aborting_other_nodes(self):
        with tempfile.TemporaryDirectory() as directory:
            cfg = config(
                state_file=Path(directory) / "state.json",
                lock_file=Path(directory) / "lock",
                node_ids=("1", "2"),
            )
            good = {"expectedMatched": True, "outputTokens": 100, "reasoningTokens": 40, "outputTokensPerSecond": 100}
            api = FakeApi(self.nodes(3), [good], fixed_fallback_ids={"1"})
            guard = quality_guard.Guard(cfg, api)
            guard.run_active_cycle()
            self.assertEqual(api.quality_calls, ["2"])
            self.assertEqual(guard.state["protected_node_ids"], ["1"])

    def test_enabled_node_promoted_to_fixed_fallback_releases_guard_ownership(self):
        with tempfile.TemporaryDirectory() as directory:
            cfg = config(
                state_file=Path(directory) / "state.json",
                lock_file=Path(directory) / "lock",
                node_ids=("1",),
                mode="passive",
                fail_closed=True,
            )
            api = FakeApi(
                self.nodes(3), [],
                [{"items": [], "hasMore": False, "nextCursor": ""}],
                fixed_fallback_ids={"1"},
            )
            guard = quality_guard.Guard(cfg, api)
            guard._state_for("1")["disabled_by_guard"] = True
            guard.run_passive_cycle()
            self.assertEqual(api.enabled_calls, [])
            self.assertNotIn("1", guard.state["nodes"])

    def test_quarantine_ownership_is_persisted_before_backend_disable(self):
        with tempfile.TemporaryDirectory() as directory:
            state_path = Path(directory) / "state.json"
            cfg = config(
                state_file=state_path,
                lock_file=Path(directory) / "lock",
                node_ids=("1",),
                fail_closed=True,
            )

            class ObservingApi(FakeApi):
                def set_enabled(self, node_id, enabled):
                    persisted = quality_guard.load_state(state_path)
                    self.assert_persisted = bool(persisted["nodes"][node_id]["disabled_by_guard"])
                    return super().set_enabled(node_id, enabled)

            bad = {"expectedMatched": True, "outputTokens": 100, "reasoningTokens": 40, "outputTokensPerSecond": 1200, "generationMs": 1500}
            api = ObservingApi(self.nodes(3), [bad])
            guard = quality_guard.Guard(cfg, api)
            guard.run_active_cycle()
            self.assertTrue(api.assert_persisted)
            self.assertTrue(quality_guard.load_state(state_path)["nodes"]["1"]["disabled_by_guard"])

    def test_quarantine_state_rolls_back_when_backend_disable_fails(self):
        with tempfile.TemporaryDirectory() as directory:
            state_path = Path(directory) / "state.json"
            cfg = config(
                state_file=state_path,
                lock_file=Path(directory) / "lock",
                node_ids=("1",),
                fail_closed=True,
            )

            class FailingApi(FakeApi):
                def set_enabled(self, node_id, enabled):
                    self.enabled_calls.append((node_id, enabled))
                    raise RuntimeError("backend unavailable")

            bad = {"expectedMatched": True, "outputTokens": 100, "reasoningTokens": 40, "outputTokensPerSecond": 1200, "generationMs": 1500}
            api = FailingApi(self.nodes(3), [bad])
            guard = quality_guard.Guard(cfg, api)
            guard.run_active_cycle()
            self.assertEqual(api.enabled_calls, [("1", False)])
            self.assertTrue(api.nodes[0]["enabled"])
            self.assertFalse(guard.state["nodes"]["1"]["disabled_by_guard"])
            self.assertFalse(quality_guard.load_state(state_path)["nodes"]["1"]["disabled_by_guard"])

    def test_removed_or_unmanaged_node_state_is_pruned(self):
        with tempfile.TemporaryDirectory() as directory:
            cfg = config(
                state_file=Path(directory) / "state.json",
                lock_file=Path(directory) / "lock",
                node_ids=("1",),
                mode="passive",
            )
            api = FakeApi(self.nodes(2), [], [{"items": [], "hasMore": False, "nextCursor": ""}])
            guard = quality_guard.Guard(cfg, api)
            guard._state_for("2")
            guard._state_for("3")["disabled_by_guard"] = True
            guard.run_passive_cycle()
            self.assertNotIn("2", guard.state["nodes"])
            self.assertNotIn("3", guard.state["nodes"])

    def test_quarantined_node_removed_from_config_is_recovered_before_release(self):
        with tempfile.TemporaryDirectory() as directory:
            cfg = config(
                state_file=Path(directory) / "state.json",
                lock_file=Path(directory) / "lock",
                node_ids=("1",),
            )
            nodes = self.nodes(2)
            nodes[0]["enabled"] = False
            nodes[1]["enabled"] = False
            good = {"expectedMatched": True, "outputTokens": 100, "reasoningTokens": 40, "outputTokensPerSecond": 100}
            api = FakeApi(nodes, [good])
            guard = quality_guard.Guard(cfg, api)
            state = guard._state_for("2")
            state.update({"disabled_by_guard": True, "quarantined_until": 0})
            guard.run_active_cycle()
            self.assertEqual(api.quality_calls, ["2"])
            self.assertEqual(api.enabled_calls, [("2", True)])
            self.assertFalse(state["disabled_by_guard"])

    def test_minimum_healthy_nodes_suppresses_quarantine(self):
        with tempfile.TemporaryDirectory() as directory:
            cfg = config(
                state_file=Path(directory) / "state.json",
                lock_file=Path(directory) / "lock",
                node_ids=("1",),
                min_healthy_nodes=3,
            )
            bad = {"expectedMatched": True, "outputTokens": 100, "reasoningTokens": 40, "outputTokensPerSecond": 1200}
            api = FakeApi(self.nodes(3), [bad])
            guard = quality_guard.Guard(cfg, api)
            guard.run_cycle()
            self.assertEqual(api.enabled_calls, [])
            self.assertFalse(guard.state["nodes"]["1"]["disabled_by_guard"])

    def test_fail_closed_rotates_soft_signal_and_restores_after_one_good_probe(self):
        with tempfile.TemporaryDirectory() as directory:
            cfg = config(
                state_file=Path(directory) / "state.json",
                lock_file=Path(directory) / "lock",
                node_ids=("1",),
                fail_closed=True,
                rotation_url="http://127.0.0.1:19099/rotate",
                rotatable_node_ids=("1",),
            )
            good = {
                "expectedMatched": True,
                "reasoningTokens": 40, "outputTokens": 128,
                "outputTokensPerSecond": 100,
                "generationMs": 1500,
            }
            audit = self.audit("user", "1", 600)
            api = FakeApi(self.nodes(3), [good], [
                {"items": [], "hasMore": False, "nextCursor": ""},
                {"items": [audit], "hasMore": False, "nextCursor": ""},
            ])
            guard = quality_guard.Guard(cfg, api)
            guard.run_passive_cycle()
            guard.run_passive_cycle()
            self.assertEqual(api.enabled_calls, [("1", False)])
            self.assertEqual(api.rotation_calls, [("1", "")])
            self.assertEqual(api.quality_calls, [])
            self.assertTrue(guard.state["nodes"]["1"]["disabled_by_guard"])

    def test_fail_closed_requires_consecutive_probe_errors_then_rotates(self):
        with tempfile.TemporaryDirectory() as directory:
            cfg = config(
                state_file=Path(directory) / "state.json",
                lock_file=Path(directory) / "lock",
                node_ids=("1",),
                fail_closed=True,
                consecutive_errors=2,
                rotation_url="http://127.0.0.1:19099/rotate",
                rotatable_node_ids=("1",),
            )
            good = {
                "expectedMatched": True,
                "reasoningTokens": 40, "outputTokens": 128,
                "outputTokensPerSecond": 100,
                "generationMs": 1500,
            }
            api = FakeApi(self.nodes(3), [
                RuntimeError("temporary"), RuntimeError("still failing"),
                RuntimeError("new IP still failing"), good,
            ])
            guard = quality_guard.Guard(cfg, api)

            guard.run_active_cycle()
            self.assertEqual(api.enabled_calls, [])
            self.assertEqual(guard.state["nodes"]["1"]["error_strikes"], 1)

            guard.run_active_cycle()
            self.assertEqual(api.enabled_calls, [("1", False)])
            self.assertEqual(api.rotation_calls, [("1", "")])
            self.assertTrue(guard.state["nodes"]["1"]["disabled_by_guard"])
            self.assertEqual(guard.state["nodes"]["1"]["last_reason"], "recovery_probe_error")

            guard.state["nodes"]["1"]["quarantined_until"] = 0
            guard.run_active_cycle()
            self.assertEqual(api.enabled_calls, [("1", False), ("1", True)])
            self.assertEqual(api.rotation_calls, [("1", ""), ("1", "")])
            self.assertFalse(guard.state["nodes"]["1"]["disabled_by_guard"])

    def test_probe_without_schedulable_account_is_deferred_without_rotation(self):
        with tempfile.TemporaryDirectory() as directory:
            cfg = config(
                state_file=Path(directory) / "state.json",
                lock_file=Path(directory) / "lock",
                node_ids=("1",),
                fail_closed=True,
                consecutive_errors=1,
                rotation_url="http://127.0.0.1:19099/rotate",
                rotatable_node_ids=("1",),
            )
            unavailable = lambda: quality_guard.ApiError(503, "egressQualityProbeNoAccount", "no account")
            api = FakeApi(self.nodes(3), [unavailable(), unavailable()])
            guard = quality_guard.Guard(cfg, api)

            guard.run_active_cycle()
            self.assertEqual(api.enabled_calls, [])
            self.assertEqual(api.rotation_calls, [])
            self.assertEqual(api.quality_calls, ["1"])
            self.assertEqual(guard.state["nodes"]["1"]["error_strikes"], 0)
            self.assertEqual(guard.state["nodes"]["1"]["last_reason"], "probe_no_account")

            guard.run_active_cycle()
            self.assertEqual(api.quality_calls, ["1"])

            api.nodes[0]["enabled"] = False
            state = guard.state["nodes"]["1"]
            state["disabled_by_guard"] = True
            recovery_at = state["quarantined_until"]
            guard._recover_quarantined(api.nodes[0], recovery_at, rotate=False)
            self.assertEqual(api.enabled_calls, [])
            self.assertEqual(api.rotation_calls, [])
            self.assertEqual(state["last_reason"], "probe_no_account")
            self.assertTrue(state["disabled_by_guard"])
            self.assertEqual(state["quarantined_until"], recovery_at + cfg.no_account_backoff_seconds)

    def test_fail_closed_buffered_burst_restores_same_ip_after_one_good_probe(self):
        with tempfile.TemporaryDirectory() as directory:
            cfg = config(
                state_file=Path(directory) / "state.json",
                lock_file=Path(directory) / "lock",
                node_ids=("1",),
                fail_closed=True,
                rotation_url="http://127.0.0.1:19099/rotate",
                rotatable_node_ids=("1",),
            )
            good = {
                "expectedMatched": True,
                "reasoningTokens": 40, "outputTokens": 128,
                "outputTokensPerSecond": 100,
                "generationMs": 1500,
            }
            api = FakeApi(self.nodes(3), [good], [
                {"items": [], "hasMore": False, "nextCursor": ""},
                {"items": [self.audit("burst", "1", 2500)], "hasMore": False, "nextCursor": ""},
            ])
            guard = quality_guard.Guard(cfg, api)
            guard.run_passive_cycle()
            guard.run_passive_cycle()
            self.assertEqual(api.enabled_calls, [("1", False)])
            self.assertEqual(api.rotation_calls, [("1", "")])
            self.assertEqual(api.quality_calls, [])
            self.assertTrue(guard.state["nodes"]["1"]["disabled_by_guard"])

    def test_fail_closed_keeps_node_isolated_when_rotated_ip_probe_is_ambiguous(self):
        with tempfile.TemporaryDirectory() as directory:
            cfg = config(
                state_file=Path(directory) / "state.json",
                lock_file=Path(directory) / "lock",
                node_ids=("1",),
                fail_closed=True,
                rotation_url="http://127.0.0.1:19099/rotate",
                rotatable_node_ids=("1",),
                min_healthy_nodes=3,
            )
            ambiguous = {
                "expectedMatched": True,
                "reasoningTokens": 40, "outputTokens": 128,
                "outputTokensPerSecond": 100,
                "generationMs": 50,
            }
            api = FakeApi(self.nodes(3), [ambiguous, ambiguous.copy()])
            guard = quality_guard.Guard(cfg, api)
            guard._probe_active(api.nodes, api.nodes[0], 1.0)
            self.assertEqual(api.enabled_calls, [("1", False)])
            self.assertEqual(api.rotation_calls, [("1", "")])
            self.assertTrue(guard.state["nodes"]["1"]["disabled_by_guard"])
            self.assertEqual(guard.state["nodes"]["1"]["last_reason"], "insufficient_generation_window")

    def test_fail_closed_manual_reenable_requires_probe(self):
        with tempfile.TemporaryDirectory() as directory:
            cfg = config(
                state_file=Path(directory) / "state.json",
                lock_file=Path(directory) / "lock",
                node_ids=("1",),
                fail_closed=True,
            )
            good = {
                "expectedMatched": True,
                "reasoningTokens": 40, "outputTokens": 128,
                "outputTokensPerSecond": 100,
                "generationMs": 1500,
            }
            nodes = self.nodes()
            api = FakeApi(nodes, [good])
            guard = quality_guard.Guard(cfg, api)
            state = guard._state_for("1")
            state.update({"disabled_by_guard": True, "last_reason": "hard_tps"})
            guard.run_active_cycle()
            self.assertEqual(api.enabled_calls, [("1", False), ("1", True)])
            self.assertEqual(api.quality_calls, ["1"])
            self.assertFalse(state["disabled_by_guard"])

    def test_model_probe_can_restore_when_generic_connectivity_probe_is_unhealthy(self):
        with tempfile.TemporaryDirectory() as directory:
            cfg = config(state_file=Path(directory) / "state.json", lock_file=Path(directory) / "lock", node_ids=("1",))
            nodes = self.nodes()
            nodes[0]["enabled"] = False
            api = FakeApi(nodes, [{"expectedMatched": True, "outputTokens": 100, "reasoningTokens": 40, "outputTokensPerSecond": 100}])
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

    def test_passive_hard_signal_quarantines_immediately_and_ignores_guard_key(self):
        with tempfile.TemporaryDirectory() as directory:
            cfg = config(state_file=Path(directory) / "state.json", lock_file=Path(directory) / "lock", mode="passive", node_ids=("2",))
            healthy = {"expectedMatched": True, "outputTokens": 100, "reasoningTokens": 40, "outputTokensPerSecond": 100}
            api = FakeApi(self.nodes(), [healthy], [
                {"items": [], "hasMore": False, "nextCursor": ""},
                {"items": [self.audit("guard", "1", 1200, quality_probe=True), self.audit("user", "2", 1200)], "hasMore": False, "nextCursor": ""},
            ])
            guard = quality_guard.Guard(cfg, api)
            guard.run_passive_cycle()
            guard.run_passive_cycle()
            self.assertEqual(api.quality_calls, [])
            self.assertEqual(api.enabled_calls, [("2", False)])
            self.assertFalse(guard.state["nodes"].get("1", {}).get("disabled_by_guard", False))
            self.assertTrue(guard.state["nodes"]["2"]["disabled_by_guard"])
            self.assertEqual(guard.state["nodes"]["2"]["passive_soft_strikes"], 0)
            self.assertEqual(guard.state["statistics"]["passive"]["total"], 1)
            self.assertEqual(guard.state["statistics"]["passive"]["hard"], 1)
            self.assertEqual(guard.state["nodes"]["2"]["quarantine_source"], "passive")
            guard.state["nodes"]["2"]["quarantined_until"] = 0
            guard.run_active_cycle()
            self.assertEqual(api.quality_calls, ["2"])
            self.assertEqual(api.enabled_calls, [("2", False), ("2", True)])
            self.assertFalse(guard.state["nodes"]["2"]["disabled_by_guard"])

    def test_passive_soft_signal_quarantines_immediately_without_probe(self):
        with tempfile.TemporaryDirectory() as directory:
            cfg = config(state_file=Path(directory) / "state.json", lock_file=Path(directory) / "lock", mode="passive")
            api = FakeApi(self.nodes(), [], [
                {"items": [], "hasMore": False, "nextCursor": ""},
                {"items": [self.audit("user", "2", 600)], "hasMore": False, "nextCursor": ""},
            ])
            guard = quality_guard.Guard(cfg, api)
            guard.run_passive_cycle()
            guard.run_passive_cycle()
            self.assertEqual(api.quality_calls, [])
            self.assertEqual(api.enabled_calls, [("2", False)])
            self.assertTrue(guard.state["nodes"]["2"]["disabled_by_guard"])

    def test_passive_soft_signal_holds_until_quarantine_window(self):
        with tempfile.TemporaryDirectory() as directory:
            cfg = config(state_file=Path(directory) / "state.json", lock_file=Path(directory) / "lock", mode="passive")
            api = FakeApi(self.nodes(), [], [
                {"items": [], "hasMore": False, "nextCursor": ""},
                {"items": [self.audit("user-1", "2", 600)], "hasMore": False, "nextCursor": ""},
                {"items": [self.audit("user-2", "2", 600)], "hasMore": False, "nextCursor": ""},
            ])
            guard = quality_guard.Guard(cfg, api)
            guard.run_passive_cycle()
            guard.run_passive_cycle()
            self.assertEqual(api.enabled_calls, [("2", False)])
            self.assertEqual(api.quality_calls, [])
            self.assertTrue(guard.state["nodes"]["2"]["disabled_by_guard"])
            guard.run_passive_cycle()
            self.assertEqual(api.quality_calls, [])
            self.assertEqual(api.enabled_calls, [("2", False)])

    def test_multiple_passive_hard_signals_only_quarantine_once(self):
        with tempfile.TemporaryDirectory() as directory:
            cfg = config(state_file=Path(directory) / "state.json", lock_file=Path(directory) / "lock", mode="passive")
            api = FakeApi(self.nodes(), [], [
                {"items": [], "hasMore": False, "nextCursor": ""},
                {"items": [self.audit("user-1", "2", 1200), self.audit("user-2", "2", 1500)], "hasMore": False, "nextCursor": ""},
            ])
            guard = quality_guard.Guard(cfg, api)
            guard.run_passive_cycle()
            guard.run_passive_cycle()
            self.assertEqual(api.quality_calls, [])
            self.assertEqual(api.enabled_calls, [("2", False)])
            self.assertEqual(guard.state["nodes"]["2"]["passive_soft_strikes"], 0)

    def test_passive_hold_restores_only_after_healthy_probe(self):
        with tempfile.TemporaryDirectory() as directory:
            cfg = config(state_file=Path(directory) / "state.json", lock_file=Path(directory) / "lock", mode="passive")
            good = {
                "expectedMatched": True,
                "reasoningTokens": 40, "outputTokens": 128,
                "outputTokensPerSecond": 80,
                "generationMs": 1500,
            }
            api = FakeApi(self.nodes(), [good], [
                {"items": [], "hasMore": False, "nextCursor": ""},
                {"items": [self.audit("user", "2", 2500)], "hasMore": False, "nextCursor": ""},
            ])
            guard = quality_guard.Guard(cfg, api)
            guard.run_passive_cycle()
            guard.run_passive_cycle()
            self.assertEqual(api.enabled_calls, [("2", False)])
            self.assertEqual(api.quality_calls, [])
            self.assertTrue(guard.state["nodes"]["2"]["disabled_by_guard"])
            self.assertEqual(guard.state["nodes"]["2"]["passive_degrade_repeats"], 1)
            guard.state["nodes"]["2"]["quarantined_until"] = 0
            guard.run_passive_cycle()
            self.assertEqual(api.quality_calls, ["2"])
            self.assertEqual(api.enabled_calls, [("2", False), ("2", True)])
            self.assertFalse(guard.state["nodes"]["2"]["disabled_by_guard"])
            self.assertEqual(guard.state["nodes"]["2"]["quarantine_source"], "")

    def test_recovery_always_uses_builtin_quality_marker_profile(self):
        with tempfile.TemporaryDirectory() as directory:
            profiles_path = Path(directory) / "profiles.json"
            profiles_path.write_text(json.dumps({
                "version": 1,
                "active_profile_id": "throughput",
                "profiles": {
                    "throughput": {
                        "id": "throughput", "built_in": True,
                        "expected_text": "", "match_mode": "contains",
                    },
                },
            }), encoding="utf-8")
            cfg = config(
                state_file=Path(directory) / "state.json",
                lock_file=Path(directory) / "lock",
                profiles_file=profiles_path,
            )
            good = {
                "expectedMatched": True, "reasoningTokens": 40, "outputTokens": 128,
                "outputTokensPerSecond": 80, "generationMs": 1500,
            }
            nodes = self.nodes()
            nodes[1]["enabled"] = False
            api = FakeApi(nodes, [good])
            guard = quality_guard.Guard(cfg, api)
            guard._state_for("2").update({"disabled_by_guard": True, "quarantined_until": 0})
            guard._recover_quarantined(nodes[1], time.time(), rotate=False)
            self.assertEqual(api.quality_profile_calls, [quality_guard.QUALITY_MARKER_PROFILE_ID])
            self.assertEqual(api.enabled_calls, [("2", True)])

    def test_passive_hold_keeps_isolated_when_recovery_probe_is_unhealthy(self):
        with tempfile.TemporaryDirectory() as directory:
            cfg = config(
                state_file=Path(directory) / "state.json",
                lock_file=Path(directory) / "lock",
                mode="passive",
                fail_closed=True,
            )
            bad = {
                "expectedMatched": True,
                "reasoningTokens": 40, "outputTokens": 128,
                "outputTokensPerSecond": 8000,
                "generationMs": 50,
            }
            api = FakeApi(self.nodes(), [bad], [
                {"items": [], "hasMore": False, "nextCursor": ""},
                {"items": [self.audit("user", "2", 2500)], "hasMore": False, "nextCursor": ""},
            ])
            guard = quality_guard.Guard(cfg, api)
            guard.run_passive_cycle()
            guard.run_passive_cycle()
            self.assertEqual(api.enabled_calls, [("2", False)])
            guard.state["nodes"]["2"]["quarantined_until"] = 0
            guard.run_passive_cycle()
            self.assertEqual(api.quality_calls, ["2"])
            self.assertEqual(api.enabled_calls, [("2", False)])
            self.assertTrue(guard.state["nodes"]["2"]["disabled_by_guard"])
            self.assertEqual(guard.state["nodes"]["2"]["last_reason"], "buffered_burst")

    def test_repeat_passive_degrade_lengthens_hold(self):
        with tempfile.TemporaryDirectory() as directory:
            cfg = config(
                state_file=Path(directory) / "state.json",
                lock_file=Path(directory) / "lock",
                mode="passive",
                quarantine_seconds=30,
            )
            good = {
                "expectedMatched": True,
                "reasoningTokens": 40, "outputTokens": 128,
                "outputTokensPerSecond": 80,
                "generationMs": 1500,
            }
            api = FakeApi(self.nodes(), [good], [
                {"items": [], "hasMore": False, "nextCursor": ""},
                {"items": [self.audit("user-1", "2", 2500)], "hasMore": False, "nextCursor": ""},
            ])
            guard = quality_guard.Guard(cfg, api)
            guard.run_passive_cycle()
            guard.run_passive_cycle()
            first_hold = guard.state["nodes"]["2"]["quarantined_until"] - time.time()
            self.assertGreater(first_hold, 20)
            self.assertLess(first_hold, 40)
            guard.state["nodes"]["2"]["quarantined_until"] = 0
            guard.run_passive_cycle()
            self.assertEqual(api.quality_calls, ["2"])
            self.assertFalse(guard.state["nodes"]["2"]["disabled_by_guard"])
            api.audit_pages.append({"items": [self.audit("user-2", "2", 2500)], "hasMore": False, "nextCursor": ""})
            before = time.time()
            guard.run_passive_cycle()
            self.assertTrue(guard.state["nodes"]["2"]["disabled_by_guard"])
            self.assertEqual(guard.state["nodes"]["2"]["passive_degrade_repeats"], 2)
            second_hold = guard.state["nodes"]["2"]["quarantined_until"] - before
            self.assertGreaterEqual(second_hold, 55)
            self.assertLess(second_hold, 70)

    def test_passive_user_anomaly_quarantines_without_waiting_for_probe(self):
        with tempfile.TemporaryDirectory() as directory:
            cfg = config(
                state_file=Path(directory) / "state.json", lock_file=Path(directory) / "lock",
                mode="passive", consecutive_errors=1,
            )
            api = FakeApi(self.nodes(), [RuntimeError("probe unavailable")], [
                {"items": [], "hasMore": False, "nextCursor": ""},
                {"items": [self.audit("user", "2", 600)], "hasMore": False, "nextCursor": ""},
            ])
            guard = quality_guard.Guard(cfg, api)
            guard.run_passive_cycle()
            guard.run_passive_cycle()
            self.assertEqual(api.enabled_calls, [("2", False)])
            self.assertEqual(api.quality_calls, [])
            self.assertTrue(guard.state["nodes"]["2"]["disabled_by_guard"])

    @staticmethod
    def audit(audit_id, node_id, output_tps, quality_probe=False):
        generation_ms = 2000
        output_tokens = int(output_tps * generation_ms / 1000)
        return {
            "id": audit_id, "requestId": f"request-{audit_id}", "qualityProbe": quality_probe,
            "provider": "grok_build", "streaming": True,
            "statusCode": 200, "firstTokenMs": 200, "durationMs": 200 + generation_ms,
            "outputTokens": output_tokens, "reasoningTokens": min(100, max(0, output_tokens - 1)),
            "egressNodeId": node_id, "errorCode": None,
        }


if __name__ == "__main__":
    unittest.main()
