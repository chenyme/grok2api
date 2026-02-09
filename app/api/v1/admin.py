import asyncio
import json
import os
import base64
import secrets
import time
import uuid
from copy import deepcopy
from pathlib import Path
from urllib.parse import urlparse

import aiofiles
import orjson
from fastapi import APIRouter, Depends, HTTPException, Query, Request, WebSocket
from fastapi.websockets import WebSocketDisconnect
from fastapi.responses import HTMLResponse, RedirectResponse, StreamingResponse

from pydantic import BaseModel
from typing import Optional

from app.core.auth import verify_admin_access, validate_admin_token, get_client_ip
from app.core.batch_tasks import create_task, get_task, expire_task
from app.core.config import config, get_config
from app.core.logger import logger
from app.core.storage import get_storage, LocalStorage, RedisStorage, SQLStorage, DATA_DIR

router = APIRouter()

# ---- Imagine session 管理 ----
IMAGINE_SESSION_TTL = 600
_IMAGINE_SESSIONS: dict[str, dict] = {}
_IMAGINE_SESSIONS_LOCK = asyncio.Lock()


async def _cleanup_imagine_sessions(now: float) -> None:
    expired = [
        key
        for key, info in _IMAGINE_SESSIONS.items()
        if now - float(info.get("created_at") or 0) > IMAGINE_SESSION_TTL
    ]
    for key in expired:
        _IMAGINE_SESSIONS.pop(key, None)


async def _create_imagine_session(prompt: str, aspect_ratio: str) -> str:
    task_id = uuid.uuid4().hex
    now = time.time()
    async with _IMAGINE_SESSIONS_LOCK:
        await _cleanup_imagine_sessions(now)
        _IMAGINE_SESSIONS[task_id] = {
            "prompt": prompt,
            "aspect_ratio": aspect_ratio,
            "created_at": now,
        }
    return task_id


async def _get_imagine_session(task_id: str) -> Optional[dict]:
    if not task_id:
        return None
    now = time.time()
    async with _IMAGINE_SESSIONS_LOCK:
        await _cleanup_imagine_sessions(now)
        info = _IMAGINE_SESSIONS.get(task_id)
        if not info:
            return None
        created_at = float(info.get("created_at") or 0)
        if now - created_at > IMAGINE_SESSION_TTL:
            _IMAGINE_SESSIONS.pop(task_id, None)
            return None
        return dict(info)


async def _delete_imagine_session(task_id: str) -> None:
    if not task_id:
        return
    async with _IMAGINE_SESSIONS_LOCK:
        _IMAGINE_SESSIONS.pop(task_id, None)


async def _delete_imagine_sessions(task_ids: list[str]) -> int:
    if not task_ids:
        return 0
    removed = 0
    async with _IMAGINE_SESSIONS_LOCK:
        for task_id in task_ids:
            if task_id and task_id in _IMAGINE_SESSIONS:
                _IMAGINE_SESSIONS.pop(task_id, None)
                removed += 1
    return removed


TEMPLATE_DIR = Path(__file__).parent.parent.parent / "static"

_WS_TOKEN_TTL_SEC = 60
_WS_TOKEN_MAX = 2048
_WS_TOKENS: dict[str, float] = {}
_REDACTED_VALUE = "***REDACTED***"
_SENSITIVE_KEYWORDS = (
    "key",
    "token",
    "secret",
    "password",
    "cookie",
    "authorization",
    "proxy",
)

_NON_SENSITIVE_CONFIG_KEYS = {
    "assets_max_tokens",
    "usage_max_tokens",
    "nsfw_max_tokens",
}


def _is_sensitive_config_key(section: str, key: str, value) -> bool:
    section_l = str(section or "").lower()
    key_l = str(key or "").lower()
    if key_l in _NON_SENSITIVE_CONFIG_KEYS:
        return False
    if key_l in {"cf_clearance"}:
        return True
    if any(word in key_l for word in _SENSITIVE_KEYWORDS):
        return True
    if section_l in {"security", "network", "proxy"} and isinstance(value, str) and value:
        return True
    return False


def _sanitize_config_payload(raw: dict) -> dict:
    data = deepcopy(raw or {})
    if not isinstance(data, dict):
        return {}

    for section, items in data.items():
        if not isinstance(items, dict):
            continue
        for key, value in list(items.items()):
            if _is_sensitive_config_key(section, key, value) and value not in ("", None):
                items[key] = _REDACTED_VALUE
    return data


def _restore_redacted_values(new_data: dict, current_data: dict) -> dict:
    merged = deepcopy(new_data or {})
    current = current_data or {}

    if not isinstance(merged, dict):
        return {}

    for section, items in merged.items():
        if not isinstance(items, dict):
            continue
        current_section = current.get(section, {}) if isinstance(current, dict) else {}
        if not isinstance(current_section, dict):
            current_section = {}

        for key, value in list(items.items()):
            if value == _REDACTED_VALUE and key in current_section:
                items[key] = current_section.get(key)

    return merged


def _cleanup_ws_tokens(now_ts: float) -> None:
    expired = [token for token, expiry in _WS_TOKENS.items() if expiry <= now_ts]
    for token in expired:
        _WS_TOKENS.pop(token, None)


def _issue_ws_token() -> str:
    now_ts = time.time()
    _cleanup_ws_tokens(now_ts)

    if len(_WS_TOKENS) >= _WS_TOKEN_MAX:
        oldest = sorted(_WS_TOKENS.items(), key=lambda x: x[1])[
            : len(_WS_TOKENS) - _WS_TOKEN_MAX + 1
        ]
        for token, _ in oldest:
            _WS_TOKENS.pop(token, None)

    token = secrets.token_urlsafe(24)
    _WS_TOKENS[token] = now_ts + _WS_TOKEN_TTL_SEC
    return token


def _consume_ws_token(token: str) -> bool:
    value = (token or "").strip()
    if not value:
        return False

    now_ts = time.time()
    _cleanup_ws_tokens(now_ts)
    expiry = _WS_TOKENS.pop(value, None)
    if not expiry:
        return False
    return expiry > now_ts


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


