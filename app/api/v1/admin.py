from fastapi import APIRouter, Depends, HTTPException, Request, Query
from fastapi.responses import HTMLResponse
from app.core.auth import verify_api_key, verify_app_key
from app.core.config import config, get_config
from app.core.storage import get_storage, LocalStorage, RedisStorage, SQLStorage
import os
from pathlib import Path
import aiofiles
import asyncio
from app.core.logger import logger


router = APIRouter()

TEMPLATE_DIR = Path(__file__).parent.parent.parent / "static"

async def render_template(filename: str):
    """渲染指定模板"""
    template_path = TEMPLATE_DIR / filename
    if not template_path.exists():
        return HTMLResponse(f"Template {filename} not found.", status_code=404)
    
    async with aiofiles.open(template_path, "r", encoding="utf-8") as f:
        content = await f.read()
    return HTMLResponse(content)

@router.get("/admin", response_class=HTMLResponse, include_in_schema=False)
async def admin_login_page():
    """管理后台登录页"""
    return await render_template("login/login.html")

@router.get("/admin/config", response_class=HTMLResponse, include_in_schema=False)
async def admin_config_page():
    """配置管理页"""
    return await render_template("config/config.html")

@router.get("/admin/token", response_class=HTMLResponse, include_in_schema=False)
async def admin_token_page():
    """Token 管理页"""
    return await render_template("token/token.html")

@router.post("/api/v1/admin/login", dependencies=[Depends(verify_app_key)])
async def admin_login_api():
    """管理后台登录验证（使用 app_key）"""
    return {"status": "success", "api_key": get_config("app.api_key", "")}

@router.get("/api/v1/admin/config", dependencies=[Depends(verify_api_key)])
async def get_config_api():
    """获取当前配置"""
    # 暴露原始配置字典
    return config._config

@router.post("/api/v1/admin/config", dependencies=[Depends(verify_api_key)])
async def update_config_api(data: dict):
    """更新配置"""
    try:
        await config.update(data)
        return {"status": "success", "message": "配置已更新"}
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))

@router.get("/api/v1/admin/storage", dependencies=[Depends(verify_api_key)])
async def get_storage_info():
    """获取当前存储模式"""
    storage_type = os.getenv("SERVER_STORAGE_TYPE", "local").lower()
    logger.info(f"Storage type: {storage_type}")
    if not storage_type:
        storage_type = str(get_config("storage.type", "")).lower()
    if not storage_type:
        storage = get_storage()
        if isinstance(storage, LocalStorage):
            storage_type = "local"
        elif isinstance(storage, RedisStorage):
            storage_type = "redis"
        elif isinstance(storage, SQLStorage):
            if storage.dialect in ("mysql", "mariadb"):
                storage_type = "mysql"
            elif storage.dialect in ("postgres", "postgresql", "pgsql"):
                storage_type = "pgsql"
            else:
                storage_type = storage.dialect
    return {"type": storage_type or "local"}

@router.get("/api/v1/admin/tokens", dependencies=[Depends(verify_api_key)])
async def get_tokens_api():
    """获取所有 Token"""
    storage = get_storage()
    tokens = await storage.load_tokens()
    return tokens or {}

@router.post("/api/v1/admin/tokens", dependencies=[Depends(verify_api_key)])
async def update_tokens_api(data: dict):
    """更新 Token 信息"""
    storage = get_storage()
    try:
        from app.services.token.manager import get_token_manager
        async with storage.acquire_lock("tokens_save", timeout=10):
            await storage.save_tokens(data)
            mgr = await get_token_manager()
            await mgr.reload()
        return {"status": "success", "message": "Token 已更新"}
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))

