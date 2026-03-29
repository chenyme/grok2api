import asyncio

import orjson

from app.services.grok.services.chat import CollectProcessor, StreamProcessor
from app.services.grok.services.responses import ResponsesService


def _json_line(payload: dict) -> bytes:
    return orjson.dumps(payload)


async def _iter_lines(lines):
    for line in lines:
        yield line


def _decode_sse_json(chunk: str) -> dict:
    assert chunk.startswith("data: ")
    return orjson.loads(chunk[6:])


def test_collect_processor_returns_estimated_usage(monkeypatch):
    monkeypatch.setattr(
        "app.services.grok.services.chat.get_config",
        lambda key, default=None: 0 if key == "chat.stream_timeout" else [],
    )

    async def _run():
        processor = CollectProcessor("grok-4", prompt_tokens=17)
        result = await processor.process(
            _iter_lines(
                [
                    _json_line(
                        {
                            "result": {
                                "response": {
                                    "llmInfo": {"modelHash": "fp_test"},
                                    "modelResponse": {
                                        "responseId": "resp_collect",
                                        "message": "你好，世界",
                                    },
                                }
                            }
                        }
                    )
                ]
            )
        )
        assert result["usage"]["prompt_tokens"] == 17
        assert result["usage"]["completion_tokens"] > 0
        assert (
            result["usage"]["total_tokens"]
            == result["usage"]["prompt_tokens"] + result["usage"]["completion_tokens"]
        )

    asyncio.run(_run())


def test_stream_processor_final_chunk_has_usage(monkeypatch):
    monkeypatch.setattr(
        "app.services.grok.services.chat.get_config",
        lambda key, default=None: 0 if key == "chat.stream_timeout" else [],
    )

    async def _run():
        processor = StreamProcessor("grok-4", prompt_tokens=11)
        chunks = []
        async for chunk in processor.process(
            _iter_lines(
                [
                    _json_line(
                        {
                            "result": {
                                "response": {
                                    "responseId": "resp_stream",
                                    "llmInfo": {"modelHash": "fp_test"},
                                    "token": "Hello",
                                }
                            }
                        }
                    ),
                    _json_line(
                        {
                            "result": {
                                "response": {
                                    "responseId": "resp_stream",
                                    "token": " world",
                                }
                            }
                        }
                    ),
                ]
            )
        ):
            chunks.append(chunk)

        assert chunks[-1] == "data: [DONE]\n\n"
        final_payload = _decode_sse_json(chunks[-2])
        assert final_payload["choices"][0]["finish_reason"] == "stop"
        assert final_payload["usage"]["prompt_tokens"] == 11
        assert final_payload["usage"]["completion_tokens"] > 0
        assert (
            final_payload["usage"]["total_tokens"]
            == final_payload["usage"]["prompt_tokens"]
            + final_payload["usage"]["completion_tokens"]
        )

    asyncio.run(_run())


def test_stream_processor_prefers_final_image_over_preview(monkeypatch):
    monkeypatch.setattr(
        "app.services.grok.services.chat.get_config",
        lambda key, default=None: 0 if key == "chat.stream_timeout" else [],
    )

    class DummyDownloadService:
        async def render_image(self, url, token, image_id="image"):
            return f"![{image_id}]({url})"

    preview = (
        "users/demo/generated/11111111-2222-3333-4444-555555555555-part-0/image.jpg"
    )
    original = (
        "https://assets.grok.com/users/demo/generated/"
        "11111111-2222-3333-4444-555555555555/image.jpg"
    )

    monkeypatch.setattr(
        StreamProcessor,
        "_get_dl",
        lambda self: DummyDownloadService(),
    )

    async def _run():
        processor = StreamProcessor("grok-4.20-beta", prompt_tokens=9, show_think=True)
        chunks = []
        async for chunk in processor.process(
            _iter_lines(
                [
                    _json_line(
                        {
                            "result": {
                                "response": {
                                    "responseId": "resp_stream_image",
                                    "streamingImageGenerationResponse": {
                                        "imageIndex": 0,
                                        "progress": 42,
                                    },
                                }
                            }
                        }
                    ),
                    _json_line(
                        {
                            "result": {
                                "response": {
                                    "responseId": "resp_stream_image",
                                    "modelResponse": {
                                        "cardAttachmentsJson": [
                                            orjson.dumps(
                                                {
                                                    "id": "card-demo",
                                                    "image_chunk": {
                                                        "imageUrl": preview
                                                    },
                                                }
                                            ).decode()
                                        ]
                                    },
                                }
                            }
                        }
                    ),
                    _json_line(
                        {
                            "result": {
                                "response": {
                                    "responseId": "resp_stream_image",
                                    "cardAttachment": {
                                        "jsonData": orjson.dumps(
                                            {
                                                "id": "card-demo",
                                                "image": {"original": original},
                                            }
                                        ).decode()
                                    },
                                }
                            }
                        }
                    ),
                ]
            )
        ):
            chunks.append(chunk)

        combined = "".join(chunks)
        assert preview not in combined
        assert combined.count(original) == 1

    asyncio.run(_run())


