"""Post创建管理器 - 用于视频生成前的会话创建"""

import orjson
from typing import Dict, Any, Optional
from curl_cffi.requests import AsyncSession

from app.services.grok.statsig import get_dynamic_headers
from app.core.exception import GrokApiException
from app.core.config import setting
from app.core.logger import logger


# 常量
ENDPOINT = "https://grok.com/rest/media/post/create"
TIMEOUT = 30
BROWSER = "chrome133a"


class PostCreateManager:
    """会话创建管理器"""

    @staticmethod
    async def create(file_id: str, file_uri: str, auth_token: str) -> Optional[Dict[str, Any]]:
        """创建会话记录
        
        Args:
            file_id: 文件ID
            file_uri: 文件URI
            auth_token: 认证令牌
            
        Returns:
            会话信息字典，包含post_id等
        """
        # 参数验证
        if not file_id or not file_uri:
            raise GrokApiException("文件ID或URI缺失", "INVALID_PARAMS")
        if not auth_token:
            raise GrokApiException("认证令牌缺失", "NO_AUTH_TOKEN")

        try:
            # 构建请求
            data = {
                "media_url": f"https://assets.grok.com/{file_uri}",
                "media_type": "MEDIA_POST_TYPE_IMAGE"
            }
            
            cf = setting.grok_config.get("cf_clearance", "")
            headers = {
                **get_dynamic_headers("/rest/media/post/create"),
                "Cookie": f"{auth_token};{cf}" if cf else auth_token
            }
            
            proxy = setting.grok_config.get("proxy_url", "")
            proxies = {"http": proxy, "https": proxy} if proxy else None

            # 发送请求
            async with AsyncSession() as session:
                response = await session.post(
                    ENDPOINT,
                    headers=headers,
                    json=data,
                    impersonate=BROWSER,
                    timeout=TIMEOUT,
                    proxies=proxies
                )

                if response.status_code == 200:
                    result = response.json()
                    post_id = result.get("post", {}).get("id", "")
                    logger.debug(f"[PostCreate] 成功，会话ID: {post_id}")
                    return {
                        "post_id": post_id,
                        "file_id": file_id,
                        "file_uri": file_uri,
                        "success": True,
                        "data": result
                    }
                
                # 错误处理
                try:
                    error = response.json()
                    msg = f"状态码: {response.status_code}, 详情: {error}"
                except:
                    msg = f"状态码: {response.status_code}, 详情: {response.text[:200]}"
                
                logger.error(f"[PostCreate] 失败: {msg}")
                raise GrokApiException(f"创建失败: {msg}", "CREATE_ERROR")

        except GrokApiException:
            raise
        except Exception as e:
            logger.error(f"[PostCreate] 异常: {e}")
            raise GrokApiException(f"创建异常: {e}", "CREATE_ERROR") from e
