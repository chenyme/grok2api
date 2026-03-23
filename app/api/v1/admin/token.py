import asyncio
import math
import re
from typing import Any, Callable

import orjson
from fastapi import APIRouter, Depends, HTTPException, Query, Request
from fastapi.responses import StreamingResponse

from app.core.auth import get_app_key, verify_app_key
from app.core.batch import create_task, expire_task, get_task
from app.core.logger import logger
from app.core.storage import get_storage
from app.services.grok.batch_services.usage import UsageService
from app.services.grok.batch_services.nsfw import NSFWService
from app.services.token.manager import get_token_manager

router = APIRouter()

_TOKEN_CHAR_REPLACEMENTS = str.maketrans(
    {
        "\u2010": "-",
        "\u2011": "-",
        "\u2012": "-",
        "\u2013": "-",
        "\u2014": "-",
        "\u2212": "-",
        "\u00a0": " ",
        "\u2007": " ",
        "\u202f": " ",
        "\u200b": "",
        "\u200c": "",
        "\u200d": "",
        "\ufeff": "",
    }
)

_TOKEN_FILTERS = {"all", "active", "cooling", "expired", "nsfw", "no-nsfw"}
MAX_TOKEN_PAGE_SIZE = 2000
TokenRef = tuple[str | None, str]


def _sanitize_token_text(value) -> str:
    token = "" if value is None else str(value)
    token = token.translate(_TOKEN_CHAR_REPLACEMENTS)
    token = re.sub(r"\s+", "", token)
    if token.startswith("sso="):
        token = token[4:]
    return token.encode("ascii", errors="ignore").decode("ascii")


def _normalize_pool_name(value: Any) -> str | None:
    if value is None:
        return None
    pool_name = str(value).strip()
    return pool_name or None


def _normalize_token_ref(
    value: Any, *, default_pool: str | None = None
) -> TokenRef | None:
    token_value = value
    pool_name = default_pool

    if isinstance(value, dict):
        token_value = value.get("token")
        pool_name = _normalize_pool_name(value.get("pool")) or default_pool

    token = _sanitize_token_text(token_value)
    if not token:
        return None

    return pool_name, token


def _collect_token_refs(data: dict[str, Any]) -> list[TokenRef]:
    refs: list[TokenRef] = []

    if "token" in data:
        single_ref = _normalize_token_ref(
            data.get("token"),
            default_pool=_normalize_pool_name(data.get("pool")),
        )
        if single_ref:
            refs.append(single_ref)

    if isinstance(data.get("tokens"), list):
        for value in data["tokens"]:
            token_ref = _normalize_token_ref(value)
            if token_ref:
                refs.append(token_ref)

    unique_refs: list[TokenRef] = []
    seen: set[TokenRef] = set()
    for token_ref in refs:
        if token_ref in seen:
            continue
        seen.add(token_ref)
        unique_refs.append(token_ref)

    return unique_refs


def _serialize_token_ref(token_ref: TokenRef) -> dict[str, Any]:
    pool_name, token = token_ref
    payload: dict[str, Any] = {"token": token}
    if pool_name:
        payload["pool"] = pool_name
    return payload


def _build_boolean_result_items(
    token_refs: list[TokenRef],
    raw_results: dict[TokenRef, dict[str, Any]],
    *,
    is_success: Callable[[dict[str, Any]], bool],
) -> tuple[list[dict[str, Any]], int, int]:
    items: list[dict[str, Any]] = []
    ok_count = 0
    fail_count = 0

    for token_ref in token_refs:
        res = raw_results.get(token_ref, {})
        ok = is_success(res)
        item = _serialize_token_ref(token_ref)
        item["ok"] = ok
        items.append(item)
        if ok:
            ok_count += 1
        else:
            fail_count += 1

    return items, ok_count, fail_count


def _normalize_token_filter(value: str | None) -> str:
    normalized = (value or "all").strip().lower()
    if normalized in _TOKEN_FILTERS:
        return normalized
    return "all"


def _token_status(info: Any) -> str:
    status = getattr(info, "status", None) or "active"
    return str(status)


def _token_tags(info: Any) -> list[str]:
    tags = getattr(info, "tags", None) or []
    return [str(tag) for tag in tags]


def _token_matches_filter(info: Any, status_filter: str) -> bool:
    if status_filter == "all":
        return True

    status = _token_status(info)
    has_nsfw = "nsfw" in _token_tags(info)

    if status_filter == "active":
        return status == "active"
    if status_filter == "cooling":
        return status == "cooling"
    if status_filter == "expired":
        return status not in {"active", "cooling"}
    if status_filter == "nsfw":
        return has_nsfw
    if status_filter == "no-nsfw":
        return not has_nsfw
    return True


