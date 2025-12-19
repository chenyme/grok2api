"""图片上传管理器 - 支持Base64和URL图片上传"""

import asyncio
import base64
import re
from typing import Tuple, Optional
from urllib.parse import urlparse
from curl_cffi.requests import AsyncSession

from app.services.grok.statsig import get_dynamic_headers
from app.core.exception import GrokApiException
from app.core.config import setting
from app.core.logger import logger


# 常量
UPLOAD_API = "https://grok.com/rest/app-chat/upload-file"
TIMEOUT = 30
BROWSER = "chrome133a"

# MIME类型
MIME_TYPES = {
    '.jpg': 'image/jpeg', '.jpeg': 'image/jpeg', '.png': 'image/png',
    '.gif': 'image/gif', '.webp': 'image/webp', '.bmp': 'image/bmp',
}
DEFAULT_MIME = "image/jpeg"
DEFAULT_EXT = "jpg"


class ImageUploadManager:
    """图片上传管理器"""

    @staticmethod
    async def upload(image_input: str, auth_token: str) -> Tuple[str, str]:
        """上传图片（支持Base64或URL）
        
        Returns:
            (file_id, file_uri) 元组
        """
        try:
            # 判断类型并处理
            if ImageUploadManager._is_url(image_input):
                buffer, mime = await ImageUploadManager._download(image_input)
                filename, _ = ImageUploadManager._get_info("", mime)
            else:
                buffer = image_input.split(",")[1] if "data:image" in image_input else image_input
                filename, mime = ImageUploadManager._get_info(image_input)

            # 构建数据
            data = {
                "fileName": filename,
                "fileMimeType": mime,
                "content": buffer,
            }


            if not auth_token:
                raise GrokApiException("认证令牌缺失", "NO_AUTH_TOKEN")

            # 外层重试：可配置状态码（401/429等）
            retry_codes = setting.grok_config.get("retry_status_codes", [401, 429])
            MAX_OUTER_RETRY = 3
            
            for outer_retry in range(MAX_OUTER_RETRY + 1):  # +1 确保实际重试3次
                try:
                    # 内层重试：403代理池重试
                    max_403_retries = 5
                    retry_403_count = 0
                    
                    while retry_403_count <= max_403_retries:
                        # 请求配置
                        cf = setting.grok_config.get("cf_clearance", "")
                        headers = {
                            **get_dynamic_headers("/rest/app-chat/upload-file"),
                            "Cookie": f"{auth_token};{cf}" if cf else auth_token,
                        }
                        
                        # 异步获取代理（支持代理池）
                        from app.core.proxy_pool import proxy_pool
                        
                        # 如果是403重试且使用代理池，强制刷新代理
                        if retry_403_count > 0 and proxy_pool._enabled:
                            logger.info(f"[Upload] 403重试 {retry_403_count}/{max_403_retries}，刷新代理...")
                            proxy = await proxy_pool.force_refresh()
                        else:
                            proxy = await setting.get_proxy_async("service")
                        
                        proxies = {"http": proxy, "https": proxy} if proxy else None

                        # 上传
                        async with AsyncSession() as session:
                            response = await session.post(
                                UPLOAD_API,
                                headers=headers,
                                json=data,
                                impersonate=BROWSER,
                                timeout=TIMEOUT,
                                proxies=proxies,
                            )

                            # 内层403重试：仅当有代理池时触发
                            if response.status_code == 403 and proxy_pool._enabled:
                                retry_403_count += 1
                                
                                if retry_403_count <= max_403_retries:
                                    logger.warning(f"[Upload] 遇到403错误，正在重试 ({retry_403_count}/{max_403_retries})...")
                                    await asyncio.sleep(0.5)
                                    continue
                                
                                # 内层重试全部失败
                                logger.error(f"[Upload] 403错误，已重试{retry_403_count-1}次，放弃")
                            
                            # 检查可配置状态码错误 - 外层重试
                            if response.status_code in retry_codes:
                                if outer_retry < MAX_OUTER_RETRY:
                                    delay = (outer_retry + 1) * 0.1  # 渐进延迟：0.1s, 0.2s, 0.3s
                                    logger.warning(f"[Upload] 遇到{response.status_code}错误，外层重试 ({outer_retry+1}/{MAX_OUTER_RETRY})，等待{delay}s...")
                                    await asyncio.sleep(delay)
                                    break  # 跳出内层循环，进入外层重试
                                else:
                                    logger.error(f"[Upload] {response.status_code}错误，已重试{outer_retry}次，放弃")
                                    return "", ""
                            
                            if response.status_code == 200:
                                result = response.json()
                                file_id = result.get("fileMetadataId", "")
                                file_uri = result.get("fileUri", "")
                                
                                if outer_retry > 0 or retry_403_count > 0:
                                    logger.info(f"[Upload] 重试成功！")
                                
                                logger.debug(f"[Upload] 成功，ID: {file_id}")
                                return file_id, file_uri
                            
                            # 其他错误直接返回
                            logger.error(f"[Upload] 失败，状态码: {response._status_code}")
                            return "", ""
                    
                    # 内层循环正常结束（非break），说明403重试全部失败
                    return "", ""
                
                except Exception as e:
                    if outer_retry < MAX_OUTER_RETRY - 1:
                        logger.warning(f"[Upload] 异常: {e}，外层重试 ({outer_retry+1}/{MAX_OUTER_RETRY})...")
                        await asyncio.sleep(0.5)
                        continue
                    
                    logger.warning(f"[Upload] 失败: {e}")
                    return "", ""
            
            return "", ""

        except Exception as e:
            logger.warning(f"[Upload] 失败: {e}")
            return "", ""

    @staticmethod
    def _is_url(input_str: str) -> bool:
        """检查是否为URL"""
        try:
            result = urlparse(input_str)
            return all([result.scheme, result.netloc]) and result.scheme in ['http', 'https']
        except:
            return False

    @staticmethod
    async def _download(url: str) -> Tuple[str, str]:
        """下载图片并转Base64
        
        Returns:
            (base64_string, mime_type) 元组
        """
        try:
            async with AsyncSession() as session:
                response = await session.get(url, timeout=5)
                response.raise_for_status()

                content_type = response.headers.get('content-type', DEFAULT_MIME)
                if not content_type.startswith('image/'):
                    content_type = DEFAULT_MIME

                b64 = base64.b64encode(response.content).decode()
                return b64, content_type
        except Exception as e:
            logger.warning(f"[Upload] 下载失败: {e}")
            return "", ""

    @staticmethod
    def _get_info(image_data: str, mime_type: Optional[str] = None) -> Tuple[str, str]:
        """获取文件名和MIME类型
        
        Returns:
            (file_name, mime_type) 元组
        """
        # 已提供MIME类型
        if mime_type:
            ext = mime_type.split("/")[1] if "/" in mime_type else DEFAULT_EXT
            return f"image.{ext}", mime_type

        # 从Base64提取
        mime = DEFAULT_MIME
        ext = DEFAULT_EXT

        if "data:image" in image_data:
            if match := re.search(r"data:([a-zA-Z0-9]+/[a-zA-Z0-9-.+]+);base64,", image_data):
                mime = match.group(1)
                ext = mime.split("/")[1]

        return f"image.{ext}", mime