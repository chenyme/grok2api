"""Function API router (function_key protected)."""

from fastapi import APIRouter, Depends

from app.api.v1.chat import router as chat_router
from app.api.v1.image import router as image_router
from app.api.v1.video import router as openai_video_router
from app.api.v1.function.imagine import router as imagine_router
from app.api.v1.function.video import router as video_router
from app.api.v1.function.voice import router as voice_router
from app.core.auth import verify_function_key

router = APIRouter()

router.include_router(chat_router, dependencies=[Depends(verify_function_key)])
router.include_router(image_router, dependencies=[Depends(verify_function_key)])
router.include_router(openai_video_router, dependencies=[Depends(verify_function_key)])
router.include_router(imagine_router)
router.include_router(video_router)
router.include_router(voice_router)

__all__ = ["router"]
