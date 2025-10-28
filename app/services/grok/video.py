"""Grok 视频生成服务模块"""

import json
import asyncio
import base64
from typing import Dict, Any, Optional, List
from pathlib import Path

from curl_cffi.requests import AsyncSession

from app.core.config import setting
from app.core.logger import logger
from app.core.exception import GrokApiException
from app.services.grok.statsig import get_dynamic_headers
from app.services.grok.token import token_manager
from app.services.grok.upload import ImageUploadManager

# 常量定义
CREATE_POST_ENDPOINT = "https://grok.com/rest/media/post/create"
CONVERSATION_ENDPOINT = "https://grok.com/rest/app-chat/conversations/new"
REQUEST_TIMEOUT = 60
IMPERSONATE_BROWSER = "chrome133a"


class GrokVideoGenerator:
    """Grok 视频生成器"""

    @staticmethod
    async def generate_video_from_image(
        image_path: str, 
        file_name: Optional[str] = None, 
        mode: str = "normal",
        model_name: str = "grok-3"
    ) -> Dict[str, Any]:
        """
        完整流程：上传图片并生成视频
        
        Args:
            image_path: 图片文件路径
            file_name: 自定义文件名（可选）
            mode: 视频生成模式
            model_name: 模型名称
        
        Returns:
            dict: 完整流程结果
        """
        try:
            # 第一步：上传图片
            upload_result = await GrokVideoGenerator._upload_image_file(image_path, file_name)
            
            if not upload_result.get("success"):
                return upload_result
            
            # 获取文件信息
            file_metadata_id = upload_result["data"].get("fileMetadataId")
            file_uri = upload_result["data"].get("fileUri")
            
            if not file_metadata_id or not file_uri:
                return {"error": "上传成功但未获取到必要信息"}
            
            # 第二步：创建媒体帖子
            post_result = await GrokVideoGenerator._create_media_post(file_uri, file_metadata_id)
            
            if not post_result.get("success"):
                return {
                    "upload_result": upload_result,
                    "post_result": post_result,
                    "error": "创建媒体帖子失败"
                }
            
            # 构建图片URL
            image_url = f"https://assets.grok.com/{file_uri}"
            
            # 第三步：创建对话并生成视频
            conversation_result = await GrokVideoGenerator._create_video_conversation(
                file_metadata_id,
                image_url,
                mode,
                model_name
            )
            
            return {
                "upload_result": upload_result,
                "post_result": post_result,
                "conversation_result": conversation_result,
                "success": conversation_result.get("success", False)
            }
            
        except Exception as e:
            logger.error(f"[Video] 视频生成流程异常: {str(e)}")
            return {"error": f"视频生成异常: {str(e)}"}

    @staticmethod
    async def _upload_image_file(image_path: str, file_name: Optional[str] = None) -> Dict[str, Any]:
        """
        上传图片文件到Grok Imagine
        
        Args:
            image_path: 图片文件路径
            file_name: 自定义文件名（可选）
        
        Returns:
            dict: 上传结果
        """
        try:
            # 读取图片文件
            image_path = Path(image_path)
            if not image_path.exists():
                return {"error": "文件不存在"}
            
            # 获取文件信息
            file_name = file_name or image_path.name
            file_mime_type = GrokVideoGenerator._get_mime_type(image_path.suffix)
            
            # 读取并编码图片
            with open(image_path, 'rb') as f:
                image_data = f.read()
                content = base64.b64encode(image_data).decode('utf-8')
            
            # 构建请求数据
            data = {
                "content": content,
                "fileMimeType": file_mime_type,
                "fileName": file_name,
                "fileSource": "IMAGINE_SELF_UPLOAD_FILE_SOURCE"
            }
            
            # 获取认证令牌
            auth_token = token_manager.get_token("grok-3")
            cf_clearance = setting.grok_config.get("cf_clearance", "")
            cookie = f"{auth_token};{cf_clearance}" if cf_clearance else auth_token
            
            # 获取代理配置
            proxy_url = setting.grok_config.get("proxy_url", "")
            proxies = {"http": proxy_url, "https": proxy_url} if proxy_url else None
            
            if proxy_url:
                logger.debug(f"[Video] 使用代理: {proxy_url.split('@')[-1] if '@' in proxy_url else proxy_url}")
            
            # 构建上传请求的headers
            upload_headers = get_dynamic_headers("/rest/app-chat/upload-file")
            upload_headers["referer"] = "https://grok.com/imagine"
            upload_headers["Cookie"] = cookie
            
            # 发送请求
            async with AsyncSession() as session:
                response = await session.post(
                    "https://grok.com/rest/app-chat/upload-file",
                    headers=upload_headers,
                    json=data,
                    impersonate=IMPERSONATE_BROWSER,
                    timeout=REQUEST_TIMEOUT,
                    proxies=proxies
                )
                
                if response.status_code == 200:
                    return {
                        "success": True,
                        "data": response.json(),
                        "status_code": response.status_code
                    }
                else:
                    return {
                        "success": False,
                        "error": f"上传失败，状态码: {response.status_code}",
                        "response": response.text
                    }
                    
        except Exception as e:
            logger.error(f"[Video] 上传图片异常: {str(e)}")
            return {"error": f"上传异常: {str(e)}"}

    @staticmethod
    async def _create_media_post(file_uri: str, file_metadata_id: str, media_type: str = "MEDIA_POST_TYPE_IMAGE") -> Dict[str, Any]:
        """
        创建媒体帖子
        
        Args:
            file_uri: 上传文件后返回的fileUri
            file_metadata_id: 文件元数据ID，用于构建referer
            media_type: 媒体类型，默认为图片
        
        Returns:
            dict: 创建结果
        """
        try:
            # 构建请求数据
            data = {
                "mediaType": media_type,
                "mediaUrl": f"https://assets.grok.com/{file_uri}"
            }
            
            # 获取认证令牌
            auth_token = token_manager.get_token("grok-3")
            cf_clearance = setting.grok_config.get("cf_clearance", "")
            cookie = f"{auth_token};{cf_clearance}" if cf_clearance else auth_token
            
            # 获取代理配置
            proxy_url = setting.grok_config.get("proxy_url", "")
            proxies = {"http": proxy_url, "https": proxy_url} if proxy_url else None
            
            # 构建媒体帖子请求的headers
            post_headers = get_dynamic_headers("/rest/media/post/create")
            post_headers["referer"] = f"https://grok.com/imagine/{file_metadata_id}"
            post_headers["Cookie"] = cookie
            
            # 发送请求
            async with AsyncSession() as session:
                response = await session.post(
                    CREATE_POST_ENDPOINT,
                    headers=post_headers,
                    json=data,
                    impersonate=IMPERSONATE_BROWSER,
                    timeout=REQUEST_TIMEOUT,
                    proxies=proxies
                )
                
                if response.status_code == 200:
                    return {
                        "success": True,
                        "data": response.json(),
                        "status_code": response.status_code
                    }
                else:
                    return {
                        "success": False,
                        "error": f"创建帖子失败，状态码: {response.status_code}",
                        "response": response.text
                    }
                    
        except Exception as e:
            logger.error(f"[Video] 创建帖子异常: {str(e)}")
            return {"error": f"创建帖子异常: {str(e)}"}

    @staticmethod
    async def _create_video_conversation(
        file_metadata_id: str, 
        image_url: str, 
        mode: str = "normal", 
        model_name: str = "grok-3"
    ) -> Dict[str, Any]:
        """
        创建新对话并生成视频
        
        Args:
            file_metadata_id: 文件元数据ID
            image_url: 图片URL
            mode: 生成模式，默认为normal
            model_name: 模型名称，默认为grok-3
        
        Returns:
            dict: 对话创建和视频生成结果
        """
        try:
            # 构建请求数据
            data = {
                "fileAttachments": [file_metadata_id],
                "message": f"{image_url} --mode={mode}",
                "modelName": model_name,
                "temporary": True,
                "toolOverrides": {"videoGen": True},
                "videoGen": True
            }
            
            # 获取认证令牌
            auth_token = token_manager.get_token(model_name)
            cf_clearance = setting.grok_config.get("cf_clearance", "")
            cookie = f"{auth_token};{cf_clearance}" if cf_clearance else auth_token
            
            # 获取代理配置
            proxy_url = setting.grok_config.get("proxy_url", "")
            proxies = {"http": proxy_url, "https": proxy_url} if proxy_url else None
            
            # 构建对话请求的headers
            conversation_headers = get_dynamic_headers("/rest/app-chat/conversations/new")
            conversation_headers["referer"] = f"https://grok.com/imagine/{file_metadata_id}"
            conversation_headers["Cookie"] = cookie
            
            # 发送请求
            async with AsyncSession() as session:
                response = await session.post(
                    CONVERSATION_ENDPOINT,
                    headers=conversation_headers,
                    json=data,
                    impersonate=IMPERSONATE_BROWSER,
                    timeout=REQUEST_TIMEOUT,
                    proxies=proxies
                )
                
                if response.status_code == 200:
                    # 处理流式响应
                    response_text = response.text
                    responses = []
                    
                    # 分割多个JSON响应
                    json_objects = response_text.strip().split('\n')
                    for json_str in json_objects:
                        if json_str.strip():
                            try:
                                json_obj = json.loads(json_str)
                                responses.append(json_obj)
                            except json.JSONDecodeError:
                                continue
                    
                    # 查找最终视频URL
                    video_url = None
                    video_id = None
                    progress = 0
                    
                    for resp in responses:
                        if 'result' in resp and 'response' in resp['result']:
                            response_data = resp['result']['response']
                            
                            # 检查是否有视频生成响应
                            if 'streamingVideoGenerationResponse' in response_data:
                                video_data = response_data['streamingVideoGenerationResponse']
                                video_id = video_data.get('videoId')
                                progress = video_data.get('progress', 0)
                                
                                # 如果进度100%，获取最终视频URL
                                if progress == 100 and 'videoUrl' in video_data:
                                    video_url = video_data['videoUrl']
                                    break
                    
                    return {
                        "success": True,
                        "data": {
                            "responses": responses,
                            "video_id": video_id,
                            "video_url": video_url,
                            "progress": progress,
                            "final_video_url": f"https://assets.grok.com/{video_url}" if video_url else None
                        },
                        "status_code": response.status_code
                    }
                else:
                    return {
                        "success": False,
                        "error": f"创建对话失败，状态码: {response.status_code}",
                        "response": response.text
                    }
                    
        except Exception as e:
            logger.error(f"[Video] 创建对话异常: {str(e)}")
            return {"error": f"创建对话异常: {str(e)}"}

    @staticmethod
    def _get_mime_type(file_extension: str) -> str:
        """根据文件扩展名获取MIME类型"""
        mime_types = {
            '.jpg': 'image/jpeg',
            '.jpeg': 'image/jpeg',
            '.png': 'image/png',
            '.gif': 'image/gif',
            '.webp': 'image/webp',
            '.bmp': 'image/bmp'
        }
        return mime_types.get(file_extension.lower(), 'image/jpeg')


# 全局视频生成器实例
video_generator = GrokVideoGenerator()
