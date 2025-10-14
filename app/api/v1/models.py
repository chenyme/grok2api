"""
模型接口模块

提供 OpenAI 兼容的 /v1/models 端点，返回系统支持的所有模型列表。
"""

import time
from typing import Dict, Any, List, Optional
from fastapi import APIRouter, HTTPException, Depends

from app.models.grok_models import Models
from app.core.auth import auth_manager
from app.core.logger import logger

# 配置日志

# 创建路由器
router = APIRouter(tags=["模型"])


@router.get("/models")
async def list_models(_: Optional[str] = Depends(auth_manager.verify)) -> Dict[str, Any]:
    """
    获取可用模型列表

    返回 OpenAI 兼容的模型列表格式，包含系统支持的所有 Grok 模型的详细信息。

    Args:
        _: 认证依赖（自动验证）

    Returns:
        Dict[str, Any]: 包含模型列表的响应数据
    """
    try:
        logger.debug("[Models] 请求获取模型列表")

        # 获取当前时间戳
        current_timestamp = int(time.time())
        
        # 构建模型数据列表
        model_data: List[Dict[str, Any]] = []
        
        for model in Models:
            model_id = model.value
            config = Models.get_model_info(model_id)
            
            # 基础信息
            model_info = {
                "id": model_id,
                "object": "model", 
                "created": current_timestamp,
                "owned_by": "x-ai",
                "display_name": config.get("display_name", model_id),
                "description": config.get("description", ""),
                "raw_model_path": config.get("raw_model_path", f"xai/{model_id}"),
                "default_temperature": config.get("default_temperature", 1.0),
                "default_max_output_tokens": config.get("default_max_output_tokens", 8192),
                "supported_max_output_tokens": config.get("supported_max_output_tokens", 131072),
                "default_top_p": config.get("default_top_p", 0.95)
            }
            
            model_data.append(model_info)
        
        # 构建响应
        response = {
            "object": "list",
            "data": model_data
        }

        logger.debug(f"[Models] 成功返回 {len(model_data)} 个模型")
        return response
        
    except Exception as e:
        logger.error(f"[Models] 获取模型列表时发生错误: {str(e)}")
        raise HTTPException(
            status_code=500,
            detail={
                "error": {
                    "message": f"Failed to retrieve models: {str(e)}",
                    "type": "internal_error",
                    "code": "model_list_error"
                }
            }
        )


@router.get("/models/{model_id}")
async def get_model(model_id: str, _: Optional[str] = Depends(auth_manager.verify)) -> Dict[str, Any]:
    """
    获取特定模型信息

    Args:
        model_id (str): 模型ID
        _: 认证依赖（自动验证）

    Returns:
        Dict[str, Any]: 模型详细信息
    """
    try:
        logger.debug(f"[Models] 请求获取模型信息: {model_id}")

        # 验证模型是否存在
        if not Models.is_valid_model(model_id):
            logger.warning(f"[Models] 请求的模型不存在: {model_id}")
            raise HTTPException(
                status_code=404,
                detail={
                    "error": {
                        "message": f"Model '{model_id}' not found",
                        "type": "invalid_request_error", 
                        "code": "model_not_found"
                    }
                }
            )
        
        # 获取当前时间戳
        current_timestamp = int(time.time())
        
        # 获取模型配置
        config = Models.get_model_info(model_id)
        
        # 构建模型信息
        model_info = {
            "id": model_id,
            "object": "model",
            "created": current_timestamp,
            "owned_by": "x-ai",
            "display_name": config.get("display_name", model_id),
            "description": config.get("description", ""),
            "raw_model_path": config.get("raw_model_path", f"xai/{model_id}"),
            "default_temperature": config.get("default_temperature", 1.0),
            "default_max_output_tokens": config.get("default_max_output_tokens", 8192),
            "supported_max_output_tokens": config.get("supported_max_output_tokens", 131072),
            "default_top_p": config.get("default_top_p", 0.95)
        }

        logger.debug(f"[Models] 成功返回模型信息: {model_id}")
        return model_info
        
    except HTTPException:
        raise
    except Exception as e:
        logger.error(f"[Models] 获取模型信息时发生错误: {str(e)}")
        raise HTTPException(
            status_code=500,
            detail={
                "error": {
                    "message": f"Failed to retrieve model: {str(e)}",
                    "type": "internal_error",
                    "code": "model_retrieve_error"
                }
            }
        )
