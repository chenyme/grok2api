"""WebUI chat API routes."""

import time

from fastapi import APIRouter, Depends, Request
from fastapi.responses import JSONResponse

from app.control.model import registry as model_registry
from app.platform.auth.middleware import verify_webui_key
from app.products.openai.router import (
    _available_pools,
    _model_available_for_pools,
    chat_completions_endpoint,
)
from app.products.openai.schemas import ChatCompletionRequest

router = APIRouter(prefix="/webui/api", dependencies=[Depends(verify_webui_key)], tags=["WebUI - Chat"])


def _capability_name(spec) -> str:
    if spec.is_image_edit():
        return "image_edit"
    if spec.is_image():
        return "image"
    if spec.is_video():
        return "video"
    return "chat"


@router.get("/models")
async def list_webui_models(request: Request):
    # Filter by account tier availability so the WebUI dropdown only shows
    # models the configured account pool can actually serve. Without this
    # the user would see super/heavy-tier models that fail with
    # "No available accounts for this model tier" on call.
    pools = await _available_pools(request)
    models = [
        {
            "id": spec.model_name,
            "object": "model",
            "created": int(time.time()),
            "owned_by": "xai",
            "name": spec.public_name,
            "capability": _capability_name(spec),
        } for spec in model_registry.list_enabled() if _model_available_for_pools(spec, pools)
    ]
    return JSONResponse({"object": "list", "data": models})


@router.post("/chat/completions")
async def webui_chat_completions(req: ChatCompletionRequest):
    return await chat_completions_endpoint(req)


__all__ = ["router"]
