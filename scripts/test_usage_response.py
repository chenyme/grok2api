"""
Simple usage probe to inspect /rest/rate-limits response.

Usage:
  python scripts/test_usage_response.py
  (optional) TOKEN_POOL=ssoBasic|ssoSuper
"""

import asyncio
import os
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from curl_cffi.requests import AsyncSession

from app.core.config import config
from app.services.reverse.rate_limits import RateLimitsReverse
from app.services.token import get_token_manager


async def main() -> int:
    await config.load()
    token = None
    pool = os.getenv("TOKEN_POOL")
    manager = await get_token_manager()
    await manager.reload_if_stale()

    if pool:
        token = manager.get_token(pool_name=pool)
    else:
        token = manager.get_token(pool_name="ssoBasic") or manager.get_token(
            pool_name="ssoSuper"
        )

    if not token:
        token = os.getenv("GROK_TOKEN") or os.getenv("SSO_TOKEN") or os.getenv("TOKEN")
    if not token:
        print("Missing token. Ensure token pool is configured or set GROK_TOKEN.")
        return 2

    async with AsyncSession() as session:
        response = await RateLimitsReverse.request(session, token)

    try:
        data = response.json()
    except Exception as exc:
        print(f"Failed to parse JSON: {exc}")
        raw = getattr(response, "text", "")
        if raw:
            print(raw)
        return 3

    print(data)
    return 0


if __name__ == "__main__":
    raise SystemExit(asyncio.run(main()))
