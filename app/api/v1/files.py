"""
文件服务 API 路由
"""

import aiofiles.os
from pathlib import Path, PurePath
from fastapi import APIRouter, HTTPException
from fastapi.responses import FileResponse

from app.core.logger import logger
from app.core.storage import DATA_DIR

router = APIRouter(tags=["Files"])

# 缓存根目录
BASE_DIR = DATA_DIR / "tmp"
IMAGE_DIR = BASE_DIR / "image"
VIDEO_DIR = BASE_DIR / "video"


def _sanitize_filename(filename: str) -> str:
    """避免路径穿越，仅允许安全文件名"""
    cleaned = (filename or "").replace("\\", "/").strip()
    if not cleaned:
        raise HTTPException(status_code=400, detail="Invalid filename")

    parts = PurePath(cleaned).parts
    if any(part in (".", "..") for part in parts):
        raise HTTPException(status_code=400, detail="Invalid filename")

    if "/" in cleaned:
        cleaned = cleaned.replace("/", "-")

    if ".." in cleaned:
        raise HTTPException(status_code=400, detail="Invalid filename")

    return cleaned


def _detect_video_content_type(file_path: Path) -> str:
    """基于文件头检测视频 MIME，避免将图片误当作视频返回。"""
    try:
        with file_path.open("rb") as f:
            head = f.read(32)
    except Exception:
        return "application/octet-stream"

    if len(head) >= 12 and head[4:8] == b"ftyp":
        return "video/mp4"
    if head.startswith(b"\x1a\x45\xdf\xa3"):
        return "video/webm"
    if head.startswith(b"RIFF") and b"AVI" in head[:16]:
        return "video/x-msvideo"

    return "application/octet-stream"


@router.get("/image/{filename:path}")
async def get_image(filename: str):
    """
    获取图片文件
    """
    filename = _sanitize_filename(filename)

    file_path = IMAGE_DIR / filename

    if await aiofiles.os.path.exists(file_path):
        if await aiofiles.os.path.isfile(file_path):
            content_type = "image/jpeg"
            if file_path.suffix.lower() == ".png":
                content_type = "image/png"
            elif file_path.suffix.lower() == ".webp":
                content_type = "image/webp"

            # 增加缓存头，支持高并发场景下的浏览器/CDN缓存
            return FileResponse(
                file_path,
                media_type=content_type,
                headers={"Cache-Control": "public, max-age=31536000, immutable"},
            )

    logger.warning(f"Image not found: {filename}")
    raise HTTPException(status_code=404, detail="Image not found")


@router.get("/video/{filename:path}")
async def get_video(filename: str):
    """
    获取视频文件
    """
    filename = _sanitize_filename(filename)

    file_path = VIDEO_DIR / filename

    if await aiofiles.os.path.exists(file_path):
        if await aiofiles.os.path.isfile(file_path):
            content_type = _detect_video_content_type(file_path)
            if not content_type.startswith("video/"):
                logger.warning(f"Cached file is not a video: {filename} ({content_type})")
                try:
                    file_path.unlink(missing_ok=True)
                except Exception:
                    pass
                raise HTTPException(status_code=415, detail="Cached asset is not a video")

            return FileResponse(
                file_path,
                media_type=content_type,
                headers={"Cache-Control": "public, max-age=31536000, immutable"},
            )

    logger.warning(f"Video not found: {filename}")
    raise HTTPException(status_code=404, detail="Video not found")
