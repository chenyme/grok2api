import asyncio
import json
from types import SimpleNamespace

import orjson

from app.api.v1 import chat as chat_api
from app.api.v1.chat import ImageConfig, _imagine_fast_server_image_config
from app.api.v1 import image as image_api
from app.api.v1.image import (
    ImageGenerationRequest,
    ShareImageResolveRequest,
    append_share_payload,
    validate_generation_request,
)
from app.core.exceptions import ValidationException
from app.services.grok.services.image import (
    _build_app_chat_share_url,
    _collect_post_id_candidates,
    _create_image_share_link,
    _extract_app_chat_share_context,
    _pick_best_post_id,
)
from app.services.grok.utils.share_resolver import (
    ShareImageResolution,
    normalize_grok_share_url,
    resolve_grok_share_image,
)


def test_validate_generation_request_allows_fast_model():
    request = ImageGenerationRequest(
        prompt="draw a cat",
        model="grok-imagine-1.0-fast",
        n=1,
        size="1024x1024",
        stream=False,
    )

    validate_generation_request(request)


def test_validate_generation_request_rejects_stream_share_url():
    request = ImageGenerationRequest(
        prompt="draw a cat",
        model="grok-imagine-1.0-fast",
        n=1,
        size="1024x1024",
        stream=True,
        return_share_url=True,
    )

    try:
        validate_generation_request(request)
    except ValidationException as exc:
        assert exc.code == "share_url_stream_not_supported"
    else:
        raise AssertionError("expected ValidationException")


def test_collect_post_id_candidates_prefers_response_post():
    payload = {
        "post": {"id": "11111111-2222-3333-4444-555555555555"},
        "postId": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
        "modelResponse": {
            "fileAttachments": [
                "users/demo/generated/99999999-8888-7777-6666-555555555555/image.jpg"
            ]
        },
    }

    post_id, rank = _pick_best_post_id(_collect_post_id_candidates(payload))

    assert post_id == "11111111-2222-3333-4444-555555555555"
    assert rank == 1


def test_imagine_fast_server_image_config_preserves_return_share_url(monkeypatch):
    monkeypatch.setattr(
        "app.api.v1.chat.get_config",
        lambda key, default=None: {
            "imagine_fast.n": 2,
            "imagine_fast.size": "1024x1024",
            "imagine_fast.response_format": "url",
        }.get(key, default),
    )

    config = _imagine_fast_server_image_config(
        ImageConfig(return_share_url=True, response_format="b64_json")
    )

    assert config.n == 2
    assert config.size == "1024x1024"
    assert config.response_format == "url"
    assert config.return_share_url is True


def test_extract_app_chat_share_context_supports_nested_result():
    payload = {
        "result": {
            "conversation": {
                "conversationId": "conv-nested",
            },
            "response": {
                "responseId": "resp-nested",
                "modelResponse": {
                    "responseId": "resp-model",
                    "cardAttachmentsJson": [
                        '{"image_chunk":{"imageUrl":"users/demo/generated/test/image.jpg"}}'
                    ],
                },
            },
        }
    }

    conversation_id, response_id = _extract_app_chat_share_context(payload)

    assert conversation_id == "conv-nested"
    assert response_id == "resp-model"


def test_extract_app_chat_share_context_supports_flat_result_events():
    payload = {
        "result": {
            "conversationId": "conv-flat",
            "responseId": "resp-flat",
            "cardAttachment": {
                "jsonData": '{"image_chunk":{"imageUrl":"users/demo/generated/test/image.jpg"}}'
            },
        }
    }

    conversation_id, response_id = _extract_app_chat_share_context(payload)

    assert conversation_id == "conv-flat"
    assert response_id == "resp-flat"


def test_build_app_chat_share_url_prefers_share_link_id():
    payload = {
        "shareLinkId": "c2hhcmQtMg_demo",
        "publicId": "should-not-win",
    }

    assert _build_app_chat_share_url(payload) == "https://grok.com/share/c2hhcmQtMg_demo"