def test_stream_processor_falls_back_to_preview_when_final_missing(monkeypatch):
    monkeypatch.setattr(
        "app.services.grok.services.chat.get_config",
        lambda key, default=None: 0 if key == "chat.stream_timeout" else [],
    )

    class DummyDownloadService:
        async def render_image(self, url, token, image_id="image"):
            return f"![{image_id}]({url})"

    preview = (
        "users/demo/generated/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee-part-0/image.jpg"
    )

    monkeypatch.setattr(
        StreamProcessor,
        "_get_dl",
        lambda self: DummyDownloadService(),
    )

    async def _run():
        processor = StreamProcessor("grok-4.20-beta", prompt_tokens=7, show_think=True)
        chunks = []
        async for chunk in processor.process(
            _iter_lines(
                [
                    _json_line(
                        {
                            "result": {
                                "response": {
                                    "responseId": "resp_stream_preview",
                                    "modelResponse": {
                                        "cardAttachmentsJson": [
                                            orjson.dumps(
                                                {
                                                    "id": "card-demo",
                                                    "image_chunk": {
                                                        "imageUrl": preview
                                                    },
                                                }
                                            ).decode()
                                        ]
                                    },
                                }
                            }
                        }
                    )
                ]
            )
        ):
            chunks.append(chunk)

        combined = "".join(chunks)
        assert combined.count(preview) == 1

    asyncio.run(_run())


def test_collect_processor_image_result_includes_share_fields(monkeypatch):
    monkeypatch.setattr(
        "app.services.grok.services.chat.get_config",
        lambda key, default=None: 0 if key == "chat.stream_timeout" else [],
    )

    class DummyDownloadService:
        async def render_image(self, url, token, image_id="image"):
            return f"![{image_id}]({url})"

    original = (
        "https://assets.grok.com/users/demo/generated/"
        "11111111-2222-3333-4444-555555555555/image.jpg"
    )

    monkeypatch.setattr(
        CollectProcessor,
        "_get_dl",
        lambda self: DummyDownloadService(),
    )
    monkeypatch.setattr(
        "app.services.grok.services.chat._create_chat_share_link",
        lambda token, conversation_id, response_id: asyncio.sleep(0, result="https://grok.com/share/demo-share"),
    )
    monkeypatch.setattr(
        "app.services.grok.services.chat._resolve_share_image_details",
        lambda share_url: asyncio.sleep(
            0,
            result=(
                "https://grok.com/share/demo-share/opengraph-image/demo-share?cache=1",
                "og:image",
                "",
            ),
        ),
    )

    async def _run():
        processor = CollectProcessor("grok-4.20-beta", token="token-demo", prompt_tokens=9)
        result = await processor.process(
            _iter_lines(
                [
                    _json_line(
                        {
                            "result": {
                                "conversationId": "conv-share",
                                "response": {
                                    "modelResponse": {
                                        "responseId": "resp-share",
                                        "message": "",
                                        "cardAttachmentsJson": [
                                            orjson.dumps(
                                                {
                                                    "id": "card-demo",
                                                    "image": {"original": original},
                                                }
                                            ).decode()
                                        ],
                                    }
                                },
                            }
                        }
                    )
                ]
            )
        )
        assert result["share_url"] == "https://grok.com/share/demo-share"
        assert (
            result["share_image_url"]
            == "https://grok.com/share/demo-share/opengraph-image/demo-share?cache=1"
        )
        assert result["share_image_source"] == "og:image"

    asyncio.run(_run())


def test_responses_stream_completed_event_uses_chat_usage(monkeypatch):
    async def fake_chat_completions(**kwargs):
        async def _gen():
            yield (
                'data: {"id":"chatcmpl_test","object":"chat.completion.chunk","created":1,'
                '"model":"grok-4","choices":[{"index":0,"delta":{"role":"assistant","content":""},'
                '"logprobs":null,"finish_reason":null}]}\n\n'
            )
            yield (
                'data: {"id":"chatcmpl_test","object":"chat.completion.chunk","created":1,'
                '"model":"grok-4","choices":[{"index":0,"delta":{"content":"Hello"},'
                '"logprobs":null,"finish_reason":null}]}\n\n'
            )
            yield (
                'data: {"id":"chatcmpl_test","object":"chat.completion.chunk","created":1,'
                '"model":"grok-4","choices":[{"index":0,"delta":{},'
                '"logprobs":null,"finish_reason":"stop"}],'
                '"usage":{"prompt_tokens":13,"completion_tokens":5,"total_tokens":18,'
                '"prompt_tokens_details":{"cached_tokens":0,"text_tokens":13,"audio_tokens":0,"image_tokens":0},'
                '"completion_tokens_details":{"text_tokens":5,"audio_tokens":0,"reasoning_tokens":0}}}\n\n'
            )
            yield "data: [DONE]\n\n"

        return _gen()

    monkeypatch.setattr(
        "app.services.grok.services.responses.ChatService.completions",
        fake_chat_completions,
    )

    async def _run():
        stream = await ResponsesService.create(
            model="grok-4",
            input_value="hi",
            stream=True,
        )
        chunks = []
        async for chunk in stream:
            chunks.append(chunk)

        completed_chunk = next(
            chunk for chunk in reversed(chunks) if "response.completed" in chunk
        )
        completed = orjson.loads(completed_chunk.split("data: ", 1)[1])
        usage = completed["response"]["usage"]
        assert usage["input_tokens"] == 13
        assert usage["output_tokens"] == 5
        assert usage["total_tokens"] == 18

    asyncio.run(_run())
