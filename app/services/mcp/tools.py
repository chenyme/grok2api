# -*- coding: utf-8 -*-
"""MCP Tools - Grok AI"""

import json
from typing import Optional
from app.services.grok.client import GrokClient
from app.core.logger import logger


async def ask_grok_impl(query: str, model: str = "grok-4", system_prompt: Optional[str] = None) -> str:
    try:
        messages = []
        if system_prompt:
            messages.append({"role": "system", "content": system_prompt})
        messages.append({"role": "user", "content": query})
        logger.info(f"[MCP] ask_grok, model={model}")
        request_data = {"model": model, "messages": messages, "stream": False}
        result = await GrokClient.openai_to_grok(request_data)
        if isinstance(result, dict):
            choices = result.get("choices", [])
            if choices:
                return choices[0].get("message", {}).get("content", "")
        return str(result)
    except Exception as e:
        logger.error(f"[MCP] ask_grok error: {e}")
        raise Exception(f"处理请求失败: {e}")


async def generate_image_impl(prompt: str, n: Optional[int] = None, size: Optional[str] = None, 
                             model: str = "grok-imagine-1.0") -> str:
    try:
        logger.info(f"[MCP] generate_image, model={model}, n={n}, size={size}")
        request_data = {"model": model, "messages": [{"role": "user", "content": prompt}], "stream": False}
        if n: request_data["n"] = n
        if size: request_data["size"] = size
        result = await GrokClient.openai_to_grok(request_data)
        if isinstance(result, dict):
            choices = result.get("choices", [])
            if choices:
                return choices[0].get("message", {}).get("content", "")
        return str(result)
    except Exception as e:
        logger.error(f"[MCP] generate_image error: {e}")
        raise Exception(f"图片生成失败: {e}")


async def edit_image_impl(prompt: str, image_source: str, n: Optional[int] = None, 
                          size: Optional[str] = None, model: str = "grok-imagine-1.0-edit") -> str:
    try:
        logger.info(f"[MCP] edit_image, model={model}, n={n}, size={size}")
        messages = [{"role": "user", "content": [
            {"type": "text", "text": prompt},
            {"type": "image_url", "image_url": {"url": image_source}},
        ]}]
        request_data = {"model": model, "messages": messages, "stream": False}
        if n: request_data["n"] = n
        if size: request_data["size"] = size
        result = await GrokClient.openai_to_grok(request_data)
        if isinstance(result, dict):
            choices = result.get("choices", [])
            if choices:
                return choices[0].get("message", {}).get("content", "")
        return str(result)
    except Exception as e:
        logger.error(f"[MCP] edit_image error: {e}")
        raise Exception(f"图片编辑失败: {e}")


async def generate_video_impl(prompt: str, image_url: str, aspect_ratio: Optional[str] = None,
                              video_length: Optional[int] = None, resolution: Optional[str] = None,
                              preset: Optional[str] = None, model: str = "grok-imagine-1.0-video") -> str:
    try:
        logger.info(f"[MCP] generate_video, model={model}, ratio={aspect_ratio}, length={video_length}, res={resolution}, preset={preset}")
        messages = [{"role": "user", "content": [
            {"type": "text", "text": prompt},
            {"type": "image_url", "image_url": {"url": image_url}},
        ]}]
        video_config = {}
        if aspect_ratio: video_config["aspect_ratio"] = aspect_ratio
        if video_length: video_config["video_length"] = video_length
        if resolution: video_config["resolution_name"] = resolution
        if preset: video_config["preset"] = preset
        request_data = {"model": model, "messages": messages, "stream": False}
        if video_config:
            request_data["video_config"] = video_config
        result = await GrokClient.openai_to_grok(request_data)
        if isinstance(result, dict):
            choices = result.get("choices", [])
            if choices:
                return choices[0].get("message", {}).get("content", "")
        return str(result)
    except Exception as e:
        logger.error(f"[MCP] generate_video error: {e}")
        raise Exception(f"视频生成失败: {e}")


async def list_models_impl() -> str:
    try:
        from app.models.grok_models import Models
        model_names = Models.get_all_model_names()
        result = []
        for name in model_names:
            info = Models.get_model_info(name)
            result.append({
                "id": name, 
                "name": info.get("display_name", name), 
                "description": info.get("description", "")
            })
        return json.dumps(result, ensure_ascii=False, indent=2)
    except Exception as e:
        logger.error(f"[MCP] list_models error: {e}")
        raise Exception(f"获取模型列表失败: {e}")