def _normalize_auth_token(raw: str) -> str:
    token = (raw or "").strip()
    if token.lower().startswith("bearer "):
        token = token[7:].strip()
    return token


async def _verify_stream_api_key(request: Request, task=None) -> None:
    stream_token = (request.query_params.get("stream_token") or "").strip()
    if task and stream_token and task.validate_stream_token(stream_token):
        return

    auth = request.headers.get("Authorization", "")
    key = _normalize_auth_token(auth)
    if not key and get_config("security.allow_query_api_key", False):
        key = _normalize_auth_token(request.query_params.get("api_key", ""))

    if not key or not await validate_admin_token(key):
        raise HTTPException(status_code=401, detail="Invalid authentication token")


@router.get("/api/v1/admin/batch/{task_id}/stream")
async def stream_batch(task_id: str, request: Request):
    task = get_task(task_id)
    if not task:
        raise HTTPException(status_code=404, detail="Task not found")
    await _verify_stream_api_key(request, task)

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

    return StreamingResponse(
        event_stream(),
        media_type="text/event-stream",
        headers={
            "Cache-Control": "no-cache",
            "Connection": "keep-alive",
            "X-Accel-Buffering": "no",
        },
    )


@router.post("/api/v1/admin/batch/{task_id}/cancel", dependencies=[Depends(verify_admin_access)])
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


@router.get("/", include_in_schema=False)
async def root_to_admin():
    """默认入口跳转到管理后台登录页"""
    return RedirectResponse(url="/admin", status_code=307)


@router.get("/admin/config", response_class=HTMLResponse, include_in_schema=False)
async def admin_config_page():
    """配置管理页"""
    return await render_template("admin/app.html")


@router.get("/admin/token", response_class=HTMLResponse, include_in_schema=False)
async def admin_token_page():
    """Token 管理页"""
    return await render_template("admin/app.html")


@router.get("/admin/imagine", response_class=HTMLResponse, include_in_schema=False)
async def admin_imagine_page():
    """Imagine 实时生成页"""
    return await render_template("admin/app.html")


@router.get("/admin/voice", response_class=HTMLResponse, include_in_schema=False)
async def admin_voice_page():
    """Voice 实时会话页"""
    return await render_template("admin/app.html")


# ---- 登录暴力破解防护 ----
_LOGIN_MAX_ATTEMPTS = 5
_LOGIN_WINDOW_SEC = 300  # 5 分钟窗口
_LOGIN_LOCKOUT_SEC = 900  # 锁定 15 分钟
_LOGIN_ATTEMPTS: dict[str, list[float]] = {}
_LOGIN_ATTEMPTS_LOCK = asyncio.Lock()


async def _check_login_rate_limit(client_ip: str) -> None:
    """检查登录频率限制，超限则抛出 429"""
    now = time.time()
    async with _LOGIN_ATTEMPTS_LOCK:
        attempts = _LOGIN_ATTEMPTS.get(client_ip, [])
        # 清除过期记录（超出锁定窗口的）
        attempts = [t for t in attempts if now - t < _LOGIN_LOCKOUT_SEC]
        _LOGIN_ATTEMPTS[client_ip] = attempts

        # 计算窗口内的失败次数
        recent = [t for t in attempts if now - t < _LOGIN_WINDOW_SEC]
        if len(recent) >= _LOGIN_MAX_ATTEMPTS:
            # 从最后一次失败开始计算剩余锁定时间
            last_attempt = max(recent)
            remaining = int(_LOGIN_LOCKOUT_SEC - (now - last_attempt))
            logger.warning(
                f"登录暴力破解防护触发: ip={client_ip}, "
                f"attempts={len(recent)}, lockout_remaining={remaining}s"
            )
            raise HTTPException(
                status_code=429,
                detail=f"Too many login attempts. Try again in {remaining} seconds.",
            )


async def _record_login_failure(client_ip: str) -> None:
    """记录一次登录失败"""
    now = time.time()
    async with _LOGIN_ATTEMPTS_LOCK:
        if client_ip not in _LOGIN_ATTEMPTS:
            _LOGIN_ATTEMPTS[client_ip] = []
        _LOGIN_ATTEMPTS[client_ip].append(now)


@router.post("/api/v1/admin/login")
async def admin_login_api(request: Request):
    """管理后台登录验证（支持两种方式）
    1. JSON body: {"username": "xxx", "password": "xxx"} - 登录页使用
    2. Bearer header: Authorization: Bearer <app_password> - 已登录用户验证
    """
    client_ip = get_client_ip(request)
    await _check_login_rate_limit(client_ip)

    app_username = get_config("app.app_username", "admin")
    app_password = get_config("app.app_password", "")

    if not app_password:
        raise HTTPException(status_code=401, detail="App password is not configured")

    # 尝试读取 JSON body
    body_username = ""
    body_password = ""
    try:
        body = await request.json()
        body_username = body.get("username", "")
        body_password = body.get("password", "")
    except Exception:
        pass

    # 方式1: JSON body 登录
    if body_username and body_password:
        if not secrets.compare_digest(body_username, app_username) or not secrets.compare_digest(
            body_password, app_password
        ):
            await _record_login_failure(client_ip)
            raise HTTPException(status_code=401, detail="Invalid username or password")
        return {"status": "success"}

    # 方式2: Bearer header 验证（已登录用户）
    auth_header = request.headers.get("Authorization", "")
    if auth_header.startswith("Bearer "):
        token = auth_header[7:]
        if await validate_admin_token(token):
            return {"status": "success"}

    await _record_login_failure(client_ip)
    raise HTTPException(status_code=401, detail="Invalid credentials")


@router.post("/api/v1/admin/ws/token", dependencies=[Depends(verify_admin_access)])
async def issue_imagine_ws_token_api():
    """签发短时一次性 WebSocket 鉴权 token。"""
    return {
        "status": "success",
        "token": _issue_ws_token(),
        "expires_in": _WS_TOKEN_TTL_SEC,
    }


