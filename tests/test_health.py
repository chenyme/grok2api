from fastapi import FastAPI
from fastapi.testclient import TestClient

from app.api.v1.health import router as health_router


def test_health_ready_ok():
    app = FastAPI()
    app.include_router(health_router)
    client = TestClient(app)

    health = client.get("/health")
    assert health.status_code == 200
    assert health.json().get("status") == "ok"

    ready = client.get("/ready")
    assert ready.status_code in (200, 503)
