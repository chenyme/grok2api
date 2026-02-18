from typing import List

from fastapi import APIRouter, Depends, HTTPException, Query, Request

from app.core.auth import verify_app_key
from app.core.batch import create_task, expire_task
from app.services.grok.batch_services.assets import ListService, DeleteService
from app.services.token.manager import get_token_manager
router = APIRouter()


@router.get("/cache", dependencies=[Depends(verify_app_key)])
async def cache_stats(request: Request):
    """获取缓存统计"""
    from app.services.grok.utils.cache import CacheService

    try:
        cache_service = CacheService()
        image_stats = cache_service.get_stats("image")
        video_stats = cache_service.get_stats("video")

        mgr = await get_token_manager()
        pools = mgr.pools
        accounts = []
        for pool_name, pool in pools.items():
            for info in pool.list():
                raw_token = (
                    info.token[4:] if info.token.startswith("sso=") else info.token
                )
                masked = (
                    f"{raw_token[:8]}...{raw_token[-16:]}"
                    if len(raw_token) > 24
                    else raw_token
                )
                accounts.append(
                    {
                        "token": raw_token,
                        "token_masked": masked,
                        "pool": pool_name,
                        "status": info.status,
                        "last_asset_clear_at": info.last_asset_clear_at,
                    }
                )

        scope = request.query_params.get("scope")
        selected_token = request.query_params.get("token")
        tokens_param = request.query_params.get("tokens")
        selected_tokens = []
        if tokens_param:
            selected_tokens = [t.strip() for t in tokens_param.split(",") if t.strip()]

        online_stats = {
            "count": 0,
            "status": "unknown",
            "token": None,
            "last_asset_clear_at": None,
        }
        online_details = []
        account_map = {a["token"]: a for a in accounts}
        if selected_tokens:
            total = 0
            raw_results = await ListService.fetch_assets_details(
                selected_tokens,
                account_map,
            )
            for token, res in raw_results.items():
                if res.get("ok"):
                    data = res.get("data", {})
                    detail = data.get("detail")
                    total += data.get("count", 0)
                else:
                    account = account_map.get(token)
                    detail = {
                        "token": token,
                        "token_masked": account["token_masked"] if account else token,
                        "count": 0,
                        "status": f"error: {res.get('error')}",
                        "last_asset_clear_at": account["last_asset_clear_at"]
                        if account
                        else None,
                    }
                if detail:
                    online_details.append(detail)
            online_stats = {
                "count": total,
                "status": "ok" if selected_tokens else "no_token",
                "token": None,
                "last_asset_clear_at": None,
            }
            scope = "selected"
        elif scope == "all":
            total = 0
            tokens = list(dict.fromkeys([account["token"] for account in accounts]))
            raw_results = await ListService.fetch_assets_details(
                tokens,
                account_map,
            )
            for token, res in raw_results.items():
                if res.get("ok"):
                    data = res.get("data", {})
                    detail = data.get("detail")
                    total += data.get("count", 0)
                else:
                    account = account_map.get(token)
                    detail = {
                        "token": token,
                        "token_masked": account["token_masked"] if account else token,
                        "count": 0,
                        "status": f"error: {res.get('error')}",
                        "last_asset_clear_at": account["last_asset_clear_at"]
                        if account
                        else None,
                    }
                if detail:
                    online_details.append(detail)
            online_stats = {
                "count": total,
                "status": "ok" if accounts else "no_token",
                "token": None,
                "last_asset_clear_at": None,
            }
        else:
            token = selected_token
            if token:
                raw_results = await ListService.fetch_assets_details(
                    [token],
                    account_map,
                )
                res = raw_results.get(token, {})
                data = res.get("data", {})
                detail = data.get("detail") if res.get("ok") else None
                if detail:
                    online_stats = {
                        "count": data.get("count", 0),
                        "status": detail.get("status", "ok"),
                        "token": detail.get("token"),
                        "token_masked": detail.get("token_masked"),
                        "last_asset_clear_at": detail.get("last_asset_clear_at"),
                    }
                else:
                    match = next((a for a in accounts if a["token"] == token), None)
                    online_stats = {
                        "count": 0,
                        "status": f"error: {res.get('error')}",
                        "token": token,
                        "token_masked": match["token_masked"] if match else token,
                        "last_asset_clear_at": match["last_asset_clear_at"]
                        if match
                        else None,
                    }
            else:
                online_stats = {
                    "count": 0,
                    "status": "not_loaded",
                    "token": None,
                    "last_asset_clear_at": None,
                }

        response = {
            "local_image": image_stats,
            "local_video": video_stats,
            "online": online_stats,
            "online_accounts": accounts,
            "online_scope": scope or "none",
            "online_details": online_details,
        }
        return response
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@router.get("/cache/list", dependencies=[Depends(verify_app_key)])
async def list_local(
    cache_type: str = "image",
    type_: str = Query(default=None, alias="type"),
    page: int = 1,
    page_size: int = 1000,
):
    """列出本地缓存文件"""
    from app.services.grok.utils.cache import CacheService

    try:
        if type_:
            cache_type = type_
        cache_service = CacheService()
        result = cache_service.list_files(cache_type, page, page_size)
        return {"status": "success", **result}
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@router.post("/cache/clear", dependencies=[Depends(verify_app_key)])
async def clear_local(data: dict):
    """清理本地缓存"""
    from app.services.grok.utils.cache import CacheService

    cache_type = data.get("type", "image")

    try:
        cache_service = CacheService()
        result = cache_service.clear(cache_type)
        return {"status": "success", "result": result}
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@router.post("/cache/item/delete", dependencies=[Depends(verify_app_key)])
async def delete_local_item(data: dict):
    """删除单个本地缓存文件"""
    from app.services.grok.utils.cache import CacheService

    cache_type = data.get("type", "image")
    name = data.get("name")
    if not name:
        raise HTTPException(status_code=400, detail="Missing file name")
    try:
        cache_service = CacheService()
        result = cache_service.delete_file(cache_type, name)
        return {"status": "success", "result": result}
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@router.post("/cache/online/clear", dependencies=[Depends(verify_app_key)])
async def clear_online(data: dict):
    """清理在线缓存"""
    try:
        mgr = await get_token_manager()
        tokens = data.get("tokens")

        if isinstance(tokens, list):
            token_list = [t.strip() for t in tokens if isinstance(t, str) and t.strip()]
            if not token_list:
                raise HTTPException(status_code=400, detail="No tokens provided")

            token_list = list(dict.fromkeys(token_list))

            results = {}
            raw_results = await DeleteService.clear_assets(
                token_list,
                mgr,
            )
            for token, res in raw_results.items():
                if res.get("ok"):
                    results[token] = res.get("data", {})
                else:
                    results[token] = {"status": "error", "error": res.get("error")}

            return {"status": "success", "results": results}

        token = data.get("token") or mgr.get_token()
        if not token:
            raise HTTPException(
                status_code=400, detail="No available token to perform cleanup"
            )

        raw_results = await DeleteService.clear_assets(
            [token],
            mgr,
        )
        res = raw_results.get(token, {})
        data = res.get("data", {})
        if res.get("ok") and data.get("status") == "success":
            return {"status": "success", "result": data.get("result")}
        return {"status": "error", "error": data.get("error") or res.get("error")}
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@router.post("/cache/online/clear/async", dependencies=[Depends(verify_app_key)])
async def clear_online_async(data: dict):
    """清理在线缓存（异步批量 + SSE 进度）"""
    mgr = await get_token_manager()
    tokens = data.get("tokens")
    if not isinstance(tokens, list):
        raise HTTPException(status_code=400, detail="No tokens provided")

    token_list = [t.strip() for t in tokens if isinstance(t, str) and t.strip()]
    if not token_list:
        raise HTTPException(status_code=400, detail="No tokens provided")

    task = create_task(len(token_list))

    async def _run():
        try:
            async def _on_item(item: str, res: dict):
                ok = bool(res.get("data", {}).get("ok"))
                task.record(ok)

            raw_results = await DeleteService.clear_assets(
                token_list,
                mgr,
                include_ok=True,
                on_item=_on_item,
                should_cancel=lambda: task.cancelled,
            )

            if task.cancelled:
                task.finish_cancelled()
                return

            results = {}
            ok_count = 0
            fail_count = 0
            for token, res in raw_results.items():
                data = res.get("data", {})
                if data.get("ok"):
                    ok_count += 1
                    results[token] = {"status": "success", "result": data.get("result")}
                else:
                    fail_count += 1
                    results[token] = {"status": "error", "error": data.get("error")}

            result = {
                "status": "success",
                "summary": {
                    "total": len(token_list),
                    "ok": ok_count,
                    "fail": fail_count,
                },
                "results": results,
            }
            task.finish(result)
        except Exception as e:
            task.fail_task(str(e))
        finally:
            import asyncio
            asyncio.create_task(expire_task(task.id, 300))

    import asyncio
    asyncio.create_task(_run())

    return {
        "status": "success",
        "task_id": task.id,
        "total": len(token_list),
    }


