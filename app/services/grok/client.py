"""Grok API 客户端 - 处理OpenAI到Grok的请求转换和响应处理"""

import asyncio
import orjson
from typing import Dict, List, Tuple, Any, Optional
from curl_cffi import requests as curl_requests

from app.core.config import setting
from app.core.logger import logger
from app.models.grok_models import Models
from app.services.grok.processer import GrokResponseProcessor
from app.services.grok.statsig import get_dynamic_headers
from app.services.grok.token import token_manager
from app.services.grok.upload import ImageUploadManager
from app.services.grok.create import PostCreateManager
from app.core.exception import GrokApiException


# 常量
API_ENDPOINT = "https://grok.com/rest/app-chat/conversations/new"
TIMEOUT = 120
BROWSER = "chrome133a"
MAX_RETRY = 3
MAX_UPLOADS = 5


class GrokClient:
    """Grok API 客户端"""
    
    _upload_sem = asyncio.Semaphore(MAX_UPLOADS)

    @staticmethod
    async def openai_to_grok(request: dict):
        """转换OpenAI请求为Grok请求"""
        model = request["model"]
        content, images = GrokClient._extract_content(request["messages"])
        stream = request.get("stream", False)
        
        # 获取模型信息
        info = Models.get_model_info(model)
        grok_model, mode = Models.to_grok(model)
        is_video = info.get("is_video_model", False)
        
        # 视频模型限制
        if is_video and len(images) > 1:
            logger.warning(f"[Client] 视频模型仅支持1张图片，已截取前1张")
            images = images[:1]
        
        return await GrokClient._retry(model, content, images, grok_model, mode, is_video, stream)

    @staticmethod
    async def _retry(model: str, content: str, images: List[str], grok_model: str, mode: str, is_video: bool, stream: bool):
        """重试请求"""
        last_err = None

        for i in range(MAX_RETRY):
            try:
                token = token_manager.get_token(model)
                img_ids, img_uris = await GrokClient._upload(images, token)

                # 视频模型创建会话
                post_id = None
                if is_video and img_ids and img_uris:
                    post_id = await GrokClient._create_post(img_ids[0], img_uris[0], token)

                payload = GrokClient._build_payload(content, grok_model, mode, img_ids, img_uris, is_video, post_id)
                return await GrokClient._request(payload, token, model, stream, post_id)

            except GrokApiException as e:
                last_err = e
                # 仅401/429可重试
                if e.error_code not in ["HTTP_ERROR", "NO_AVAILABLE_TOKEN"]:
                    raise

                status = e.context.get("status") if e.context else None
                if status not in [401, 429]:
                    raise

                if i < MAX_RETRY - 1:
                    logger.warning(f"[Client] 失败(状态:{status}), 重试 {i+1}/{MAX_RETRY}")
                    await asyncio.sleep(0.5)

        raise last_err or GrokApiException("请求失败", "REQUEST_ERROR")

    @staticmethod
    def _extract_content(messages: List[Dict]) -> Tuple[str, List[str]]:
        """提取文本和图片"""
        texts, images = [], []
        
        for msg in messages:
            content = msg.get("content", "")
            
            if isinstance(content, list):
                for item in content:
                    if item.get("type") == "text":
                        texts.append(item.get("text", ""))
                    elif item.get("type") == "image_url":
                        if url := item.get("image_url", {}).get("url"):
                            images.append(url)
            else:
                texts.append(content)
        
        return "".join(texts), images

    @staticmethod
    async def _upload(urls: List[str], token: str) -> Tuple[List[str], List[str]]:
        """并发上传图片"""
        if not urls:
            return [], []
        
        async def upload_limited(url):
            async with GrokClient._upload_sem:
                return await ImageUploadManager.upload(url, token)
        
        results = await asyncio.gather(*[upload_limited(u) for u in urls], return_exceptions=True)
        
        ids, uris = [], []
        for url, result in zip(urls, results):
            if isinstance(result, Exception):
                logger.warning(f"[Client] 上传失败: {url} - {result}")
            elif isinstance(result, tuple) and len(result) == 2:
                fid, furi = result
                if fid:
                    ids.append(fid)
                    uris.append(furi)
        
        return ids, uris

    @staticmethod
    async def _create_post(file_id: str, file_uri: str, token: str) -> Optional[str]:
        """创建视频会话"""
        try:
            result = await PostCreateManager.create(file_id, file_uri, token)
            if result and result.get("success"):
                return result.get("post_id")
        except Exception as e:
            logger.warning(f"[Client] 创建会话失败: {e}")
        return None

    @staticmethod
    def _build_payload(content: str, model: str, mode: str, img_ids: List[str], img_uris: List[str], is_video: bool = False, post_id: str = None) -> Dict:
        """构建请求载荷"""
        # 视频模型特殊处理
        if is_video and img_uris:
            img_msg = f"https://grok.com/imagine/{post_id}" if post_id else f"https://assets.grok.com/post/{img_uris[0]}"
            return {
                "temporary": True,
                "modelName": "grok-3",
                "message": f"{img_msg}  {content} --mode=custom",
                "fileAttachments": img_ids,
                "toolOverrides": {"videoGen": True}
            }
        
        # 标准载荷
        return {
            "temporary": setting.grok_config.get("temporary", True),
            "modelName": model,
            "message": content,
            "fileAttachments": img_ids,
            "imageAttachments": [],
            "disableSearch": False,
            "enableImageGeneration": True,
            "returnImageBytes": False,
            "returnRawGrokInXaiRequest": False,
            "enableImageStreaming": True,
            "imageGenerationCount": 2,
            "forceConcise": False,
            "toolOverrides": {},
            "enableSideBySide": True,
            "sendFinalMetadata": True,
            "isReasoning": False,
            "webpageUrls": [],
            "disableTextFollowUps": True,
            "responseMetadata": {"requestModelDetails": {"modelId": model}},
            "disableMemory": False,
            "forceSideBySide": False,
            "modelMode": mode,
            "isAsyncChat": False
        }

    @staticmethod
    async def _request(payload: dict, token: str, model: str, stream: bool, post_id: str = None):
        """发送请求"""
        if not token:
            raise GrokApiException("认证令牌缺失", "NO_AUTH_TOKEN")

        try:
            # 构建请求
            headers = GrokClient._build_headers(token)
            if model == "grok-imagine-0.9":
                file_attachments = payload.get("fileAttachments", [])
                ref_id = post_id or (file_attachments[0] if file_attachments else "")
                if ref_id:
                    headers["Referer"] = f"https://grok.com/imagine/{ref_id}"
            
            proxy = setting.get_proxy("service")
            proxies = {"http": proxy, "https": proxy} if proxy else None
            
            # 执行请求
            response = await asyncio.to_thread(
                curl_requests.post,
                API_ENDPOINT,
                headers=headers,
                data=orjson.dumps(payload),
                impersonate=BROWSER,
                timeout=TIMEOUT,
                stream=True,
                proxies=proxies
            )
            
            if response.status_code != 200:
                GrokClient._handle_error(response, token)
            
            # 成功 - 重置失败计数
            asyncio.create_task(token_manager.reset_failure(token))
            
            # 处理响应
            result = (GrokResponseProcessor.process_stream(response, token) if stream 
                     else await GrokResponseProcessor.process_normal(response, token, model))
            
            asyncio.create_task(GrokClient._update_limits(token, model))
            return result
            
        except curl_requests.RequestsError as e:
            logger.error(f"[Client] 网络错误: {e}")
            raise GrokApiException(f"网络错误: {e}", "NETWORK_ERROR") from e
        except Exception as e:
            logger.error(f"[Client] 请求错误: {e}")
            raise GrokApiException(f"请求错误: {e}", "REQUEST_ERROR") from e

    @staticmethod
    def _build_headers(token: str) -> Dict[str, str]:
        """构建请求头"""
        headers = get_dynamic_headers("/rest/app-chat/conversations/new")
        cf = setting.grok_config.get("cf_clearance", "")
        headers["Cookie"] = f"{token};{cf}" if cf else token
        return headers

    @staticmethod
    def _handle_error(response, token: str):
        """处理错误"""
        if response.status_code == 403:
            msg = "您的IP被拦截，请尝试以下方法之一: 1.更换IP 2.使用代理 3.配置CF值"
            data = {"cf_blocked": True, "status": 403}
            logger.warning(f"[Client] {msg}")
        else:
            try:
                data = response.json()
                msg = str(data)
            except:
                data = response.text
                msg = data[:200] if data else "未知错误"
        
        asyncio.create_task(token_manager.record_failure(token, response.status_code, msg))
        raise GrokApiException(
            f"请求失败: {response.status_code} - {msg}",
            "HTTP_ERROR",
            {"status": response.status_code, "data": data}
        )

    @staticmethod
    async def _update_limits(token: str, model: str):
        """更新速率限制"""
        try:
            await token_manager.check_limits(token, model)
        except Exception as e:
            logger.error(f"[Client] 更新限制失败: {e}")