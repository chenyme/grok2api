import copy

from fastapi import FastAPI, Request
from fastapi.testclient import TestClient

from app.core.config import config
from app.core.security_middleware import (
    BodySizeLimitMiddleware,
    RateLimitMiddleware,
    _rate_limiter,
)


def _make_app() -> FastAPI:
    app = FastAPI()
    app.add_middleware(BodySizeLimitMiddleware)
    app.add_middleware(RateLimitMiddleware)

    @app.post("/echo")
    async def echo(request: Request):
        data = await request.body()
        return {"size": len(data)}

    @app.get("/ping")
    async def ping():
        return {"status": "ok"}

    return app


def test_body_size_limit_rejects_large():
    original = copy.deepcopy(config._config)
    try:
        config._config = {
            "security": {"max_body_size_mb": 0.0001, "rate_limit_enabled": False}
        }
        app = _make_app()
        client = TestClient(app)
        res = client.post("/echo", content=b"a" * 1024)
        assert res.status_code == 413
    finally:
        config._config = original


def test_rate_limit_blocks_after_burst():
    original = copy.deepcopy(config._config)
    try:
        config._config = {
            "security": {
                "max_body_size_mb": 50,
                "rate_limit_enabled": True,
                "rate_limit_per_minute": 60,
                "rate_limit_burst": 1,
                "rate_limit_key": "ip",
                "rate_limit_ttl_sec": 60,
                "rate_limit_exempt_paths": [],
            }
        }
        _rate_limiter._buckets.clear()
        app = _make_app()
        client = TestClient(app)

        ok = client.get("/ping")
        assert ok.status_code == 200
        blocked = client.get("/ping")
        assert blocked.status_code == 429
    finally:
        config._config = original


def test_query_api_key_blocked_by_default():
    """query string 中的 api_key 默认不被读取（allow_query_api_key=false）"""
    original = copy.deepcopy(config._config)
    try:
        config._config = {
            "security": {
                "max_body_size_mb": 50,
                "rate_limit_enabled": True,
                "rate_limit_per_minute": 60,
                "rate_limit_burst": 1,
                "rate_limit_key": "auto",
                "rate_limit_ttl_sec": 60,
                "rate_limit_exempt_paths": [],
                "allow_query_api_key": False,
            }
        }
        _rate_limiter._buckets.clear()
        app = _make_app()
        client = TestClient(app)

        # 第一次请求消耗 burst
        ok = client.get("/ping?api_key=secret123")
        assert ok.status_code == 200

        # 第二次请求：即使带了不同 api_key 也应被限速（因为 query key 被忽略，回退到 IP）
        blocked = client.get("/ping?api_key=other456")
        assert blocked.status_code == 429
    finally:
        config._config = original
