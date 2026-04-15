import os
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import patch

from app.platform.storage.media_cache import save_media_bytes


class _StubConfig:
    def __init__(
        self,
        *,
        media_max_mb: float = 0.0,
        image_max_mb: float = 0.0,
        video_max_mb: float = 0.0,
    ) -> None:
        self._floats = {
            "storage.media_max_mb": media_max_mb,
            "storage.image_max_mb": image_max_mb,
            "storage.video_max_mb": video_max_mb,
        }

    def get_float(self, key: str, default: float = 0.0) -> float:
        return self._floats.get(key, default)


class MediaCacheLimitTests(unittest.TestCase):
    def test_save_media_bytes_prunes_oldest_file_when_type_limit_exceeded(self) -> None:
        with tempfile.TemporaryDirectory() as tmpdir:
            image_dir = Path(tmpdir) / "images"
            video_dir = Path(tmpdir) / "videos"
            image_dir.mkdir()
            video_dir.mkdir()

            old_path = image_dir / "old.png"
            old_path.write_bytes(b"a" * 80)
            old_mtime = time.time() - 60
            os.utime(old_path, (old_mtime, old_mtime))

            config = _StubConfig(image_max_mb=100 / (1024 * 1024))
            with patch("app.platform.storage.media_cache.get_config", return_value=config):
                with patch("app.platform.storage.media_cache.image_files_dir", return_value=image_dir):
                    with patch("app.platform.storage.media_cache.video_files_dir", return_value=video_dir):
                        new_path = save_media_bytes(
                            b"b" * 40,
                            image_dir / "new.png",
                            media_type="image",
                        )

            self.assertTrue(new_path.exists())
            self.assertFalse(old_path.exists())

        self.assertEqual(new_path.name, "new.png")

    def test_save_media_bytes_prunes_across_media_types_when_total_limit_exceeded(self) -> None:
        with tempfile.TemporaryDirectory() as tmpdir:
            image_dir = Path(tmpdir) / "images"
            video_dir = Path(tmpdir) / "videos"
            image_dir.mkdir()
            video_dir.mkdir()

            old_image = image_dir / "old.png"
            old_image.write_bytes(b"a" * 80)
            old_mtime = time.time() - 60
            os.utime(old_image, (old_mtime, old_mtime))

            config = _StubConfig(media_max_mb=100 / (1024 * 1024))
            with patch("app.platform.storage.media_cache.get_config", return_value=config):
                with patch("app.platform.storage.media_cache.image_files_dir", return_value=image_dir):
                    with patch("app.platform.storage.media_cache.video_files_dir", return_value=video_dir):
                        new_video = save_media_bytes(
                            b"b" * 40,
                            video_dir / "new.mp4",
                            media_type="video",
                        )

            self.assertTrue(new_video.exists())
            self.assertFalse(old_image.exists())


if __name__ == "__main__":
    unittest.main()
