import asyncio

import orjson
from fastapi import FastAPI
from fastapi.testclient import TestClient

from app.api.v1.chat import router as chat_router
from app.core.config import config
from app.services.grok.model import ModelService
from app.services.grok.processors.video_processors import (
    VideoCollectProcessor,
    VideoStreamProcessor,
    _normalize_progress,
)
from app.services.grok.chat import MessageExtractor
from app.services.grok.media import VideoService


def _make_chat_app() -> FastAPI:
    app = FastAPI()
    app.include_router(chat_router)
    return app


def test_chat_video_allows_text_to_video(monkeypatch):
    async def fake_video_completions(**kwargs):
        assert kwargs.get("model") == "grok-imagine-1.0-video"
        assert isinstance(kwargs.get("messages"), list)
        return {
            "id": "chatcmpl-video-test",
            "object": "chat.completion",
            "created": 0,
            "model": "grok-imagine-1.0-video",
            "choices": [
                {
                    "index": 0,
                    "message": {"role": "assistant", "content": "ok"},
                    "finish_reason": "stop",
                }
            ],
            "usage": {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
        }

    monkeypatch.setattr(
        "app.services.grok.services.media.VideoService.completions",
        fake_video_completions,
    )

    client = TestClient(_make_chat_app())
    response = client.post(
        "/chat/completions",
        json={
            "model": "grok-imagine-1.0-video",
            "stream": False,
            "messages": [{"role": "user", "content": "make a sci-fi trailer"}],
        },
    )

    assert response.status_code == 200
    payload = response.json()
    assert payload["choices"][0]["message"]["content"] == "ok"


def test_video_collect_url_mode_returns_only_url(monkeypatch):
    cfg_backup = config._config.copy()
    try:
        config._config = {
            "app": {"video_format": "url", "app_url": ""},
            "timeout": {"video_idle_timeout": 30},
        }

        async def fake_process_url(
            self,
            path: str,
            media_type: str = "video",
            strict_media: bool = False,
        ) -> str:
            return path

        monkeypatch.setattr(VideoCollectProcessor, "process_url", fake_process_url)

        class _FakeResponse:
            def __aiter__(self):
                async def _generator():
                    event = {
                        "result": {
                            "response": {
                                "responseId": "resp-1",
                                "streamingVideoGenerationResponse": {
                                    "progress": 100,
                                    "videoUrl": "https://assets.grok.com/video/test.mp4",
                                    "thumbnailImageUrl": "https://assets.grok.com/image/test.jpg",
                                },
                            }
                        }
                    }
                    yield f"data: {orjson.dumps(event).decode()}\n\n".encode()

                return _generator()

        processor = VideoCollectProcessor("grok-imagine-1.0-video", token="tok")
        result = asyncio.run(processor.process(_FakeResponse()))
        content = result["choices"][0]["message"]["content"]

        assert content == "https://assets.grok.com/video/test.mp4"
    finally:
        config._config = cfg_backup


def test_video_collect_supports_video_generation_response_payload(monkeypatch):
    cfg_backup = config._config.copy()
    try:
        config._config = {
            "app": {"video_format": "url", "app_url": ""},
            "timeout": {"video_idle_timeout": 30},
        }

        def fake_schedule(*args, **kwargs):
            pass

        monkeypatch.setattr(
            "app.services.grok.processors.video_processors._schedule_video_cache_warm",
            fake_schedule,
        )

        class _FakeResponse:
            def __aiter__(self):
                async def _generator():
                    event = {
                        "result": {
                            "response": {
                                "responseId": "resp-2",
                                "videoGenerationResponse": {
                                    "status": "completed",
                                    "videoUrls": ["https://assets.grok.com/video/alt.mp4"],
                                    "thumbnailUrl": "https://assets.grok.com/image/alt.jpg",
                                },
                            }
                        }
                    }
                    yield f"data: {orjson.dumps(event).decode()}\n\n".encode()

                return _generator()

        processor = VideoCollectProcessor("grok-imagine-1.0-video", token="tok")
        result = asyncio.run(processor.process(_FakeResponse()))
        content = result["choices"][0]["message"]["content"]

        assert "https://assets.grok.com/video/alt.mp4" in content
    finally:
        config._config = cfg_backup


def test_video_collect_finds_url_from_top_level_payload(monkeypatch):
    cfg_backup = config._config.copy()
    try:
        config._config = {
            "app": {"video_format": "url", "app_url": ""},
            "timeout": {"video_idle_timeout": 30},
        }

        def fake_schedule(*args, **kwargs):
            pass

        monkeypatch.setattr(
            "app.services.grok.processors.video_processors._schedule_video_cache_warm",
            fake_schedule,
        )

        class _FakeResponse:
            def __aiter__(self):
                async def _generator():
                    event = {
                        "result": {
                            "response": {
                                "responseId": "resp-3",
                                "streamingVideoGenerationResponse": {
                                    "progress": 100,
                                    "videoUrl": "https://assets.grok.com/video/from-top-level.mp4",
                                },
                            },
                        }
                    }
                    yield f"data: {orjson.dumps(event).decode()}\n\n".encode()

                return _generator()

        processor = VideoCollectProcessor("grok-imagine-1.0-video", token="tok")
        result = asyncio.run(processor.process(_FakeResponse()))
        content = result["choices"][0]["message"]["content"]

        assert "https://assets.grok.com/video/from-top-level.mp4" in content
    finally:
        config._config = cfg_backup


def test_video_collect_markdown_mode_returns_click_download(monkeypatch):
    cfg_backup = config._config.copy()
    try:
        config._config = {
            "app": {"video_format": "markdown", "app_url": ""},
            "timeout": {"video_idle_timeout": 30},
        }

        async def fake_process_url(
            self,
            path: str,
            media_type: str = "video",
            strict_media: bool = False,
        ) -> str:
            return path

        monkeypatch.setattr(VideoCollectProcessor, "process_url", fake_process_url)

        class _FakeResponse:
            def __aiter__(self):
                async def _generator():
                    event = {
                        "result": {
                            "response": {
                                "responseId": "resp-4",
                                "streamingVideoGenerationResponse": {
                                    "progress": 100,
                                    "videoUrl": "https://assets.grok.com/video/md.mp4",
                                },
                            }
                        }
                    }
                    yield f"data: {orjson.dumps(event).decode()}\n\n".encode()

                return _generator()

        processor = VideoCollectProcessor("grok-imagine-1.0-video", token="tok")
        result = asyncio.run(processor.process(_FakeResponse()))
        content = result["choices"][0]["message"]["content"]

        assert content == "🎬 视频已生成：[点击下载](https://assets.grok.com/video/md.mp4)\n"
    finally:
        config._config = cfg_backup


def test_video_collect_extracts_url_from_streaming_response(monkeypatch):
    cfg_backup = config._config.copy()
    try:
        config._config = {
            "app": {"video_format": "url", "app_url": ""},
            "timeout": {"video_idle_timeout": 30},
        }

        async def fake_process_url(
            self,
            path: str,
            media_type: str = "video",
            strict_media: bool = False,
        ) -> str:
            return path

        monkeypatch.setattr(VideoCollectProcessor, "process_url", fake_process_url)

        class _FakeResponse:
            def __aiter__(self):
                async def _generator():
                    event = {
                        "result": {
                            "response": {
                                "responseId": "resp-5",
                                "streamingVideoGenerationResponse": {
                                    "progress": 100,
                                    "videoUrl": "https://assets.grok.com/video/no-payload.mp4",
                                },
                            }
                        }
                    }
                    yield f"data: {orjson.dumps(event).decode()}\n\n".encode()

                return _generator()

        processor = VideoCollectProcessor("grok-imagine-1.0-video", token="tok")
        result = asyncio.run(processor.process(_FakeResponse()))
        content = result["choices"][0]["message"]["content"]

        assert "https://assets.grok.com/video/no-payload.mp4" in content
    finally:
        config._config = cfg_backup


def test_video_collect_ignores_non_video_domain_url(monkeypatch):
    cfg_backup = config._config.copy()
    try:
        config._config = {
            "app": {"video_format": "url", "app_url": "http://127.0.0.1:8000"},
            "timeout": {"video_idle_timeout": 30, "video_result_wait_timeout": 0},
        }

        async def fake_process_url(
            self,
            path: str,
            media_type: str = "video",
            strict_media: bool = False,
        ) -> str:
            return path

        monkeypatch.setattr(VideoCollectProcessor, "process_url", fake_process_url)

        class _FakeResponse:
            def __aiter__(self):
                async def _generator():
                    event = {
                        "result": {
                            "response": {
                                "responseId": "resp-6",
                                "status": "completed",
                                "url": "https://example.com",
                            }
                        }
                    }
                    yield f"data: {orjson.dumps(event).decode()}\n\n".encode()

                return _generator()

        processor = VideoCollectProcessor("grok-imagine-1.0-video", token="tok")
        result = asyncio.run(processor.process(_FakeResponse()))
        content = result["choices"][0]["message"]["content"]

        assert "未返回可用下载链接" in content
    finally:
        config._config = cfg_backup


def test_video_process_url_keeps_non_assets_absolute_url():
    cfg_backup = config._config.copy()
    try:
        config._config = {
            "app": {"video_format": "url", "app_url": "http://127.0.0.1:8000"},
        }

        processor = VideoCollectProcessor("grok-imagine-1.0-video", token="tok")
        result = asyncio.run(processor.process_url("https://example.com", "video"))
        assert result == "https://example.com"
    finally:
        config._config = cfg_backup


def test_model_service_contains_image_edit_model():
    edit_model = ModelService.get("grok-imagine-1.0-edit")
    assert edit_model is not None
    assert edit_model.is_image is True


def test_message_extractor_image_generation_prefix():
    message, attachments = MessageExtractor.extract(
        [
            {"role": "system", "content": "你是图像助手"},
            {"role": "user", "content": "古风美女"},
        ],
        is_image=True,
    )
    assert message == "Image Generation:古风美女"
    assert attachments == []


def test_message_extractor_image_edit_prefix_with_attachment():
    message, attachments = MessageExtractor.extract(
        [
            {
                "role": "user",
                "content": [
                    {"type": "text", "text": "把图片变成水彩风"},
                    {
                        "type": "image_url",
                        "image_url": {"url": "https://example.com/a.png"},
                    },
                ],
            }
        ],
        is_image=True,
    )
    assert message == "Image Edit:把图片变成水彩风"
    assert attachments == [("image", "https://example.com/a.png")]


def test_collect_image_urls_supports_markdown_asset_link():
    from app.services.grok.processors.base import _collect_image_urls

    payload = {
        "result": {
            "response": {
                "modelResponse": {
                    "message": "生成完成: ![img](https://assets.grok.com/users/u/abc/content)",
                }
            }
        }
    }
    urls = _collect_image_urls(payload)
    assert "https://assets.grok.com/users/u/abc/content" in urls


def test_video_stream_think_progress_then_link(monkeypatch):
    cfg_backup = config._config.copy()
    try:
        config._config = {
            "app": {"video_format": "url", "app_url": ""},
            "timeout": {"video_idle_timeout": 30},
            "chat": {"thinking": True},
        }

        async def fake_process_url(
            self,
            path: str,
            media_type: str = "video",
            strict_media: bool = False,
        ) -> str:
            return path

        monkeypatch.setattr(VideoStreamProcessor, "process_url", fake_process_url)

        class _FakeResponse:
            def __aiter__(self):
                async def _generator():
                    ev1 = {
                        "result": {
                            "response": {
                                "responseId": "resp-think-1",
                                "streamingVideoGenerationResponse": {
                                    "progress": 42,
                                    "status": "processing",
                                },
                            }
                        }
                    }
                    ev2 = {
                        "result": {
                            "response": {
                                "responseId": "resp-think-1",
                                "streamingVideoGenerationResponse": {
                                    "progress": 100,
                                    "status": "completed",
                                    "videoUrl": "https://assets.grok.com/video/final.mp4",
                                },
                            }
                        }
                    }
                    yield f"data: {orjson.dumps(ev1).decode()}\n\n".encode()
                    yield f"data: {orjson.dumps(ev2).decode()}\n\n".encode()

                return _generator()

        async def _drain_text(generator):
            chunks = []
            async for chunk in generator:
                chunks.append(chunk)
            return "".join(chunks)

        processor = VideoStreamProcessor("grok-imagine-1.0-video", token="tok", think=True)
        text = asyncio.run(_drain_text(processor.process(_FakeResponse())))

        assert "<think>" in text
        assert "视频已生成42%" in text
        assert "</think>" in text
        assert "https://assets.grok.com/video/final.mp4" in text
        assert text.index("</think>") < text.index("https://assets.grok.com/video/final.mp4")
    finally:
        config._config = cfg_backup


def test_normalize_progress_supports_percent_string():
    assert _normalize_progress("50%") == 50.0
    assert _normalize_progress("  87.5 % ") == 87.5


def test_normalize_progress_integer_1_is_not_100():
    """Grok 发送 progress=1 表示 1%，不应被转换为 100%"""
    assert _normalize_progress(1) == 1.0
    assert _normalize_progress(1.0) == 1.0
    assert _normalize_progress(5) == 5.0
    assert _normalize_progress(100) == 100.0
    # 小数比例仍应转为百分比
    assert _normalize_progress(0.5) == 50.0
    assert _normalize_progress(0.01) == 1.0


def test_video_collect_accepts_assets_content_url(monkeypatch):
    cfg_backup = config._config.copy()
    try:
        config._config = {
            "app": {"video_format": "url", "app_url": ""},
            "timeout": {"video_idle_timeout": 30},
        }

        async def fake_process_url(
            self,
            path: str,
            media_type: str = "video",
            strict_media: bool = False,
        ) -> str:
            return path

        monkeypatch.setattr(VideoCollectProcessor, "process_url", fake_process_url)

        class _FakeResponse:
            def __aiter__(self):
                async def _generator():
                    event = {
                        "result": {
                            "response": {
                                "responseId": "resp-content-1",
                                "streamingVideoGenerationResponse": {
                                    "progress": 100,
                                    "status": "completed",
                                    "videoUrl": "https://assets.grok.com/users/u/generated/v001/content",
                                },
                            }
                        }
                    }
                    yield f"data: {orjson.dumps(event).decode()}\n\n".encode()

                return _generator()

        processor = VideoCollectProcessor("grok-imagine-1.0-video", token="tok")
        result = asyncio.run(processor.process(_FakeResponse()))
        content = result["choices"][0]["message"]["content"]

        assert "https://assets.grok.com/users/u/generated/v001/content" in content
    finally:
        config._config = cfg_backup


def test_video_collect_ignores_example_url_when_assets_url_exists(monkeypatch):
    cfg_backup = config._config.copy()
    try:
        config._config = {
            "app": {"video_format": "url", "app_url": ""},
            "timeout": {"video_idle_timeout": 30},
        }

        def fake_schedule(*args, **kwargs):
            pass

        monkeypatch.setattr(
            "app.services.grok.processors.video_processors._schedule_video_cache_warm",
            fake_schedule,
        )

        class _FakeResponse:
            def __aiter__(self):
                async def _generator():
                    event = {
                        "result": {
                            "response": {
                                "responseId": "resp-content-2",
                                "status": "completed",
                                "token": "示例链接 https://example.com/video.mp4",
                                "streamingVideoGenerationResponse": {
                                    "progress": 100,
                                    "status": "completed",
                                    "videoUrl": "https://assets.grok.com/users/u/generated/v-real/content",
                                },
                            },
                        }
                    }
                    yield f"data: {orjson.dumps(event).decode()}\n\n".encode()

                return _generator()

        processor = VideoCollectProcessor("grok-imagine-1.0-video", token="tok")
        result = asyncio.run(processor.process(_FakeResponse()))
        content = result["choices"][0]["message"]["content"]

        assert "https://assets.grok.com/users/u/generated/v-real/content" in content
    finally:
        config._config = cfg_backup


def test_video_stream_no_url_in_stream_emits_fallback(monkeypatch):
    """流中 progress=100 但无 videoUrl 时，直接返回兜底提示（不再轮询）"""
    cfg_backup = config._config.copy()
    try:
        config._config = {
            "app": {"video_format": "url", "app_url": ""},
            "timeout": {
                "video_idle_timeout": 30,
            },
            "chat": {"thinking": True},
        }

        class _FakeResponse:
            def __aiter__(self):
                async def _generator():
                    event = {
                        "result": {
                            "response": {
                                "responseId": "resp-poll-1",
                                "streamingVideoGenerationResponse": {
                                    "progress": 100,
                                    "status": "completed",
                                },
                            }
                        }
                    }
                    yield f"data: {orjson.dumps(event).decode()}\n\n".encode()

                return _generator()

        async def _drain_text(generator):
            chunks = []
            async for chunk in generator:
                chunks.append(chunk)
            return "".join(chunks)

        processor = VideoStreamProcessor(
            "grok-imagine-1.0-video",
            token="tok",
            think=True,
            post_id="post-poll-1",
        )
        text = asyncio.run(_drain_text(processor.process(_FakeResponse())))

        assert "未返回可用下载链接" in text
    finally:
        config._config = cfg_backup


def test_video_service_auto_length_uses_super_10s(monkeypatch):
    from app.services.grok.media import VideoService

    cfg_backup = config._config.copy()
    try:
        config._config = {
            "chat": {"stream": False},
            "network": {"base_proxy_url": "", "timeout": 30},
        }

        captured = {}

        class _TokenManager:
            async def reload_if_stale(self):
                return None

            def get_token(self, pool_name):
                if pool_name == "ssoBasic":
                    return ""
                if pool_name == "ssoSuper":
                    return "tok-super"
                return ""

            async def consume(self, token, effort):
                return None

        _mgr = _TokenManager()

        async def fake_acquire(model, pool_priority_override=None):
            return _mgr, "tok-super", pool_priority_override or ["ssoSuper", "ssoBasic"]

        async def fake_generate(
            self,
            token: str,
            prompt: str,
            aspect_ratio: str,
            video_length: int,
            resolution_name: str,
            preset: str,
        ):
            captured["length"] = video_length
            self.last_post_id = "post-super-1"

            async def _generator():
                if False:
                    yield b""

            return _generator()

        async def fake_collect(self, response):
            return {
                "id": "ok",
                "object": "chat.completion",
                "created": 0,
                "model": "grok-imagine-1.0-video",
                "choices": [
                    {
                        "index": 0,
                        "message": {"role": "assistant", "content": "ok"},
                        "finish_reason": "stop",
                    }
                ],
                "usage": {
                    "prompt_tokens": 0,
                    "completion_tokens": 0,
                    "total_tokens": 0,
                },
            }

        monkeypatch.setattr(
            "app.services.grok.services.media.acquire_token_for_model", fake_acquire
        )
        monkeypatch.setattr(
            "app.services.grok.services.chat.MessageExtractor.extract",
            lambda messages, is_video=False: ("prompt", []),
        )
        monkeypatch.setattr("app.services.grok.services.media.VideoService.generate", fake_generate)
        monkeypatch.setattr(
            "app.services.grok.services.media.VideoCollectProcessor.process",
            fake_collect,
        )

        result = asyncio.run(
            VideoService.completions(
                model="grok-imagine-1.0-video",
                messages=[{"role": "user", "content": "make video"}],
                stream=False,
                video_length=None,
            )
        )

        assert result["choices"][0]["message"]["content"] == "ok"
        assert captured["length"] == 10
    finally:
        config._config = cfg_backup


def test_video_service_downgrades_10s_when_only_basic_available(monkeypatch):
    from app.services.grok.media import VideoService

    cfg_backup = config._config.copy()
    try:
        config._config = {
            "chat": {"stream": False},
            "network": {"base_proxy_url": "", "timeout": 30},
        }

        captured = {}

        class _TokenManager:
            async def reload_if_stale(self):
                return None

            def get_token(self, pool_name):
                if pool_name == "ssoBasic":
                    return "tok-basic"
                return ""

            async def consume(self, token, effort):
                return None

        _mgr = _TokenManager()

        async def fake_acquire(model, pool_priority_override=None):
            return _mgr, "tok-basic", pool_priority_override or ["ssoSuper", "ssoBasic"]

        async def fake_generate(
            self,
            token: str,
            prompt: str,
            aspect_ratio: str,
            video_length: int,
            resolution_name: str,
            preset: str,
        ):
            captured["length"] = video_length
            self.last_post_id = "post-basic-1"

            async def _generator():
                if False:
                    yield b""

            return _generator()

        async def fake_collect(self, response):
            return {
                "id": "ok",
                "object": "chat.completion",
                "created": 0,
                "model": "grok-imagine-1.0-video",
                "choices": [
                    {
                        "index": 0,
                        "message": {"role": "assistant", "content": "ok"},
                        "finish_reason": "stop",
                    }
                ],
                "usage": {
                    "prompt_tokens": 0,
                    "completion_tokens": 0,
                    "total_tokens": 0,
                },
            }

        monkeypatch.setattr(
            "app.services.grok.services.media.acquire_token_for_model", fake_acquire
        )
        monkeypatch.setattr(
            "app.services.grok.services.chat.MessageExtractor.extract",
            lambda messages, is_video=False: ("prompt", []),
        )
        monkeypatch.setattr("app.services.grok.services.media.VideoService.generate", fake_generate)
        monkeypatch.setattr(
            "app.services.grok.services.media.VideoCollectProcessor.process",
            fake_collect,
        )

        result = asyncio.run(
            VideoService.completions(
                model="grok-imagine-1.0-video",
                messages=[{"role": "user", "content": "make video"}],
                stream=False,
                video_length=10,
            )
        )

        assert result["choices"][0]["message"]["content"] == "ok"
        assert captured["length"] == 6
    finally:
        config._config = cfg_backup


def test_pick_video_from_assets_allows_image_content_matching_post_id():
    """图生视频场景：IMAGE 类型资产匹配 post_id 时应返回 URL（由调用方 MIME 校验）"""
    from app.services.grok.processors.video_processors import _pick_video_from_assets

    assets = [
        {
            "assetId": "post-image-1",
            "fileUri": "/users/u/post-image-1/content",
            "fileMimeType": "image/png",
        }
    ]

    video_url, thumbnail_url = _pick_video_from_assets(assets, "post-image-1")
    assert video_url == "https://assets.grok.com/users/u/post-image-1/content"
    assert thumbnail_url == ""


def test_pick_video_from_assets_accepts_video_content_by_hint():
    from app.services.grok.processors.video_processors import _pick_video_from_assets

    assets = [
        {
            "assetId": "post-video-1",
            "fileUri": "/users/u/post-video-1/content",
            "fileMimeType": "video/mp4",
        }
    ]

    video_url, thumbnail_url = _pick_video_from_assets(assets, "post-video-1")
    assert video_url == "https://assets.grok.com/users/u/post-video-1/content"
    assert thumbnail_url == ""


def test_pick_video_from_assets_allows_same_post_ambiguous_content_url():
    """匹配 post_id 的模糊 /content URL 不再预过滤，交给 strict_media 校验"""
    from app.services.grok.processors.video_processors import _pick_video_from_assets

    post_id = "same-post-1"
    assets = [
        {
            "assetId": post_id,
            "fileUri": f"/users/u/{post_id}/content",
        }
    ]

    video_url, thumbnail_url = _pick_video_from_assets(assets, post_id)
    assert video_url == f"https://assets.grok.com/users/u/{post_id}/content"
    assert thumbnail_url == ""


def test_pick_video_from_assets_allows_other_ambiguous_content_url():
    from app.services.grok.processors.video_processors import _pick_video_from_assets

    post_id = "source-post-1"
    assets = [
        {
            "assetId": "output-post-2",
            "fileUri": "/users/u/output-post-2/content",
        }
    ]

    video_url, thumbnail_url = _pick_video_from_assets(assets, post_id)
    assert video_url == "https://assets.grok.com/users/u/output-post-2/content"
    assert thumbnail_url == ""


def test_pick_video_from_assets_prefers_real_video_over_source_image_post_id():
    from app.services.grok.processors.video_processors import _pick_video_from_assets

    post_id = "source-post-1"
    assets = [
        {
            "assetId": post_id,
            "fileUri": f"/users/u/{post_id}/content",
            "fileMimeType": "image/png",
        },
        {
            "assetId": "video-post-2",
            "fileUri": "/users/u/generated/video-post-2/content",
            "fileMimeType": "video/mp4",
        },
    ]

    video_url, _ = _pick_video_from_assets(assets, post_id)
    assert video_url == "https://assets.grok.com/users/u/generated/video-post-2/content"


def test_video_stream_directly_emits_url_with_background_cache(monkeypatch):
    """流中有 videoUrl 时，直接构建代理 URL 返回，并触发后台缓存"""
    cfg_backup = config._config.copy()
    try:
        config._config = {
            "app": {"video_format": "url", "app_url": "http://127.0.0.1:8000"},
            "timeout": {"video_idle_timeout": 30},
            "chat": {"thinking": True, "video_think_min_sec": 0},
        }

        source_url = "https://assets.grok.com/users/u/source-post/content"
        captured = {}

        def fake_schedule(token: str, asset_path: str, model: str, post_id: str):
            captured["token"] = token
            captured["asset_path"] = asset_path
            captured["post_id"] = post_id

        monkeypatch.setattr(
            "app.services.grok.processors.video_processors._schedule_video_cache_warm",
            fake_schedule,
        )

        class _FakeResponse:
            def __aiter__(self):
                async def _generator():
                    event = {
                        "result": {
                            "response": {
                                "responseId": "resp-direct-1",
                                "streamingVideoGenerationResponse": {
                                    "progress": 100,
                                    "status": "completed",
                                    "videoUrl": source_url,
                                },
                            }
                        }
                    }
                    yield f"data: {orjson.dumps(event).decode()}\n\n".encode()

                return _generator()

        async def _drain_text(generator):
            chunks = []
            async for chunk in generator:
                chunks.append(chunk)
            return "".join(chunks)

        processor = VideoStreamProcessor(
            "grok-imagine-1.0-video",
            token="tok",
            think=True,
            post_id="source-post",
        )
        text = asyncio.run(_drain_text(processor.process(_FakeResponse())))

        # 验证直接输出了本地代理 URL
        assert "http://127.0.0.1:8000/v1/files/video/users/u/source-post/content" in text
        assert "未返回可用下载链接" not in text
        # 验证触发了后台缓存
        assert captured["token"] == "tok"
        assert captured["asset_path"] == "/users/u/source-post/content"
        assert captured["post_id"] == "source-post"
    finally:
        config._config = cfg_backup


def test_video_collect_directly_builds_proxy_url_when_app_url_configured(monkeypatch):
    cfg_backup = config._config.copy()
    try:
        config._config = {
            "app": {"video_format": "url", "app_url": "http://127.0.0.1:8000"},
            "timeout": {"video_idle_timeout": 30},
            "chat": {"thinking": True, "video_think_min_sec": 0},
        }

        source_url = "https://assets.grok.com/users/u/source-post/content"
        captured = {}

        def fake_schedule(token: str, asset_path: str, model: str, post_id: str):
            captured["token"] = token
            captured["asset_path"] = asset_path
            captured["model"] = model
            captured["post_id"] = post_id

        monkeypatch.setattr(
            "app.services.grok.processors.video_processors._schedule_video_cache_warm",
            fake_schedule,
        )

        class _FakeResponse:
            def __aiter__(self):
                async def _generator():
                    event = {
                        "result": {
                            "response": {
                                "responseId": "resp-pending-local-1",
                                "streamingVideoGenerationResponse": {
                                    "progress": 100,
                                    "status": "completed",
                                    "videoUrl": source_url,
                                },
                            }
                        }
                    }
                    payload = f"data: {orjson.dumps(event).decode()}\n\n"
                    yield payload.encode()

                return _generator()

        processor = VideoCollectProcessor(
            "grok-imagine-1.0-video",
            token="tok",
            post_id="source-post",
        )
        result = asyncio.run(processor.process(_FakeResponse()))
        content = result["choices"][0]["message"]["content"]

        assert content == "http://127.0.0.1:8000/v1/files/video/users/u/source-post/content"
        assert captured["token"] == "tok"
        assert captured["asset_path"] == "/users/u/source-post/content"
        assert captured["post_id"] == "source-post"
    finally:
        config._config = cfg_backup


def test_video_payload_text_to_video_has_empty_parent_post_id():
    """文生视频 payload: parentPostId 为空，message 不含图片 URL"""
    service = VideoService.__new__(VideoService)
    payload = service._build_payload(
        prompt="a cat running",
        post_id="",
        aspect_ratio="3:2",
        video_length=6,
        resolution_name="480p",
        preset="normal",
        image_url="",
    )
    config_map = payload["responseMetadata"]["modelConfigOverride"]["modelMap"]
    assert config_map["videoGenModelConfig"]["parentPostId"] == ""
    assert "https://" not in payload["message"]
    assert "a cat running" in payload["message"]
    assert "--mode=normal" in payload["message"]


def test_video_payload_image_to_video_includes_image_url():
    """图生视频 payload: message 包含图片 URL，parentPostId 非空"""
    service = VideoService.__new__(VideoService)
    image_url = "https://assets.grok.com/users/abc/uploads/img.jpg"
    payload = service._build_payload(
        prompt="make it dance",
        post_id="post-123",
        aspect_ratio="2:3",
        video_length=6,
        resolution_name="480p",
        preset="normal",
        image_url=image_url,
    )
    config_map = payload["responseMetadata"]["modelConfigOverride"]["modelMap"]
    assert config_map["videoGenModelConfig"]["parentPostId"] == "post-123"
    assert image_url in payload["message"]
    assert "make it dance" in payload["message"]
    assert "--mode=normal" in payload["message"]


def test_video_stream_emits_url_directly_when_url_in_stream(monkeypatch):
    """流中有 videoUrl 且 app_url 为空时，直接返回上游 URL"""
    cfg_backup = config._config.copy()
    try:
        config._config = {
            "app": {"video_format": "url", "app_url": ""},
            "timeout": {"video_idle_timeout": 30},
            "chat": {"thinking": True, "video_think_min_sec": 0},
        }

        source_url = "https://assets.grok.com/users/u/source-post/content"

        class _FakeResponse:
            def __aiter__(self):
                async def _generator():
                    event = {
                        "result": {
                            "response": {
                                "responseId": "resp-pending-1",
                                "streamingVideoGenerationResponse": {
                                    "progress": 100,
                                    "status": "completed",
                                    "videoUrl": source_url,
                                },
                            }
                        }
                    }
                    payload = f"data: {orjson.dumps(event).decode()}\n\n"
                    yield payload.encode()

                return _generator()

        async def _drain_text(generator):
            chunks = []
            async for chunk in generator:
                chunks.append(chunk)
            return "".join(chunks)

        processor = VideoStreamProcessor(
            "grok-imagine-1.0-video",
            token="tok",
            think=True,
            post_id="source-post",
        )
        text = asyncio.run(_drain_text(processor.process(_FakeResponse())))

        assert source_url in text
        assert "未返回可用下载链接" not in text
    finally:
        config._config = cfg_backup
