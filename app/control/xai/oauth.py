"""xAI OAuth2 + PKCE core logic.

Ports CLIProxyAPI's ``internal/auth/xai`` (discovery, authorize URL, token
exchange, refresh, id_token parsing).  All outbound HTTP goes through
``_http`` (curl_cffi, proxy-aware).  No FastAPI dependencies here.
"""

import base64
import binascii
from urllib.parse import urlencode, urlparse

import orjson

from app.platform.config.snapshot import get_config
from app.platform.errors import UpstreamError
from app.control.proxy.models import ProxyLease
from . import _http
from .constants import DEFAULT_API_BASE, OAUTH_REDIRECT_URI

# ---------------------------------------------------------------------------
# Constants (the public grok-cli OAuth client)
# ---------------------------------------------------------------------------

DEFAULT_CLIENT_ID = "b1a00492-073a-47ea-816f-4c329264a828"
SCOPE = "openid profile email offline_access grok-cli:access api:access"
ISSUER = "https://auth.x.ai"
DISCOVERY_URL = "https://auth.x.ai/.well-known/openid-configuration"
DEFAULT_TOKEN_ENDPOINT = "https://auth.x.ai/oauth/token"

# Refresh tokens this many milliseconds before the recorded expiry.
REFRESH_LEAD_MS = 5 * 60 * 1000


def client_id() -> str:
    """Return the configured OAuth client id (falls back to the public id)."""
    return get_config().get_str("xai.client_id", "") or DEFAULT_CLIENT_ID


# ---------------------------------------------------------------------------
# URL validation
# ---------------------------------------------------------------------------


def _validate_xai_url(url: str, *, field: str) -> str:
    """Ensure *url* is HTTPS and on the x.ai domain; return it unchanged."""
    parsed = urlparse(url)
    if parsed.scheme != "https":
        raise UpstreamError(f"xAI {field} must be HTTPS: {url!r}", status=502)
    host = (parsed.hostname or "").lower()
    if host != "x.ai" and not host.endswith(".x.ai"):
        raise UpstreamError(
            f"xAI {field} must be on the x.ai domain: {url!r}", status=502
        )
    return url


# ---------------------------------------------------------------------------
# Discovery
# ---------------------------------------------------------------------------


async def discover(*, lease: ProxyLease | None = None) -> tuple[str, str]:
    """Return ``(authorization_endpoint, token_endpoint)`` from discovery."""
    doc = await _http.get_json(DISCOVERY_URL, lease=lease)
    auth_ep = doc.get("authorization_endpoint")
    token_ep = doc.get("token_endpoint")
    if not auth_ep or not token_ep:
        raise UpstreamError("xAI discovery missing endpoints", status=502)
    return (
        _validate_xai_url(auth_ep, field="authorization_endpoint"),
        _validate_xai_url(token_ep, field="token_endpoint"),
    )


# ---------------------------------------------------------------------------
# Authorize URL
# ---------------------------------------------------------------------------


def build_authorize_url(
    authorization_endpoint: str,
    *,
    redirect_uri: str,
    code_challenge: str,
    state: str,
    nonce: str,
) -> str:
    """Build the browser authorize URL with all required OAuth params."""
    params = {
        "response_type": "code",
        "client_id": client_id(),
        "redirect_uri": redirect_uri,
        "scope": SCOPE,
        "code_challenge": code_challenge,
        "code_challenge_method": "S256",
        "state": state,
        "nonce": nonce,
        "plan": "generic",
        "referrer": "grok2api",
    }
    sep = "&" if "?" in authorization_endpoint else "?"
    return f"{authorization_endpoint}{sep}{urlencode(params)}"


# ---------------------------------------------------------------------------
# Token exchange / refresh
# ---------------------------------------------------------------------------


async def exchange_code(
    token_endpoint: str,
    *,
    code: str,
    redirect_uri: str,
    code_verifier: str,
    code_challenge: str = "",
    lease: ProxyLease | None = None,
) -> dict:
    """Exchange an authorization code for tokens (authorization_code grant)."""
    form = {
        "grant_type": "authorization_code",
        "code": code,
        "redirect_uri": redirect_uri,
        "client_id": client_id(),
        "code_verifier": code_verifier,
    }
    # xAI re-validates PKCE challenge fields for the grok-cli public client.
    if code_challenge:
        form["code_challenge"] = code_challenge
        form["code_challenge_method"] = "S256"
    return await _http.post_form_json(token_endpoint, form, lease=lease)


async def refresh_tokens(
    token_endpoint: str,
    refresh_token: str,
    *,
    lease: ProxyLease | None = None,
) -> dict:
    """Exchange a refresh token for a new access token (refresh_token grant)."""
    form = {
        "grant_type": "refresh_token",
        "client_id": client_id(),
        "refresh_token": refresh_token,
    }
    return await _http.post_form_json(token_endpoint, form, lease=lease)


# ---------------------------------------------------------------------------
# id_token parsing
# ---------------------------------------------------------------------------


def parse_id_token(id_token: str) -> tuple[str | None, str | None]:
    """Return ``(email, sub)`` extracted from a JWT id_token payload.

    Does NOT verify the signature (the token came directly from the trusted
    token endpoint over TLS); only decodes the middle (payload) segment.
    """
    if not id_token:
        return None, None
    parts = id_token.split(".")
    if len(parts) < 2:
        return None, None
    payload_seg = parts[1]
    # Restore base64url padding.
    padding = "=" * (-len(payload_seg) % 4)
    try:
        raw = base64.urlsafe_b64decode(payload_seg + padding)
        claims = orjson.loads(raw)
    except (binascii.Error, ValueError, orjson.JSONDecodeError):
        return None, None
    email = claims.get("email")
    sub = claims.get("sub")
    return (email if isinstance(email, str) else None,
            sub if isinstance(sub, str) else None)


__all__ = [
    "DEFAULT_CLIENT_ID",
    "DEFAULT_API_BASE",
    "DEFAULT_TOKEN_ENDPOINT",
    "REFRESH_LEAD_MS",
    "SCOPE",
    "ISSUER",
    "DISCOVERY_URL",
    "OAUTH_REDIRECT_URI",
    "client_id",
    "discover",
    "build_authorize_url",
    "exchange_code",
    "refresh_tokens",
    "parse_id_token",
]
