import unittest
import asyncio
import importlib
import re
import tempfile
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import orjson

from app.control.model.enums import Capability, ModeId, Tier
from app.control.model.spec import ModelSpec
from app.products.openai.images import _normalize_response_format
from app.products.openai.chat import _extract_message, _inline_generated_image_id
from app.products.openai.schemas import ChatCompletionRequest, ImageConfig, MessageItem


class ImageResponseFormatTests(unittest.TestCase):
    def test_local_url_is_accepted_for_webui_proxy_output(self):
        self.assertEqual(_normalize_response_format("local_url"), "local_url")

    def test_inline_generated_image_id_matches_local_image_route(self):
        file_id = _inline_generated_image_id(
            "https://assets.grok.com/users/u/generated/i/image.jpg"
        )

        self.assertRegex(file_id, re.compile(r"^[0-9a-f\-]{16,36}$"))

    def test_local_image_route_serves_legacy_inline_generated_image_id(self):
        async def run_case():
            router_module = importlib.import_module("app.products.openai.router")

            legacy_id = "inline-d4d42c540c607db8dc13a2f2"
            with tempfile.TemporaryDirectory() as tmp:
                img_dir = Path(tmp)
                (img_dir / f"{legacy_id}.jpg").write_bytes(b"image-bytes")
                with patch.object(router_module, "image_files_dir", return_value=img_dir):
                    response = await router_module.serve_image(legacy_id)

            self.assertEqual(response.media_type, "image/jpeg")

        asyncio.run(run_case())

    def test_chat_history_strips_assistant_image_markdown_before_upstream(self):
        message, files = _extract_message(
            [
                {"role": "user", "content": "画一张图"},
                {
                    "role": "assistant",
                    "content": "![image](/v1/files/image?id=abc123)",
                },
                {"role": "user", "content": "你好"},
            ]
        )

        self.assertEqual(files, [])
        self.assertIn("[user]: 画一张图", message)
        self.assertIn("[user]: 你好", message)
        self.assertNotIn("/v1/files/image", message)
        self.assertNotIn("![image]", message)

    def test_chat_history_strips_assistant_inline_generated_image_url(self):
        message, files = _extract_message(
            [
                {"role": "user", "content": "画一张图"},
                {
                    "role": "assistant",
                    "content": "完成了 https://assets.grok.com/users/u/generated/i/image.jpg",
                },
                {"role": "user", "content": "你好"},
            ]
        )

        self.assertEqual(files, [])
        self.assertIn("[user]: 你好", message)
        self.assertNotIn("assets.grok.com", message)
        self.assertNotIn("image.jpg", message)

    def test_chat_endpoint_passes_image_format_override_to_chat_service(self):
        async def run_case():
            router_module = importlib.import_module("app.products.openai.router")

            spec = ModelSpec(
                "grok-4.3-latest",
                ModeId.GROK_4_3,
                Tier.BASIC,
                Capability.CHAT,
                True,
                "Grok 4.3 Latest",
                upstream_model_name="grok-4.3-latest",
            )
            captured = {}

            async def fake_chat_completions(**kwargs):
                captured.update(kwargs)
                return {
                    "id": "chatcmpl-test",
                    "object": "chat.completion",
                    "choices": [{"message": {"role": "assistant", "content": "ok"}}],
                }

            with (
                patch.object(router_module.model_registry, "get", return_value=spec),
                patch.object(router_module, "chat_completions", fake_chat_completions),
            ):
                response = await router_module.chat_completions_endpoint(
                    ChatCompletionRequest(
                        model="grok-4.3-latest",
                        messages=[MessageItem(role="user", content="画一张图")],
                        image_config=ImageConfig(response_format="local_md"),
                    )
                )

            self.assertEqual(response.status_code, 200)
            self.assertEqual(captured["image_format"], "local_md")

        asyncio.run(run_case())

    def test_manual_direct_chat_localizes_inline_generated_image_markdown(self):
        async def run_case():
            from app.products.openai import chat
            from app.dataplane import account as account_module

            class FakeConfig:
                def get_bool(self, key, default=False):
                    values = {
                        "features.stream": False,
                        "features.thinking": False,
                    }
                    return values.get(key, default)

                def get_float(self, key, default=0.0):
                    return default

                def get_str(self, key, default=""):
                    values = {
                        "features.image_format": "grok_url",
                        "app.app_url": "",
                    }
                    return values.get(key, default)

                def get(self, key, default=None):
                    return default

            class FakeDirectory:
                async def release(self, acct):
                    return None

                async def feedback(self, token, kind, mode_id, now_s_val=None):
                    return None

            async def fake_stream_chat(**kwargs):
                payload = {
                    "result": {
                        "response": {
                            "messageTag": "final",
                            "token": (
                                "已生成\n"
                                "![image](https://assets.grok.com/users/u/generated/i/image.jpg)"
                            ),
                            "isThinking": False,
                        }
                    }
                }
                yield "data: " + orjson.dumps(payload).decode()
                yield "data: [DONE]"

            spec = ModelSpec(
                "grok-4.3-latest",
                ModeId.GROK_4_3,
                Tier.BASIC,
                Capability.CHAT,
                True,
                "Grok 4.3 Latest",
                upstream_model_name="grok-4.3-latest",
            )

            account_module._directory = FakeDirectory()
            try:
                with (
                    patch.object(chat, "get_config", return_value=FakeConfig()),
                    patch.object(chat, "resolve_model", return_value=spec),
                    patch.object(
                        chat,
                        "reserve_account",
                        return_value=(SimpleNamespace(token="token-a"), ModeId.GROK_4_3),
                    ),
                    patch.object(chat, "_stream_chat", fake_stream_chat),
                    patch.object(
                        chat,
                        "_download_image_bytes",
                        return_value=(b"image-bytes", "image/jpeg"),
                    ),
                    patch.object(chat, "_save_image", return_value="local-123"),
                    patch.object(chat, "_quota_sync", return_value=None),
                    patch.object(chat, "_fail_sync", return_value=None),
                ):
                    response = await chat.completions(
                        model="grok-4.3-latest",
                        messages=[{"role": "user", "content": "画一个日系的"}],
                        stream=False,
                    )
            finally:
                account_module._directory = None

            content = response["choices"][0]["message"]["content"]
            self.assertIn("![image](/v1/files/image?id=local-123)", content)
            self.assertNotIn("assets.grok.com", content)

        asyncio.run(run_case())

    def test_manual_direct_stream_buffers_and_localizes_inline_generated_image(self):
        async def run_case():
            from app.products.openai import chat
            from app.dataplane import account as account_module

            class FakeConfig:
                def get_bool(self, key, default=False):
                    values = {
                        "features.stream": True,
                        "features.thinking": False,
                    }
                    return values.get(key, default)

                def get_float(self, key, default=0.0):
                    return default

                def get_str(self, key, default=""):
                    values = {
                        "features.image_format": "grok_url",
                        "app.app_url": "",
                    }
                    return values.get(key, default)

                def get(self, key, default=None):
                    return default

            class FakeDirectory:
                async def release(self, acct):
                    return None

                async def feedback(self, token, kind, mode_id, now_s_val=None):
                    return None

            async def fake_stream_chat(**kwargs):
                for token in [
                    "已生成\n",
                    "![image](https://assets.grok.com/users/u/generated/i/image.jpg)",
                ]:
                    payload = {
                        "result": {
                            "response": {
                                "messageTag": "final",
                                "token": token,
                                "isThinking": False,
                            }
                        }
                    }
                    yield "data: " + orjson.dumps(payload).decode()
                yield "data: [DONE]"

            spec = ModelSpec(
                "grok-4.3-latest",
                ModeId.GROK_4_3,
                Tier.BASIC,
                Capability.CHAT,
                True,
                "Grok 4.3 Latest",
                upstream_model_name="grok-4.3-latest",
            )

            account_module._directory = FakeDirectory()
            try:
                with (
                    patch.object(chat, "get_config", return_value=FakeConfig()),
                    patch.object(chat, "resolve_model", return_value=spec),
                    patch.object(
                        chat,
                        "reserve_account",
                        return_value=(SimpleNamespace(token="token-a"), ModeId.GROK_4_3),
                    ),
                    patch.object(chat, "_stream_chat", fake_stream_chat),
                    patch.object(
                        chat,
                        "_download_image_bytes",
                        return_value=(b"image-bytes", "image/jpeg"),
                    ),
                    patch.object(chat, "_save_image", return_value="local-123"),
                    patch.object(chat, "_quota_sync", return_value=None),
                    patch.object(chat, "_fail_sync", return_value=None),
                ):
                    stream = await chat.completions(
                        model="grok-4.3-latest",
                        messages=[{"role": "user", "content": "画一个日系的"}],
                        stream=True,
                    )
                    chunks = []
                    async for line in stream:
                        if not line.startswith("data: ") or "[DONE]" in line:
                            continue
                        chunks.append(orjson.loads(line[6:]))
            finally:
                account_module._directory = None

            content = "".join(
                chunk["choices"][0]["delta"].get("content", "") for chunk in chunks
            )
            self.assertIn("![image](/v1/files/image?id=local-123)", content)
            self.assertNotIn("assets.grok.com", content)

        asyncio.run(run_case())

    def test_builtin_chat_does_not_localize_inline_generated_image_markdown(self):
        async def run_case():
            from app.products.openai import chat
            from app.dataplane import account as account_module

            class FakeConfig:
                def get_bool(self, key, default=False):
                    values = {
                        "features.stream": False,
                        "features.thinking": False,
                    }
                    return values.get(key, default)

                def get_float(self, key, default=0.0):
                    return default

                def get_str(self, key, default=""):
                    return default

                def get(self, key, default=None):
                    return default

            class FakeDirectory:
                async def release(self, acct):
                    return None

                async def feedback(self, token, kind, mode_id, now_s_val=None):
                    return None

            async def fake_stream_chat(**kwargs):
                payload = {
                    "result": {
                        "response": {
                            "messageTag": "final",
                            "token": (
                                "已生成\n"
                                "![image](https://assets.grok.com/users/u/generated/i/image.jpg)"
                            ),
                            "isThinking": False,
                        }
                    }
                }
                yield "data: " + orjson.dumps(payload).decode()
                yield "data: [DONE]"

            spec = ModelSpec(
                "grok-4.3-beta",
                ModeId.GROK_4_3,
                Tier.BASIC,
                Capability.CHAT,
                True,
                "Grok 4.3 Beta",
            )

            account_module._directory = FakeDirectory()
            try:
                with (
                    patch.object(chat, "get_config", return_value=FakeConfig()),
                    patch.object(chat, "resolve_model", return_value=spec),
                    patch.object(
                        chat,
                        "reserve_account",
                        return_value=(SimpleNamespace(token="token-a"), ModeId.GROK_4_3),
                    ),
                    patch.object(chat, "_stream_chat", fake_stream_chat),
                    patch.object(chat, "_download_image_bytes") as download_image,
                    patch.object(chat, "_quota_sync", return_value=None),
                    patch.object(chat, "_fail_sync", return_value=None),
                ):
                    response = await chat.completions(
                        model="grok-4.3-beta",
                        messages=[{"role": "user", "content": "画一个日系的"}],
                        stream=False,
                    )
            finally:
                account_module._directory = None

            content = response["choices"][0]["message"]["content"]
            self.assertIn("assets.grok.com", content)
            self.assertIn("![image](https://assets.grok.com", content)
            download_image.assert_not_called()

        asyncio.run(run_case())


if __name__ == "__main__":
    unittest.main()
