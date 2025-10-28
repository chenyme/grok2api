"""视频生成API端点"""

from fastapi import APIRouter, HTTPException, UploadFile, File, Form
from fastapi.responses import JSONResponse
from typing import Optional
import tempfile
import os

from app.core.logger import logger
from app.services.grok.video import video_generator

router = APIRouter(prefix="/api/v1/video", tags=["video"])


@router.post("/generate")
async def generate_video_from_image(
    image: UploadFile = File(..., description="图片文件"),
    mode: str = Form(default="normal", description="视频生成模式"),
    model_name: str = Form(default="grok-3", description="模型名称"),
    file_name: Optional[str] = Form(default=None, description="自定义文件名")
):
    """
    从图片生成视频
    
    Args:
        image: 上传的图片文件
        mode: 视频生成模式 (normal, custom等)
        model_name: 使用的模型名称
        file_name: 自定义文件名
    
    Returns:
        JSON响应包含视频生成结果
    """
    try:
        # 验证文件类型
        if not image.content_type or not image.content_type.startswith('image/'):
            raise HTTPException(status_code=400, detail="只支持图片文件")
        
        # 创建临时文件
        with tempfile.NamedTemporaryFile(delete=False, suffix=f".{image.filename.split('.')[-1] if '.' in image.filename else 'jpg'}") as temp_file:
            content = await image.read()
            temp_file.write(content)
            temp_file_path = temp_file.name
        
        try:
            # 调用视频生成服务
            result = await video_generator.generate_video_from_image(
                image_path=temp_file_path,
                file_name=file_name or image.filename,
                mode=mode,
                model_name=model_name
            )
            
            if result.get("success"):
                logger.info(f"[Video API] 视频生成成功: {result.get('conversation_result', {}).get('data', {}).get('final_video_url', 'N/A')}")
                return JSONResponse(content={
                    "success": True,
                    "message": "视频生成成功",
                    "data": {
                        "video_url": result.get('conversation_result', {}).get('data', {}).get('final_video_url'),
                        "video_id": result.get('conversation_result', {}).get('data', {}).get('video_id'),
                        "progress": result.get('conversation_result', {}).get('data', {}).get('progress'),
                        "upload_info": result.get('upload_result', {}).get('data'),
                        "post_info": result.get('post_result', {}).get('data')
                    }
                })
            else:
                logger.error(f"[Video API] 视频生成失败: {result.get('error', '未知错误')}")
                return JSONResponse(
                    status_code=500,
                    content={
                        "success": False,
                        "message": "视频生成失败",
                        "error": result.get('error', '未知错误'),
                        "details": result
                    }
                )
                
        finally:
            # 清理临时文件
            if os.path.exists(temp_file_path):
                os.unlink(temp_file_path)
                
    except Exception as e:
        logger.error(f"[Video API] 视频生成异常: {str(e)}")
        raise HTTPException(status_code=500, detail=f"视频生成异常: {str(e)}")


@router.post("/generate-from-path")
async def generate_video_from_path(
    image_path: str = Form(..., description="图片文件路径"),
    mode: str = Form(default="normal", description="视频生成模式"),
    model_name: str = Form(default="grok-3", description="模型名称"),
    file_name: Optional[str] = Form(default=None, description="自定义文件名")
):
    """
    从本地图片路径生成视频
    
    Args:
        image_path: 本地图片文件路径
        mode: 视频生成模式
        model_name: 使用的模型名称
        file_name: 自定义文件名
    
    Returns:
        JSON响应包含视频生成结果
    """
    try:
        # 验证文件是否存在
        if not os.path.exists(image_path):
            raise HTTPException(status_code=400, detail="图片文件不存在")
        
        # 调用视频生成服务
        result = await video_generator.generate_video_from_image(
            image_path=image_path,
            file_name=file_name,
            mode=mode,
            model_name=model_name
        )
        
        if result.get("success"):
            logger.info(f"[Video API] 视频生成成功: {result.get('conversation_result', {}).get('data', {}).get('final_video_url', 'N/A')}")
            return JSONResponse(content={
                "success": True,
                "message": "视频生成成功",
                "data": {
                    "video_url": result.get('conversation_result', {}).get('data', {}).get('final_video_url'),
                    "video_id": result.get('conversation_result', {}).get('data', {}).get('video_id'),
                    "progress": result.get('conversation_result', {}).get('data', {}).get('progress'),
                    "upload_info": result.get('upload_result', {}).get('data'),
                    "post_info": result.get('post_result', {}).get('data')
                }
            })
        else:
            logger.error(f"[Video API] 视频生成失败: {result.get('error', '未知错误')}")
            return JSONResponse(
                status_code=500,
                content={
                    "success": False,
                    "message": "视频生成失败",
                    "error": result.get('error', '未知错误'),
                    "details": result
                }
            )
            
    except Exception as e:
        logger.error(f"[Video API] 视频生成异常: {str(e)}")
        raise HTTPException(status_code=500, detail=f"视频生成异常: {str(e)}")


@router.get("/status/{video_id}")
async def get_video_status(video_id: str):
    """
    获取视频生成状态
    
    Args:
        video_id: 视频ID
    
    Returns:
        JSON响应包含视频状态信息
    """
    try:
        # 这里可以实现视频状态查询逻辑
        # 目前返回基本信息
        return JSONResponse(content={
            "success": True,
            "message": "状态查询功能待实现",
            "data": {
                "video_id": video_id,
                "status": "unknown"
            }
        })
        
    except Exception as e:
        logger.error(f"[Video API] 状态查询异常: {str(e)}")
        raise HTTPException(status_code=500, detail=f"状态查询异常: {str(e)}")
