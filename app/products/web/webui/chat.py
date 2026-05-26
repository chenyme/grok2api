"""WebUI chat API routes."""

import time

from fastapi import APIRouter, Depends
from fastapi.responses import JSONResponse

from app.control.model import registry as model_registry
from app.platform.auth.middleware import verify_webui_key
from app.products.openai.router import chat_completions_endpoint
from app.products.openai.images import generate as generate_image
from app.products.openai.schemas import ChatCompletionRequest, ImageGenerationRequest

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
    models = [
        {
            "id": spec.model_name,
            "object": "model",
            "created": int(time.time()),
            "owned_by": "xai",
            "name": spec.public_name,
            "capability": _capability_name(spec),
        }
        for spec in model_registry.list_enabled()
    ]
    return JSONResponse({"object": "list", "data": models})


@router.post("/chat/completions")
async def webui_chat_completions(req: ChatCompletionRequest):
    return await chat_completions_endpoint(req)


@router.post("/images/generations")
async def webui_image_generations(req: ImageGenerationRequest):
    result = await generate_image(
        model=req.model,
        prompt=req.prompt,
        n=req.n or 1,
        size=req.size or "1024x1024",
        aspect_ratio=req.aspect_ratio,
        response_format=req.response_format or "url",
        stream=False,
        chat_format=False,
    )
    return JSONResponse(result)


__all__ = ["router"]