@router.get("/api/v1/admin/voice/token", dependencies=[Depends(verify_admin_access)])
async def get_voice_token_api(
    voice: str = Query(default="ara"),
    personality: str = Query(default="assistant"),
    speed: float = Query(default=1.0, ge=0.5, le=2.0),
):
    """获取 LiveKit 语音会话 token"""
    from app.services.grok.voice import VoiceService
    from app.services.token.service import TokenService

    token = await TokenService.get_token("ssoBasic")
    if not token:
        token = await TokenService.get_token("ssoSuper")
    if not token:
        raise HTTPException(status_code=429, detail="No available tokens")

    service = VoiceService()
    result = await service.get_token(token=token, voice=voice, personality=personality, speed=speed)

    livekit_token = result.get("token")
    livekit_url = (
        result.get("livekitUrl")
        or result.get("url")
        or get_config("voice.livekit_url", "wss://livekit.grok.com")
    )
    if not livekit_token:
        raise HTTPException(status_code=502, detail="Invalid voice token response")

    return {"token": livekit_token, "url": livekit_url}


def _normalize_base64_blob(blob: str) -> str:
    value = (blob or "").strip()
    if not value:
        return ""
    if "," in value and "base64" in value.split(",", 1)[0].lower():
        return value.split(",", 1)[1]
    return value


def _guess_image_ext_by_signature(raw: bytes) -> str:
    if raw.startswith(b"\x89PNG\r\n\x1a\n"):
        return ".png"
    if raw.startswith(b"\xff\xd8\xff"):
        return ".jpg"
    if raw.startswith((b"GIF87a", b"GIF89a")):
        return ".gif"
    if raw.startswith(b"RIFF") and b"WEBP" in raw[:16]:
        return ".webp"
    if raw.startswith(b"BM"):
        return ".bmp"
    if len(raw) >= 12 and raw[4:8] == b"ftyp" and raw[8:12] in {b"avif", b"avis"}:
        return ".avif"
    return ".jpg"


async def _cache_imagine_base64_image(blob: str, run_id: str, seq: int, app_url: str) -> str:
    b64 = _normalize_base64_blob(blob)
    if not b64:
        return ""

    try:
        raw = base64.b64decode(b64, validate=False)
    except Exception:
        return ""

    if len(raw) < 100:
        return ""

    image_dir = DATA_DIR / "tmp" / "image"
    image_dir.mkdir(parents=True, exist_ok=True)
    ext = _guess_image_ext_by_signature(raw)
    filename = f"imagine-ws-{(run_id or 'run')[:8]}-{seq}-{uuid.uuid4().hex[:8]}{ext}"
    cache_path = image_dir / filename

    async with aiofiles.open(cache_path, "wb") as f:
        await f.write(raw)

    local_source = f"/v1/files/image/{filename}"
    if app_url:
        local_source = f"{app_url}{local_source}"
    return local_source


def _normalize_image_count(raw: object) -> int:
    try:
        value = int(raw)
    except (TypeError, ValueError):
        value = 4
    return value if value in {1, 4, 9, 16} else 4


async def _record_admin_request_log(
    model: str,
    success: bool,
    duration_sec: float,
    ip: str,
    key_name: str,
    token_suffix: str = "",
    error: str = "",
    status_code: int | None = None,
):
    if not get_config("stats.enabled", True):
        return

    try:
        from app.services.stats import request_stats, request_logger

        await request_stats.record_request(model, success)
        await request_logger.add_log(
            ip=ip,
            model=model,
            duration=duration_sec,
            status=status_code if status_code is not None else (200 if success else 500),
            key_name=key_name,
            token_suffix=token_suffix,
            error=error,
        )
    except Exception as e:
        logger.warning(f"Failed to record admin request stats: {e}")


