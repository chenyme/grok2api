"""HTTP/WebSocket header builders for reverse-proxy requests.

All values are sanitized to ASCII-safe Latin-1 before use.
"""

import base64
import random
import re
import string
import uuid
from typing import Optional
from urllib.parse import urlparse


from app.platform.logging.logger import logger
from app.platform.config.snapshot import get_config
from app.control.proxy.models import ProxyLease

# ---------------------------------------------------------------------------
# Unicode → ASCII normalisation map
# ---------------------------------------------------------------------------

_CHAR_MAP = str.maketrans(
    {
        "\u2010": "-",
        "\u2011": "-",
        "\u2012": "-",
        "\u2013": "-",
        "\u2014": "-",
        "\u2212": "-",
        "\u2018": "'",
        "\u2019": "'",
        "\u201c": '"',
        "\u201d": '"',
        "\u00a0": " ",
        "\u2007": " ",
        "\u202f": " ",
        "\u200b": "",
        "\u200c": "",
        "\u200d": "",
        "\ufeff": "",
    }
)


def _sanitize(value: Optional[str], *, field: str, strip_spaces: bool = False) -> str:
    raw = "" if value is None else str(value)
    out = raw.translate(_CHAR_MAP)
    out = re.sub(r"\s+", "", out) if strip_spaces else out.strip()
    out = out.encode("latin-1", errors="ignore").decode("latin-1")
    if out != raw:
        logger.debug(
            "header sanitized: field={} original_len={} sanitized_len={}",
            field,
            len(raw),
            len(out),
        )
    return out


# ---------------------------------------------------------------------------
# Statsig / request-id generation
# ---------------------------------------------------------------------------


def _statsig_id() -> str:
    cfg = get_config()
    if cfg.get_bool("features.dynamic_statsig", False):
        if random.choice((True, False)):
            rand = "".join(random.choices(string.ascii_lowercase + string.digits, k=5))
            msg = f"e:TypeError: Cannot read properties of null (reading 'children['{rand}']')"
        else:
            rand = "".join(random.choices(string.ascii_lowercase, k=10))
            msg = f"e:TypeError: Cannot read properties of undefined (reading '{rand}')"
        return base64.b64encode(msg.encode()).decode()
    return (
        "ZTpUeXBlRXJyb3I6IENhbm5vdCByZWFkIHByb3BlcnRpZXMgb2YgdW5kZWZpbmVkIChyZWFkaW5nICdjaGls"
        "ZE5vZGVzJyk="
    )


# ---------------------------------------------------------------------------
# Client-hints helpers
# ---------------------------------------------------------------------------


def _major_version(browser: Optional[str], ua: Optional[str]) -> Optional[str]:
    for src in (browser or "", ua or ""):
        m = re.search(r"(\d{2,3})", src)
        if m:
            return m.group(1)
    return None


def _platform(ua: str) -> Optional[str]:
    u = ua.lower()
    if "windows" in u:
        return "Windows"
    if "mac os x" in u or "macintosh" in u:
        return "macOS"
    if "android" in u:
        return "Android"
    if "iphone" in u or "ipad" in u:
        return "iOS"
    if "linux" in u:
        return "Linux"
    return None


def _arch(ua: str) -> Optional[str]:
    u = ua.lower()
    if "aarch64" in u or "arm" in u:
        return "arm"
    if "x86_64" in u or "x64" in u or "win64" in u or "intel" in u:
        return "x86"
    return None


def _client_hints(browser: Optional[str], ua: Optional[str]) -> dict[str, str]:
    b = (browser or "").lower()
    u = (ua or "").lower()
    is_chromium = any(k in b for k in ("chrome", "chromium", "edge", "brave")) or any(
        k in u for k in ("chrome", "chromium", "edg")
    )
    if not is_chromium or "firefox" in u or ("safari" in u and "chrome" not in u):
        return {}
    ver = _major_version(browser, ua)
    if not ver:
        return {}
    if "edge" in b or "edg" in u:
        brand = "Microsoft Edge"
    elif "brave" in b:
        brand = "Brave"
    elif "chromium" in b:
        brand = "Chromium"
    else:
        brand = "Google Chrome"

    sec_ch_ua = f'"{brand}";v="{ver}", "Chromium";v="{ver}", "Not(A:Brand";v="24"'
    plat = _platform(ua or "")
    arch = _arch(ua or "")
    mobile = "?1" if ("mobile" in u or plat in ("Android", "iOS")) else "?0"

    hints: dict[str, str] = {
        "Sec-Ch-Ua": sec_ch_ua,
        "Sec-Ch-Ua-Mobile": mobile,
        "Sec-Ch-Ua-Model": "",
    }
    if plat:
        hints["Sec-Ch-Ua-Platform"] = f'"{plat}"'
    if arch:
        hints["Sec-Ch-Ua-Arch"] = arch
        hints["Sec-Ch-Ua-Bitness"] = "64"
    return hints


