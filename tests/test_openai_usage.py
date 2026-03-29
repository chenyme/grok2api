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


def test_collect_processor_extracts_web_search_citations(monkeypatch):
    monkeypatch.setattr(
        "app.services.grok.services.chat.get_config",
        lambda key, default=None: 0 if key == "chat.stream_timeout" else [],
    )

    async def _run():
        processor = CollectProcessor("grok-4", prompt_tokens=7)
        result = await processor.process(
            _iter_lines(
                [
                    _json_line(
                        {
                            "result": {
                                "response": {
                                    "modelResponse": {
                                        "responseId": "resp_collect_citations",
                                        "message": "这里是答案",
                                        "webSearchResults": [
                                            {
                                                "url": "https://example.com/a",
                                                "title": "Example A",
                                                "preview": "Preview A",
                                            },
                                            {
                                                "url": "https://example.com/b",
                                                "title": "Example B",
                                                "preview": "Preview B",
                                            },
                                        ],
                                        "xposts": [
                                            {
                                                "username": "alice",
                                                "name": "Alice",
                                                "text": "Hello from X",
                                                "createTime": "2026-03-29T16:04:00Z",
                                                "profileImageUrl": "https://example.com/alice.jpg",
                                                "postId": "12345",
                                                "communityNote": "",
                                            }
                                        ],
                                    }
                                }
                            }
                        }
                    )
                ]
            )
        )
        assert result["citations"] == [
            {
                "type": "web_page",
                "url": "https://example.com/a",
                "title": "Example A",
                "preview": "Preview A",
            },
            {
                "type": "web_page",
                "url": "https://example.com/b",
                "title": "Example B",
                "preview": "Preview B",
            },
            {
                "type": "x_post",
                "url": "https://x.com/alice/status/12345",
                "post_id": "12345",
                "username": "alice",
                "name": "Alice",
                "title": "Alice (@alice)",
                "text": "Hello from X",
                "preview": "Hello from X",
                "create_time": "2026-03-29T16:04:00Z",
                "profile_image_url": "https://example.com/alice.jpg",
            },
        ]

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


def test_stream_processor_final_chunk_includes_citations(monkeypatch):
    monkeypatch.setattr(
        "app.services.grok.services.chat.get_config",
        lambda key, default=None: 0 if key == "chat.stream_timeout" else [],
    )

    async def _run():
        processor = StreamProcessor("grok-4", prompt_tokens=5)
        chunks = []
        async for chunk in processor.process(
            _iter_lines(
                [
                    _json_line(
                        {
                            "result": {
                                "response": {
                                    "responseId": "resp_stream_citations",
                                    "token": "hello",
                                }
                            }
                        }
                    ),
                    _json_line(
                        {
                            "result": {
                                "response": {
                                    "modelResponse": {
                                        "responseId": "resp_stream_citations",
                                        "message": "hello",
                                        "webSearchResults": [
                                            {
                                                "url": "https://example.com/search",
                                                "title": "Search Result",
                                                "preview": "Search Preview",
                                            }
                                        ],
                                    }
                                }
                            }
                        }
                    ),
                ]
            )
        ):
            chunks.append(chunk)

        final_payload = _decode_sse_json(chunks[-2])
        assert final_payload["citations"] == [
            {
                "type": "web_page",
                "url": "https://example.com/search",
                "title": "Search Result",
                "preview": "Search Preview",
            }
        ]

    asyncio.run(_run())


def test_responses_non_stream_includes_chat_citations(monkeypatch):
    async def fake_chat_completions(**kwargs):
        return {
            "id": "chatcmpl_test",
            "object": "chat.completion",
            "created": 1,
            "model": "grok-4",
            "citations": [
                {
                    "url": "https://example.com/citation",
                    "title": "Citation",
                    "preview": "Citation Preview",
                }
            ],
            "choices": [
                {
                    "index": 0,
                    "message": {"role": "assistant", "content": "Hello"},
                    "finish_reason": "stop",
                }
            ],
            "usage": {
                "prompt_tokens": 3,
                "completion_tokens": 2,
                "total_tokens": 5,
                "prompt_tokens_details": {
                    "cached_tokens": 0,
                    "text_tokens": 3,
                    "audio_tokens": 0,
                    "image_tokens": 0,
                },
                "completion_tokens_details": {
                    "text_tokens": 2,
                    "audio_tokens": 0,
                    "reasoning_tokens": 0,
                },
            },
        }

    monkeypatch.setattr(
        "app.services.grok.services.responses.ChatService.completions",
        fake_chat_completions,
    )

    async def _run():
        response = await ResponsesService.create(
            model="grok-4",
            input_value="hi",
            stream=False,
        )
        assert response["citations"] == [
            {
                "type": "web_page",
                "url": "https://example.com/citation",
                "title": "Citation",
                "preview": "Citation Preview",
            }
        ]

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
                '"citations":[{"url":"https://example.com/citation","title":"Citation","preview":"Citation Preview"}],'
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
        assert completed["response"]["citations"] == [
            {
                "type": "web_page",
                "url": "https://example.com/citation",
                "title": "Citation",
                "preview": "Citation Preview",
            }
        ]

    asyncio.run(_run())
