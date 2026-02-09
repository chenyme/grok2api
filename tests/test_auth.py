import copy

from fastapi import Depends, FastAPI
from fastapi.testclient import TestClient

from app.core.auth import verify_admin_access
from app.core.config import config
from app.services.api_keys import api_key_manager


def _make_app() -> FastAPI:
    app = FastAPI()

    @app.get("/secure", dependencies=[Depends(verify_admin_access)])
    async def secure():
        return {"ok": True}

    return app


def _backup_api_keys():
    return (copy.deepcopy(api_key_manager._keys), api_key_manager._loaded)


def _restore_api_keys(state):
    api_key_manager._keys, api_key_manager._loaded = state


def test_admin_access_without_keys_denies_by_default():
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
        res = client.get("/secure")
        assert res.status_code == 401
    finally:
        config._config = cfg_backup
        _restore_api_keys(api_backup)


def test_admin_access_without_keys_allows_when_enabled():
    cfg_backup = copy.deepcopy(config._config)
    api_backup = _backup_api_keys()
    try:
        api_key_manager._keys = []
        api_key_manager._loaded = True
        config._config = {
            "app": {"app_password": "", "api_key": ""},
            "security": {"allow_anonymous_admin": True},
        }
        client = TestClient(_make_app())
        res = client.get("/secure")
        assert res.status_code == 200
    finally:
        config._config = cfg_backup
        _restore_api_keys(api_backup)


def test_admin_access_with_app_password_requires_auth():
    cfg_backup = copy.deepcopy(config._config)
    api_backup = _backup_api_keys()
    try:
        api_key_manager._keys = []
        api_key_manager._loaded = True
        config._config = {"app": {"app_password": "secret", "api_key": ""}}
        client = TestClient(_make_app())
        res = client.get("/secure")
        assert res.status_code == 401
        res = client.get("/secure", headers={"Authorization": "Bearer secret"})
        assert res.status_code == 200
    finally:
        config._config = cfg_backup
        _restore_api_keys(api_backup)


def test_admin_access_with_api_key_and_custom_key():
    cfg_backup = copy.deepcopy(config._config)
    api_backup = _backup_api_keys()
    try:
        api_key_manager._keys = []
        api_key_manager._loaded = True
        config._config = {"app": {"app_password": "", "api_key": "adminkey"}}
        client = TestClient(_make_app())
        res = client.get("/secure", headers={"Authorization": "Bearer adminkey"})
        assert res.status_code == 200

        # 普通 custom key（非 admin）不能访问 admin 端点（RBAC 修复）
        api_key_manager._keys = [{"key": "sk-123", "name": "k", "enabled": True}]
        config._config = {"app": {"app_password": "", "api_key": ""}}
        res = client.get("/secure", headers={"Authorization": "Bearer sk-123"})
        assert res.status_code == 401
    finally:
        config._config = cfg_backup
        _restore_api_keys(api_backup)
