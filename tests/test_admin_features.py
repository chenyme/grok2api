import copy

from fastapi import FastAPI
from fastapi.testclient import TestClient

from app.api.v1.admin import router as admin_router
from app.api.v1.admin import _sanitize_config_payload, _REDACTED_VALUE
from app.core.batch_tasks import create_task
from app.core.config import config
from app.services.api_keys import api_key_manager


def _make_app() -> FastAPI:
    app = FastAPI()
    app.include_router(admin_router)
    return app


def _backup_api_keys():
    return (copy.deepcopy(api_key_manager._keys), api_key_manager._loaded)


def _restore_api_keys(state):
    api_key_manager._keys, api_key_manager._loaded = state


def test_admin_pages_include_voice_and_imagine():
    client = TestClient(_make_app())
    assert client.get("/admin/imagine").status_code == 200
    assert client.get("/admin/voice").status_code == 200


def test_root_redirects_to_admin_login():
    client = TestClient(_make_app())
    res = client.get("/", follow_redirects=False)
    assert res.status_code == 307
    assert res.headers.get("location") == "/admin"


def test_voice_token_endpoint_requires_auth():
    cfg_backup = copy.deepcopy(config._config)
    api_backup = _backup_api_keys()
    try:
        api_key_manager._keys = []
        api_key_manager._loaded = True
        config._config = {
            "app": {"app_password": "", "api_key": ""},
            "security": {"allow_anonymous_admin": False},
        }
        client = TestClient(_make_app())
        res = client.get("/api/v1/admin/voice/token")
        assert res.status_code == 401
    finally:
        config._config = cfg_backup
        _restore_api_keys(api_backup)


def test_voice_token_endpoint_success(monkeypatch):
    cfg_backup = copy.deepcopy(config._config)
    api_backup = _backup_api_keys()
    try:
        api_key_manager._keys = []
        api_key_manager._loaded = True
        config._config = {
            "app": {"app_password": "adminkey", "api_key": ""},
            "security": {"allow_anonymous_admin": False},
        }

        async def fake_get_pool_token(pool_name="ssoBasic"):
            return "tok"

        async def fake_get_voice_token(
            self, token: str, voice: str, personality: str, speed: float
        ):
            assert token == "tok"
            return {"token": "livekit-token", "livekitUrl": "wss://livekit.example"}

        monkeypatch.setattr(
            "app.services.token.service.TokenService.get_token", fake_get_pool_token
        )
        monkeypatch.setattr(
            "app.services.grok.services.voice.VoiceService.get_token",
            fake_get_voice_token,
        )

        client = TestClient(_make_app())
        res = client.get(
            "/api/v1/admin/voice/token?voice=ara&personality=assistant&speed=1.1",
            headers={"Authorization": "Bearer adminkey"},
        )
        assert res.status_code == 200
        body = res.json()
        assert body["token"] == "livekit-token"
        assert body["url"] == "wss://livekit.example"
    finally:
        config._config = cfg_backup
        _restore_api_keys(api_backup)


def test_imagine_ws_start_and_image(monkeypatch):
    cfg_backup = copy.deepcopy(config._config)
    api_backup = _backup_api_keys()
    try:
        api_key_manager._keys = []
        api_key_manager._loaded = True
        config._config = {
            "app": {"app_password": "adminkey", "api_key": ""},
            "security": {"allow_anonymous_admin": False},
            "image": {"image_ws_nsfw": True},
            "retry": {"max_retry": 1},
        }

        async def fake_get_pool_token(pool_name="ssoBasic"):
            return "tok"

        async def fake_consume(token, effort):
            return True

        stream_calls = []

        async def fake_stream(**kwargs):
            stream_calls.append(kwargs)
            yield {
                "type": "image",
                "blob": "data:image/png;base64,QUJD",
                "url": "https://assets.grok.com/images/test-preview.png",
                "stage": "medium",
                "is_final": False,
            }
            yield {
                "type": "image",
                "blob": "QUJD",
                "url": "https://assets.grok.com/images/test-final.jpg",
                "stage": "final",
                "is_final": True,
            }

        monkeypatch.setattr(
            "app.services.token.service.TokenService.get_token", fake_get_pool_token
        )
        monkeypatch.setattr("app.services.token.service.TokenService.consume", fake_consume)
        from app.services.grok.image import image_service

        monkeypatch.setattr(image_service, "stream", fake_stream)

        client = TestClient(_make_app())
        token_res = client.post(
            "/api/v1/admin/ws/token",
            headers={"Authorization": "Bearer adminkey"},
        )
        assert token_res.status_code == 200
        ws_token = token_res.json()["token"]

        with client.websocket_connect(f"/api/v1/admin/imagine/ws?ws_token={ws_token}") as ws:
            ws.send_json(
                {
                    "type": "start",
                    "prompt": "cat",
                    "aspect_ratio": "2:3",
                    "image_count": 9,
                    "output_mode": "url",
                }
            )
            first = ws.receive_json()
            assert first["type"] == "status"
            assert first["status"] == "running"
            assert first["image_count"] == 9
            assert first["output_mode"] == "url"

            got_image = False
            for _ in range(5):
                msg = ws.receive_json()
                if msg.get("type") == "image":
                    got_image = True
                    assert "source_url" in msg
                    assert "b64_json" not in msg
                if msg.get("type") == "status" and msg.get("status") == "stopped":
                    break

            assert got_image
            assert stream_calls
            assert stream_calls[0]["n"] == 6
            assert all(call["n"] <= 6 for call in stream_calls)
    finally:
        config._config = cfg_backup
        _restore_api_keys(api_backup)


def test_batch_stream_supports_stream_token_without_admin_key():
    cfg_backup = copy.deepcopy(config._config)
    api_backup = _backup_api_keys()
    try:
        api_key_manager._keys = []
        api_key_manager._loaded = True
        config._config = {
            "app": {"app_password": "", "api_key": ""},
            "security": {
                "allow_anonymous_admin": False,
                "allow_query_api_key": True,
            },
        }

        task = create_task(1)
        task.finish({"status": "ok"})

        client = TestClient(_make_app())
        denied = client.get(f"/api/v1/admin/batch/{task.id}/stream")
        assert denied.status_code == 401

        allowed = client.get(
            f"/api/v1/admin/batch/{task.id}/stream?stream_token={task.stream_token}"
        )
        assert allowed.status_code == 200
        assert "snapshot" in allowed.text
    finally:
        config._config = cfg_backup
        _restore_api_keys(api_backup)


def test_sanitize_config_keeps_non_sensitive_max_tokens_fields():
    raw = {
        "performance": {
            "assets_max_tokens": 10000,
            "usage_max_tokens": 15000,
            "nsfw_max_tokens": 15000,
        }
    }

    sanitized = _sanitize_config_payload(raw)

    assert sanitized["performance"]["assets_max_tokens"] == 10000
    assert sanitized["performance"]["usage_max_tokens"] == 15000
    assert sanitized["performance"]["nsfw_max_tokens"] == 15000


def test_sanitize_config_redacts_sensitive_keys():
    raw = {
        "app": {"api_key": "sk-abc"},
        "security": {"cf_clearance": "cookie"},
    }

    sanitized = _sanitize_config_payload(raw)

    # api_key 是敏感字段，必须脱敏（Phase 1 安全修复）
    assert sanitized["app"]["api_key"] == _REDACTED_VALUE
    assert sanitized["security"]["cf_clearance"] == _REDACTED_VALUE
