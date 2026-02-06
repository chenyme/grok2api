from fastapi import APIRouter, Depends, HTTPException, Request, Query
from fastapi.responses import HTMLResponse, StreamingResponse
from pydantic import BaseModel
from app.core.auth import verify_api_key, verify_app_key, get_admin_api_key
from app.core.config import config, get_config
from app.core.batch_tasks import create_task, get_task, expire_task
from app.core.storage import get_storage, LocalStorage, RedisStorage, SQLStorage
from app.core.exceptions import AppException
import os
from pathlib import Path
import aiofiles
import asyncio
import orjson
from app.core.logger import logger
from app.services.grok.services.voice import VoiceService
from app.services.token import get_token_manager


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


def _sse_event(payload: dict) -> str:
    return f"data: {orjson.dumps(payload).decode()}\n\n"


def _verify_stream_api_key(request: Request) -> None:
    api_key = get_admin_api_key()
    if not api_key:
        return
    key = request.query_params.get("api_key")
    if key != api_key:
        raise HTTPException(status_code=401, detail="Invalid authentication token")


@router.get("/api/v1/admin/batch/{task_id}/stream")
async def stream_batch(task_id: str, request: Request):
    _verify_stream_api_key(request)
    task = get_task(task_id)
    if not task:
        raise HTTPException(status_code=404, detail="Task not found")

    async def event_stream():
        queue = task.attach()
        try:
            yield _sse_event({"type": "snapshot", **task.snapshot()})

            final = task.final_event()
            if final:
                yield _sse_event(final)
                return

            while True:
                try:
                    event = await asyncio.wait_for(queue.get(), timeout=15)
                except asyncio.TimeoutError:
                    yield ": ping\n\n"
                    final = task.final_event()
                    if final:
                        yield _sse_event(final)
                        return
                    continue

                yield _sse_event(event)
                if event.get("type") in ("done", "error", "cancelled"):
                    return
        finally:
            task.detach(queue)

    return StreamingResponse(event_stream(), media_type="text/event-stream")


@router.post(
    "/api/v1/admin/batch/{task_id}/cancel", dependencies=[Depends(verify_api_key)]
)
async def cancel_batch(task_id: str):
    task = get_task(task_id)
    if not task:
        raise HTTPException(status_code=404, detail="Task not found")
    task.cancel()
    return {"status": "success"}


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


@router.get("/admin/voice", response_class=HTMLResponse, include_in_schema=False)
async def admin_voice_page():
    """Voice Live 调试页"""
    return await render_template("voice/voice.html")


class VoiceTokenResponse(BaseModel):
    token: str
    url: str
    participant_name: str = ""
    room_name: str = ""


@router.get(
    "/api/v1/admin/voice/token",
    dependencies=[Depends(verify_api_key)],
    response_model=VoiceTokenResponse,
)
async def admin_voice_token(
    voice: str = "ara",
    personality: str = "assistant",
    speed: float = 1.0,
):
    """获取 Grok Voice Mode (LiveKit) Token"""
    token_mgr = await get_token_manager()
    sso_token = None
    for pool_name in ("ssoBasic", "ssoSuper"):
        sso_token = token_mgr.get_token(pool_name)
        if sso_token:
            break

    if not sso_token:
        raise AppException(
            "No available tokens for voice mode",
            code="no_token",
            status_code=503,
        )

    service = VoiceService()
    try:
        data = await service.get_token(
            token=sso_token,
            voice=voice,
            personality=personality,
            speed=speed,
        )
        token = data.get("token")
        if not token:
            raise AppException(
                "Upstream returned no voice token",
                code="upstream_error",
                status_code=502,
            )

        return VoiceTokenResponse(
            token=token,
            url="wss://livekit.grok.com",
            participant_name="",
            room_name="",
        )

    except Exception as e:
        if isinstance(e, AppException):
            raise
        raise AppException(
            f"Voice token error: {str(e)}",
            code="voice_error",
            status_code=500,
        )


