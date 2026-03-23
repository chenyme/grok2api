from pathlib import Path

from fastapi import APIRouter, HTTPException
from fastapi.responses import FileResponse, HTMLResponse

router = APIRouter()
STATIC_DIR = Path(__file__).resolve().parents[3] / "_public" / "static"
ADMIN_KEY_STORAGE = "grok2api_app_key"


def _admin_page_response(relative_path: str) -> FileResponse:
    file_path = STATIC_DIR / relative_path
    if not file_path.exists():
        raise HTTPException(status_code=404, detail="Page not found")
    return FileResponse(file_path)


def _admin_entry_response() -> HTMLResponse:
    return HTMLResponse(
        f"""<!DOCTYPE html>
<html lang="zh-CN">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <meta name="robots" content="noindex">
  <title>Grok2API</title>
  <script>
    (() => {{
      const fallbackUrl = '/admin/login';
      const authedUrl = '/admin/token';
      try {{
        const stored = (window.localStorage.getItem('{ADMIN_KEY_STORAGE}') || '').trim();
        window.location.replace(stored ? authedUrl : fallbackUrl);
      }} catch (error) {{
        window.location.replace(fallbackUrl);
      }}
    }})();
  </script>
</head>
<body></body>
</html>"""
    )


@router.get("/admin", include_in_schema=False)
async def admin_root():
    return _admin_entry_response()


@router.get("/admin/login", include_in_schema=False)
async def admin_login():
    return _admin_page_response("admin/pages/login.html")


@router.get("/admin/config", include_in_schema=False)
async def admin_config():
    return _admin_page_response("admin/pages/config.html")


@router.get("/admin/cache", include_in_schema=False)
async def admin_cache():
    return _admin_page_response("admin/pages/cache.html")


@router.get("/admin/token", include_in_schema=False)
async def admin_token():
    return _admin_page_response("admin/pages/token.html")
