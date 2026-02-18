from pathlib import Path

from fastapi import APIRouter, HTTPException
from fastapi.responses import FileResponse, RedirectResponse

from app.core.auth import is_public_enabled

router = APIRouter()
STATIC_DIR = Path(__file__).resolve().parents[2] / "static"


@router.get("/", include_in_schema=False)
async def root():
    if is_public_enabled():
        return RedirectResponse(url="/login")
    return RedirectResponse(url="/admin/login")


@router.get("/login", include_in_schema=False)
async def public_login():
    if not is_public_enabled():
        raise HTTPException(status_code=404, detail="Not Found")
    return FileResponse(STATIC_DIR / "public/pages/login.html")


@router.get("/imagine", include_in_schema=False)
async def public_imagine():
    if not is_public_enabled():
        raise HTTPException(status_code=404, detail="Not Found")
    return FileResponse(STATIC_DIR / "public/pages/imagine.html")


@router.get("/voice", include_in_schema=False)
async def public_voice():
    if not is_public_enabled():
        raise HTTPException(status_code=404, detail="Not Found")
    return FileResponse(STATIC_DIR / "public/pages/voice.html")


@router.get("/video", include_in_schema=False)
async def public_video():
    if not is_public_enabled():
        raise HTTPException(status_code=404, detail="Not Found")
    return FileResponse(STATIC_DIR / "public/pages/video.html")