@router.post("/api/v1/admin/login", dependencies=[Depends(verify_app_key)])
async def admin_login_api():
    """管理后台登录验证（使用 app_key）"""
    return {"status": "success", "api_key": get_admin_api_key()}


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
        from app.services.token.models import TokenInfo

        async with storage.acquire_lock("tokens_save", timeout=10):
            existing = await storage.load_tokens() or {}
            normalized = {}
            allowed_fields = set(TokenInfo.model_fields.keys())
            existing_map = {}
            for pool_name, tokens in existing.items():
                if not isinstance(tokens, list):
                    continue
                pool_map = {}
                for item in tokens:
                    if isinstance(item, str):
                        token_data = {"token": item}
                    elif isinstance(item, dict):
                        token_data = dict(item)
                    else:
                        continue
                    raw_token = token_data.get("token")
                    if isinstance(raw_token, str) and raw_token.startswith("sso="):
                        token_data["token"] = raw_token[4:]
                    token_key = token_data.get("token")
                    if isinstance(token_key, str):
                        pool_map[token_key] = token_data
                existing_map[pool_name] = pool_map
            for pool_name, tokens in (data or {}).items():
                if not isinstance(tokens, list):
                    continue
                pool_list = []
                for item in tokens:
                    if isinstance(item, str):
                        token_data = {"token": item}
                    elif isinstance(item, dict):
                        token_data = dict(item)
                    else:
                        continue

                    raw_token = token_data.get("token")
                    if isinstance(raw_token, str) and raw_token.startswith("sso="):
                        token_data["token"] = raw_token[4:]

                    base = existing_map.get(pool_name, {}).get(token_data.get("token"), {})
                    merged = dict(base)
                    merged.update(token_data)
                    if merged.get("tags") is None:
                        merged["tags"] = []

                    filtered = {k: v for k, v in merged.items() if k in allowed_fields}
                    try:
                        info = TokenInfo(**filtered)
                        pool_list.append(info.model_dump())
                    except Exception as e:
                        logger.warning(f"Skip invalid token in pool '{pool_name}': {e}")
                        continue
                normalized[pool_name] = pool_list

            await storage.save_tokens(normalized)
            mgr = await get_token_manager()
            await mgr.reload()
        return {"status": "success", "message": "Token 已更新"}
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@router.post("/api/v1/admin/tokens/refresh", dependencies=[Depends(verify_api_key)])
async def refresh_tokens_api(data: dict):
    """刷新 Token 状态"""
    from app.services.token.manager import get_token_manager
    from app.services.grok.utils.batch import run_in_batches

    try:
        mgr = await get_token_manager()
        tokens = []
        if "token" in data:
            tokens.append(data["token"])
        if "tokens" in data and isinstance(data["tokens"], list):
            tokens.extend(data["tokens"])

        if not tokens:
            raise HTTPException(status_code=400, detail="No tokens provided")

        # 去重并保持顺序
        unique_tokens = list(dict.fromkeys(tokens))

        # 最大数量限制
        max_tokens = get_config("performance.usage_max_tokens", 1000)
        try:
            max_tokens = int(max_tokens)
        except Exception:
            max_tokens = 1000

        truncated = False
        original_count = len(unique_tokens)
        if len(unique_tokens) > max_tokens:
            unique_tokens = unique_tokens[:max_tokens]
            truncated = True
            logger.warning(
                f"Usage refresh: truncated from {original_count} to {max_tokens} tokens"
            )

        # 批量执行配置
        max_concurrent = get_config("performance.usage_max_concurrent", 25)
        batch_size = get_config("performance.usage_batch_size", 50)

        async def _refresh_one(t):
            return await mgr.sync_usage(
                t, "grok-3", consume_on_fail=False, is_usage=False
            )

        raw_results = await run_in_batches(
            unique_tokens,
            _refresh_one,
            max_concurrent=max_concurrent,
            batch_size=batch_size,
        )

        results = {}
        for token, res in raw_results.items():
            if res.get("ok"):
                results[token] = res.get("data", False)
            else:
                results[token] = False

        response = {"status": "success", "results": results}
        if truncated:
            response["warning"] = (
                f"数量超出限制，仅处理前 {max_tokens} 个（共 {original_count} 个）"
            )
        return response
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@router.post(
    "/api/v1/admin/tokens/refresh/async", dependencies=[Depends(verify_api_key)]
)
async def refresh_tokens_api_async(data: dict):
    """刷新 Token 状态（异步批量 + SSE 进度）"""
    from app.services.token.manager import get_token_manager
    from app.services.grok.utils.batch import run_in_batches

    mgr = await get_token_manager()
    tokens: list[str] = []
    if isinstance(data.get("token"), str) and data["token"].strip():
        tokens.append(data["token"].strip())
    if isinstance(data.get("tokens"), list):
        tokens.extend([str(t).strip() for t in data["tokens"] if str(t).strip()])

    if not tokens:
        raise HTTPException(status_code=400, detail="No tokens provided")

    unique_tokens = list(dict.fromkeys(tokens))

    max_tokens = get_config("performance.usage_max_tokens", 1000)
    try:
        max_tokens = int(max_tokens)
    except Exception:
        max_tokens = 1000

    truncated = False
    original_count = len(unique_tokens)
    if len(unique_tokens) > max_tokens:
        unique_tokens = unique_tokens[:max_tokens]
        truncated = True
        logger.warning(
            f"Usage refresh: truncated from {original_count} to {max_tokens} tokens"
        )

    max_concurrent = get_config("performance.usage_max_concurrent", 25)
    batch_size = get_config("performance.usage_batch_size", 50)

    task = create_task(len(unique_tokens))

    async def _run():
        try:

            async def _refresh_one(t: str):
                return await mgr.sync_usage(
                    t, "grok-3", consume_on_fail=False, is_usage=False
                )

            async def _on_item(item: str, res: dict):
                task.record(bool(res.get("ok")))

            raw_results = await run_in_batches(
                unique_tokens,
                _refresh_one,
                max_concurrent=max_concurrent,
                batch_size=batch_size,
                on_item=_on_item,
                should_cancel=lambda: task.cancelled,
            )

            if task.cancelled:
                task.finish_cancelled()
                return

            results: dict[str, bool] = {}
            ok_count = 0
            fail_count = 0
            for token, res in raw_results.items():
                if res.get("ok") and res.get("data") is True:
                    ok_count += 1
                    results[token] = True
                else:
                    fail_count += 1
                    results[token] = False

            await mgr._save()

            result = {
                "status": "success",
                "summary": {
                    "total": len(unique_tokens),
                    "ok": ok_count,
                    "fail": fail_count,
                },
                "results": results,
            }
            warning = None
            if truncated:
                warning = (
                    f"数量超出限制，仅处理前 {max_tokens} 个（共 {original_count} 个）"
                )
            task.finish(result, warning=warning)
        except Exception as e:
            task.fail_task(str(e))
        finally:
            asyncio.create_task(expire_task(task.id, 300))

    asyncio.create_task(_run())

    return {
        "status": "success",
        "task_id": task.id,
        "total": len(unique_tokens),
    }


