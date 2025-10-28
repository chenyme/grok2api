"""Grok API 客户端模块"""

import asyncio
import json
from typing import Dict, List, Tuple, Any

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

# 常量定义
GROK_API_ENDPOINT = "https://grok.com/rest/app-chat/conversations/new"
REQUEST_TIMEOUT = 120
IMPERSONATE_BROWSER = "chrome133a"
MAX_RETRY = 3  # 最大重试次数


class GrokClient:
    """Grok API 客户端"""

    @staticmethod
    async def openai_to_grok(openai_request: dict):
        """转换OpenAI请求为Grok请求并处理响应"""
        model = openai_request["model"]
        messages = openai_request["messages"]
        stream = openai_request.get("stream", False)

        logger.debug(f"[Client] 处理请求 - 模型:{model}, 消息数:{len(messages)}, 流式:{stream}")

        # 提取消息内容和图片URL
        content, image_urls = GrokClient._extract_content(messages)
        model_name, model_mode = Models.to_grok(model)
        is_video_model = Models.get_model_info(model).get("is_video_model", False)
        
        # 视频模型特殊处理
        if is_video_model:
            if len(image_urls) > 1:
                logger.warning(f"[Client] 视频模型只允许一张图片，当前有{len(image_urls)}张，只使用第一张")
                image_urls = image_urls[:1]
            logger.debug(f"[Client] 视频模型文本处理: {content}")

        # 重试逻辑
        return await GrokClient._try(model, content, image_urls, model_name, model_mode, is_video_model, stream)

    @staticmethod
    async def _try(model: str, content: str, image_urls: List[str], model_name: str, model_mode: str, is_video: bool, stream: bool):
        """带重试的请求执行"""
        last_err = None
        
        for i in range(MAX_RETRY):
            try:
                # 获取token
                auth_token = token_manager.get_token(model)
                
                # 上传图片
                imgs, uris = await GrokClient._upload_imgs(image_urls, auth_token)
                
                # 视频模型需要额外的create操作
                post_id = None
                if is_video and imgs and uris:
                    logger.debug(f"[Client] 检测到视频模型，执行post create操作")
                    try:
                        create_result = await PostCreateManager.create(imgs[0], uris[0], auth_token)
                        if create_result and create_result.get("success"):
                            post_id = create_result.get("post_id")
                            logger.debug(f"[Client] Post创建成功: {post_id}")
                        else:
                            logger.warning(f"[Client] Post创建失败，继续使用原有流程")
                    except Exception as e:
                        logger.warning(f"[Client] Post创建异常: {e}，继续使用原有流程")
                
                # 构建并发送请求
                payload = GrokClient._build_payload(content, model_name, model_mode, imgs, uris, is_video, post_id)
                logger.debug(f"[Client] 请求载荷配置: {payload}")
                return await GrokClient._send_request(payload, auth_token, model, stream, post_id)
                
            except GrokApiException as e:
                last_err = e
                # 401/429 可重试，其他错误直接抛出
                if e.error_code not in ["HTTP_ERROR", "NO_AVAILABLE_TOKEN"]:
                    raise
                
                # 检查是否为可重试的状态码
                status = e.context.get("status") if e.context else None
                if status not in [401, 429]:
                    raise
                
                if i < MAX_RETRY - 1:
                    logger.warning(f"[Client] 请求失败(状态码:{status}), 重试 {i+1}/{MAX_RETRY}")
                    await asyncio.sleep(0.5)  # 短暂延迟
                else:
                    logger.error(f"[Client] 重试{MAX_RETRY}次后仍失败")
        
        raise last_err if last_err else GrokApiException("请求失败", "REQUEST_ERROR")

    @staticmethod
    def _extract_content(messages: List[Dict]) -> Tuple[str, List[str]]:
        """提取消息内容和图片URL"""
        content_parts = []
        image_urls = []

        for msg in messages:
            msg_content = msg.get("content", "")

            # 处理复杂消息格式（包含文本和图片）
            if isinstance(msg_content, list):
                for item in msg_content:
                    item_type = item.get("type")
                    if item_type == "text":
                        content_parts.append(item.get("text", ""))
                    elif item_type == "image_url":
                        url = item.get("image_url", {}).get("url", "")
                        if url:
                            image_urls.append(url)
            # 处理纯文本消息
            else:
                content_parts.append(msg_content)

        return "".join(content_parts), image_urls

    @staticmethod
    async def _upload_imgs(image_urls: List[str], auth_token: str) -> Tuple[List[str], List[str]]:
        """上传图片并返回附件ID列表"""
        image_attachments = []
        image_uris = []
        # 并发上传所有图片
        tasks = [ImageUploadManager.upload(url, auth_token) for url in image_urls]
        results = await asyncio.gather(*tasks, return_exceptions=True)

        for url, (file_id, file_uri) in zip(image_urls, results):
            if isinstance(file_id, Exception):
                logger.warning(f"[Client] 图片上传失败: {url}, 错误: {file_id}")
            elif file_id:
                image_attachments.append(file_id)
                image_uris.append(file_uri)

        return image_attachments, image_uris

    @staticmethod
    def _build_payload(content: str, model_name: str, model_mode: str, image_attachments: List[str], image_uris: List[str], is_video_model: bool = False, post_id: str = None) -> Dict[str, Any]:
        """构建Grok API请求载荷"""
        payload = {
            "temporary": setting.grok_config.get("temporary", True),
            "modelName": model_name,
            "message": content,
            "fileAttachments": image_attachments,
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
            "responseMetadata": {"requestModelDetails": {"modelId": model_name}},
            "disableMemory": False,
            "forceSideBySide": False,
            "modelMode": model_mode,
            "isAsyncChat": False
        }
        
        # 视频模型特殊配置
        if is_video_model and image_uris:
            image_url = image_uris[0]
            logger.debug(f"[Client] 视频模型图片URL: {image_url}")
            
            # 根据是否有post_id选择不同的URL格式
            if post_id:
                logger.debug(f"[Client] 使用PostID构建URL: {post_id}")
                image_message = f"https://grok.com/imagine/{post_id}  {content} --mode=custom"
            else:
                logger.debug(f"[Client] 使用文件URI构建URL: {image_url}")
                image_message = f"https://assets.grok.com/post/{image_url}  {content} --mode=custom"
            
            payload = {
                "temporary": True,
                "modelName": "grok-3",
                "message": image_message,
                "fileAttachments": image_attachments,
                "toolOverrides": {"videoGen": True}
            }
            logger.debug(f"[Client] 视频模型载荷配置: {payload}")
            logger.debug("[Client] 视频模型载荷配置: toolOverrides.videoGen = True")
        
        return payload

    @staticmethod
    async def _send_request(payload: dict, auth_token: str, model: str, stream: bool, post_id: str = None):
        """发送HTTP请求到Grok API"""
        # 验证认证令牌
        if not auth_token:
            raise GrokApiException("认证令牌缺失", "NO_AUTH_TOKEN")

        try:
            # 构建请求头
            headers = GrokClient._build_headers(auth_token)
            if model == "grok-imagine-0.9":
                # 优先使用传入的post_id，否则使用fileAttachments中的第一个
                referer_id = post_id if post_id else payload.get("fileAttachments", [""])[0]
                if referer_id:
                    headers["Referer"] = f"https://grok.com/imagine/{referer_id}"
                    logger.debug(f"[Client] 设置Referer: {headers['Referer']}")
            
            # 使用服务代理
            proxy_url = setting.get_service_proxy()
            proxies = {"http": proxy_url, "https": proxy_url} if proxy_url else None
            
            if proxy_url:
                logger.debug(f"[Client] 使用服务代理: {proxy_url.split('@')[-1] if '@' in proxy_url else proxy_url}")

            # 构建请求参数
            request_kwargs = {
                "headers": headers,
                "data": json.dumps(payload),
                "impersonate": IMPERSONATE_BROWSER,
                "timeout": REQUEST_TIMEOUT,
                "stream": True,
                "proxies": proxies
            }

            # 在线程池中执行同步HTTP请求，避免阻塞事件循环
            response = await asyncio.to_thread(
                curl_requests.post,
                GROK_API_ENDPOINT,
                **request_kwargs
            )

            logger.debug(f"[Client] API响应状态码: {response.status_code}")

            # 处理非成功响应
            if response.status_code != 200:
                GrokClient._handle_error(response, auth_token)

            # 请求成功，重置失败计数
            asyncio.create_task(token_manager.reset_failure(auth_token))

            # 处理并返回响应
            return await GrokClient._process_response(response, auth_token, model, stream)

        except curl_requests.RequestsError as e:
            logger.error(f"[Client] 网络请求错误: {e}")
            raise GrokApiException(f"网络错误: {e}", "NETWORK_ERROR") from e
        except json.JSONDecodeError as e:
            logger.error(f"[Client] JSON解析错误: {e}")
            raise GrokApiException(f"JSON解析错误: {e}", "JSON_ERROR") from e
        except Exception as e:
            logger.error(f"[Client] 未知请求错误: {type(e).__name__}: {e}")
            raise GrokApiException(f"请求处理错误: {e}", "REQUEST_ERROR") from e

    @staticmethod
    def _build_headers(auth_token: str) -> Dict[str, str]:
        """构建请求头"""
        headers = get_dynamic_headers("/rest/app-chat/conversations/new")

        # 构建Cookie
        cf_clearance = setting.grok_config.get("cf_clearance", "")
        headers["Cookie"] = f"{auth_token};{cf_clearance}" if cf_clearance else auth_token

        return headers

    @staticmethod
    def _handle_error(response, auth_token: str):
        """处理错误响应"""
        try:
            error_data = response.json()
            error_message = str(error_data)
        except Exception as e:
            error_data = response.text
            error_message = error_data[:200] if error_data else e

        # 记录Token失败
        asyncio.create_task(token_manager.record_failure(auth_token, response.status_code, error_message))

        raise GrokApiException(
            f"请求失败: {response.status_code} - {error_message}",
            "HTTP_ERROR",
            {"status": response.status_code, "data": error_data}
        )

    @staticmethod
    async def _process_response(response, auth_token: str, model: str, stream: bool):
        """处理API响应"""
        if stream:
            result = GrokResponseProcessor.process_stream(response, auth_token)
            asyncio.create_task(GrokClient._update_rate_limits(auth_token, model))
        else:
            result = await GrokResponseProcessor.process_normal(response, auth_token, model)
            asyncio.create_task(GrokClient._update_rate_limits(auth_token, model))

        return result

    @staticmethod
    async def _update_rate_limits(auth_token: str, model: str):
        """异步更新速率限制信息"""
        try:
            await token_manager.check_limits(auth_token, model)
        except Exception as e:
            logger.error(f"[Client] 更新速率限制失败: {e}")