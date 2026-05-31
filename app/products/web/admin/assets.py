"""Admin online-asset management — list, delete, clear per token."""

import asyncio
from typing import TYPE_CHECKING

import orjson
from fastapi import APIRouter, Depends
from fastapi.responses import Response
from pydantic import BaseModel

from app.control.account.commands import ListAccountsQuery
from app.control.account.invalid_credentials import mark_account_invalid_credentials
from app.control.account.state_machine import is_manageable
from app.platform.errors import UpstreamError
from app.platform.logging.logger import logger

if TYPE_CHECKING:
    from app.control.account.repository import AccountRepository

from . import get_repo

router = APIRouter(prefix="/assets", tags=["Admin - Assets"])


def _mask(token: str) -> str:
    return f"{token[:8]}...{token[-8:]}" if len(token) > 20 else token


def _asset_row(token: str, items: list[dict], *, error: str | None = None) -> dict:
    return {
        "token":  token,
        "masked": _mask(token),
        "count":  len(items),
        "assets": [
            {
                "id":           item.get("id") or item.get("assetId") or "",
                "name":         item.get("fileName") or item.get("name") or "",
                "file_path":    item.get("filePath") or item.get("file_path") or "",
                "content_type": item.get("contentType") or item.get("content_type") or "",
                "size":         item.get("fileSize") or item.get("size") or 0,
                "created_at":   item.get("createdAt") or item.get("created_at") or "",
            }
            for item in items
        ],
        "error": error,
    }


async def _list_all_tokens(repo: "AccountRepository") -> list[str]:
    page_num, tokens = 1, []
    while True:
        page = await repo.list_accounts(ListAccountsQuery(page=page_num, page_size=2000))
        tokens.extend(r.token for r in page.items if is_manageable(r))
        if page_num * 2000 >= page.total:
            break
        page_num += 1
    return tokens


class DeleteItemRequest(BaseModel):
    token:    str
    asset_id: str


class ClearTokenRequest(BaseModel):
    token: str


@router.get("")
async def list_all_assets(repo: "AccountRepository" = Depends(get_repo)):
    """Fetch asset lists for all tokens concurrently."""
    from app.dataplane.reverse.transport.assets import list_assets

    tokens = await _list_all_tokens(repo)
    if not tokens:
        return Response(
            content=orjson.dumps({"tokens": [], "total_assets": 0}),
            media_type="application/json",
        )

    async def _fetch_row(token: str) -> dict:
        try:
            resp = await list_assets(token)
        except Exception as exc:
            await mark_account_invalid_credentials(repo, token, exc, source="asset list")
            return _asset_row(token, [], error=str(exc))

        items = resp.get("assets", resp.get("items", []))
        return _asset_row(token, items)

    results = await asyncio.gather(*[_fetch_row(t) for t in tokens])
    total = sum(r["count"] for r in results)
    return Response(
        content=orjson.dumps({"tokens": list(results), "total_assets": total}),
        media_type="application/json",
    )


@router.post("/delete-item")
async def delete_item(req: DeleteItemRequest, repo: "AccountRepository" = Depends(get_repo)):
    """Delete a single asset by token + asset_id."""
    from app.dataplane.reverse.transport.assets import delete_asset

    try:
        await delete_asset(req.token, req.asset_id)
        return {"status": "success"}
    except Exception as exc:
        await mark_account_invalid_credentials(repo, req.token, exc, source="asset delete")
        raise UpstreamError(str(exc)) from exc


@router.post("/clear-token")
async def clear_token_assets(req: ClearTokenRequest, repo: "AccountRepository" = Depends(get_repo)):
    """Delete all assets for one token concurrently."""
    from app.dataplane.reverse.transport.assets import delete_asset, list_assets

    try:
        resp = await list_assets(req.token)
        items = resp.get("assets", resp.get("items", []))

        async def _delete_one(item: dict) -> int:
            asset_id = item.get("id") or item.get("assetId")
            if not asset_id:
                return 0
            await delete_asset(req.token, asset_id)
            return 1

        results = await asyncio.gather(*[_delete_one(item) for item in items], return_exceptions=True)
        for result in results:
            if not isinstance(result, Exception):
                continue
            if await mark_account_invalid_credentials(repo, req.token, result, source="asset clear"):
                raise result
        deleted = sum(result for result in results if isinstance(result, int))
        return {"status": "success", "deleted": deleted}
    except Exception as exc:
        await mark_account_invalid_credentials(repo, req.token, exc, source="asset clear")
        raise UpstreamError(str(exc)) from exc


@router.post("/cleanup-assets")
async def trigger_cleanup(repo: "AccountRepository" = Depends(get_repo)):
    """Manually trigger stale asset cleanup (runs async, returns immediately)."""
    from app.control.asset_cleanup import run_cleanup_once

    async def _run():
        try:
            result = await run_cleanup_once(lambda: repo)
            logger.info(
                "manual asset cleanup done: scanned={} found={} deleted={} failed={}",
                result.scanned, result.total_assets, result.deleted, result.failed,
            )
        except Exception as exc:
            logger.error("manual asset cleanup failed: error={}", exc)

    asyncio.create_task(_run())
    return Response(
        content=orjson.dumps({"status": "started"}),
        media_type="application/json",
    )


__all__ = ["router"]