@router.post("/api/v1/admin/tokens/nsfw/enable", dependencies=[Depends(verify_api_key)])
async def enable_nsfw_api(data: dict):
    """批量开启 NSFW (Unhinged) 模式"""
    from app.services.grok.services.nsfw import NSFWService
    from app.services.grok.utils.batch import run_in_batches
    from app.services.token.manager import get_token_manager

    try:
        mgr = await get_token_manager()
        nsfw_service = NSFWService()

        # 收集 token 列表
        tokens: list[str] = []
        if isinstance(data.get("token"), str) and data["token"].strip():
            tokens.append(data["token"].strip())
        if isinstance(data.get("tokens"), list):
            tokens.extend([str(t).strip() for t in data["tokens"] if str(t).strip()])

        # 若未指定，则使用所有 pool 中的 token
        if not tokens:
            for pool_name, pool in mgr.pools.items():
                for info in pool.list():
                    raw = (
                        info.token[4:] if info.token.startswith("sso=") else info.token
                    )
                    tokens.append(raw)

        if not tokens:
            raise HTTPException(status_code=400, detail="No tokens available")

        # 去重并保持顺序
        unique_tokens = list(dict.fromkeys(tokens))

        # 限制最大数量（超出时截取前 N 个）
        max_tokens = get_config("performance.nsfw_max_tokens", 1000)
        try:
            max_tokens = int(max_tokens)
        except Exception:
            max_tokens = 1000

        truncated = False
        original_count = len(unique_tokens)
        if len(unique_tokens) > max_tokens:
            unique_tokens = unique_tokens[:max_tokens]
            truncated = True
            logger.warning(
                f"NSFW enable: truncated from {original_count} to {max_tokens} tokens"
            )

        # 批量执行配置
        max_concurrent = get_config("performance.nsfw_max_concurrent", 10)
        batch_size = get_config("performance.nsfw_batch_size", 50)

        # 定义 worker
        async def _enable(token: str):
            result = await nsfw_service.enable(token)
            # 成功后添加 nsfw tag
            if result.success:
                await mgr.add_tag(token, "nsfw")
            return {
                "success": result.success,
                "http_status": result.http_status,
                "grpc_status": result.grpc_status,
                "grpc_message": result.grpc_message,
                "error": result.error,
            }

        # 执行批量操作
        raw_results = await run_in_batches(
            unique_tokens, _enable, max_concurrent=max_concurrent, batch_size=batch_size
        )

        # 构造返回结果（mask token）
        results = {}
        ok_count = 0
        fail_count = 0

        for token, res in raw_results.items():
            masked = f"{token[:8]}...{token[-8:]}" if len(token) > 20 else token
            if res.get("ok") and res.get("data", {}).get("success"):
                ok_count += 1
                results[masked] = res.get("data", {})
            else:
                fail_count += 1
                results[masked] = res.get("data") or {"error": res.get("error")}

        response = {
            "status": "success",
            "summary": {
                "total": len(unique_tokens),
                "ok": ok_count,
                "fail": fail_count,
            },
            "results": results,
        }

        # 添加截断提示
        if truncated:
            response["warning"] = (
                f"数量超出限制，仅处理前 {max_tokens} 个（共 {original_count} 个）"
            )

        return response

    except HTTPException:
        raise
    except Exception as e:
        logger.error(f"Enable NSFW failed: {e}")
        raise HTTPException(status_code=500, detail=str(e))


