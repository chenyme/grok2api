"""
Retry helpers for token switching.
"""

from typing import Optional, Set

from app.core.exceptions import UpstreamException
from app.services.grok.services.model import ModelService


async def pick_token(
    token_mgr,
    model_id: str,
    tried: Set[str],
    preferred: Optional[str] = None,
) -> Optional[str]:
    if preferred and preferred not in tried:
        return preferred

    token = None
    for pool_name in ModelService.pool_candidates_for_model(model_id):
        token = token_mgr.get_token(pool_name, exclude=tried)
        if token:
            break

    if not token and not tried:
        result = await token_mgr.refresh_cooling_tokens()
        if result.get("recovered", 0) > 0:
            for pool_name in ModelService.pool_candidates_for_model(model_id):
                token = token_mgr.get_token(pool_name)
                if token:
                    break

    return token


def rate_limited(error: Exception) -> bool:
    if not isinstance(error, UpstreamException):
        return False
    status = error.details.get("status") if error.details else None
    code = error.details.get("error_code") if error.details else None
    return status == 429 or code == "rate_limit_exceeded"


__all__ = ["pick_token", "rate_limited"]
