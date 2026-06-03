"""WebUI chat API routes."""

import time

from fastapi import APIRouter, Depends
from fastapi.responses import JSONResponse

from app.control.model import registry as model_registry
from app.platform.auth.middleware import verify_webui_key
from app.products.openai.router import chat_completions_endpoint
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
async def list_webui_models():
    created = int(time.time())
    models = [
        {
            "id": spec.model_name,
            "object": "model",
            "created": created,
            "owned_by": "xai",
            "name": spec.public_name,
            "capability": _capability_name(spec),
            **model_registry.describe(spec.model_name),
        }
        for spec in model_registry.list_enabled()
    ]
    seen = {item["id"] for item in models}
    from app.plugins.model_registry import service as model_registry_overlay

    for manual in model_registry_overlay.manual_models():
        model_id = manual["id"]
        if model_id in seen:
            continue
        models.append(
            {
                "id": model_id,
                "object": "model",
                "created": created,
                "owned_by": "manual",
                "name": manual.get("name") or model_id,
                "capability": "chat",
                "source": "manual",
                "manual": True,
                "mapped_to": manual.get("mapped_to") or None,
                "executable": False,
            }
        )
        seen.add(model_id)
    return JSONResponse({"object": "list", "data": models})


@router.post("/chat/completions")
async def webui_chat_completions(req: ChatCompletionRequest):
    return await chat_completions_endpoint(req)


__all__ = ["router"]