def _empty_token_summary() -> dict[str, int]:
    return {
        "total": 0,
        "active": 0,
        "cooling": 0,
        "invalid": 0,
        "nsfw": 0,
        "no_nsfw": 0,
        "chat_quota": 0,
        "image_quota": 0,
        "total_consumed": 0,
        "total_calls": 0,
    }


def _accumulate_token_summary(summary: dict[str, int], info: Any) -> None:
    summary["total"] += 1

    status = _token_status(info)
    quota = int(getattr(info, "quota", 0) or 0)
    consumed = int(getattr(info, "consumed", 0) or 0)
    use_count = int(getattr(info, "use_count", 0) or 0)
    has_nsfw = "nsfw" in _token_tags(info)

    if status == "active":
        summary["active"] += 1
        summary["chat_quota"] += quota
    elif status == "cooling":
        summary["cooling"] += 1
    else:
        summary["invalid"] += 1

    if has_nsfw:
        summary["nsfw"] += 1
    else:
        summary["no_nsfw"] += 1

    summary["total_consumed"] += consumed
    summary["total_calls"] += use_count


def _serialize_token(pool_name: str, info: Any) -> dict[str, Any]:
    data = {
        "token": getattr(info, "token", ""),
        "pool": pool_name,
        "status": _token_status(info),
        "quota": int(getattr(info, "quota", 0) or 0),
        "consumed": int(getattr(info, "consumed", 0) or 0),
        "note": getattr(info, "note", "") or "",
        "fail_count": int(getattr(info, "fail_count", 0) or 0),
        "use_count": int(getattr(info, "use_count", 0) or 0),
        "tags": _token_tags(info),
    }

    for key in (
        "created_at",
        "last_used_at",
        "last_fail_at",
        "last_fail_reason",
        "last_sync_at",
        "last_asset_clear_at",
    ):
        value = getattr(info, key, None)
        if value is not None:
            data[key] = value

    return data


def _collect_token_page_items(
    mgr: Any,
    status_filter: str,
    start_index: int,
    end_index: int,
) -> list[dict[str, Any]]:
    items: list[dict[str, Any]] = []
    matched = 0

    for pool_name, pool in mgr.pools.items():
        for info in pool.list():
            if not _token_matches_filter(info, status_filter):
                continue
            if start_index <= matched < end_index:
                items.append(_serialize_token(pool_name, info))
            matched += 1
            if matched >= end_index and items:
                continue

    return items


def _build_paginated_token_payload(
    mgr: Any,
    *,
    status_filter: str,
    page: int,
    page_size: int,
    consumed_mode_enabled: bool,
    keys_only: bool = False,
) -> dict[str, Any]:
    summary = _empty_token_summary()
    matched_items: list[dict[str, str]] = []
    filtered_total = 0

    for pool_name, pool in mgr.pools.items():
        for info in pool.list():
            _accumulate_token_summary(summary, info)

            if not _token_matches_filter(info, status_filter):
                continue

            filtered_total += 1
            if keys_only:
                matched_items.append(
                    {
                        "token": getattr(info, "token", ""),
                        "pool": pool_name,
                    }
                )

    summary["image_quota"] = summary["chat_quota"] // 2
    counts = {
        "all": summary["total"],
        "active": summary["active"],
        "cooling": summary["cooling"],
        "expired": summary["invalid"],
        "nsfw": summary["nsfw"],
        "no-nsfw": summary["no_nsfw"],
    }

    if keys_only:
        return {
            "items": matched_items,
            "total": filtered_total,
            "filter": status_filter,
            "counts": counts,
            "summary": summary,
            "consumed_mode_enabled": consumed_mode_enabled,
        }

    total_pages = max(1, math.ceil(filtered_total / page_size)) if filtered_total else 1
    current_page = min(max(page, 1), total_pages)
    start_index = (current_page - 1) * page_size
    end_index = start_index + page_size
    items = _collect_token_page_items(mgr, status_filter, start_index, end_index)

    return {
        "items": items,
        "page": current_page,
        "page_size": page_size,
        "total": filtered_total,
        "total_pages": total_pages,
        "filter": status_filter,
        "counts": counts,
        "summary": summary,
        "consumed_mode_enabled": consumed_mode_enabled,
    }