def test_normalize_grok_share_url_strips_extra_path():
    raw = (
        "https://grok.com/share/c2hhcmQtMg_demo/"
        "opengraph-image/c2hhcmQtMg_demo?cache=1"
    )

    assert normalize_grok_share_url(raw) == "https://grok.com/share/c2hhcmQtMg_demo"


def test_resolve_grok_share_image_prefers_assets_candidate(monkeypatch):
    html = """
    <html>
      <head>
        <meta property="og:image" content="https://grok.com/share/demo/opengraph-image/demo?cache=1" />
      </head>
      <body>
        <script>
          window.__demo = "https://assets.grok.com/users/demo/generated/final/image.jpg";
        </script>
      </body>
    </html>
    """

    async def fake_public(share_url):
        assert share_url == "https://grok.com/share/demo"
        return {}

    async def fake_fetch(share_url):
        assert share_url == "https://grok.com/share/demo"
        return html

    monkeypatch.setattr(
        "app.services.grok.utils.share_resolver._fetch_public_share_payload",
        fake_public,
    )
    monkeypatch.setattr(
        "app.services.grok.utils.share_resolver._fetch_share_html",
        fake_fetch,
    )

    resolved = asyncio.run(resolve_grok_share_image("https://grok.com/share/demo"))

    assert resolved.share_url == "https://grok.com/share/demo"
    assert (
        resolved.image_url
        == "https://assets.grok.com/users/demo/generated/final/image.jpg"
    )
    assert resolved.source == "assets"


def test_resolve_grok_share_image_prefers_public_json_assets(monkeypatch):
    payload = {
        "responses": [
            {
                "cardAttachmentsJson": [
                    json.dumps(
                        {
                            "image_chunk": {
                                "imageUrl": "users/demo/generated/demo-image-part-0/image.jpg"
                            }
                        }
                    ),
                    json.dumps(
                        {
                            "image_chunk": {
                                "imageUrl": "users/demo/generated/demo-image/image.jpg"
                            }
                        }
                    ),
                ]
            }
        ]
    }

    async def fake_public(share_url):
        assert share_url == "https://grok.com/share/demo-public"
        return payload

    async def fake_fetch(share_url):
        assert share_url == "https://grok.com/share/demo-public"
        return """
        <html>
          <head>
            <meta property="og:image" content="https://grok.com/share/demo-public/opengraph-image/demo-public?cache=1" />
          </head>
        </html>
        """

    monkeypatch.setattr(
        "app.services.grok.utils.share_resolver._fetch_public_share_payload",
        fake_public,
    )
    monkeypatch.setattr(
        "app.services.grok.utils.share_resolver._fetch_share_html",
        fake_fetch,
    )

    resolved = asyncio.run(resolve_grok_share_image("https://grok.com/share/demo-public"))

    assert (
        resolved.image_url
        == "https://assets.grok.com/users/demo/generated/demo-image/image.jpg"
    )
    assert resolved.source == "public_json"


def test_append_share_payload_includes_resolved_fields():
    payload = {"created": 1}
    result = SimpleNamespace(
        share_url="https://grok.com/share/demo",
        share_image_url="https://assets.grok.com/users/demo/generated/final/image.jpg",
        share_image_source="assets",
        share_image_expires_at="2026-03-29T00:00:00Z",
    )

    merged = append_share_payload(payload, result)

    assert merged["share_url"] == "https://grok.com/share/demo"
    assert (
        merged["share_image_url"]
        == "https://assets.grok.com/users/demo/generated/final/image.jpg"
    )
    assert merged["share_image_source"] == "assets"
    assert merged["share_image_expires_at"] == "2026-03-29T00:00:00Z"


