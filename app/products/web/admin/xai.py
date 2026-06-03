"""Admin API — xAI official-API OAuth login + account management.

Endpoints (all under ``/admin/api``, admin-guarded):
  POST   /xai/oauth/start     — discovery + PKCE, loopback listener, authorize URL
  POST   /xai/oauth/complete  — manual paste: callback URL / code + state
  GET    /xai/oauth/poll      — poll loopback/manual completion outcome
  GET    /xai/accounts        — list saved xAI accounts (masked)
  DELETE /xai/accounts        — remove saved xAI account(s)

OAuth uses the grok-cli registered loopback redirect
``http://127.0.0.1:56121/callback`` (not ``app.app_url``).
"""

import asyncio
from typing import TYPE_CHECKING, Any

import orjson
from fastapi import APIRouter, Body, Depends, Query
from fastapi.responses import Response
from pydantic import BaseModel, Field

from app.platform.errors import ValidationError
from app.platform.logging.logger import logger
from app.control.xai import account as xai_account
from app.control.xai import oauth, pending
from app.control.xai.callback_parse import parse_oauth_callback
from app.control.xai.constants import OAUTH_REDIRECT_URI
from app.control.xai.flow import finish_oauth
from app.control.xai.loopback import get_loopback
from app.control.xai import sessions as oauth_sessions
from app.dataplane.proxy import get_proxy_runtime

if TYPE_CHECKING:
    from app.control.account.repository import AccountRepository

from . import get_repo

router = APIRouter(tags=["Admin - xAI"])


def _json(data) -> Response:
    return Response(content=orjson.dumps(data), media_type="application/json")


def _mask(token: str) -> str:
    return f"{token[:10]}...{token[-6:]}" if len(token) > 20 else token


class XaiOAuthCompleteRequest(BaseModel):
    state: str = Field(min_length=8)
    callback: str = Field(min_length=1)


async def _begin_oauth(repo: "AccountRepository") -> dict[str, Any]:
    await pending.cleanup_expired(repo)
    proxy = await get_proxy_runtime()
    lease = await proxy.acquire()
    auth_ep, token_ep = await oauth.discover(lease=lease)

    from app.control.xai.pkce import generate_pkce

    verifier, challenge = generate_pkce()
    state = pending.new_state()
    nonce = pending.new_state()

    await pending.save(
        repo,
        state=state,
        code_verifier=verifier,
        code_challenge=challenge,
        token_endpoint=token_ep,
        redirect_uri=OAUTH_REDIRECT_URI,
    )

    authorize_url = oauth.build_authorize_url(
        auth_ep,
        redirect_uri=OAUTH_REDIRECT_URI,
        code_challenge=challenge,
        state=state,
        nonce=nonce,
    )

    loopback = get_loopback()
    loopback_available = await loopback.ensure_started()
    if loopback_available:
        oauth_sessions.clear_outcome(state)
        asyncio.create_task(_wait_loopback(repo, state)).add_done_callback(
            _log_background_task_error
        )

    logger.info(
        "xai oauth started: redirect_uri={} loopback={}",
        OAUTH_REDIRECT_URI,
        loopback_available,
    )
    return {
        "authorize_url": authorize_url,
        "state": state,
        "redirect_uri": OAUTH_REDIRECT_URI,
        "loopback_available": loopback_available,
    }


def _log_background_task_error(task: asyncio.Task) -> None:
    if task.cancelled():
        return
    exc = task.exception()
    if exc is not None:
        logger.warning("xai oauth background task failed: error={}", exc)


async def _wait_loopback(repo: "AccountRepository", state: str) -> None:
    loopback = get_loopback()
    try:
        code = await loopback.wait_for_code(state)
        result = await finish_oauth(repo, state=state, code=code)
        oauth_sessions.set_outcome(
            state,
            status="ok",
            message=result.get("message", ""),
            email=result.get("email"),
        )
    except asyncio.TimeoutError:
        loopback.cancel_waiter(state)
        oauth_sessions.set_outcome(
            state,
            status="timeout",
            message="授权超时，请重新登录或使用「手动粘贴」。",
        )
    except Exception as exc:  # noqa: BLE001
        loopback.cancel_waiter(state)
        oauth_sessions.set_outcome(state, status="error", message=str(exc))


@router.post("/xai/oauth/start")
async def xai_oauth_start(repo: "AccountRepository" = Depends(get_repo)):
    """Begin xAI OAuth; start loopback listener and return the authorize URL."""
    return _json(await _begin_oauth(repo))


@router.post("/xai/oauth/complete")
async def xai_oauth_complete(
    req: XaiOAuthCompleteRequest,
    repo: "AccountRepository" = Depends(get_repo),
):
    """Complete OAuth via pasted callback URL, query string, or bare code."""
    get_loopback().cancel_waiter(req.state)
    try:
        code, parsed_state = parse_oauth_callback(
            req.callback, expected_state=req.state
        )
    except ValueError as exc:
        raise ValidationError(
            str(exc),
            param="callback",
            code="oauth_callback_invalid",
        ) from exc

    if parsed_state != req.state:
        raise ValidationError(
            "state 与当前登录会话不匹配，请确认粘贴的是本次登录的回调。",
            param="callback",
            code="oauth_state_mismatch",
        )

    result = await finish_oauth(repo, state=req.state, code=code)
    oauth_sessions.set_outcome(
        req.state,
        status="ok",
        message=result.get("message", ""),
        email=result.get("email"),
    )
    return _json(result)


@router.get("/xai/oauth/poll")
async def xai_oauth_poll(
    state: str = Query(min_length=8),
    repo: "AccountRepository" = Depends(get_repo),
):
    """Poll OAuth completion after ``/xai/oauth/start`` (loopback or manual)."""
    outcome = oauth_sessions.peek_outcome(state)
    if outcome:
        oauth_sessions.pop_outcome(state)
        return _json(outcome)

    if await pending.exists(repo, state):
        return _json({"status": "pending"})

    return _json(
        {
            "status": "expired",
            "message": "登录会话无效或已过期，请重新发起 xAI 登录。",
        }
    )


@router.get("/xai/accounts")
async def xai_list_accounts(repo: "AccountRepository" = Depends(get_repo)):
    """List saved xAI accounts (token masked)."""
    records = await xai_account.list_xai_accounts(repo)
    items = [
        {
            "token": _mask(r.token),
            "email": (r.ext or {}).get("email"),
            "expires_at": (r.ext or {}).get("expires_at"),
            "last_refresh": (r.ext or {}).get("last_refresh"),
            "status": r.status,
        }
        for r in records
    ]
    return _json({"accounts": items})


@router.delete("/xai/accounts")
async def xai_delete_accounts(
    tokens: list[str] | None = Body(default=None),
    repo: "AccountRepository" = Depends(get_repo),
):
    """Delete saved xAI account(s). With no body, removes all xAI accounts."""
    if tokens:
        targets = tokens
    else:
        targets = [r.token for r in await xai_account.list_xai_accounts(repo)]
    if not targets:
        return _json({"deleted": 0})
    await repo.delete_accounts(targets)
    logger.info("xai accounts deleted: count={}", len(targets))
    return _json({"deleted": len(targets)})


__all__ = ["router"]