@router.websocket("/api/v1/admin/imagine/ws")
async def imagine_ws(websocket: WebSocket):
    """Imagine 实时 WebSocket 接口"""
    # task_id 认证（SSE 模式创建的 session）
    session_id: str | None = None
    task_id_param = (websocket.query_params.get("task_id") or "").strip()
    auth_ok = False
    if task_id_param:
        info = await _get_imagine_session(task_id_param)
        if info:
            auth_ok = True
            session_id = task_id_param

    # ws_token 一次性令牌认证
    if not auth_ok:
        ws_token = (websocket.query_params.get("ws_token") or "").strip()
        auth_ok = bool(ws_token and _consume_ws_token(ws_token))

    # api_key 认证
    if not auth_ok:
        raw_key = _normalize_auth_token(websocket.query_params.get("api_key", ""))
        allow_query_api_key = bool(get_config("security.allow_query_api_key", False))
        auth_ok = bool(allow_query_api_key and raw_key and await validate_admin_token(raw_key))

    if not auth_ok:
        await websocket.close(code=1008)
        return

    await websocket.accept()

    from app.services.grok.image import image_service
    from app.services.grok.assets import DownloadService
    from app.services.token.models import EffortType
    from app.services.token.service import TokenService

    run_id = ""
    client_ip = "unknown"
    if websocket.client and websocket.client.host:
        client_ip = websocket.client.host
    stream_task: asyncio.Task | None = None
    stop_event = asyncio.Event()
    stop_reason = ""

    async def _stop_stream(reason: str = "") -> None:
        nonlocal stop_reason
        if reason:
            stop_reason = reason
        stop_event.set()
        nonlocal stream_task
        if stream_task and not stream_task.done():
            stream_task.cancel()
            try:
                await stream_task
            except asyncio.CancelledError:
                pass
            except Exception:
                pass
        stream_task = None

    async def _send_json(payload: dict) -> bool:
        nonlocal stop_reason
        try:
            await websocket.send_json(payload)
            return True
        except (WebSocketDisconnect, RuntimeError):
            if not stop_reason:
                stop_reason = "ws_send_failed"
            stop_event.set()
            return False
        except Exception:
            return False

    async def _stream_images(
        prompt: str,
        aspect_ratio: str,
        image_count: int,
        output_mode: str,
        rid: str,
    ):
        nonlocal stop_reason
        err_msg = ""
        has_final = False
        cancelled = False
        seq = 0
        start_ts = asyncio.get_running_loop().time()
        app_url = str(get_config("app.app_url") or "").rstrip("/")
        source_url_cache: dict[str, str] = {}
        download_service = DownloadService()
        last_token = ""

        image_ws_nsfw = bool(get_config("image.image_ws_nsfw", True))
        retries = int(get_config("retry.max_retry", 1))

        max_per_request = get_config("image.image_ws_max_per_request", 6)
        try:
            max_per_request = int(max_per_request)
        except Exception:
            max_per_request = 6
        max_per_request = max(1, min(max_per_request, 6))

        remaining = max(1, int(image_count))
        batch_index = 0

        try:
            while remaining > 0 and not stop_event.is_set():
                batch_index += 1
                request_n = min(max_per_request, remaining)

                token_pool = "ssoBasic"
                token = await TokenService.get_token("ssoBasic")
                if not token:
                    token_pool = "ssoSuper"
                    token = await TokenService.get_token("ssoSuper")

                if not token:
                    err_msg = "No available tokens"
                    await _send_json({"type": "error", "message": "No available tokens"})
                    break

                last_token = token
                logger.info(
                    "Imagine WS token selected from pool="
                    f"{token_pool}, suffix={token[-6:] if len(token) >= 6 else token}, "
                    f"batch={batch_index}, request_n={request_n}, remaining={remaining}"
                )

                batch_final_count = 0
                async for item in image_service.stream(
                    token=token,
                    prompt=prompt,
                    aspect_ratio=aspect_ratio,
                    n=request_n,
                    enable_nsfw=image_ws_nsfw,
                    max_retries=max(1, retries),
                ):
                    if stop_event.is_set():
                        break

                    if item.get("type") == "error":
                        err_msg = item.get("error") or "generation failed"
                        await _send_json(
                            {
                                "type": "error",
                                "message": err_msg,
                                "code": item.get("error_code") or "upstream_error",
                            }
                        )
                        break

                    if item.get("type") != "image":
                        continue

                    if not bool(item.get("is_final")):
                        continue

                    upstream_source_url = str(item.get("url") or "").strip()
                    b64 = _normalize_base64_blob(item.get("blob", ""))
                    if not b64 and not upstream_source_url:
                        continue

                    source_url = ""
                    if upstream_source_url:
                        source_url = source_url_cache.get(upstream_source_url, "")
                        if not source_url:
                            try:
                                file_path = upstream_source_url
                                if upstream_source_url.startswith("http"):
                                    file_path = urlparse(upstream_source_url).path
                                if not file_path.startswith("/"):
                                    file_path = f"/{file_path}"

                                await download_service.download(file_path, token, "image")

                                local_source = f"/v1/files/image{file_path}"
                                if app_url:
                                    local_source = f"{app_url}{local_source}"
                                source_url = local_source
                            except Exception as e:
                                logger.debug(
                                    f"Failed to cache imagine image url, fallback to upstream: {e}"
                                )
                                source_url = ""

                            if not source_url and b64:
                                try:
                                    source_url = await _cache_imagine_base64_image(
                                        b64,
                                        rid,
                                        seq + 1,
                                        app_url,
                                    )
                                except Exception as e:
                                    logger.debug(f"Failed to cache imagine base64 image: {e}")

                            if not source_url:
                                source_url = upstream_source_url

                            source_url_cache[upstream_source_url] = source_url

                    if not source_url and b64:
                        try:
                            source_url = await _cache_imagine_base64_image(
                                b64,
                                rid,
                                seq + 1,
                                app_url,
                            )
                        except Exception as e:
                            logger.debug(f"Failed to cache imagine base64-only image: {e}")

                    seq += 1
                    batch_final_count += 1
                    has_final = True
                    remaining = max(0, image_count - seq)

                    elapsed_ms = int((asyncio.get_running_loop().time() - start_ts) * 1000)
                    image_payload = {
                        "type": "image",
                        "image_id": item.get("image_id") or f"img-{seq}",
                        "source_url": source_url,
                        "stage": item.get("stage") or "preview",
                        "sequence": seq,
                        "is_final": True,
                        "elapsed_ms": elapsed_ms,
                    }
                    if output_mode != "url" or not source_url:
                        image_payload["b64_json"] = b64

                    sent = await _send_json(image_payload)
                    if not sent:
                        if not stop_reason:
                            stop_reason = "ws_send_failed"
                        if not err_msg:
                            err_msg = "WebSocket disconnected while sending image"
                        stop_event.set()
                        break

                    if remaining <= 0:
                        break

                if stop_event.is_set() or remaining <= 0:
                    if batch_final_count > 0:
                        try:
                            await TokenService.consume(token, EffortType.HIGH)
                        except Exception as e:
                            logger.warning(f"Failed to consume token for imagine ws: {e}")
                    break

                if err_msg:
                    break

                if batch_final_count > 0:
                    try:
                        await TokenService.consume(token, EffortType.HIGH)
                    except Exception as e:
                        logger.warning(f"Failed to consume token for imagine ws: {e}")
                else:
                    err_msg = "No final image received in current batch"
                    logger.warning(
                        "Imagine WS batch returned no final images",
                        extra={
                            "requested": request_n,
                            "remaining": remaining,
                            "batch": batch_index,
                        },
                    )
                    await _send_json(
                        {
                            "type": "error",
                            "message": "No final image received, please retry.",
                            "code": "empty_image",
                        }
                    )
                    break

        except asyncio.CancelledError:
            cancelled = True
            if not err_msg:
                err_msg = "Stream cancelled by client"
            return
        except Exception as e:
            err_msg = str(e)
            await _send_json(
                {
                    "type": "error",
                    "message": f"imagine stream failed: {str(e)}",
                    "code": "stream_failed",
                }
            )
        finally:
            try:
                await download_service.close()
            except Exception:
                pass

            elapsed = max(0.0, asyncio.get_running_loop().time() - start_ts)

            status_code = 200 if has_final else 500
            if not has_final and (
                cancelled
                or stop_reason
                in {"client_stop", "client_disconnected", "ws_send_failed", "runtime_disconnected"}
            ):
                status_code = 499
                if not err_msg:
                    err_msg = "Client disconnected before final image generation completed"

            if status_code >= 500 and not err_msg:
                err_msg = "Generation failed without detailed error"

            await _record_admin_request_log(
                model="grok-imagine-1.0",
                success=has_final,
                duration_sec=elapsed,
                ip=client_ip,
                key_name="admin-imagine",
                token_suffix=last_token[-8:] if last_token and len(last_token) >= 8 else last_token,
                error=err_msg,
                status_code=status_code,
            )

            await _send_json({"type": "status", "status": "stopped", "run_id": rid})

    try:
        while True:
            try:
                text = await websocket.receive_text()
            except WebSocketDisconnect:
                if not stop_reason:
                    stop_reason = "client_disconnected"
                break
            except RuntimeError:
                if not stop_reason:
                    stop_reason = "runtime_disconnected"
                break

            try:
                payload = json.loads(text)
            except Exception:
                await _send_json({"type": "error", "message": "Invalid JSON payload"})
                continue

            action = str(payload.get("type") or "").lower()

            if action == "stop":
                await _stop_stream("client_stop")
                if run_id:
                    await _send_json({"type": "status", "status": "stopped", "run_id": run_id})
                continue

            if action != "start":
                await _send_json(
                    {
                        "type": "error",
                        "message": "Unsupported action, use start/stop",
                    }
                )
                continue

            prompt = (payload.get("prompt") or "").strip()
            if not prompt:
                await _send_json({"type": "error", "message": "Prompt is required"})
                continue

            aspect_ratio = (payload.get("aspect_ratio") or "2:3").strip()
            if aspect_ratio not in {"2:3", "3:2", "1:1", "9:16", "16:9"}:
                aspect_ratio = "2:3"

            image_count = _normalize_image_count(payload.get("image_count", payload.get("n", 4)))
            output_mode = str(payload.get("output_mode") or "base64").strip().lower()
            if output_mode not in {"base64", "url"}:
                output_mode = "base64"

            await _stop_stream("restart")
            stop_event = asyncio.Event()
            stop_reason = ""
            run_id = uuid.uuid4().hex

            await _send_json(
                {
                    "type": "status",
                    "status": "running",
                    "run_id": run_id,
                    "image_count": image_count,
                    "output_mode": output_mode,
                }
            )
            stream_task = asyncio.create_task(
                _stream_images(prompt, aspect_ratio, image_count, output_mode, run_id)
            )
    finally:
        await _stop_stream()
        if session_id:
            await _delete_imagine_session(session_id)


