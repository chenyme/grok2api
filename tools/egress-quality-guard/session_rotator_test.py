import importlib.util
import sys
import tempfile
import unittest
from pathlib import Path


MODULE_PATH = Path(__file__).with_name("session_rotator.py")
SPEC = importlib.util.spec_from_file_location("session_rotator", MODULE_PATH)
session_rotator = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
sys.modules[SPEC.name] = session_rotator
SPEC.loader.exec_module(session_rotator)


class SessionRotatorTests(unittest.TestCase):
    def test_replaces_only_selected_proxy_username(self):
        value = """proxies:
- name: 1024Proxy-Grok-Sticky-01
  type: socks5
  username: account-region-US-sid-old1111-t-120
- name: 1024Proxy-Grok-Sticky-02
  type: socks5
  username: account-region-US-sid-old2222-t-120
"""
        result = session_rotator.replace_username_in_mihomo(
            value,
            1,
            "account-region-US-sid-new2222-t-10",
        )
        self.assertIn("sid-old1111-t-120", result)
        self.assertIn("sid-new2222-t-10", result)
        self.assertNotIn("sid-old2222-t-120", result)

    def test_update_session_sets_new_sid_and_ten_minute_stickiness(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            credentials = root / "credentials.list"
            mihomo = root / "mihomo.yaml"
            credentials.write_text(
                "proxy.example:3000:account-region-US-sid-old1111-t-120:secret\n",
                encoding="utf-8",
            )
            mihomo.write_text(
                "- name: 1024Proxy-Grok-Sticky-01\n"
                "  type: socks5\n"
                "  username: account-region-US-sid-old1111-t-120\n",
                encoding="utf-8",
            )
            cfg = session_rotator.Config(
                listen="127.0.0.1",
                port=19099,
                token="",
                credentials_file=credentials,
                mihomo_config_file=mihomo,
                mihomo_controller_url="http://127.0.0.1:9099",
                mihomo_reload_path="/root/.config/mihomo/config.yaml",
                node_id_base=8,
                listener_port_base=7951,
                sticky_minutes=10,
                max_attempts=1,
                verify_timeout_seconds=5,
            )
            rotator = session_rotator.Rotator(cfg)
            rotator._update_session(0, "new1111")
            self.assertIn("sid-new1111-t-10", credentials.read_text(encoding="utf-8"))
            self.assertIn("sid-new1111-t-10", mihomo.read_text(encoding="utf-8"))


if __name__ == "__main__":
    unittest.main()