# ---------------------------------------------------------------------------
# Lease resolution
# ---------------------------------------------------------------------------


def _resolve_profile(lease: ProxyLease | None) -> tuple[str, str, str]:
    """Return (cf_cookies, user_agent, cf_clearance) from lease or config.

    Field-level fallback: if the lease exists but a field is empty (e.g. no
    clearance bundle yet), the config default is used rather than returning an
    empty string.  This prevents bare requests with no User-Agent when running
    in direct mode or when FlareSolverr has not yet delivered a bundle.

    Priority order (flat legacy first, nested fallback second):
      - ``proxy.cf_clearance`` / ``proxy.cf_cookies`` / ``proxy.user_agent``
        (flat v1.x-style keys, written by external cf-refresher sidecars)
      - ``proxy.clearance.cf_clearance`` / ``proxy.clearance.cf_cookies`` /
        ``proxy.clearance.user_agent`` (nested v2.x schema, populated by
        ``config.defaults.toml`` and the internal clearance manager)

    Rationale: when an external sidecar refreshes cf_clearance and writes to
    the flat keys, those values must *override* the stale defaults shipped in
    ``config.defaults.toml`` under ``[proxy.clearance]``. Previously the order
    was reversed, so a non-empty placeholder UA in defaults shadowed the fresh
    UA supplied by the refresher, causing a UA/cf_clearance mismatch and a 403
    from Cloudflare (the cookie is bound to the UA of the acquiring browser).
    """
    cfg = get_config()
    cfg_cookies = (
        cfg.get_str("proxy.cf_cookies", "")
        or cfg.get_str("proxy.clearance.cf_cookies", "")
    )
    cfg_ua = (
        cfg.get_str("proxy.user_agent", "")
        or cfg.get_str("proxy.clearance.user_agent", "")
    )
    cfg_clearance = (
        cfg.get_str("proxy.cf_clearance", "")
        or cfg.get_str("proxy.clearance.cf_clearance", "")
    )
    if lease is not None:
        return (
            lease.cf_cookies or cfg_cookies,
            lease.user_agent or cfg_ua,
            cfg_clearance,
        )
    return (cfg_cookies, cfg_ua, cfg_clearance)


def _resolve_browser(lease: ProxyLease | None) -> str:
    if lease is not None and lease.user_agent:
        m = re.search(r"Chrome/(\d+)", lease.user_agent)
        return f"chrome{m.group(1)}" if m else "chrome120"
    return get_config().get_str("proxy.clearance.browser", "chrome120")


# ---------------------------------------------------------------------------
# Public builders
# ---------------------------------------------------------------------------


