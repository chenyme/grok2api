"""Shared xAI OAuth completion (loopback + manual paste)."""

from typing import TYPE_CHECKING

from app.platform.errors import ValidationError
from app.platform.logging.logger import logger
from app.control.xai import account as xai_account
from app.control.xai import oauth, pending
from app.dataplane.proxy import get_proxy_runtime

if TYPE_CHECKING:
    from app.control.account.repository import AccountRepository


async def finish_oauth(
    repo: "AccountRepository",
    *,
    state: str,
    code: str,
) -> dict:
    """Consume pending PKCE state, exchange the code, and persist the xAI account."""
    pend = await pending.consume(repo, state)
    if pend is None:
        raise ValidationError(
            "登录会话无效或已过期，请重新发起 xAI 登录。",
            param="state",
            code="oauth_state_invalid",
        )

    proxy = await get_proxy_runtime()
    lease = await proxy.acquire()
    try:
        token_resp = await oauth.exchange_code(
            pend["token_endpoint"],
            code=code,
            redirect_uri=pend["redirect_uri"],
            code_verifier=pend["code_verifier"],
            code_challenge=pend.get("code_challenge") or "",
            lease=lease,
        )
    except Exception as exc:  # noqa: BLE001
        logger.warning("xai oauth token exchange failed: error={}", exc)
        raise ValidationError(
            f"令牌交换失败：{exc}",
            param="callback",
            code="oauth_token_exchange_failed",
        ) from exc

    email, sub = oauth.parse_id_token(token_resp.get("id_token", ""))
    ext = xai_account.build_ext_from_token_response(
        token_resp,
        token_endpoint=pend["token_endpoint"],
        base_url=oauth.DEFAULT_API_BASE,
        email=email,
        sub=sub,
    )
    await xai_account.upsert_xai_account(repo, ext=ext, email=email, sub=sub)
    removed = await pending.cleanup_all(repo)
    if removed:
        logger.debug("xai oauth cleared stale pending rows: count={}", removed)
    logger.info("xai oauth completed: email={} sub={}", email or "-", sub or "-")
    return {
        "status": "ok",
        "email": email,
        "sub": sub,
        "message": f"已成功登录并保存 xAI 账号：{email or sub or '(未知)'}",
    }


__all__ = ["finish_oauth"]