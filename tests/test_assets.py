import asyncio

from app.core.config import config
from app.services.grok.assets import DownloadService


def test_delete_file_sanitizes_name(tmp_path):
    service = DownloadService()
    service.image_dir = tmp_path
    service.video_dir = tmp_path

    safe = tmp_path / "safe.jpg"
    safe.write_bytes(b"x")

    res_ok = service.delete_file("image", "safe.jpg")
    assert res_ok["deleted"] is True
    assert not safe.exists()

    res_bad = service.delete_file("image", "../evil")
    assert res_bad["deleted"] is False

    res_bad2 = service.delete_file("image", "..\\evil")
    assert res_bad2["deleted"] is False


def test_check_limit_uses_split_limits_without_legacy_fallback(tmp_path):
    cfg_backup = config._config.copy()
    try:
        config._config = {
            "cache": {
                "enable_auto_clean": True,
                "image_limit_mb": 1,
                "video_limit_mb": 1,
            }
        }

        service = DownloadService()
        service.image_dir = tmp_path / "image"
        service.video_dir = tmp_path / "video"
        service.image_dir.mkdir(parents=True, exist_ok=True)
        service.video_dir.mkdir(parents=True, exist_ok=True)

        image_file = service.image_dir / "a.jpg"
        image_file.write_bytes(b"x" * 1024)

        asyncio.run(service.check_limit())

        assert image_file.exists()
    finally:
        config._config = cfg_backup


def test_check_limit_cleans_image_and_keeps_video(tmp_path):
    cfg_backup = config._config.copy()
    try:
        config._config = {
            "cache": {
                "enable_auto_clean": True,
                "image_limit_mb": 0.0001,
                "video_limit_mb": 100,
            }
        }

        service = DownloadService()
        service.image_dir = tmp_path / "image"
        service.video_dir = tmp_path / "video"
        service.image_dir.mkdir(parents=True, exist_ok=True)
        service.video_dir.mkdir(parents=True, exist_ok=True)

        old_image = service.image_dir / "old.jpg"
        old_image.write_bytes(b"x" * 4096)
        keep_video = service.video_dir / "keep.mp4"
        keep_video.write_bytes(b"y" * 4096)

        asyncio.run(service.check_limit())

        assert not old_image.exists()
        assert keep_video.exists()
    finally:
        config._config = cfg_backup


def test_get_mime_uses_file_signature_for_extensionless_video(tmp_path):
    service = DownloadService()
    video_file = tmp_path / "users-u-post-content"
    video_file.write_bytes(bytes.fromhex("000000186674797069736f6d") + b"\x00" * 24)

    mime = service._get_mime(video_file)
    assert mime == "video/mp4"


def test_list_files_includes_extensionless_image_by_signature(tmp_path):
    service = DownloadService()
    service.image_dir = tmp_path / "image"
    service.video_dir = tmp_path / "video"
    service.image_dir.mkdir(parents=True, exist_ok=True)
    service.video_dir.mkdir(parents=True, exist_ok=True)

    image_file = service.image_dir / "users-u-image-post-content"
    image_file.write_bytes(b"\x89PNG\r\n\x1a\n" + b"\x00" * 16)

    result = service.list_files("image")

    assert result["total"] == 1
    assert result["items"][0]["name"] == "users-u-image-post-content"


def test_get_stats_counts_extensionless_image_by_signature(tmp_path):
    service = DownloadService()
    service.image_dir = tmp_path / "image"
    service.video_dir = tmp_path / "video"
    service.image_dir.mkdir(parents=True, exist_ok=True)
    service.video_dir.mkdir(parents=True, exist_ok=True)

    image_file = service.image_dir / "users-u-image-post-content"
    image_file.write_bytes(b"\x89PNG\r\n\x1a\n" + b"\x00" * 16)

    stats = service.get_stats("image")

    assert stats["count"] == 1
