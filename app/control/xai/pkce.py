"""PKCE (RFC 7636) code generation for the xAI OAuth flow.

Mirrors CLIProxyAPI's ``internal/auth/xai/pkce.go``:
  - verifier  = base64url(96 random bytes)
  - challenge = base64url(SHA256(verifier))
"""

import base64
import hashlib
import os


def _b64url(raw: bytes) -> str:
    """Base64-URL encode without padding (per RFC 7636)."""
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode("ascii")


def generate_pkce() -> tuple[str, str]:
    """Return a ``(code_verifier, code_challenge)`` pair using the S256 method."""
    verifier = _b64url(os.urandom(96))
    digest = hashlib.sha256(verifier.encode("ascii")).digest()
    challenge = _b64url(digest)
    return verifier, challenge


__all__ = ["generate_pkce"]