# ---- Imagine Session REST API ----


def _resolve_aspect_ratio(raw: str) -> str:
    value = (raw or "2:3").strip()
    if value in {"2:3", "3:2", "1:1", "9:16", "16:9"}:
        return value
    return "2:3"


class ImagineStartRequest(BaseModel):
    prompt: str
    aspect_ratio: Optional[str] = "2:3"


@router.post("/api/v1/admin/imagine/start", dependencies=[Depends(verify_admin_access)])
async def admin_imagine_start(data: ImagineStartRequest):
    prompt = (data.prompt or "").strip()
    if not prompt:
        raise HTTPException(status_code=400, detail="Prompt cannot be empty")
    ratio = _resolve_aspect_ratio(str(data.aspect_ratio or "2:3").strip() or "2:3")
    task_id = await _create_imagine_session(prompt, ratio)
    return {"task_id": task_id, "aspect_ratio": ratio}


class ImagineStopRequest(BaseModel):
    task_ids: list[str]


@router.post("/api/v1/admin/imagine/stop", dependencies=[Depends(verify_admin_access)])
async def admin_imagine_stop(data: ImagineStopRequest):
    removed = await _delete_imagine_sessions(data.task_ids or [])
    return {"status": "success", "removed": removed}


@router.get("/api/v1/admin/imagine/sse")
async def admin_imagine_sse(
    request: Request,
    task_id: str = Query(""),
    prompt: str = Query(""),
    aspect_ratio: str = Query("2:3"),
):
    """Imagine 图片瀑布流（SSE 兜底）"""
    from app.services.grok.image import image_service
    from app.services.grok.models.model import ModelService
    from app.services.grok.processors.image_ws_processors import ImageWSCollectProcessor
    from app.services.token.models import EffortType
    from app.services.token.manager import get_token_manager

    session = None
    if task_id:
        session = await _get_imagine_session(task_id)
        if not session:
            raise HTTPException(status_code=404, detail="Task not found")
    else:
        await _verify_stream_api_key(request)

    if session:
        prompt = str(session.get("prompt") or "").strip()
        ratio = str(session.get("aspect_ratio") or "2:3").strip() or "2:3"
    else:
        prompt = (prompt or "").strip()
        if not prompt:
            raise HTTPException(status_code=400, detail="Prompt cannot be empty")
        ratio = _resolve_aspect_ratio(str(aspect_ratio or "2:3").strip() or "2:3")

    async def event_stream():
        try:
            model_id = "grok-imagine-1.0"
            model_info = ModelService.get(model_id)
            if not model_info or not model_info.is_image:
                yield _sse_event(
                    {
                        "type": "error",
                        "message": "Image model is not available.",
                        "code": "model_not_supported",
                    }
                )
                return

            token_mgr = await get_token_manager()
            enable_nsfw = bool(get_config("image.image_ws_nsfw", True))
            sequence = 0
            run_id = uuid.uuid4().hex

            yield _sse_event(
                {
                    "type": "status",
                    "status": "running",
                    "prompt": prompt,
                    "aspect_ratio": ratio,
                    "run_id": run_id,
                }
            )

            while True:
                if await request.is_disconnected():
                    break
                if task_id:
                    session_alive = await _get_imagine_session(task_id)
                    if not session_alive:
                        break

                try:
                    await token_mgr.reload_if_stale()
                    token = None
                    for pool_name in ModelService.pool_candidates_for_model(model_info.model_id):
                        token = token_mgr.get_token(pool_name)
                        if token:
                            break

                    if not token:
                        yield _sse_event(
                            {
                                "type": "error",
                                "message": "No available tokens. Please try again later.",
                                "code": "rate_limit_exceeded",
                            }
                        )
                        await asyncio.sleep(2)
                        continue

                    upstream = image_service.stream(
                        token=token,
                        prompt=prompt,
                        aspect_ratio=ratio,
                        n=6,
                        enable_nsfw=enable_nsfw,
                    )

                    processor = ImageWSCollectProcessor(
                        model_info.model_id,
                        token,
                        n=6,
                        response_format="b64_json",
                    )

                    start_at = time.time()
                    images = await processor.process(upstream)
                    elapsed_ms = int((time.time() - start_at) * 1000)

                    if images and all(img and img != "error" for img in images):
                        for img_b64 in images:
                            sequence += 1
                            yield _sse_event(
                                {
                                    "type": "image",
                                    "b64_json": img_b64,
                                    "sequence": sequence,
                                    "created_at": int(time.time() * 1000),
                                    "elapsed_ms": elapsed_ms,
                                    "aspect_ratio": ratio,
                                    "run_id": run_id,
                                }
                            )

                        try:
                            effort = (
                                EffortType.HIGH
                                if (model_info and model_info.cost.value == "high")
                                else EffortType.LOW
                            )
                            await token_mgr.consume(token, effort)
                        except Exception as e:
                            logger.warning(f"Failed to consume token: {e}")
                    else:
                        yield _sse_event(
                            {
                                "type": "error",
                                "message": "Image generation returned empty data.",
                                "code": "empty_image",
                            }
                        )
                except asyncio.CancelledError:
                    break
                except Exception as e:
                    logger.warning(f"Imagine SSE error: {e}")
                    yield _sse_event({"type": "error", "message": str(e), "code": "internal_error"})
                    await asyncio.sleep(1.5)

            yield _sse_event({"type": "status", "status": "stopped", "run_id": run_id})
        finally:
            if task_id:
                await _delete_imagine_session(task_id)

    return StreamingResponse(
        event_stream(),
        media_type="text/event-stream",
        headers={"Cache-Control": "no-cache", "Connection": "keep-alive"},
    )


