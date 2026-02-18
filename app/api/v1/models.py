"""
Models API 路由
"""

from fastapi import APIRouter

from app.services.grok.services.model import ModelService


router = APIRouter(tags=["Models"])


@router.get("/models")
async def list_models():
    """OpenAI 兼容 models 列表接口"""
    data = [
        {
            "id": m.model_id,
            "object": "model",
            "created": 0,
            "owned_by": "grok2api@chenyme",
        }
        for m in ModelService.list()
    ]
    return {"object": "list", "data": data}


__all__ = ["router"]
