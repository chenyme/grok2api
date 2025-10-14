"""缓存服务模块"""

import asyncio
import base64
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

    def _cache_path(self, file_path: str) -> Path:
        """获取缓存文件的完整路径"""
        filename = file_path.lstrip('/').replace('/', '-')
        return self.cache_dir / filename

    async def download_file(self, file_path: str, auth_token: str, timeout: float = 30.0) -> Optional[Path]:
        """下载并缓存文件"""
        cache_path = self._cache_path(file_path)
        if cache_path.exists():
            logger.debug(f"[{self.cache_type.upper()}Cache] 文件已缓存: {cache_path}")
            return cache_path

        try:
            cf_clearance = setting.grok_config.get("cf_clearance", "")
            headers = {
                **get_dynamic_headers(pathname=file_path),
                "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7",
                "Sec-Fetch-Dest": "document",
                "Sec-Fetch-Mode": "navigate",
                "Sec-Fetch-Site": "same-site",
                "Sec-Fetch-User": "?1",
                "Upgrade-Insecure-Requests": "1",
                "Referer": "https://grok.com/",
                "Cookie": f"{auth_token};{cf_clearance}" if cf_clearance else auth_token
            }

            proxy_url = setting.grok_config.get("proxy_url")
            proxies = {"http": proxy_url, "https": proxy_url} if proxy_url else {}

            async with AsyncSession() as session:
                logger.debug(f"[{self.cache_type.upper()}Cache] 开始下载: https://assets.grok.com{file_path}")
                response = await session.get(
                    f"https://assets.grok.com{file_path}",
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

    def get_cached(self, file_path: str) -> Optional[Path]:
        """获取缓存的文件路径"""
        cache_path = self._cache_path(file_path)
        return cache_path if cache_path.exists() else None

    async def cleanup_cache(self):
        """清理缓存目录，确保不超过配置的大小限制"""
        try:
            max_size_mb = setting.global_config.get(f"{self.cache_type}_cache_max_size_mb", 500)
            max_size_bytes = max_size_mb * 1024 * 1024

            files = [(fp, (stat := fp.stat()).st_size, stat.st_mtime)
                     for fp in self.cache_dir.glob("*") if fp.is_file()]
            total_size = sum(size for _, size, _ in files)

            if total_size <= max_size_bytes:
                logger.debug(f"[{self.cache_type.upper()}Cache] 缓存大小 {total_size / 1024 / 1024:.2f}MB，未超限")
                return

            logger.info(f"[{self.cache_type.upper()}Cache] 缓存大小 {total_size / 1024 / 1024:.2f}MB 超过限制 {max_size_mb}MB，开始清理")
            files.sort(key=lambda x: x[2])

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
        """下载并缓存图片"""
        return await self.download_file(image_path, auth_token, timeout=30.0)

    def get_cached(self, image_path: str) -> Optional[Path]:
        """获取缓存的图片路径"""
        return super().get_cached(image_path)

    @staticmethod
    def to_base64(image_path: Path) -> Optional[str]:
        """将图片转换为 base64 编码"""
        try:
            if not image_path.exists():
                logger.error(f"[ImageCache] 图片文件不存在: {image_path}")
                return None

            with open(image_path, "rb") as f:
                base64_data = base64.b64encode(f.read()).decode('utf-8')

            mime_type = {'.jpg': 'image/jpeg', '.jpeg': 'image/jpeg', '.png': 'image/png',
                         '.gif': 'image/gif', '.webp': 'image/webp'}.get(image_path.suffix.lower(), 'image/jpeg')

            return f"data:{mime_type};base64,{base64_data}"
        except Exception as e:
            logger.error(f"[ImageCache] 图片转 base64 失败: {e}")
            return None

    async def download_base64(self, image_path: str, auth_token: str) -> Optional[str]:
        """下载图片并转换为 base64 编码（转换后立即删除缓存文件）"""
        try:
            cache_path = await self.download_file(image_path, auth_token, timeout=30.0)
            if not cache_path:
                return None

            base64_str = self.to_base64(cache_path)

            try:
                cache_path.unlink()
                logger.debug(f"[ImageCache] 已删除临时文件: {cache_path}")
            except Exception as e:
                logger.warning(f"[ImageCache] 删除临时文件失败: {e}")

            return base64_str
        except Exception as e:
            logger.error(f"[ImageCache] 下载并转换 base64 失败: {e}")
            return None


class VideoCacheService(CacheService):
    """视频缓存服务"""

    def __init__(self):
        super().__init__("video")

    async def download_video(self, video_path: str, auth_token: str) -> Optional[Path]:
        """下载并缓存视频"""
        return await self.download_file(video_path, auth_token, timeout=60.0)

    def get_cached(self, video_path: str) -> Optional[Path]:
        """获取缓存的视频路径"""
        return super().get_cached(video_path)


# 全局实例
image_cache_service = ImageCacheService()
video_cache_service = VideoCacheService()