@router.get("/api/v1/admin/config", dependencies=[Depends(verify_admin_access)])
async def get_config_api():
    """获取当前配置（敏感字段脱敏）"""
    return _sanitize_config_payload(config._config)


@router.post("/api/v1/admin/config", dependencies=[Depends(verify_admin_access)])
async def update_config_api(data: dict):
    """更新配置（支持保留脱敏字段原值）"""
    try:
        restored = _restore_redacted_values(data, config._config)
        await config.update(restored)
        return {"status": "success", "message": "配置已更新"}
    except Exception as e:
        logger.error(f"Update config failed: {e}")
        raise HTTPException(status_code=500, detail="Internal server error")


@router.get("/api/v1/admin/storage", dependencies=[Depends(verify_admin_access)])
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


@router.get("/api/v1/admin/tokens", dependencies=[Depends(verify_admin_access)])
async def get_tokens_api():
    """获取所有 Token（使用 TokenManager 缓存）"""
    from app.services.token.manager import get_token_manager

    mgr = await get_token_manager()
    # 性能优化：直接从 TokenManager 内存缓存获取，避免每次查询存储
    return mgr.export_pools()


@router.post("/api/v1/admin/tokens", dependencies=[Depends(verify_admin_access)])
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
        logger.error(f"Update tokens failed: {e}")
        raise HTTPException(status_code=500, detail="Internal server error")


@router.post("/api/v1/admin/tokens/refresh", dependencies=[Depends(verify_admin_access)])
async def refresh_tokens_api(data: dict):
    """刷新 Token 状态"""
    from app.services.token.manager import get_token_manager
    from app.services.grok.batch import run_in_batches

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
            logger.warning(f"Usage refresh: truncated from {original_count} to {max_tokens} tokens")

        # 批量执行配置
        max_concurrent = get_config("performance.usage_max_concurrent", 25)
        batch_size = get_config("performance.usage_batch_size", 50)

        async def _refresh_one(t):
            return await mgr.sync_usage(t, "grok-3", consume_on_fail=False, is_usage=False)

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
        logger.error(f"Refresh tokens failed: {e}")
        raise HTTPException(status_code=500, detail="Internal server error")


@router.post("/api/v1/admin/tokens/refresh/async", dependencies=[Depends(verify_admin_access)])
async def refresh_tokens_api_async(data: dict):
    """刷新 Token 状态（异步批量 + SSE 进度）"""
    from app.services.token.manager import get_token_manager
    from app.services.grok.batch import run_in_batches

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
        logger.warning(f"Usage refresh: truncated from {original_count} to {max_tokens} tokens")

    max_concurrent = get_config("performance.usage_max_concurrent", 25)
    batch_size = get_config("performance.usage_batch_size", 50)

    task = create_task(len(unique_tokens))

    async def _run():
        try:

            async def _refresh_one(t: str):
                return await mgr.sync_usage(t, "grok-3", consume_on_fail=False, is_usage=False)

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
                warning = f"数量超出限制，仅处理前 {max_tokens} 个（共 {original_count} 个）"
            task.finish(result, warning=warning)
        except Exception as e:
            task.fail_task(str(e))
        finally:
            asyncio.create_task(expire_task(task.id, 300))

    asyncio.create_task(_run())

    return {
        "status": "success",
        "task_id": task.id,
        "stream_token": task.stream_token,
        "total": len(unique_tokens),
    }


