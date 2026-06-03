"""Web product — unified pages + API for the statics-based frontend."""

from pathlib import Path

from fastapi import APIRouter, Depends, HTTPException, Query
from fastapi.responses import FileResponse, HTMLResponse, RedirectResponse

from app.platform.auth.middleware import is_webui_enabled, verify_webui_key
from app.platform.meta import get_project_version
from app.platform.update_check import get_latest_release_info
from .static_html import serve_static_html
from .admin import router as admin_api_router
from .webui import router as webui_router

router = APIRouter()

# Mount admin API sub-router (/admin/api/*)
router.include_router(admin_api_router)
router.include_router(webui_router)

_DIR = Path(__file__).resolve().parents[2] / "statics"


def _serve(path: str) -> FileResponse:
    f = _DIR / path
    if not f.exists():
        raise HTTPException(404, "Page not found")
    return FileResponse(f)


def _serve_html(path: str):
    return serve_static_html(_DIR / path)


@router.get("/", include_in_schema=False)
async def root():
    return RedirectResponse("/admin")


# --- Admin pages ---
@router.get("/admin", include_in_schema=False)
async def admin_root():
    return RedirectResponse("/admin/login")

@router.get("/admin/login", include_in_schema=False)
async def admin_login():
    return _serve_html("admin/login.html")

@router.get("/admin/account", include_in_schema=False)
async def admin_account():
    return _serve_html("admin/account.html")

@router.get("/admin/config", include_in_schema=False)
async def admin_config():
    return _serve_html("admin/config.html")

@router.get("/admin/cache", include_in_schema=False)
async def admin_cache():
    return _serve_html("admin/cache.html")


# --- Legacy xAI OAuth callback (deprecated) ---
@router.get("/admin/xai/callback", include_in_schema=False)
async def admin_xai_callback_legacy():
    """OAuth no longer uses ``app.app_url``; redirect users to the admin UI."""
    html = """<!doctype html><html lang="zh"><head><meta charset="utf-8">
<title>xAI OAuth</title><meta name="viewport" content="width=device-width,initial-scale=1">
<style>body{font-family:system-ui,sans-serif;background:#0b0f17;color:#e5e7eb;
display:flex;align-items:center;justify-content:center;height:100vh;margin:0}
.card{background:#111827;border:1px solid #1f2937;border-radius:14px;padding:32px 40px;
max-width:480px;text-align:center}.m{color:#9ca3af;font-size:14px;line-height:1.7}
.b{margin-top:20px;display:inline-block;padding:8px 18px;border-radius:8px;
background:#2563eb;color:#fff;text-decoration:none;font-size:14px}</style></head>
<body><div class="card"><div class="m">xAI OAuth 已改为本机回环地址
<code style="color:#e5e7eb">http://127.0.0.1:56121/callback</code>。
请在管理后台「账号管理」点击「登录 xAI」；若浏览器未自动跳回，请使用「手动粘贴」。</div>
<a class="b" href="/admin/account">返回账号管理</a></div></body></html>"""
    return HTMLResponse(content=html, status_code=200)


# --- WebUI ---
@router.get("/webui", include_in_schema=False)
async def webui_root():
    return RedirectResponse("/webui/login")

@router.get("/webui/login", include_in_schema=False)
async def webui_login():
    if not is_webui_enabled():
        raise HTTPException(404, "Not Found")
    return _serve_html("webui/login.html")

@router.get("/webui/api/verify", dependencies=[Depends(verify_webui_key)], tags=["WebUI - System"])
async def webui_verify():
    return {"status": "ok"}


@router.get("/meta", include_in_schema=False)
async def app_meta():
    return {"version": get_project_version()}


@router.get("/meta/update", include_in_schema=False)
async def app_update_meta(force: bool = Query(False)):
    return await get_latest_release_info(force=force)