def build_sso_cookie(
    sso_token: str,
    *,
    lease: ProxyLease | None = None,
    cf_cookies: str | None = None,
    cf_clearance: str | None = None,
) -> str:
    """Build the Cookie header value for an SSO-authenticated request.

    When *cf_clearance* is not provided, the value is resolved from the lease's
    cf_cookies profile or falls back to the config's cf_clearance (supporting
    both ``proxy.clearance.cf_clearance`` and legacy ``proxy.cf_clearance`` paths).
    Historical bug: earlier v2.0 releases silently defaulted cf_clearance to the
    empty string when not passed explicitly, causing Cookies without a CF
    clearance token and immediate 403 from Cloudflare on every grok.com call.
    """
    tok = sso_token[4:] if sso_token.startswith("sso=") else sso_token
    tok = _sanitize(tok, field="sso_token", strip_spaces=True)

    cookie = f"sso={tok}; sso-rw={tok}"
    profile_cookies, _, profile_clearance = _resolve_profile(lease)
    eff_cookies = _sanitize(
        cf_cookies if cf_cookies is not None else profile_cookies, field="cf_cookies"
    )
    eff_clearance = _sanitize(
        cf_clearance if cf_clearance is not None else profile_clearance,
        field="cf_clearance",
        strip_spaces=True,
    )

    if eff_clearance and eff_cookies:
        if re.search(r"(?:^|;\s*)cf_clearance=", eff_cookies):
            eff_cookies = re.sub(
                r"(^|;\s*)cf_clearance=[^;]*",
                r"\1cf_clearance=" + eff_clearance,
                eff_cookies,
                count=1,
            )
        else:
            eff_cookies = f"{eff_cookies.rstrip('; ')}; cf_clearance={eff_clearance}"
    elif eff_clearance:
        eff_cookies = f"cf_clearance={eff_clearance}"

    if eff_cookies:
        cookie += f"; {eff_cookies}"
    return cookie


def build_http_headers(
    cookie_token: str,
    *,
    content_type: Optional[str] = None,
    origin: Optional[str] = None,
    referer: Optional[str] = None,
    lease: ProxyLease | None = None,
) -> dict[str, str]:
    """Build headers for a standard HTTP reverse-proxy request."""
    _, raw_ua, _ = _resolve_profile(lease)
    ua = _sanitize(raw_ua, field="user_agent")
    browser = _resolve_browser(lease)
    org = _sanitize(origin or "https://grok.com", field="origin")
    ref = _sanitize(referer or "https://grok.com/", field="referer")

    ct = content_type or "application/json"
    if ct == "application/json":
        accept = "*/*"
        fd = "empty"
    elif ct in ("image/jpeg", "image/png", "video/mp4", "video/webm"):
        accept = (
            "text/html,application/xhtml+xml,application/xml;q=0.9,"
            "image/avif,image/webp,image/apng,*/*;q=0.8"
        )
        fd = "document"
    else:
        accept = "*/*"
        fd = "empty"

    org_host = urlparse(org).hostname
    ref_host = urlparse(ref).hostname
    site = "same-origin" if org_host and org_host == ref_host else "same-site"

    headers: dict[str, str] = {
        "Accept": accept,
        "Accept-Encoding": "gzip, deflate, br, zstd",
        "Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
        "Baggage": (
            "sentry-environment=production,"
            "sentry-release=d6add6fb0460641fd482d767a335ef72b9b6abb8,"
            "sentry-public_key=b311e0f2690c81f25e2c4cf6d4f7ce1c"
        ),
        "Content-Type": ct,
        "Origin": org,
        "Priority": "u=1, i",
        "Referer": ref,
        "Sec-Fetch-Dest": fd,
        "Sec-Fetch-Mode": "cors",
        "Sec-Fetch-Site": site,
        "User-Agent": ua,
        "x-statsig-id": _statsig_id(),
        "x-xai-request-id": str(uuid.uuid4()),
    }
    headers.update(_client_hints(browser, raw_ua))
    headers["Cookie"] = build_sso_cookie(cookie_token, lease=lease)

    logger.debug("http headers built: header_count={}", len(headers))
    return headers


def build_ws_headers(
    token: Optional[str] = None,
    *,
    origin: Optional[str] = None,
    extra: Optional[dict[str, str]] = None,
    lease: ProxyLease | None = None,
) -> dict[str, str]:
    """Build headers for a WebSocket upgrade request."""
    _, raw_ua, _ = _resolve_profile(lease)
    ua = _sanitize(raw_ua, field="user_agent")
    browser = _resolve_browser(lease)
    org = _sanitize(origin or "https://grok.com", field="origin")

    headers: dict[str, str] = {
        "Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
        "Cache-Control": "no-cache",
        "Origin": org,
        "Pragma": "no-cache",
        "User-Agent": ua,
    }
    headers.update(_client_hints(browser, raw_ua))
    if token:
        headers["Cookie"] = build_sso_cookie(token, lease=lease)
    if extra:
        headers.update(extra)
    return headers


__all__ = ["build_http_headers", "build_sso_cookie", "build_ws_headers"]