@router.post("/api/v1/admin/tokens/nsfw/enable", dependencies=[Depends(verify_admin_access)])
async def enable_nsfw_api(data: dict):
    """批量开启 NSFW (Unhinged) 模式"""
    from app.services.grok.nsfw import NSFWService
    from app.services.grok.batch import run_in_batches
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
                    raw = info.token[4:] if info.token.startswith("sso=") else info.token
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
            logger.warning(f"NSFW enable: truncated from {original_count} to {max_tokens} tokens")

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
        raise HTTPException(status_code=500, detail="Internal server error")


@router.post("/api/v1/admin/tokens/nsfw/enable/async", dependencies=[Depends(verify_admin_access)])
async def enable_nsfw_api_async(data: dict):
    """批量开启 NSFW (Unhinged) 模式（异步批量 + SSE 进度）"""
    from app.services.grok.nsfw import NSFWService
    from app.services.grok.batch import run_in_batches
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
        logger.warning(f"NSFW enable: truncated from {original_count} to {max_tokens} tokens")

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
                warning = f"数量超出限制，仅处理前 {max_tokens} 个（共 {original_count} 个）"
            task.finish(result, warning=warning)
        except Exception as e:
            task.fail_task(str(e))
        finally:
            asyncio.create_task(expire_task(task.id, 300))

    asyncio.create_task(_run())

    return {
        "status": "success",
        "task_id": task.id,
        "stream_token": task.stream_token,
        "total": len(unique_tokens),
    }


@router.get("/admin/cache", response_class=HTMLResponse, include_in_schema=False)
async def admin_cache_page():
    """缓存管理页"""
    return await render_template("admin/app.html")


@router.get("/admin/stats", response_class=HTMLResponse, include_in_schema=False)
async def admin_stats_page():
    """统计监控页"""
    return await render_template("admin/app.html")


@router.get("/admin/keys", response_class=HTMLResponse, include_in_schema=False)
async def admin_keys_page():
    """Key 管理页"""
    return await render_template("admin/app.html")


