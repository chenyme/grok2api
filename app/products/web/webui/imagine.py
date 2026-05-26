"""WebUI imagine endpoint backed by Grok Imagine WebSocket only."""

import asyncio
import hmac
import uuid
from typing import Optional

import orjson
from fastapi import APIRouter, WebSocket, WebSocketDisconnect

from app.platform.auth.middleware import get_admin_key, get_webui_key, is_webui_enabled
from app.platform.config.snapshot import get_config
from app.platform.logging.logger import logger
from app.platform.runtime.clock import now_s
from app.products.openai.images import resolve_aspect_ratio

router = APIRouter()


async def _acquire_token():
    from app.dataplane.account import _directory as _acct_dir
    if _acct_dir is None:
        return None, None
    from app.control.model.registry import get as get_model
    spec = get_model("grok-imagine-image")
    if spec is None:
        return None, None
    acct = await _acct_dir.reserve(
        pool_candidates=spec.pool_candidates(),
        mode_id=int(spec.mode_id),
        now_s_override=now_s(),
    )
    if acct is None:
        return None, None
    return acct.token, acct


def _extract_token(value: str | None) -> str:
    raw = str(value or "").strip()
    if not raw:
        return ""
    scheme, _, token = raw.partition(" ")
    if scheme.lower() == "bearer" and token:
        return token.strip()
    return raw


def _is_allowed(token: str, *, admin: bool = False) -> bool:
    if admin:
        admin_key = get_admin_key()
        return bool(admin_key) and bool(token) and hmac.compare_digest(token, admin_key)
    webui_key = get_webui_key()
    if not webui_key:
        return is_webui_enabled()
    return bool(token) and hmac.compare_digest(token, webui_key)


def _websocket_token(websocket: WebSocket) -> str:
    return (
        _extract_token(websocket.headers.get("authorization"))
        or str(websocket.query_params.get("access_token") or "").strip()
        or str(websocket.query_params.get("app_key") or "").strip()
    )


@router.websocket("/imagine/ws")
async def imagine_ws(websocket: WebSocket):
    admin_socket = websocket.url.path.startswith("/admin/api/")
    if not _is_allowed(_websocket_token(websocket), admin=admin_socket):
        await websocket.close(code=1008)
        return

    await websocket.accept()
    stop_event = asyncio.Event()
    run_task: Optional[asyncio.Task] = None

    async def _send(payload: dict) -> bool:
        try:
            await websocket.send_text(orjson.dumps(payload).decode())
            return True
        except Exception:
            return False

    async def _stop_run():
        nonlocal run_task
        stop_event.set()
        if run_task and not run_task.done():
            run_task.cancel()
            try:
                await run_task
            except Exception:
                pass
        run_task = None
        stop_event.clear()

    async def _run(
        prompt: str,
        aspect_ratio: str,
        nsfw: Optional[bool],
        count: int,
        quality: str,
    ):
        from app.dataplane.account import _directory as _acct_dir
        from app.dataplane.reverse.transport.imagine_ws import stream_images

        run_id = uuid.uuid4().hex
        enable_pro = quality == "quality"
        await _send({
            "type": "status",
            "status": "running",
            "prompt": prompt,
            "aspect_ratio": aspect_ratio,
            "run_id": run_id,
            "count": count,
            "quality": quality,
        })

        acct = None
        try:
            token, acct = await _acquire_token()
            if not token:
                await _send({
                    "type": "error",
                    "message": "No available accounts for this model tier",
                    "code": "rate_limit_exceeded",
                })
                return

            enable_nsfw = nsfw if nsfw is not None else get_config().get_bool("features.enable_nsfw", True)
            async for event in stream_images(
                token,
                prompt,
                aspect_ratio=aspect_ratio,
                n=count,
                enable_nsfw=enable_nsfw,
                enable_pro=enable_pro,
            ):
                if stop_event.is_set():
                    return
                if not isinstance(event, dict) or event.get("type") == "_meta":
                    continue
                event.setdefault("run_id", run_id)
                await _send(event)
                if event.get("type") == "error":
                    return

            if not stop_event.is_set():
                await _send({
                    "type": "status",
                    "status": "completed",
                    "run_id": run_id,
                    "count": count,
                })
        except asyncio.CancelledError:
            pass
        except Exception as exc:
            logger.error(
                "webui imagine run failed: error_type={} error={}",
                type(exc).__name__,
                exc,
            )
            await _send({
                "type": "error",
                "message": str(exc),
                "code": "internal_error",
            })
        finally:
            if acct and _acct_dir:
                await _acct_dir.release(acct)
            if stop_event.is_set():
                await _send({"type": "status", "status": "stopped", "run_id": run_id})

    try:
        while True:
            try:
                raw = await websocket.receive_text()
            except (RuntimeError, WebSocketDisconnect):
                break

            try:
                payload = orjson.loads(raw)
            except Exception:
                await _send({
                    "type": "error",
                    "message": "Invalid message format.",
                    "code": "invalid_payload",
                })
                continue

            action = payload.get("type")
            if action == "start":
                prompt = str(payload.get("prompt") or "").strip()
                if not prompt:
                    await _send({
                        "type": "error",
                        "message": "Prompt cannot be empty.",
                        "code": "invalid_prompt",
                    })
                    continue
                aspect_ratio = resolve_aspect_ratio(str(payload.get("aspect_ratio") or "2:3").strip() or "2:3")
                quality = str(payload.get("quality") or "speed").strip().lower()
                if quality not in {"speed", "quality"}:
                    quality = "speed"
                nsfw = payload.get("nsfw")
                if nsfw is not None:
                    if isinstance(nsfw, str):
                        nsfw = nsfw.strip().lower() in {"1", "true", "yes", "on"}
                    else:
                        nsfw = bool(nsfw)
                try:
                    count = int(payload.get("count") or 6)
                except (TypeError, ValueError):
                    count = 6
                count = max(1, min(count, 6))
                await _stop_run()
                run_task = asyncio.create_task(_run(prompt, aspect_ratio, nsfw, count, quality))
                continue

            if action == "stop":
                await _stop_run()
                continue

            await _send({
                "type": "error",
                "message": "Unknown action.",
                "code": "invalid_action",
            })
    except WebSocketDisconnect:
        pass
    except Exception as exc:
        logger.error(
            "webui imagine websocket handler failed: error_type={} error={}",
            type(exc).__name__,
            exc,
        )
    finally:
        await _stop_run()
        try:
            from starlette.websockets import WebSocketState
            if websocket.client_state == WebSocketState.CONNECTED:
                await websocket.close(code=1000, reason="Server closing connection")
        except Exception:
            pass