def test_create_image_share_link_uses_app_chat_share(monkeypatch):
    class DummyResponse:
        def json(self):
            return {"shareLinkId": "c2hhcmQtMg_demo"}

    class DummySession:
        async def __aenter__(self):
            return object()

        async def __aexit__(self, exc_type, exc, tb):
            return False

    async def fake_request(session, token, conversation_id, response_id):
        assert token == "token-demo"
        assert conversation_id == "conv-demo"
        assert response_id == "resp-demo"
        return DummyResponse()

    monkeypatch.setattr(
        "app.services.grok.services.image._new_session",
        lambda: DummySession(),
    )
    monkeypatch.setattr(
        "app.services.grok.services.image.AppChatShareReverse.request",
        fake_request,
    )

    share_url = asyncio.run(
        _create_image_share_link("token-demo", "conv-demo", "resp-demo")
    )

    assert share_url == "https://grok.com/share/c2hhcmQtMg_demo"


def test_chat_completions_imagine_fast_preserves_return_share_url(monkeypatch):
    captured = {}

    class DummyTokenMgr:
        async def reload_if_stale(self):
            return None

        def get_token(self, pool_name):
            return "token-demo"

    async def fake_get_token_manager():
        return DummyTokenMgr()

    async def fake_generate(self, **kwargs):
        captured.update(kwargs)
        return SimpleNamespace(
            stream=False,
            data=["http://localhost/image.jpg"],
            usage_override={"total_tokens": 0},
            share_url="https://grok.com/share/demo-fast",
            share_image_url="https://assets.grok.com/users/demo/generated/final/image.jpg",
            share_image_source="assets",
            share_image_expires_at="2026-03-29T00:00:00Z",
        )

    monkeypatch.setattr(chat_api, "get_config", lambda key, default=None: {
        "imagine_fast.n": 2,
        "imagine_fast.size": "1024x1024",
        "imagine_fast.response_format": "url",
    }.get(key, default))
    monkeypatch.setattr(chat_api, "get_token_manager", fake_get_token_manager)
    monkeypatch.setattr(chat_api.ModelService, "valid", lambda model: True)
    monkeypatch.setattr(
        chat_api.ModelService,
        "get",
        lambda model: SimpleNamespace(
            is_image=True,
            is_image_edit=False,
            is_video=False,
            model_id=model,
            grok_model="grok-4-image",
            model_mode="auto",
        ),
    )
    monkeypatch.setattr(
        chat_api.ModelService,
        "pool_candidates_for_model",
        lambda model: ["demo-pool"],
    )
    monkeypatch.setattr(
        "app.api.v1.chat.ImageGenerationService.generate",
        fake_generate,
    )

    request = chat_api.ChatCompletionRequest(
        model="grok-imagine-1.0-fast",
        stream=False,
        messages=[chat_api.MessageItem(role="user", content="draw a dog")],
        image_config=chat_api.ImageConfig(return_share_url=True),
    )

    response = asyncio.run(chat_api.chat_completions(request))
    payload = orjson.loads(response.body)

    assert captured["return_share_url"] is True
    assert payload["share_url"] == "https://grok.com/share/demo-fast"
    assert payload["choices"][0]["message"]["content"] == "![image](http://localhost/image.jpg)"
    assert (
        payload["share_image_url"]
        == "https://assets.grok.com/users/demo/generated/final/image.jpg"
    )
    assert payload["share_image_source"] == "assets"
    assert payload["share_image_expires_at"] == "2026-03-29T00:00:00Z"


def test_resolve_share_image_endpoint_returns_current_direct_link(monkeypatch):
    async def fake_resolve(share_url):
        assert share_url == "https://grok.com/share/demo"
        return ShareImageResolution(
            share_url=share_url,
            image_url="https://assets.grok.com/users/demo/generated/final/image.jpg",
            source="assets",
            expires_at="2026-03-29T00:00:00Z",
        )

    monkeypatch.setattr(
        "app.api.v1.image.resolve_grok_share_image",
        fake_resolve,
    )

    response = asyncio.run(
        image_api.resolve_share_image(
            ShareImageResolveRequest(share_url="https://grok.com/share/demo")
        )
    )
    payload = orjson.loads(response.body)

    assert payload["resolved"] is True
    assert payload["share_url"] == "https://grok.com/share/demo"
    assert (
        payload["share_image_url"]
        == "https://assets.grok.com/users/demo/generated/final/image.jpg"
    )
