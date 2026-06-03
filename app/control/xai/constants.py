"""xAI OAuth constants (grok-cli / Grok Build public client)."""

OAUTH_CALLBACK_HOST = "127.0.0.1"
OAUTH_CALLBACK_PORT = 56121
OAUTH_CALLBACK_PATH = "/callback"
OAUTH_REDIRECT_URI = (
    f"http://{OAUTH_CALLBACK_HOST}:{OAUTH_CALLBACK_PORT}{OAUTH_CALLBACK_PATH}"
)

# xAI's browser redirect may CORS-preflight the loopback URI; echo these origins.
OAUTH_CORS_ORIGIN_ALLOWLIST = frozenset(
    {"https://auth.x.ai", "https://accounts.x.ai"}
)

OAUTH_WAIT_TIMEOUT_S = 10 * 60

# Grok-cli OAuth subscription models (not the full public api.x.ai catalog).
GROK_BUILD_MODEL_IDS = frozenset({"grok-build-0.1"})
GROK_COMPOSER_MODEL_IDS = frozenset({"grok-composer-2.5-fast"})
XAI_OAUTH_MODEL_IDS = GROK_BUILD_MODEL_IDS | GROK_COMPOSER_MODEL_IDS

DEFAULT_API_BASE = "https://api.x.ai/v1"


def resolve_chat_base_url(model: str, *, api_base: str) -> str:
    """Return the upstream base URL for an OAuth-entitled chat model."""
    _ = model  # all OAuth models use api.x.ai (see CLIProxyAPI xai executor)
    return api_base.rstrip("/")


__all__ = [
    "OAUTH_CALLBACK_HOST",
    "OAUTH_CALLBACK_PORT",
    "OAUTH_CALLBACK_PATH",
    "OAUTH_REDIRECT_URI",
    "OAUTH_CORS_ORIGIN_ALLOWLIST",
    "OAUTH_WAIT_TIMEOUT_S",
    "GROK_BUILD_MODEL_IDS",
    "GROK_COMPOSER_MODEL_IDS",
    "XAI_OAUTH_MODEL_IDS",
    "DEFAULT_API_BASE",
    "resolve_chat_base_url",
]