@router.get("/tokens", dependencies=[Depends(verify_app_key)])
async def get_tokens(
    page: int | None = Query(None, ge=1),
    page_size: int | None = Query(None, ge=1, le=MAX_TOKEN_PAGE_SIZE),
    status_filter: str | None = Query(None, alias="filter"),
    keys_only: bool = Query(False),
):
    """获取所有 Token"""
    # 获取消耗模式配置
    from app.core.config import get_config

    mgr = await get_token_manager()
    consumed_mode = get_config("token.consumed_mode_enabled", False)
    normalized_filter = _normalize_token_filter(status_filter)

    if page is not None or page_size is not None or keys_only or normalized_filter != "all":
        return _build_paginated_token_payload(
            mgr,
            status_filter=normalized_filter,
            page=page or 1,
            page_size=page_size or 50,
            consumed_mode_enabled=consumed_mode,
            keys_only=keys_only,
        )

    results = {}
    for pool_name, pool in mgr.pools.items():
        results[pool_name] = [t.model_dump() for t in pool.list()]
    return {
        "tokens": results or {},
        "consumed_mode_enabled": consumed_mode,
    }


@router.post("/tokens", dependencies=[Depends(verify_app_key)])
async def update_tokens(data: dict):
    """更新 Token 信息"""
    storage = get_storage()
    try:
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
                    if raw_token is not None:
                        token_data["token"] = _sanitize_token_text(raw_token)
                    token_key = token_data.get("token")
                    if isinstance(token_key, str):
                        pool_map[token_key] = token_data
                existing_map[pool_name] = pool_map
            for pool_name, tokens in (data or {}).items():
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
                    if raw_token is not None:
                        token_data["token"] = _sanitize_token_text(raw_token)
                    if not token_data.get("token"):
                        logger.warning(f"Skip empty token in pool '{pool_name}'")
                        continue

                    token_key = token_data.get("token")
                    base = pool_map.get(token_key) or existing_map.get(pool_name, {}).get(
                        token_key, {}
                    )
                    merged = dict(base)
                    merged.update(token_data)
                    if merged.get("tags") is None:
                        merged["tags"] = []

                    filtered = {k: v for k, v in merged.items() if k in allowed_fields}
                    try:
                        info = TokenInfo(**filtered)
                        pool_map[token_key] = info.model_dump()
                    except Exception as e:
                        logger.warning(f"Skip invalid token in pool '{pool_name}': {e}")
                        continue
                normalized[pool_name] = list(pool_map.values())

            await storage.save_tokens(normalized)
            mgr = await get_token_manager()
            await mgr.reload()
        return {"status": "success", "message": "Token 已更新"}
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@router.post("/tokens/refresh", dependencies=[Depends(verify_app_key)])
async def refresh_tokens(data: dict):
    """刷新 Token 状态"""
    try:
        mgr = await get_token_manager()
        token_refs = _collect_token_refs(data)

        if not token_refs:
            raise HTTPException(status_code=400, detail="No tokens provided")

        raw_results = await UsageService.batch(token_refs, mgr)

        # 强制保存变更到存储
        await mgr._save(force=True)

        items, ok_count, fail_count = _build_boolean_result_items(
            token_refs,
            raw_results,
            is_success=lambda res: bool(res.get("ok")) and res.get("data") is True,
        )

        response = {
            "status": "success",
            "summary": {
                "total": len(token_refs),
                "ok": ok_count,
                "fail": fail_count,
            },
            "items": items,
        }
        return response
    except HTTPException:
        raise
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@router.post("/tokens/refresh/async", dependencies=[Depends(verify_app_key)])
async def refresh_tokens_async(data: dict):
    """刷新 Token 状态（异步批量 + SSE 进度）"""
    mgr = await get_token_manager()
    token_refs = _collect_token_refs(data)

    if not token_refs:
        raise HTTPException(status_code=400, detail="No tokens provided")

    task = create_task(len(token_refs))

    async def _run():
        try:

            async def _on_item(item: TokenRef, res: dict):
                task.record(bool(res.get("ok")) and res.get("data") is True)

            raw_results = await UsageService.batch(
                token_refs,
                mgr,
                on_item=_on_item,
                should_cancel=lambda: task.cancelled,
            )

            if task.cancelled:
                task.finish_cancelled()
                return

            items, ok_count, fail_count = _build_boolean_result_items(
                token_refs,
                raw_results,
                is_success=lambda res: bool(res.get("ok")) and res.get("data") is True,
            )

            await mgr._save(force=True)

            result = {
                "status": "success",
                "summary": {
                    "total": len(token_refs),
                    "ok": ok_count,
                    "fail": fail_count,
                },
                "items": items,
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
        "total": len(token_refs),
    }


