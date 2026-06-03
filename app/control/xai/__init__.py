"""xAI official-API OAuth provider.

This package implements the "grok-cli / Grok Build" OAuth2 + PKCE flow against
``auth.x.ai`` and the credential lifecycle for subscription models
(``grok-build-0.1`` via ``/chat/completions``,
``grok-composer-2.5-fast`` via ``/responses`` on api.x.ai).

It is fully separate from the grok.com web reverse-proxy path: xAI accounts are
stored with ``pool="xai"`` and excluded from grok selection / quota machinery
(see ``app.dataplane.shared.enums.GROK_POOLS``).
"""
