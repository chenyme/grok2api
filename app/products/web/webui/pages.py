"""Static page routes for the statics-based WebUI."""

from pathlib import Path

from fastapi import APIRouter, HTTPException
from fastapi.responses import FileResponse, RedirectResponse

from app.platform.auth.middleware import is_webui_enabled
from ..static_html import serve_static_html

router = APIRouter(include_in_schema=False)

STATIC_DIR = Path(__file__).resolve().parents[3] / "statics" / "webui"


def _serve(filename: str) -> FileResponse:
    path = STATIC_DIR / filename
    if not path.exists():
        raise HTTPException(status_code=404, detail="Page not found")
    return FileResponse(path)


def _serve_html(filename: str):
    return serve_static_html(STATIC_DIR / filename)


@router.get("/webui/chat")
async def webui_chat_page():
    if not is_webui_enabled():
        raise HTTPException(status_code=404, detail="Not Found")
    return _serve_html("chat.html")


@router.get("/webui/chatkit")
async def webui_chatkit_page():
    if not is_webui_enabled():
        raise HTTPException(status_code=404, detail="Not Found")
    return _serve_html("chatkit.html")


@router.get("/webui/images")
async def webui_images_page():
    return RedirectResponse("/admin/images")


@router.get("/webui/masonry")
async def webui_masonry_page():
    if not is_webui_enabled():
        raise HTTPException(status_code=404, detail="Not Found")
    return _serve_html("masonry.html")


__all__ = ["router"]
