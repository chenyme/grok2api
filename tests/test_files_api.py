from fastapi import FastAPI
from fastapi.testclient import TestClient

from app.api.v1 import files as files_api


def _make_files_app() -> TestClient:
    app = FastAPI()
    app.include_router(files_api.router, prefix="/v1/files")
    return TestClient(app)


def test_get_video_returns_mp4_for_ftyp_signature(tmp_path, monkeypatch):
    monkeypatch.setattr(files_api, "VIDEO_DIR", tmp_path)

    filename = "users-u-video-1-content"
    file_path = tmp_path / filename
    file_path.write_bytes(b"\x00\x00\x00\x18ftypisom" + b"\x00" * 24)

    client = _make_files_app()
    resp = client.get(f"/v1/files/video/{filename}")

    assert resp.status_code == 200
    assert resp.headers.get("content-type", "").startswith("video/mp4")


def test_get_video_rejects_non_video_and_cleans_cache(tmp_path, monkeypatch):
    monkeypatch.setattr(files_api, "VIDEO_DIR", tmp_path)

    filename = "users-u-not-video-content"
    file_path = tmp_path / filename
    file_path.write_bytes(b"\x89PNG\r\n\x1a\n" + b"\x00" * 16)

    client = _make_files_app()
    resp = client.get(f"/v1/files/video/{filename}")

    assert resp.status_code == 415
    assert not file_path.exists()
