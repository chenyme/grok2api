from fastapi import APIRouter, Depends, HTTPException, Query

from app.core.auth import verify_app_key
from app.core.log_viewer import (
    LogViewerError,
    delete_log_files,
    list_log_files,
    read_log_entries,
)

router = APIRouter()


@router.get("/logs/files", dependencies=[Depends(verify_app_key)])
async def get_log_files():
    return {"files": list_log_files()}


@router.get("/logs", dependencies=[Depends(verify_app_key)])
async def get_logs(
    file: str,
    limit: int = Query(default=200, ge=1, le=1000),
    level: str | None = Query(default=None),
    keyword: str | None = Query(default=None),
    exclude_admin_routes: bool = Query(default=False),
):
    try:
        exclude_prefixes = ["/v1/admin/"] if exclude_admin_routes else []
        return read_log_entries(
            file,
            limit=limit,
            level=level,
            keyword=keyword,
            exclude_prefixes=exclude_prefixes,
        )
    except FileNotFoundError as exc:
        raise HTTPException(
            status_code=404,
            detail=f"Log file not found: {exc.args[0]}",
        )
    except LogViewerError as exc:
        raise HTTPException(status_code=400, detail=str(exc))


@router.post("/logs/delete", dependencies=[Depends(verify_app_key)])
async def remove_logs(payload: dict):
    try:
        files = payload.get("files") or []
        if not isinstance(files, list) or not files:
            raise HTTPException(status_code=400, detail="No log files selected")
        return delete_log_files([str(item) for item in files])
    except LogViewerError as exc:
        raise HTTPException(status_code=400, detail=str(exc))