@router.post("/cache/online/load/async", dependencies=[Depends(verify_app_key)])
async def load_cache_async(data: dict):
    """在线资产统计（异步批量 + SSE 进度）"""
    from app.services.grok.utils.cache import CacheService

    mgr = await get_token_manager()

    accounts = []
    for pool_name, pool in mgr.pools.items():
        for info in pool.list():
            raw_token = info.token[4:] if info.token.startswith("sso=") else info.token
            masked = (
                f"{raw_token[:8]}...{raw_token[-16:]}"
                if len(raw_token) > 24
                else raw_token
            )
            accounts.append(
                {
                    "token": raw_token,
                    "token_masked": masked,
                    "pool": pool_name,
                    "status": info.status,
                    "last_asset_clear_at": info.last_asset_clear_at,
                }
            )

    account_map = {a["token"]: a for a in accounts}

    tokens = data.get("tokens")
    scope = data.get("scope")
    selected_tokens: List[str] = []
    if isinstance(tokens, list):
        selected_tokens = [str(t).strip() for t in tokens if str(t).strip()]

    if not selected_tokens and scope == "all":
        selected_tokens = [account["token"] for account in accounts]
        scope = "all"
    elif selected_tokens:
        scope = "selected"
    else:
        raise HTTPException(status_code=400, detail="No tokens provided")

    task = create_task(len(selected_tokens))

    async def _run():
        try:
            cache_service = CacheService()
            image_stats = cache_service.get_stats("image")
            video_stats = cache_service.get_stats("video")

            async def _on_item(item: str, res: dict):
                ok = bool(res.get("data", {}).get("ok"))
                task.record(ok)

            raw_results = await ListService.fetch_assets_details(
                selected_tokens,
                account_map,
                include_ok=True,
                on_item=_on_item,
                should_cancel=lambda: task.cancelled,
            )

            if task.cancelled:
                task.finish_cancelled()
                return

            online_details = []
            total = 0
            for token, res in raw_results.items():
                data = res.get("data", {})
                detail = data.get("detail")
                if detail:
                    online_details.append(detail)
                total += data.get("count", 0)

            online_stats = {
                "count": total,
                "status": "ok" if selected_tokens else "no_token",
                "token": None,
                "last_asset_clear_at": None,
            }

            result = {
                "local_image": image_stats,
                "local_video": video_stats,
                "online": online_stats,
                "online_accounts": accounts,
                "online_scope": scope or "none",
                "online_details": online_details,
            }
            task.finish(result)
        except Exception as e:
            task.fail_task(str(e))
        finally:
            import asyncio
            asyncio.create_task(expire_task(task.id, 300))

    import asyncio
    asyncio.create_task(_run())

    return {
        "status": "success",
        "task_id": task.id,
        "total": len(selected_tokens),
    }

