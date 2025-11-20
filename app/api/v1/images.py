"""图片服务API - 提供缓存的图片和视频文件"""

from fastapi import APIRouter, HTTPException
from fastapi.responses import FileResponse

from app.core.logger import logger
from app.services.grok.cache import image_cache_service, video_cache_service


router = APIRouter()


@router.get("/images/{img_path:path}")
async def get_image(img_path: str):
    """获取缓存的图片或视频
    
    Args:
        img_path: 文件路径（格式：users-xxx-generated-xxx-image.jpg）
    """
    try:
        # 转换路径（短横线→斜杠）
        original_path = "/" + img_path.replace('-', '/')

        # 判断类型
        is_video = any(original_path.lower().endswith(ext) for ext in ['.mp4', '.webm', '.mov', '.avi'])
        
        if is_video:
            cache_path = video_cache_service.get_cached(original_path)
            media_type = "video/mp4"
        else:
            cache_path = image_cache_service.get_cached(original_path)
            media_type = "image/jpeg"

        if cache_path and cache_path.exists():
            logger.debug(f"[MediaAPI] 返回缓存: {cache_path}")
            return FileResponse(
                path=str(cache_path),
                media_type=media_type,
                headers={
                    "Cache-Control": "public, max-age=86400",
                    "Access-Control-Allow-Origin": "*"
                }
            )

        # 文件不存在
        logger.warning(f"[MediaAPI] 未找到: {original_path}")
        raise HTTPException(status_code=404, detail="File not found")

    except HTTPException:
        raise
    except Exception as e:
        logger.error(f"[MediaAPI] 获取失败: {e}")
        raise HTTPException(status_code=500, detail=str(e))