@router.post(
    "/api/v1/admin/tokens/nsfw/enable/async", dependencies=[Depends(verify_api_key)]
)
async def enable_nsfw_api_async(data: dict):
    """批量开启 NSFW (Unhinged) 模式（异步批量 + SSE 进度）"""
    from app.services.grok.services.nsfw import NSFWService
    from app.services.grok.utils.batch import run_in_batches
    from app.services.token.manager import get_token_manager

    mgr = await get_token_manager()
    nsfw_service = NSFWService()

    tokens: list[str] = []
    if isinstance(data.get("token"), str) and data["token"].strip():
        tokens.append(data["token"].strip())
    if isinstance(data.get("tokens"), list):
        tokens.extend([str(t).strip() for t in data["tokens"] if str(t).strip()])

    if not tokens:
        for pool_name, pool in mgr.pools.items():
            for info in pool.list():
                raw = info.token[4:] if info.token.startswith("sso=") else info.token
                tokens.append(raw)

    if not tokens:
        raise HTTPException(status_code=400, detail="No tokens available")

    unique_tokens = list(dict.fromkeys(tokens))

    max_tokens = get_config("performance.nsfw_max_tokens", 1000)
    try:
        max_tokens = int(max_tokens)
    except Exception:
        max_tokens = 1000

    truncated = False
    original_count = len(unique_tokens)
    if len(unique_tokens) > max_tokens:
        unique_tokens = unique_tokens[:max_tokens]
        truncated = True
        logger.warning(
            f"NSFW enable: truncated from {original_count} to {max_tokens} tokens"
        )

    max_concurrent = get_config("performance.nsfw_max_concurrent", 10)
    batch_size = get_config("performance.nsfw_batch_size", 50)

    task = create_task(len(unique_tokens))

    async def _run():
        try:

            async def _enable(token: str):
                result = await nsfw_service.enable(token)
                if result.success:
                    await mgr.add_tag(token, "nsfw")
                return {
                    "success": result.success,
                    "http_status": result.http_status,
                    "grpc_status": result.grpc_status,
                    "grpc_message": result.grpc_message,
                    "error": result.error,
                }

            async def _on_item(item: str, res: dict):
                ok = bool(res.get("ok") and res.get("data", {}).get("success"))
                task.record(ok)

            raw_results = await run_in_batches(
                unique_tokens,
                _enable,
                max_concurrent=max_concurrent,
                batch_size=batch_size,
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
                masked = f"{token[:8]}...{token[-8:]}" if len(token) > 20 else token
                if res.get("ok") and res.get("data", {}).get("success"):
                    ok_count += 1
                    results[masked] = res.get("data", {})
                else:
                    fail_count += 1
                    results[masked] = res.get("data") or {"error": res.get("error")}

            await mgr._save()

            result = {
                "status": "success",
                "summary": {
                    "total": len(unique_tokens),
                    "ok": ok_count,
                    "fail": fail_count,
                },
                "results": results,
            }
            warning = None
            if truncated:
                warning = (
                    f"数量超出限制，仅处理前 {max_tokens} 个（共 {original_count} 个）"
                )
            task.finish(result, warning=warning)
        except Exception as e:
            task.fail_task(str(e))
        finally:
            asyncio.create_task(expire_task(task.id, 300))

    asyncio.create_task(_run())

    return {
        "status": "success",
        "task_id": task.id,
        "total": len(unique_tokens),
    }


@router.get("/admin/cache", response_class=HTMLResponse, include_in_schema=False)
async def admin_cache_page():
    """缓存管理页"""
    return await render_template("cache/cache.html")


@router.get("/api/v1/admin/cache", dependencies=[Depends(verify_api_key)])
async def get_cache_stats_api(request: Request):
    """获取缓存统计"""
    from app.services.grok.services.assets import DownloadService, ListService
    from app.services.token.manager import get_token_manager
    from app.services.grok.utils.batch import run_in_batches

    try:
        dl_service = DownloadService()
        image_stats = dl_service.get_stats("image")
        video_stats = dl_service.get_stats("video")

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
        max_concurrent = get_config("performance.assets_max_concurrent", 25)
        batch_size = get_config("performance.assets_batch_size", 10)
        try:
            max_concurrent = int(max_concurrent)
        except Exception:
            max_concurrent = 25
        try:
            batch_size = int(batch_size)
        except Exception:
            batch_size = 10
        max_concurrent = max(1, max_concurrent)
        batch_size = max(1, batch_size)

        max_tokens = get_config("performance.assets_max_tokens", 1000)
        try:
            max_tokens = int(max_tokens)
        except Exception:
            max_tokens = 1000

        truncated = False
        original_count = 0

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
                return {
                    "detail": {
                        "token": token,
                        "token_masked": account["token_masked"] if account else token,
                        "count": count,
                        "status": "ok",
                        "last_asset_clear_at": account["last_asset_clear_at"]
                        if account
                        else None,
                    },
                    "count": count,
                }
            except Exception as e:
                return {
                    "detail": {
                        "token": token,
                        "token_masked": account["token_masked"] if account else token,
                        "count": 0,
                        "status": f"error: {str(e)}",
                        "last_asset_clear_at": account["last_asset_clear_at"]
                        if account
                        else None,
                    },
                    "count": 0,
                }

        if selected_tokens:
            selected_tokens = list(dict.fromkeys(selected_tokens))
            original_count = len(selected_tokens)
            if len(selected_tokens) > max_tokens:
                selected_tokens = selected_tokens[:max_tokens]
                truncated = True
            total = 0
            raw_results = await run_in_batches(
                selected_tokens,
                _fetch_detail,
                max_concurrent=max_concurrent,
                batch_size=batch_size,
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
            original_count = len(tokens)
            if len(tokens) > max_tokens:
                tokens = tokens[:max_tokens]
                truncated = True
            raw_results = await run_in_batches(
                tokens,
                _fetch_detail,
                max_concurrent=max_concurrent,
                batch_size=batch_size,
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
                try:
                    count = await _fetch_assets(token)
                    match = next((a for a in accounts if a["token"] == token), None)
                    online_stats = {
                        "count": count,
                        "status": "ok",
                        "token": token,
                        "token_masked": match["token_masked"] if match else token,
                        "last_asset_clear_at": match["last_asset_clear_at"]
                        if match
                        else None,
                    }
                except Exception as e:
                    match = next((a for a in accounts if a["token"] == token), None)
                    online_stats = {
                        "count": 0,
                        "status": f"error: {str(e)}",
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
        if truncated:
            response["warning"] = (
                f"数量超出限制，仅处理前 {max_tokens} 个（共 {original_count} 个）"
            )
        return response
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@router.post(
    "/api/v1/admin/cache/online/load/async", dependencies=[Depends(verify_api_key)]
)
async def load_online_cache_api_async(data: dict):
    """在线资产统计（异步批量 + SSE 进度）"""
    from app.services.grok.services.assets import DownloadService, ListService
    from app.services.token.manager import get_token_manager
    from app.services.grok.utils.batch import run_in_batches

    mgr = await get_token_manager()

    # 账号列表
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
    selected_tokens: list[str] = []
    if isinstance(tokens, list):
        selected_tokens = [str(t).strip() for t in tokens if str(t).strip()]

    if not selected_tokens and scope == "all":
        selected_tokens = [account["token"] for account in accounts]
        scope = "all"
    elif selected_tokens:
        scope = "selected"
    else:
        raise HTTPException(status_code=400, detail="No tokens provided")

    selected_tokens = list(dict.fromkeys(selected_tokens))

    max_tokens = get_config("performance.assets_max_tokens", 1000)
    try:
        max_tokens = int(max_tokens)
    except Exception:
        max_tokens = 1000

    truncated = False
    original_count = len(selected_tokens)
    if len(selected_tokens) > max_tokens:
        selected_tokens = selected_tokens[:max_tokens]
        truncated = True

    max_concurrent = get_config("performance.assets_max_concurrent", 25)
    batch_size = get_config("performance.assets_batch_size", 10)

    task = create_task(len(selected_tokens))

    async def _run():
        try:
            dl_service = DownloadService()
            image_stats = dl_service.get_stats("image")
            video_stats = dl_service.get_stats("video")

            async def _fetch_detail(token: str):
                account = account_map.get(token)
                list_service = ListService()
                try:
                    count = await list_service.count(token)
                    detail = {
                        "token": token,
                        "token_masked": account["token_masked"] if account else token,
                        "count": count,
                        "status": "ok",
                        "last_asset_clear_at": account["last_asset_clear_at"]
                        if account
                        else None,
                    }
                    return {"ok": True, "detail": detail, "count": count}
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
                    return {"ok": False, "detail": detail, "count": 0}
                finally:
                    await list_service.close()

            async def _on_item(item: str, res: dict):
                ok = bool(res.get("data", {}).get("ok"))
                task.record(ok)

            raw_results = await run_in_batches(
                selected_tokens,
                _fetch_detail,
                max_concurrent=max_concurrent,
                batch_size=batch_size,
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
            warning = None
            if truncated:
                warning = (
                    f"数量超出限制，仅处理前 {max_tokens} 个（共 {original_count} 个）"
                )
            task.finish(result, warning=warning)
        except Exception as e:
            task.fail_task(str(e))
        finally:
            asyncio.create_task(expire_task(task.id, 300))

    asyncio.create_task(_run())

    return {
        "status": "success",
        "task_id": task.id,
        "total": len(selected_tokens),
    }


@router.post("/api/v1/admin/cache/clear", dependencies=[Depends(verify_api_key)])
async def clear_local_cache_api(data: dict):
    """清理本地缓存"""
    from app.services.grok.services.assets import DownloadService

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
    page_size: int = 1000,
):
    """列出本地缓存文件"""
    from app.services.grok.services.assets import DownloadService

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
    from app.services.grok.services.assets import DownloadService

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
    from app.services.grok.services.assets import DeleteService
    from app.services.token.manager import get_token_manager
    from app.services.grok.utils.batch import run_in_batches

    delete_service = None
    try:
        mgr = await get_token_manager()
        tokens = data.get("tokens")
        delete_service = DeleteService()

        if isinstance(tokens, list):
            token_list = [t.strip() for t in tokens if isinstance(t, str) and t.strip()]
            if not token_list:
                raise HTTPException(status_code=400, detail="No tokens provided")

            # 去重并保持顺序
            token_list = list(dict.fromkeys(token_list))

            # 最大数量限制
            max_tokens = get_config("performance.assets_max_tokens", 1000)
            try:
                max_tokens = int(max_tokens)
            except Exception:
                max_tokens = 1000
            truncated = False
            original_count = len(token_list)
            if len(token_list) > max_tokens:
                token_list = token_list[:max_tokens]
                truncated = True

            results = {}
            max_concurrent = get_config("performance.assets_max_concurrent", 25)
            batch_size = get_config("performance.assets_batch_size", 10)
            try:
                max_concurrent = int(max_concurrent)
            except Exception:
                max_concurrent = 25
            try:
                batch_size = int(batch_size)
            except Exception:
                batch_size = 10
            max_concurrent = max(1, max_concurrent)
            batch_size = max(1, batch_size)

            async def _clear_one(t: str):
                try:
                    result = await delete_service.delete_all(t)
                    await mgr.mark_asset_clear(t)
                    return {"status": "success", "result": result}
                except Exception as e:
                    return {"status": "error", "error": str(e)}

            raw_results = await run_in_batches(
                token_list,
                _clear_one,
                max_concurrent=max_concurrent,
                batch_size=batch_size,
            )
            for token, res in raw_results.items():
                if res.get("ok"):
                    results[token] = res.get("data", {})
                else:
                    results[token] = {"status": "error", "error": res.get("error")}

            response = {"status": "success", "results": results}
            if truncated:
                response["warning"] = (
                    f"数量超出限制，仅处理前 {max_tokens} 个（共 {original_count} 个）"
                )
            return response

        token = data.get("token") or mgr.get_token()
        if not token:
            raise HTTPException(
                status_code=400, detail="No available token to perform cleanup"
            )

        result = await delete_service.delete_all(token)
        await mgr.mark_asset_clear(token)
        return {"status": "success", "result": result}
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))
    finally:
        if delete_service:
            await delete_service.close()


@router.post(
    "/api/v1/admin/cache/online/clear/async", dependencies=[Depends(verify_api_key)]
)
async def clear_online_cache_api_async(data: dict):
    """清理在线缓存（异步批量 + SSE 进度）"""
    from app.services.grok.services.assets import DeleteService
    from app.services.token.manager import get_token_manager
    from app.services.grok.utils.batch import run_in_batches

    mgr = await get_token_manager()
    tokens = data.get("tokens")
    if not isinstance(tokens, list):
        raise HTTPException(status_code=400, detail="No tokens provided")

    token_list = [t.strip() for t in tokens if isinstance(t, str) and t.strip()]
    if not token_list:
        raise HTTPException(status_code=400, detail="No tokens provided")

    token_list = list(dict.fromkeys(token_list))

    max_tokens = get_config("performance.assets_max_tokens", 1000)
    try:
        max_tokens = int(max_tokens)
    except Exception:
        max_tokens = 1000
    truncated = False
    original_count = len(token_list)
    if len(token_list) > max_tokens:
        token_list = token_list[:max_tokens]
        truncated = True

    max_concurrent = get_config("performance.assets_max_concurrent", 25)
    batch_size = get_config("performance.assets_batch_size", 10)

    task = create_task(len(token_list))

    async def _run():
        delete_service = DeleteService()
        try:

            async def _clear_one(t: str):
                try:
                    result = await delete_service.delete_all(t)
                    await mgr.mark_asset_clear(t)
                    return {"ok": True, "result": result}
                except Exception as e:
                    return {"ok": False, "error": str(e)}

            async def _on_item(item: str, res: dict):
                ok = bool(res.get("data", {}).get("ok"))
                task.record(ok)

            raw_results = await run_in_batches(
                token_list,
                _clear_one,
                max_concurrent=max_concurrent,
                batch_size=batch_size,
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
            warning = None
            if truncated:
                warning = (
                    f"数量超出限制，仅处理前 {max_tokens} 个（共 {original_count} 个）"
                )
            task.finish(result, warning=warning)
        except Exception as e:
            task.fail_task(str(e))
        finally:
            await delete_service.close()
            asyncio.create_task(expire_task(task.id, 300))

    asyncio.create_task(_run())

    return {
        "status": "success",
        "task_id": task.id,
        "total": len(token_list),
    }