@router.post("/api/v1/admin/tokens/refresh", dependencies=[Depends(verify_api_key)])
async def refresh_tokens_api(data: dict):
    """刷新 Token 状态"""
    from app.services.token.manager import get_token_manager
    
    try:
        mgr = await get_token_manager()
        tokens = []
        if "token" in data:
            tokens.append(data["token"])
        if "tokens" in data and isinstance(data["tokens"], list):
            tokens.extend(data["tokens"])
            
        if not tokens:
             raise HTTPException(status_code=400, detail="No tokens provided")
             
        unique_tokens = list(set(tokens))
        
        sem = asyncio.Semaphore(10)
        
        async def _refresh_one(t):
            async with sem:
                return t, await mgr.sync_usage(t, "grok-3", consume_on_fail=False, is_usage=False)
        
        results_list = await asyncio.gather(*[_refresh_one(t) for t in unique_tokens])
        results = dict(results_list)
            
        return {"status": "success", "results": results}
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))

@router.get("/admin/cache", response_class=HTMLResponse, include_in_schema=False)
async def admin_cache_page():
    """缓存管理页"""
    return await render_template("cache/cache.html")

@router.get("/api/v1/admin/cache", dependencies=[Depends(verify_api_key)])
async def get_cache_stats_api(request: Request):
    """获取缓存统计"""
    from app.services.grok.assets import DownloadService, ListService
    from app.services.token.manager import get_token_manager
    
    try:
        dl_service = DownloadService()
        image_stats = dl_service.get_stats("image")
        video_stats = dl_service.get_stats("video")
        
        mgr = await get_token_manager()
        pools = mgr.pools
        accounts = []
        for pool_name, pool in pools.items():
            for info in pool.list():
                raw_token = info.token[4:] if info.token.startswith("sso=") else info.token
                masked = f"{raw_token[:8]}...{raw_token[-16:]}" if len(raw_token) > 24 else raw_token
                accounts.append({
                    "token": raw_token,
                    "token_masked": masked,
                    "pool": pool_name,
                    "status": info.status,
                    "last_asset_clear_at": info.last_asset_clear_at
                })

        scope = request.query_params.get("scope")
        selected_token = request.query_params.get("token")
        tokens_param = request.query_params.get("tokens")
        selected_tokens = []
        if tokens_param:
            selected_tokens = [t.strip() for t in tokens_param.split(",") if t.strip()]

        online_stats = {"count": 0, "status": "unknown", "token": None, "last_asset_clear_at": None}
        online_details = []
        account_map = {a["token"]: a for a in accounts}
        batch_size = get_config("performance.admin_assets_batch_size", 10)
        try:
            batch_size = int(batch_size)
        except Exception:
            batch_size = 10
        batch_size = max(1, batch_size)

        async def _fetch_assets(token: str):
            list_service = ListService()
            try:
                return await list_service.count(token)
            finally:
                await list_service.close()

        async def _fetch_detail(token: str):
            account = account_map.get(token)
            try:
                count = await _fetch_assets(token)
                return ({
                    "token": token,
                    "token_masked": account["token_masked"] if account else token,
                    "count": count,
                    "status": "ok",
                    "last_asset_clear_at": account["last_asset_clear_at"] if account else None
                }, count)
            except Exception as e:
                return ({
                    "token": token,
                    "token_masked": account["token_masked"] if account else token,
                    "count": 0,
                    "status": f"error: {str(e)}",
                    "last_asset_clear_at": account["last_asset_clear_at"] if account else None
                }, 0)

        if selected_tokens:
            total = 0
            for i in range(0, len(selected_tokens), batch_size):
                chunk = selected_tokens[i:i + batch_size]
                results = await asyncio.gather(*[_fetch_detail(token) for token in chunk])
                for detail, count in results:
                    online_details.append(detail)
                    total += count
            online_stats = {"count": total, "status": "ok" if selected_tokens else "no_token", "token": None, "last_asset_clear_at": None}
            scope = "selected"
        elif scope == "all":
            total = 0
            tokens = [account["token"] for account in accounts]
            for i in range(0, len(tokens), batch_size):
                chunk = tokens[i:i + batch_size]
                results = await asyncio.gather(*[_fetch_detail(token) for token in chunk])
                for detail, count in results:
                    online_details.append(detail)
                    total += count
            online_stats = {"count": total, "status": "ok" if accounts else "no_token", "token": None, "last_asset_clear_at": None}
        else:
            token = selected_token
            if token:
                try:
                    count = await _fetch_assets(token)
                    match = next((a for a in accounts if a["token"] == token), None)
                    online_stats = {
                        "count": count,
                        "status": "ok",
                        "token": token,
                        "token_masked": match["token_masked"] if match else token,
                        "last_asset_clear_at": match["last_asset_clear_at"] if match else None
                    }
                except Exception as e:
                    match = next((a for a in accounts if a["token"] == token), None)
                    online_stats = {
                        "count": 0,
                        "status": f"error: {str(e)}",
                        "token": token,
                        "token_masked": match["token_masked"] if match else token,
                        "last_asset_clear_at": match["last_asset_clear_at"] if match else None
                    }
            else:
                online_stats = {"count": 0, "status": "not_loaded", "token": None, "last_asset_clear_at": None}
            
        return {
            "local_image": image_stats,
            "local_video": video_stats,
            "online": online_stats,
            "online_accounts": accounts,
            "online_scope": scope or "none",
            "online_details": online_details
        }
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))