@router.get("/api/v1/admin/cache", dependencies=[Depends(verify_admin_access)])
async def get_cache_stats_api(request: Request):
    """获取缓存统计"""
    from app.services.grok.assets import DownloadService, ListService
    from app.services.token.manager import get_token_manager
    from app.services.grok.batch import run_in_batches

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
                masked = (
                    f"{raw_token[:8]}...{raw_token[-16:]}" if len(raw_token) > 24 else raw_token
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
                        "last_asset_clear_at": (
                            account["last_asset_clear_at"] if account else None
                        ),
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
                        "last_asset_clear_at": (
                            account["last_asset_clear_at"] if account else None
                        ),
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
                        "last_asset_clear_at": (
                            account["last_asset_clear_at"] if account else None
                        ),
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
                        "last_asset_clear_at": (
                            account["last_asset_clear_at"] if account else None
                        ),
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
                        "last_asset_clear_at": (match["last_asset_clear_at"] if match else None),
                    }
                except Exception as e:
                    match = next((a for a in accounts if a["token"] == token), None)
                    online_stats = {
                        "count": 0,
                        "status": f"error: {str(e)}",
                        "token": token,
                        "token_masked": match["token_masked"] if match else token,
                        "last_asset_clear_at": (match["last_asset_clear_at"] if match else None),
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
        logger.error(f"Get cache stats failed: {e}")
        raise HTTPException(status_code=500, detail="Internal server error")


@router.post("/api/v1/admin/cache/online/load/async", dependencies=[Depends(verify_admin_access)])
async def load_online_cache_api_async(data: dict):
    """在线资产统计（异步批量 + SSE 进度）"""
    from app.services.grok.assets import DownloadService, ListService
    from app.services.token.manager import get_token_manager
    from app.services.grok.batch import run_in_batches

    mgr = await get_token_manager()

    # 账号列表
    accounts = []
    for pool_name, pool in mgr.pools.items():
        for info in pool.list():
            raw_token = info.token[4:] if info.token.startswith("sso=") else info.token
            masked = f"{raw_token[:8]}...{raw_token[-16:]}" if len(raw_token) > 24 else raw_token
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
                        "last_asset_clear_at": (
                            account["last_asset_clear_at"] if account else None
                        ),
                    }
                    return {"ok": True, "detail": detail, "count": count}
                except Exception as e:
                    detail = {
                        "token": token,
                        "token_masked": account["token_masked"] if account else token,
                        "count": 0,
                        "status": f"error: {str(e)}",
                        "last_asset_clear_at": (
                            account["last_asset_clear_at"] if account else None
                        ),
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
                warning = f"数量超出限制，仅处理前 {max_tokens} 个（共 {original_count} 个）"
            task.finish(result, warning=warning)
        except Exception as e:
            task.fail_task(str(e))
        finally:
            asyncio.create_task(expire_task(task.id, 300))

    asyncio.create_task(_run())

    return {
        "status": "success",
        "task_id": task.id,
        "stream_token": task.stream_token,
        "total": len(selected_tokens),
    }


@router.post("/api/v1/admin/cache/clear", dependencies=[Depends(verify_admin_access)])
async def clear_local_cache_api(data: dict):
    """清理本地缓存"""
    from app.services.grok.assets import DownloadService

    cache_type = data.get("type", "image")

    try:
        dl_service = DownloadService()
        result = dl_service.clear(cache_type)
        return {"status": "success", "result": result}
    except Exception as e:
        logger.error(f"Clear local cache failed: {e}")
        raise HTTPException(status_code=500, detail="Internal server error")


@router.get("/api/v1/admin/cache/list", dependencies=[Depends(verify_admin_access)])
async def list_local_cache_api(
    cache_type: str = "image",
    type_: str = Query(default=None, alias="type"),
    page: int = 1,
    page_size: int = 1000,
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
        logger.error(f"List local cache failed: {e}")
        raise HTTPException(status_code=500, detail="Internal server error")


@router.post("/api/v1/admin/cache/item/delete", dependencies=[Depends(verify_admin_access)])
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
        logger.error(f"Delete cache item failed: {e}")
        raise HTTPException(status_code=500, detail="Internal server error")


@router.post("/api/v1/admin/cache/online/clear", dependencies=[Depends(verify_admin_access)])
async def clear_online_cache_api(data: dict):
    """清理在线缓存"""
    from app.services.grok.assets import DeleteService
    from app.services.token.manager import get_token_manager
    from app.services.grok.batch import run_in_batches

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
            raise HTTPException(status_code=400, detail="No available token to perform cleanup")

        result = await delete_service.delete_all(token)
        await mgr.mark_asset_clear(token)
        return {"status": "success", "result": result}
    except Exception as e:
        logger.error(f"Clear online cache failed: {e}")
        raise HTTPException(status_code=500, detail="Internal server error")
    finally:
        if delete_service:
            await delete_service.close()


@router.post("/api/v1/admin/cache/online/clear/async", dependencies=[Depends(verify_admin_access)])
async def clear_online_cache_api_async(data: dict):
    """清理在线缓存（异步批量 + SSE 进度）"""
    from app.services.grok.assets import DeleteService
    from app.services.token.manager import get_token_manager
    from app.services.grok.batch import run_in_batches

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
                warning = f"数量超出限制，仅处理前 {max_tokens} 个（共 {original_count} 个）"
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
        "stream_token": task.stream_token,
        "total": len(token_list),
    }


# ==================== 统计 API ====================


@router.get("/api/v1/admin/stats/requests", dependencies=[Depends(verify_admin_access)])
async def get_request_stats_api(hours: int = 24, days: int = 7):
    """获取请求统计数据"""
    if not get_config("stats.enabled", True):
        raise HTTPException(status_code=400, detail="Stats feature is disabled")

    from app.services.stats.request_stats import request_stats

    return {"status": "success", "data": request_stats.get_stats(hours, days)}


@router.post("/api/v1/admin/stats/reset", dependencies=[Depends(verify_admin_access)])
async def reset_stats_api():
    """重置统计数据"""
    if not get_config("stats.enabled", True):
        raise HTTPException(status_code=400, detail="Stats feature is disabled")

    from app.services.stats.request_stats import request_stats

    await request_stats.reset()
    return {"status": "success", "message": "统计数据已重置"}


# ==================== 日志 API ====================


@router.get("/api/v1/admin/logs", dependencies=[Depends(verify_admin_access)])
async def get_logs_api(limit: int = 1000):
    """获取请求日志"""
    if not get_config("stats.enabled", True):
        raise HTTPException(status_code=400, detail="Stats feature is disabled")

    from app.services.stats.request_logger import request_logger

    logs = await request_logger.get_logs(min(limit, 5000))
    return {"status": "success", "data": logs}


@router.post("/api/v1/admin/logs/clear", dependencies=[Depends(verify_admin_access)])
async def clear_logs_api():
    """清空日志"""
    if not get_config("stats.enabled", True):
        raise HTTPException(status_code=400, detail="Stats feature is disabled")

    from app.services.stats.request_logger import request_logger

    await request_logger.clear_logs()
    return {"status": "success", "message": "日志已清空"}


# ==================== 代理池 API ====================


@router.get("/api/v1/admin/proxy", dependencies=[Depends(verify_admin_access)])
async def get_proxy_status_api():
    """获取代理池状态"""
    from app.core.proxy_pool import proxy_pool

    return {
        "status": "success",
        "data": {
            "enabled": proxy_pool._enabled,
            "current_proxy": proxy_pool.get_current_proxy(),
            "pool_url": proxy_pool._pool_url,
            "interval": proxy_pool._fetch_interval,
        },
    }


@router.post("/api/v1/admin/proxy/refresh", dependencies=[Depends(verify_admin_access)])
async def refresh_proxy_api():
    """强制刷新代理"""
    from app.core.proxy_pool import proxy_pool

    new_proxy = await proxy_pool.force_refresh()
    return {"status": "success", "proxy": new_proxy}


# ==================== Key 管理 API ====================


@router.get("/api/v1/admin/keys", dependencies=[Depends(verify_admin_access)])
async def get_keys_api():
    """获取所有 API Key"""
    from app.services.api_keys import api_key_manager

    await api_key_manager.init()
    keys = api_key_manager.get_all_keys()
    return {"status": "success", "data": keys}


@router.post("/api/v1/admin/keys", dependencies=[Depends(verify_admin_access)])
async def create_key_api(request: Request):
    """创建 API Key"""
    from app.services.api_keys import api_key_manager

    await api_key_manager.init()
    body = await request.json()
    name = body.get("name", "")
    new_key = await api_key_manager.add_key(name)
    return {"status": "success", "data": new_key}


@router.post("/api/v1/admin/keys/batch", dependencies=[Depends(verify_admin_access)])
async def batch_create_keys_api(request: Request):
    """批量创建 API Key"""
    from app.services.api_keys import api_key_manager

    await api_key_manager.init()
    body = await request.json()
    count = min(int(body.get("count", 5)), 100)
    prefix = body.get("prefix", "")
    new_keys = await api_key_manager.batch_add_keys(prefix, count)
    return {"status": "success", "data": new_keys}


@router.patch("/api/v1/admin/keys/{key_id}", dependencies=[Depends(verify_admin_access)])
async def update_key_api(key_id: str, request: Request):
    """更新 API Key"""
    from app.services.api_keys import api_key_manager

    await api_key_manager.init()
    body = await request.json()
    name = body.get("name")
    enabled = body.get("enabled")
    success = await api_key_manager.update_key(key_id, name, enabled)
    if success:
        return {"status": "success"}
    raise HTTPException(status_code=404, detail="Key not found")


@router.delete("/api/v1/admin/keys/{key_id}", dependencies=[Depends(verify_admin_access)])
async def delete_key_api(key_id: str):
    """删除 API Key"""
    from app.services.api_keys import api_key_manager

    await api_key_manager.init()
    success = await api_key_manager.delete_key(key_id)
    if success:
        return {"status": "success"}
    raise HTTPException(status_code=404, detail="Key not found")
