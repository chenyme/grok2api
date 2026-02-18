"""
Batch assets service.
"""

import asyncio
from typing import Dict, List, Optional

from app.core.config import get_config
from app.core.logger import logger
from app.services.reverse.assets_list import AssetsListReverse
from app.services.reverse.assets_delete import AssetsDeleteReverse
from app.services.reverse.utils.session import ResettableSession
from app.core.batch import run_batch


class BaseAssetsService:
    """Base assets service."""

    def __init__(self):
        self._session: Optional[ResettableSession] = None

    async def _get_session(self) -> ResettableSession:
        if self._session is None:
            self._session = ResettableSession()
        return self._session

    async def close(self):
        if self._session:
            await self._session.close()
            self._session = None


_LIST_SEMAPHORE = None
_LIST_SEM_VALUE = None
_DELETE_SEMAPHORE = None
_DELETE_SEM_VALUE = None


def _get_list_semaphore() -> asyncio.Semaphore:
    value = max(1, int(get_config("asset.list_concurrent")))
    global _LIST_SEMAPHORE, _LIST_SEM_VALUE
    if _LIST_SEMAPHORE is None or value != _LIST_SEM_VALUE:
        _LIST_SEM_VALUE = value
        _LIST_SEMAPHORE = asyncio.Semaphore(value)
    return _LIST_SEMAPHORE


def _get_delete_semaphore() -> asyncio.Semaphore:
    value = max(1, int(get_config("asset.delete_concurrent")))
    global _DELETE_SEMAPHORE, _DELETE_SEM_VALUE
    if _DELETE_SEMAPHORE is None or value != _DELETE_SEM_VALUE:
        _DELETE_SEM_VALUE = value
        _DELETE_SEMAPHORE = asyncio.Semaphore(value)
    return _DELETE_SEMAPHORE


class ListService(BaseAssetsService):
    """Assets list service."""

    async def list(self, token: str) -> Dict[str, List[str] | int]:
        params = {
            "pageSize": 50,
            "orderBy": "ORDER_BY_LAST_USE_TIME",
            "source": "SOURCE_ANY",
            "isLatest": "true",
        }
        page_token = None
        seen_tokens = set()
        asset_ids: List[str] = []
        session = await self._get_session()
        while True:
            if page_token:
                if page_token in seen_tokens:
                    logger.warning("Pagination stopped: repeated page token")
                    break
                seen_tokens.add(page_token)
                params["pageToken"] = page_token
            else:
                params.pop("pageToken", None)

            async with _get_list_semaphore():
                response = await AssetsListReverse.request(
                    session,
                    token,
                    params,
                )

            result = response.json()
            page_assets = result.get("assets", [])
            if page_assets:
                for asset in page_assets:
                    asset_id = asset.get("assetId")
                    if asset_id:
                        asset_ids.append(asset_id)

            page_token = result.get("nextPageToken")
            if not page_token:
                break

        logger.info(f"List success: {len(asset_ids)} files")
        return {"asset_ids": asset_ids, "count": len(asset_ids)}

    @staticmethod
    async def fetch_assets_details(
        tokens: List[str],
        account_map: dict,
        *,
        include_ok: bool = False,
        on_item=None,
        should_cancel=None,
    ) -> dict:
        """Batch fetch assets details for tokens."""
        account_map = account_map or {}
        shared_service = ListService()
        batch_size = max(1, int(get_config("asset.list_batch_size")))

        async def _fetch_detail(token: str):
            account = account_map.get(token)
            try:
                result = await shared_service.list(token)
                asset_ids = result.get("asset_ids", [])
                count = result.get("count", len(asset_ids))
                detail = {
                    "token": token,
                    "token_masked": account["token_masked"] if account else token,
                    "count": count,
                    "status": "ok",
                    "last_asset_clear_at": account["last_asset_clear_at"]
                    if account
                    else None,
                }
                if include_ok:
                    return {"ok": True, "detail": detail, "count": count}
                return {"detail": detail, "count": count}
            except Exception as e:
                detail = {
                    "token": token,
                    "token_masked": account["token_masked"] if account else token,
                    "count": 0,
                    "status": f"error: {str(e)}",
                    "last_asset_clear_at": account["last_asset_clear_at"]
                    if account
                    else None,
                }
                if include_ok:
                    return {"ok": False, "detail": detail, "count": 0}
                return {"detail": detail, "count": 0}

        try:
            return await run_batch(
                tokens,
                _fetch_detail,
                batch_size=batch_size,
                on_item=on_item,
                should_cancel=should_cancel,
            )
        finally:
            await shared_service.close()


class DeleteService(BaseAssetsService):
    """Assets delete service."""

    async def delete(self, token: str, asset_ids: List[str]) -> Dict[str, int]:
        if not asset_ids:
            logger.info("No assets to delete")
            return {"total": 0, "success": 0, "failed": 0, "skipped": True}

        total = len(asset_ids)
        success = 0
        failed = 0
        session = await self._get_session()

        async def _delete_one(asset_id: str):
            async with _get_delete_semaphore():
                await AssetsDeleteReverse.request(session, token, asset_id)

        tasks = [_delete_one(asset_id) for asset_id in asset_ids if asset_id]
        results = await asyncio.gather(*tasks, return_exceptions=True)
        for res in results:
            if isinstance(res, Exception):
                failed += 1
            else:
                success += 1

        logger.info(f"Delete all: total={total}, success={success}, failed={failed}")
        return {"total": total, "success": success, "failed": failed}

    @staticmethod
    async def clear_assets(
        tokens: List[str],
        mgr,
        *,
        include_ok: bool = False,
        on_item=None,
        should_cancel=None,
    ) -> dict:
        """Batch clear assets for tokens."""
        delete_service = DeleteService()
        list_service = ListService()
        batch_size = max(1, int(get_config("asset.delete_batch_size")))

        async def _clear_one(token: str):
            try:
                result = await list_service.list(token)
                asset_ids = result.get("asset_ids", [])
                result = await delete_service.delete(token, asset_ids)
                await mgr.mark_asset_clear(token)
                if include_ok:
                    return {"ok": True, "result": result}
                return {"status": "success", "result": result}
            except Exception as e:
                if include_ok:
                    return {"ok": False, "error": str(e)}
                return {"status": "error", "error": str(e)}

        try:
            return await run_batch(
                tokens,
                _clear_one,
                batch_size=batch_size,
                on_item=on_item,
                should_cancel=should_cancel,
            )
        finally:
            await delete_service.close()
            await list_service.close()


__all__ = ["ListService", "DeleteService"]