@router.post("/api/v1/admin/cache/clear", dependencies=[Depends(verify_api_key)])
async def clear_local_cache_api(data: dict):
    """清理本地缓存"""
    from app.services.grok.assets import DownloadService
    cache_type = data.get("type", "image")
    
    try:
        dl_service = DownloadService()
        result = dl_service.clear(cache_type)
        return {"status": "success", "result": result}
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))

@router.get("/api/v1/admin/cache/list", dependencies=[Depends(verify_api_key)])
async def list_local_cache_api(
    cache_type: str = "image",
    type_: str = Query(default=None, alias="type"),
    page: int = 1,
    page_size: int = 1000
):
    """列出本地缓存文件"""
    from app.services.grok.assets import DownloadService
    try:
        if type_:
            cache_type = type_
        dl_service = DownloadService()
        result = dl_service.list_files(cache_type, page, page_size)
        return {"status": "success", **result}
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))

@router.post("/api/v1/admin/cache/item/delete", dependencies=[Depends(verify_api_key)])
async def delete_local_cache_item_api(data: dict):
    """删除单个本地缓存文件"""
    from app.services.grok.assets import DownloadService
    cache_type = data.get("type", "image")
    name = data.get("name")
    if not name:
        raise HTTPException(status_code=400, detail="Missing file name")
    try:
        dl_service = DownloadService()
        result = dl_service.delete_file(cache_type, name)
        return {"status": "success", "result": result}
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))

@router.post("/api/v1/admin/cache/online/clear", dependencies=[Depends(verify_api_key)])
async def clear_online_cache_api(data: dict):
    """清理在线缓存"""
    from app.services.grok.assets import DeleteService
    from app.services.token.manager import get_token_manager
    
    delete_service = None
    try:
        mgr = await get_token_manager()
        tokens = data.get("tokens")
        delete_service = DeleteService()

        if isinstance(tokens, list):
            token_list = [t.strip() for t in tokens if isinstance(t, str) and t.strip()]
            if not token_list:
                raise HTTPException(status_code=400, detail="No tokens provided")

            results = {}
            batch_size = get_config("performance.admin_assets_batch_size", 10)
            try:
                batch_size = int(batch_size)
            except Exception:
                batch_size = 10
            batch_size = max(1, batch_size)

            async def _clear_one(t: str):
                try:
                    result = await delete_service.delete_all(t)
                    await mgr.mark_asset_clear(t)
                    return t, {"status": "success", "result": result}
                except Exception as e:
                    return t, {"status": "error", "error": str(e)}

            for i in range(0, len(token_list), batch_size):
                chunk = token_list[i:i + batch_size]
                res_list = await asyncio.gather(*[_clear_one(t) for t in chunk])
                for t, res in res_list:
                    results[t] = res

            return {"status": "success", "results": results}

        token = data.get("token") or mgr.get_token()
        if not token:
            raise HTTPException(status_code=400, detail="No available token to perform cleanup")

        result = await delete_service.delete_all(token)
        await mgr.mark_asset_clear(token)
        return {"status": "success", "result": result}
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))
    finally:
        if delete_service:
            await delete_service.close()
