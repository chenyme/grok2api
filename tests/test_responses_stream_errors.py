import asyncio
import unittest

import orjson

from app.platform.errors import UpstreamError
from app.products.openai._format import format_sse, make_resp_object
from app.products.openai.router import _safe_sse_responses


def _event_names(chunks: list[str]) -> list[str]:
    names = []
    for chunk in chunks:
        for line in chunk.splitlines():
            if line.startswith("event: "):
                names.append(line.removeprefix("event: "))
    return names


def _event_payload(chunks: list[str], event_name: str) -> dict:
    for chunk in chunks:
        if f"event: {event_name}" not in chunk:
            continue
        for line in chunk.splitlines():
            if line.startswith("data: "):
                return orjson.loads(line.removeprefix("data: "))
    raise AssertionError(f"missing event payload for {event_name}")


class ResponsesStreamErrorTests(unittest.TestCase):
    def test_safe_sse_responses_emits_failed_terminal_event_on_stream_error(self):
        async def broken_stream():
            yield format_sse(
                "response.created",
                {
                    "type": "response.created",
                    "response": make_resp_object("resp_test", "grok-test", "in_progress", []),
                },
            )
            raise UpstreamError("upstream closed", status=502, body="bad gateway")

        async def collect():
            return [chunk async for chunk in _safe_sse_responses(broken_stream())]

        chunks = asyncio.run(collect())
        names = _event_names(chunks)

        self.assertIn("response.created", names)
        self.assertIn("response.failed", names)
        self.assertLess(names.index("response.created"), names.index("response.failed"))
        self.assertEqual(chunks[-1], "data: [DONE]\n\n")

        payload = _event_payload(chunks, "response.failed")
        self.assertEqual(payload["type"], "response.failed")
        self.assertEqual(payload["response"]["id"], "resp_test")
        self.assertEqual(payload["response"]["status"], "failed")
        self.assertEqual(payload["response"]["error"]["message"], "upstream closed")
