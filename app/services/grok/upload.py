"""图片上传管理器"""

import base64
import re
from typing import Tuple, Optional
from urllib.parse import urlparse

from curl_cffi.requests import AsyncSession

from app.services.grok.statsig import get_dynamic_headers
from app.core.exception import GrokApiException
from app.core.config import setting
from app.core.logger import logger

# 常量定义
UPLOAD_ENDPOINT = "https://grok.com/rest/app-chat/upload-file"
REQUEST_TIMEOUT = 30
IMPERSONATE_BROWSER = "chrome133a"
DEFAULT_MIME_TYPE = "image/jpeg"
DEFAULT_EXTENSION = "jpg"


class ImageUploadManager:
    """
    Grok图片上传管理器
    
    提供图片上传功能，支持：
    - Base64格式图片上传
    - URL图片下载并上传
    - 多种图片格式支持
    """

    @staticmethod
    async def upload(image_input: str, auth_token: str) -> str:
        """上传图片到Grok，支持Base64或URL"""
        try:
            if ImageUploadManager._is_url(image_input):
                # 下载 URL 图片
                image_buffer, mime_type = await ImageUploadManager._download(image_input)

                # 获取图片信息
                file_name, _ = ImageUploadManager._get_info("", mime_type)

            else:
                # 处理 base64 数据
                image_buffer = image_input.split(",")[1] if "data:image" in image_input else image_input

                # 获取图片信息
                file_name, mime_type = ImageUploadManager._get_info(image_input)

            # 构建上传数据
            upload_data = {
                "fileName": file_name,
                "fileMimeType": mime_type,
                "content": image_buffer,
            }

            # 获取认证令牌
            if not auth_token:
                raise GrokApiException("认证令牌缺失或为空", "NO_AUTH_TOKEN")

            cf_clearance = setting.grok_config.get("cf_clearance", "")
            cookie = f"{auth_token};{cf_clearance}" if cf_clearance else auth_token
            
            proxy_url = setting.grok_config.get("proxy_url", "")
            if proxy_url:
                logger.debug(f"[Upload] 使用代理: {proxy_url.split('@')[-1] if '@' in proxy_url else proxy_url}")

            proxies = {"http": proxy_url, "https": proxy_url} if proxy_url else None

            # 发送异步请求
            async with AsyncSession() as session:
                response = await session.post(
                    UPLOAD_ENDPOINT,
                    headers={
                        **get_dynamic_headers("/rest/app-chat/upload-file"),
                        "Cookie": cookie,
                    },
                    json=upload_data,
                    impersonate=IMPERSONATE_BROWSER,
                    timeout=REQUEST_TIMEOUT,
                    proxies=proxies,
                )

                # 检查响应
                if response.status_code == 200:
                    result = response.json()
                    file_id = result.get("fileMetadataId", "")
                    file_uri = result.get("fileUri", "")
                    logger.debug(f"[Upload] 图片上传成功，文件ID: {file_id}")
                    return file_id, file_uri

            return "", ""

        except Exception as e:
            logger.warning(f"[Upload] 上传图片失败: {e}")
            return ""

    @staticmethod
    def _is_url(image_input: str) -> bool:
        """检查输入是否为有效的URL"""
        try:
            result = urlparse(image_input)
            return all([result.scheme, result.netloc]) and result.scheme in ['http', 'https']
        except Exception as e:
            logger.warning(f"[Upload] URL解析失败: {e}")
            return False

    @staticmethod
    async def _download(url: str) -> Tuple[str, str]:
        """下载图片并转换为Base64"""
        try:
            async with AsyncSession() as session:
                response = await session.get(url, timeout=5)
                response.raise_for_status()

                # 获取内容类型
                content_type = response.headers.get('content-type', DEFAULT_MIME_TYPE)
                if not content_type.startswith('image/'):
                    content_type = DEFAULT_MIME_TYPE

                # 转换为 Base64
                image_base64 = base64.b64encode(response.content).decode('utf-8')
                return image_base64, content_type
        except Exception as e:
            logger.warning(f"[Upload] 下载图片失败: {e}")
            return "", ""

    @staticmethod
    def _get_info(image_data: str, mime_type: Optional[str] = None) -> Tuple[str, str]:
        """获取图片文件名和MIME类型"""
        # mime_type 有值，直接使用
        if mime_type:
            extension = mime_type.split("/")[1] if "/" in mime_type else DEFAULT_EXTENSION
            file_name = f"image.{extension}"
            return file_name, mime_type

        # mime_type 没有值，使用默认值
        mime_type = DEFAULT_MIME_TYPE
        extension = DEFAULT_EXTENSION

        # 从 Base64 数据中提取 MIME 类型
        if "data:image" in image_data:
            match = re.search(r"data:([a-zA-Z0-9]+/[a-zA-Z0-9-.+]+);base64,", image_data)
            if match:
                mime_type = match.group(1)
                extension = mime_type.split("/")[1]

        file_name = f"image.{extension}"
        return file_name, mime_type