@router.get("/batch/{task_id}/stream")
async def batch_stream(task_id: str, request: Request):
    app_key = get_app_key()
    if app_key:
        key = request.query_params.get("app_key")
        if key != app_key:
            raise HTTPException(status_code=401, detail="Invalid authentication token")
    task = get_task(task_id)
    if not task:
        raise HTTPException(status_code=404, detail="Task not found")

    async def event_stream():
        queue = task.attach()
        try:
            yield f"data: {orjson.dumps({'type': 'snapshot', **task.snapshot()}).decode()}\n\n"

            final = task.final_event()
            if final:
                yield f"data: {orjson.dumps(final).decode()}\n\n"
                return

            while True:
                try:
                    event = await asyncio.wait_for(queue.get(), timeout=15)
                except asyncio.TimeoutError:
                    yield ": ping\n\n"
                    final = task.final_event()
                    if final:
                        yield f"data: {orjson.dumps(final).decode()}\n\n"
                        return
                    continue

                yield f"data: {orjson.dumps(event).decode()}\n\n"
                if event.get("type") in ("done", "error", "cancelled"):
                    return
        finally:
            task.detach(queue)

    return StreamingResponse(event_stream(), media_type="text/event-stream")


@router.post("/batch/{task_id}/cancel", dependencies=[Depends(verify_app_key)])
async def batch_cancel(task_id: str):
    task = get_task(task_id)
    if not task:
        raise HTTPException(status_code=404, detail="Task not found")
    task.cancel()
    return {"status": "success"}


@router.post("/tokens/nsfw/enable", dependencies=[Depends(verify_app_key)])
async def enable_nsfw(data: dict):
    """批量开启 NSFW (Unhinged) 模式"""
    try:
        mgr = await get_token_manager()

        token_refs = _collect_token_refs(data)

        if not token_refs:
            for pool_name, pool in mgr.pools.items():
                for info in pool.list():
                    token_ref = _normalize_token_ref(
                        info.token,
                        default_pool=pool_name,
                    )
                    if token_ref:
                        token_refs.append(token_ref)

        if not token_refs:
            raise HTTPException(status_code=400, detail="No tokens available")

        raw_results = await NSFWService.batch(token_refs, mgr)

        items, ok_count, fail_count = _build_boolean_result_items(
            token_refs,
            raw_results,
            is_success=lambda res: bool(res.get("ok"))
            and bool(res.get("data", {}).get("success")),
        )

        response = {
            "status": "success",
            "summary": {
                "total": len(token_refs),
                "ok": ok_count,
                "fail": fail_count,
            },
            "items": items,
        }

        return response

    except HTTPException:
        raise
    except Exception as e:
        logger.error(f"Enable NSFW failed: {e}")
        raise HTTPException(status_code=500, detail=str(e))


@router.post("/tokens/nsfw/enable/async", dependencies=[Depends(verify_app_key)])
async def enable_nsfw_async(data: dict):
    """批量开启 NSFW (Unhinged) 模式（异步批量 + SSE 进度）"""
    mgr = await get_token_manager()

    token_refs = _collect_token_refs(data)

    if not token_refs:
        for pool_name, pool in mgr.pools.items():
            for info in pool.list():
                token_ref = _normalize_token_ref(info.token, default_pool=pool_name)
                if token_ref:
                    token_refs.append(token_ref)

    if not token_refs:
        raise HTTPException(status_code=400, detail="No tokens available")

    task = create_task(len(token_refs))

    async def _run():
        try:

            async def _on_item(item: TokenRef, res: dict):
                ok = bool(res.get("ok") and res.get("data", {}).get("success"))
                task.record(ok)

            raw_results = await NSFWService.batch(
                token_refs,
                mgr,
                on_item=_on_item,
                should_cancel=lambda: task.cancelled,
            )

            if task.cancelled:
                task.finish_cancelled()
                return

            items, ok_count, fail_count = _build_boolean_result_items(
                token_refs,
                raw_results,
                is_success=lambda res: bool(res.get("ok"))
                and bool(res.get("data", {}).get("success")),
            )

            await mgr._save(force=True)

            result = {
                "status": "success",
                "summary": {
                    "total": len(token_refs),
                    "ok": ok_count,
                    "fail": fail_count,
                },
                "items": items,
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
        "total": len(token_refs),
    }
