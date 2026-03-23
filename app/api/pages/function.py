from pathlib import Path

from fastapi import APIRouter, HTTPException
from fastapi.responses import FileResponse, RedirectResponse

from app.api.pages.admin import _admin_entry_response
from app.core.auth import is_function_enabled

router = APIRouter()
STATIC_DIR = Path(__file__).resolve().parents[3] / "_public" / "static"


def _function_page_response(relative_path: str) -> FileResponse:
    file_path = STATIC_DIR / relative_path
    if not file_path.exists():
        raise HTTPException(status_code=404, detail="Page not found")
    return FileResponse(file_path)


@router.get("/", include_in_schema=False)
async def root():
    if is_function_enabled():
        return RedirectResponse(url="/login")
    return _admin_entry_response()


@router.get("/login", include_in_schema=False)
async def function_login():
    if not is_function_enabled():
        raise HTTPException(status_code=404, detail="Not Found")
    return _function_page_response("function/pages/login.html")


@router.get("/imagine", include_in_schema=False)
async def function_imagine():
    if not is_function_enabled():
        raise HTTPException(status_code=404, detail="Not Found")
    return _function_page_response("function/pages/imagine.html")


@router.get("/voice", include_in_schema=False)
async def function_voice():
    if not is_function_enabled():
        raise HTTPException(status_code=404, detail="Not Found")
    return _function_page_response("function/pages/voice.html")


@router.get("/video", include_in_schema=False)
async def function_video():
    if not is_function_enabled():
        raise HTTPException(status_code=404, detail="Not Found")
    return _function_page_response("function/pages/video.html")


@router.get("/chat", include_in_schema=False)
async def function_chat():
    if not is_function_enabled():
        raise HTTPException(status_code=404, detail="Not Found")
    return _function_page_response("function/pages/chat.html")
