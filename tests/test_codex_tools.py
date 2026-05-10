import json
import unittest

from app.dataplane.reverse.protocol.tool_parser import ParsedToolCall
from app.products.openai._codex_tools import (
    _normalize_codex_tool_calls,
    _synthesize_codex_tool_call,
)


class CodexToolFallbackTests(unittest.TestCase):
    def test_synthesizes_write_stdin_for_session_input(self):
        calls = _synthesize_codex_tool_call(
            ["exec_command", "write_stdin"],
            "[user]: 向 session_id 123 输入 'hello'",
            "",
        )

        self.assertEqual(len(calls), 1)
        self.assertEqual(calls[0].name, "write_stdin")
        self.assertEqual(json.loads(calls[0].arguments), {"session_id": 123, "chars": "hello"})

    def test_synthesizes_apply_patch_for_simple_create_file_request(self):
        calls = _synthesize_codex_tool_call(
            ["exec_command"],
            "[user]: 请创建文件 codex_apply_patch_probe.txt，内容为 ok",
            "",
        )

        self.assertEqual(len(calls), 1)
        args = json.loads(calls[0].arguments)
        self.assertIn("apply_patch <<'PATCH'", args["cmd"])
        self.assertIn("*** Add File: codex_apply_patch_probe.txt", args["cmd"])
        self.assertIn("+ok", args["cmd"])

    def test_does_not_treat_model_name_as_filename(self):
        calls = _synthesize_codex_tool_call(
            ["exec_command"],
            "[system]: Available models include gpt-5.5.\n\n[user]: 帮我改一下项目结构",
            "",
        )

        self.assertEqual(calls, [])

    def test_rewrites_simple_echo_write_to_apply_patch(self):
        call = ParsedToolCall.make("exec_command", {"cmd": "echo 'ok' > probe.txt"})

        normalized = _normalize_codex_tool_calls(
            [call],
            tool_names=["exec_command"],
            message="[user]: 请创建文件 probe.txt，内容为 ok",
        )

        self.assertEqual(len(normalized), 1)
        cmd = json.loads(normalized[0].arguments)["cmd"]
        self.assertIn("apply_patch <<'PATCH'", cmd)
        self.assertIn("*** Add File: probe.txt", cmd)
        self.assertIn("+ok", cmd)

    def test_suppresses_duplicate_completed_apply_patch(self):
        patch_cmd = "\n".join(
            [
                "apply_patch <<'PATCH'",
                "*** Begin Patch",
                "*** Add File: probe.txt",
                "+ok",
                "*** End Patch",
                "PATCH",
            ]
        )
        call = ParsedToolCall.make("exec_command", {"cmd": patch_cmd})

        normalized = _normalize_codex_tool_calls(
            [call],
            tool_names=["exec_command"],
            message="[tool result]:\npatch: completed\n/abs/probe.txt\n*** Add File: probe.txt",
        )

        self.assertEqual(normalized, [])

    def test_synthesizes_desktop_listing_despite_injected_tool_text(self):
        message = (
            "[system]: AVAILABLE TOOLS:\n"
            "function_call_output session_id patch: completed\n\n"
            "[user]: 帮我看看我的桌面文件有哪些"
        )

        calls = _synthesize_codex_tool_call(["exec_command"], message, "")

        self.assertEqual(len(calls), 1)
        self.assertEqual(
            json.loads(calls[0].arguments),
            {"cmd": "ls -la ~/Desktop | sed -n '1,40p'"},
        )

    def test_does_not_repeat_desktop_listing_after_command_output(self):
        message = (
            "[user]: 帮我看看我的桌面文件有哪些\n\n"
            "[tool result]: command_execution exit_code=0 aggregated_output='total 77944'"
        )

        calls = _synthesize_codex_tool_call(["exec_command"], message, "")

        self.assertEqual(calls, [])


if __name__ == "__main__":
    unittest.main()
