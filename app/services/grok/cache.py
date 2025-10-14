"""缓存服务模块"""

import asyncio
from pathlib import Path
from typing import Optional
from curl_cffi.requests import AsyncSession

from app.core.config import setting
from app.core.logger import logger
from app.services.grok.statsig import get_dynamic_headers


class CacheService:
    """缓存服务基类"""

    def __init__(self, cache_type: str):
        """初始化缓存服务"""
        self.cache_type = cache_type
        self.cache_dir = Path(f"data/temp/{cache_type}")
        self.cache_dir.mkdir(parents=True, exist_ok=True)

    @staticmethod
    def _get_cache_filename(file_path: str) -> str:
        """将文件路径转换为缓存文件名"""
        filename = file_path.lstrip('/').replace('/', '-')
        return filename

    def _get_cache_path(self, file_path: str) -> Path:
        """获取缓存文件的完整路径"""
        filename = self._get_cache_filename(file_path)
        return self.cache_dir / filename

    async def download_file(self, file_path: str, auth_token: str, timeout: float = 30.0) -> Optional[Path]:
        """下载并缓存文件

        Args:
            file_path: 文件路径，如 /users/xxx/generated/xxx/file.jpg
            auth_token: 认证令牌
            timeout: 下载超时时间（秒）

        Returns:
            缓存文件路径，下载失败返回 None
        """
        cache_path = self._get_cache_path(file_path)

        if cache_path.exists():
            logger.debug(f"[{self.cache_type.upper()}Cache] 文件已缓存: {cache_path}")
            return cache_path

        file_url = f"https://assets.grok.com{file_path}"

        try:
            # 构建 Cookie
            cf_clearance = setting.grok_config.get("cf_clearance", "")
            cookie = f"{auth_token};{cf_clearance}" if cf_clearance else auth_token

            # 构建请求头
            headers = {
                **get_dynamic_headers(pathname=file_path),
                "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7",
                "Sec-Fetch-Dest": "document",
                "Sec-Fetch-Mode": "navigate",
                "Sec-Fetch-Site": "same-site",
                "Sec-Fetch-User": "?1",
                "Upgrade-Insecure-Requests": "1",
                "Referer": "https://grok.com/",
                "Cookie": cookie
            }

            # 代理配置
            proxy_url = setting.grok_config.get("proxy_url")
            proxies = {"http": proxy_url, "https": proxy_url} if proxy_url else {}

            async with AsyncSession() as session:
                logger.debug(f"[{self.cache_type.upper()}Cache] 开始下载: {file_url}")
                response = await session.get(
                    file_url,
                    headers=headers,
                    proxies=proxies,
                    timeout=timeout,
                    allow_redirects=True,
                    impersonate="chrome133a"
                )
                response.raise_for_status()

                cache_path.write_bytes(response.content)
                logger.debug(f"[{self.cache_type.upper()}Cache] 文件已缓存: {cache_path} ({len(response.content)} bytes)")

                asyncio.create_task(self.cleanup_cache())

                return cache_path

        except Exception as e:
            logger.error(f"[{self.cache_type.upper()}Cache] 下载文件失败: {e}")
            return None

    def get_cached_file(self, file_path: str) -> Optional[Path]:
        """获取缓存的文件路径

        Args:
            file_path: 文件路径

        Returns:
            缓存文件路径，不存在返回 None
        """
        cache_path = self._get_cache_path(file_path)
        return cache_path if cache_path.exists() else None

    async def cleanup_cache(self):
        """清理缓存目录，确保不超过配置的大小限制"""
        try:
            # 获取配置的最大缓存大小
            config_key = f"{self.cache_type}_cache_max_size_mb"
            max_size_mb = setting.global_config.get(config_key, 500)
            max_size_bytes = max_size_mb * 1024 * 1024

            # 获取缓存大小和修改时间
            files = []
            total_size = 0

            for file_path in self.cache_dir.glob("*"):
                if file_path.is_file():
                    size = file_path.stat().st_size
                    mtime = file_path.stat().st_mtime
                    files.append((file_path, size, mtime))
                    total_size += size

            # 如果总大小未超限，无需清理
            if total_size <= max_size_bytes:
                logger.debug(f"[{self.cache_type.upper()}Cache] 缓存大小 {total_size / 1024 / 1024:.2f}MB，未超限")
                return

            logger.info(f"[{self.cache_type.upper()}Cache] 缓存大小 {total_size / 1024 / 1024:.2f}MB 超过限制 {max_size_mb}MB，开始清理")

            # 按修改时间排序
            files.sort(key=lambda x: x[2])

            # 删除文件直到总大小低于限制
            for file_path, size, _ in files:
                if total_size <= max_size_bytes:
                    break

                file_path.unlink()
                total_size -= size
                logger.debug(f"[{self.cache_type.upper()}Cache] 已删除缓存文件: {file_path}")

            logger.info(f"[{self.cache_type.upper()}Cache] 缓存清理完成，当前大小 {total_size / 1024 / 1024:.2f}MB")

        except Exception as e:
            logger.error(f"[{self.cache_type.upper()}Cache] 清理缓存失败: {e}")


class ImageCacheService(CacheService):
    """图片缓存服务"""

    def __init__(self):
        super().__init__("image")

    async def download_image(self, image_path: str, auth_token: str) -> Optional[Path]:
        """下载并缓存图片

        Args:
            image_path: 图片路径，如 /users/xxx/generated/xxx/image.jpg
            auth_token: 认证令牌

        Returns:
            缓存文件路径，下载失败返回 None`
        """
        return await self.download_file(image_path, auth_token, timeout=30.0)

    def get_cached_image(self, image_path: str) -> Optional[Path]:
        """获取缓存的图片路径

        Args:
            image_path: 图片路径

        Returns:
            缓存文件路径，不存在返回 None
        """
        return self.get_cached_file(image_path)


class VideoCacheService(CacheService):
    """视频缓存服务"""

    def __init__(self):
        super().__init__("video")

    async def download_video(self, video_path: str, auth_token: str) -> Optional[Path]:
        """下载并缓存视频

        Args:
            video_path: 视频路径，如 /users/xxx/generated/xxx/video.mp4
            auth_token: 认证令牌

        Returns:
            缓存文件路径，下载失败返回 None
        """
        return await self.download_file(video_path, auth_token, timeout=60.0)

    def get_cached_video(self, video_path: str) -> Optional[Path]:
        """获取缓存的视频路径

        Args:
            video_path: 视频路径

        Returns:
            缓存文件路径，不存在返回 None
        """
        return self.get_cached_file(video_path)


# 全局实例
image_cache_service = ImageCacheService()
video_cache_service = VideoCacheService()

