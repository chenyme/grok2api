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
from app.core.exception import GrokApiException

# 常量定义
GROK_API_ENDPOINT = "https://grok.com/rest/app-chat/conversations/new"
REQUEST_TIMEOUT = 120
IMPERSONATE_BROWSER = "chrome133a"


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

        # 获取认证令牌和模型信息
        auth_token = token_manager.get_token(model)
        model_name, model_mode = Models.to_grok(model)

        # 检查是否为视频模型
        is_video_model = Models.get_model_info(model).get("is_video_model", False)
        
        # 视频模型特殊处理：只允许一张图片
        if is_video_model and len(image_urls) > 1:
            logger.warning(f"[Client] 视频模型只允许一张图片，当前有{len(image_urls)}张，只使用第一张")
            image_urls = image_urls[:1]
        
        # 上传图片并获取附件ID列表
        image_attachments = await GrokClient._upload_imgs(image_urls, auth_token)

        # 视频模型：文本添加 --mode=custom
        if is_video_model:
            content = f"{content} --mode=custom"
            logger.debug(f"[Client] 视频模型文本处理: {content}")

        # 构建Grok请求载荷
        payload = GrokClient._build_payload(content, model_name, model_mode, image_attachments, is_video_model)

        return await GrokClient._send_request(payload, auth_token, model, stream)

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
    async def _upload_imgs(image_urls: List[str], auth_token: str) -> List[str]:
        """上传图片并返回附件ID列表"""
        image_attachments = []
        # 并发上传所有图片
        tasks = [ImageUploadManager.upload(url, auth_token) for url in image_urls]
        results = await asyncio.gather(*tasks, return_exceptions=True)

        for url, result in zip(image_urls, results):
            if isinstance(result, Exception):
                logger.warning(f"[Client] 图片上传失败: {url}, 错误: {result}")
            elif result:
                image_attachments.append(result)

        return image_attachments

    @staticmethod
    def _build_payload(content: str, model_name: str, model_mode: str, image_attachments: List[str], is_video_model: bool = False) -> Dict[str, Any]:
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
        if is_video_model:
            payload["toolOverrides"] = {"videoGen": True}
            logger.debug("[Client] 视频模型载荷配置: toolOverrides.videoGen = True")
        
        return payload

    @staticmethod
    async def _send_request(payload: dict, auth_token: str, model: str, stream: bool):
        """发送HTTP请求到Grok API"""
        # 验证认证令牌
        if not auth_token:
            raise GrokApiException("认证令牌缺失", "NO_AUTH_TOKEN")

        try:
            # 构建请求头和代理
            headers = GrokClient._build_headers(auth_token)
            proxies = GrokClient._get_proxy()

            # 在线程池中执行同步HTTP请求，避免阻塞事件循环
            response = await asyncio.to_thread(
                curl_requests.post,
                GROK_API_ENDPOINT,
                headers=headers,
                data=json.dumps(payload),
                impersonate=IMPERSONATE_BROWSER,
                timeout=REQUEST_TIMEOUT,
                stream=True,
                **proxies
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
            raise GrokApiException(f"网络错误: {e}", "NETWORK_ERROR") from e
        except json.JSONDecodeError as e:
            raise GrokApiException(f"JSON解析错误: {e}", "JSON_ERROR") from e

    @staticmethod
    def _build_headers(auth_token: str) -> Dict[str, str]:
        """构建请求头"""
        headers = get_dynamic_headers("/rest/app-chat/conversations/new")

        # 构建Cookie
        cf_clearance = setting.grok_config.get("cf_clearance", "")
        headers["Cookie"] = f"{auth_token};{cf_clearance}" if cf_clearance else auth_token

        return headers

    @staticmethod
    def _get_proxy() -> Dict[str, str]:
        """获取代理配置"""
        proxy_url = setting.grok_config.get("proxy_url", "")
        if proxy_url:
            return {"http": proxy_url, "https": proxy_url}
        return {}

    @staticmethod
    def _handle_error(response, auth_token: str):
        """处理错误响应"""
        try:
            error_data = response.json()
            error_message = str(error_data)
        except Exception:
            error_data = response.text
            error_message = error_data[:200] if error_data else "未知错误"

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