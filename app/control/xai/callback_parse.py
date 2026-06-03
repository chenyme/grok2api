"""Parse authorization codes from loopback callbacks or manual paste."""

from urllib.parse import parse_qs, urlparse


def parse_oauth_callback(
    raw: str,
    *,
    expected_state: str | None = None,
) -> tuple[str, str]:
    """Return ``(code, state)`` from a callback URL, query string, or bare code.

    Accepts:
    - full URL: ``http://127.0.0.1:56121/callback?code=...&state=...``
    - query fragment: ``?code=...&state=...`` or ``code=...&state=...``
    - bare authorization code (requires *expected_state*)
    """
    text = (raw or "").strip()
    if not text:
        raise ValueError("回调内容为空")

    if "://" in text:
        parsed = urlparse(text)
        qs = parse_qs(parsed.query, keep_blank_values=False)
    elif text.startswith("?") or "code=" in text:
        qs = parse_qs(text.lstrip("?"), keep_blank_values=False)
    else:
        if not expected_state:
            raise ValueError("仅粘贴授权码时需要有效的登录会话，请先点击「登录 xAI」")
        return text, expected_state

    codes = qs.get("code") or []
    states = qs.get("state") or []
    if not codes or not str(codes[0]).strip():
        raise ValueError("未找到 authorization code（code 参数）")
    code = str(codes[0]).strip()
    state = str(states[0]).strip() if states else (expected_state or "")
    if not state:
        raise ValueError("未找到 state 参数；请粘贴完整回调 URL")
    return code, state


__all__ = ["parse_oauth_callback"]