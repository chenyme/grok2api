"""Platform storage helpers."""

from .media_paths import image_files_dir, video_files_dir
from .media_cache import save_media_bytes

__all__ = ["image_files_dir", "video_files_dir", "save_media_bytes"]
