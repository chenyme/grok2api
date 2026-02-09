"""
Health and readiness endpoints.
"""

from fastapi import APIRouter
from fastapi.responses import JSONResponse

from app.core.storage import get_storage

router = APIRouter(tags=["Health"])


@router.get("/health", include_in_schema=False)
async def health():
    return {"status": "ok"}


@router.get("/ready", include_in_schema=False)
async def ready():
    storage = get_storage()
    ok = True
    try:
        ok = await storage.verify_connection()
    except Exception:
        ok = False

    payload = {"status": "ok" if ok else "degraded", "storage_ok": bool(ok)}
    if ok:
        return payload
    return JSONResponse(status_code=503, content=payload)


__all__ = ["router"]
