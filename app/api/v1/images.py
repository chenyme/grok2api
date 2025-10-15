"""图片服务API路由"""

from fastapi import APIRouter, HTTPException
from fastapi.responses import FileResponse

from app.core.logger import logger
from app.services.grok.cache import image_cache_service, video_cache_service


router = APIRouter()


@router.get("/images/{img_path:path}")
async def get_image(img_path: str):
    """获取缓存的图片或视频

    Args:
        img_path: 文件路径，格式如 users-xxx-generated-xxx-image.jpg 或 users-xxx-generated-xxx-video.mp4

    Returns:
        文件响应
    """
    try:
        # 将路径转换回原始格式（短横线转斜杠）
        original_path = "/" + img_path.replace('-', '/')

        # 判断是图片还是视频
        is_video = any(original_path.lower().endswith(ext) for ext in ['.mp4', '.webm', '.mov', '.avi'])
        
        if is_video:
            # 检查视频缓存
            cache_path = video_cache_service.get_cached(original_path)
            media_type = "video/mp4"
        else:
            # 检查图片缓存
            cache_path = image_cache_service.get_cached(original_path)
            media_type = "image/jpeg"

        if cache_path and cache_path.exists():
            logger.debug(f"[MediaAPI] 返回缓存文件: {cache_path}")
            return FileResponse(
                path=str(cache_path),
                media_type=media_type,
                headers={
                    "Cache-Control": "public, max-age=86400",
                    "Access-Control-Allow-Origin": "*"
                }
            )

        # 文件不存在
        logger.warning(f"[MediaAPI] 文件未找到: {original_path}")
        raise HTTPException(status_code=404, detail="File not found")

    except HTTPException:
        raise
    except Exception as e:
        logger.error(f"[MediaAPI] 获取文件失败: {e}")
        raise HTTPException(status_code=500, detail=str(e))
