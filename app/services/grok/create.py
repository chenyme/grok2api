"""Post创建管理器 - 用于视频生成前的会话创建"""

import asyncio
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
            
            # 外层重试：可配置状态码（401/429等）
            retry_codes = setting.grok_config.get("retry_status_codes", [401, 429])
            MAX_OUTER_RETRY = 3
            
            for outer_retry in range(MAX_OUTER_RETRY + 1):  # +1 确保实际重试3次
                # 内层重试：403代理池重试
                max_403_retries = 5
                retry_403_count = 0
                
                while retry_403_count <= max_403_retries:
                    # 异步获取代理（支持代理池）
                    from app.core.proxy_pool import proxy_pool
                    
                    # 如果是403重试且使用代理池，强制刷新代理
                    if retry_403_count > 0 and proxy_pool._enabled:
                        logger.info(f"[PostCreate] 403重试 {retry_403_count}/{max_403_retries}，刷新代理...")
                        proxy = await proxy_pool.force_refresh()
                    else:
                        proxy = await setting.get_proxy_async("service")
                    
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

                        # 内层403重试：仅当有代理池时触发
                        if response.status_code == 403 and proxy_pool._enabled:
                            retry_403_count += 1
                            
                            if retry_403_count <= max_403_retries:
                                logger.warning(f"[PostCreate] 遇到403错误，正在重试 ({retry_403_count}/{max_403_retries})...")
                                await asyncio.sleep(0.5)
                                continue
                            
                            # 内层重试全部失败
                            logger.error(f"[PostCreate] 403错误，已重试{retry_403_count-1}次，放弃")
                        
                        # 检查可配置状态码错误 - 外层重试
                        if response.status_code in retry_codes:
                            if outer_retry < MAX_OUTER_RETRY:
                                delay = (outer_retry + 1) * 0.1  # 渐进延迟：0.1s, 0.2s, 0.3s
                                logger.warning(f"[PostCreate] 遇到{response.status_code}错误，外层重试 ({outer_retry+1}/{MAX_OUTER_RETRY})，等待{delay}s...")
                                await asyncio.sleep(delay)
                                break  # 跳出内层循环，进入外层重试
                            else:
                                logger.error(f"[PostCreate] {response.status_code}错误，已重试{outer_retry}次，放弃")
                                raise GrokApiException(f"创建失败: {response.status_code}错误", "CREATE_ERROR")

                        if response.status_code == 200:
                            result = response.json()
                            post_id = result.get("post", {}).get("id", "")
                            
                            if outer_retry > 0 or retry_403_count > 0:
                                logger.info(f"[PostCreate] 重试成功！")
                            
                            logger.debug(f"[PostCreate] 成功，会话ID: {post_id}")
                            return {
                                "post_id": post_id,
                                "file_id": file_id,
                                "file_uri": file_uri,
                                "success": True,
                                "data": result
                            }
                        
                        # 其他错误处理
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
