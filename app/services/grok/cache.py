"""缓存服务模块 - 提供图片和视频的下载、缓存和清理功能"""

import asyncio
import base64
from pathlib import Path
from typing import Optional, Tuple
from curl_cffi.requests import AsyncSession

from app.core.config import setting
from app.core.logger import logger
from app.services.grok.statsig import get_dynamic_headers


# 常量定义
MIME_TYPES = {
    '.jpg': 'image/jpeg', '.jpeg': 'image/jpeg', '.png': 'image/png',
    '.gif': 'image/gif', '.webp': 'image/webp', '.bmp': 'image/bmp',
}
DEFAULT_MIME = 'image/jpeg'
ASSETS_URL = "https://assets.grok.com"


class CacheService:
    """缓存服务基类"""

    def __init__(self, cache_type: str, timeout: float = 30.0):
        self.cache_type = cache_type
        self.cache_dir = Path(f"data/temp/{cache_type}")
        self.cache_dir.mkdir(parents=True, exist_ok=True)
        self.timeout = timeout
        self._cleanup_lock = asyncio.Lock()

    def _get_path(self, file_path: str) -> Path:
        """转换文件路径为缓存路径"""
        return self.cache_dir / file_path.lstrip('/').replace('/', '-')

    def _log(self, level: str, msg: str):
        """统一日志输出"""
        getattr(logger, level)(f"[{self.cache_type.upper()}Cache] {msg}")

    def _build_headers(self, file_path: str, auth_token: str) -> dict:
        """构建请求头"""
        cf = setting.grok_config.get("cf_clearance", "")
        return {
            **get_dynamic_headers(pathname=file_path),
            "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
            "Sec-Fetch-Dest": "document",
            "Sec-Fetch-Mode": "navigate",
            "Sec-Fetch-Site": "same-site",
            "Sec-Fetch-User": "?1",
            "Upgrade-Insecure-Requests": "1",
            "Referer": "https://grok.com/",
            "Cookie": f"{auth_token};{cf}" if cf else auth_token
        }

    async def download(self, file_path: str, auth_token: str, timeout: Optional[float] = None) -> Optional[Path]:
        """下载并缓存文件"""
        cache_path = self._get_path(file_path)
        if cache_path.exists():
            self._log("debug", "文件已缓存")
            return cache_path

        # 外层重试：可配置状态码（401/429等）
        retry_codes = setting.grok_config.get("retry_status_codes", [401, 429])
        MAX_OUTER_RETRY = 3
        
        for outer_retry in range(MAX_OUTER_RETRY + 1):  # +1 确保实际重试3次
            try:
                # 内层重试：403代理池重试（cache使用缓存代理，不支持代理池，所以403只重试一次）
                max_403_retries = 5
                retry_403_count = 0
                
                while retry_403_count <= max_403_retries:
                    proxy = await setting.get_proxy_async("cache")
                    proxies = {"http": proxy, "https": proxy} if proxy else {}
                    
                    if proxy and outer_retry == 0 and retry_403_count == 0:
                        self._log("debug", f"使用代理: {proxy.split('@')[-1] if '@' in proxy else proxy}")

                    async with AsyncSession() as session:
                        url = f"{ASSETS_URL}{file_path}"
                        if outer_retry == 0 and retry_403_count == 0:
                            self._log("debug", f"下载: {url}")
                        
                        response = await session.get(
                            url,
                            headers=self._build_headers(file_path, auth_token),
                            proxies=proxies,
                            timeout=timeout or self.timeout,
                            allow_redirects=True,
                            impersonate="chrome133a"
                        )
                        
                        # 检查403错误 - 内层重试(cache不使用代理池，所以直接失败)
                        if response.status_code == 403:
                            retry_403_count += 1
                            
                            if retry_403_count <= max_403_retries:
                                self._log("warning", f"遇到403错误，正在重试 ({retry_403_count}/{max_403_retries})...")
                                await asyncio.sleep(0.5)
                                continue
                            
                            self._log("error", f"403错误，已重试{retry_403_count-1}次，放弃")
                            return None
                        
                        # 检查可配置状态码错误 - 外层重试
                        if response.status_code in retry_codes:
                            if outer_retry < MAX_OUTER_RETRY:
                                delay = (outer_retry + 1) * 0.1  # 渐进延迟：0.1s, 0.2s, 0.3s
                                self._log("warning", f"遇到{response.status_code}错误，外层重试 ({outer_retry+1}/{MAX_OUTER_RETRY})，等待{delay}s...")
                                await asyncio.sleep(delay)
                                break  # 跳出内层循环，进入外层重试
                            else:
                                self._log("error", f"{response.status_code}错误，已重试{outer_retry}次，放弃")
                                return None
                        
                        response.raise_for_status()
                        
                        cache_path.write_bytes(response.content)
                        
                        if outer_retry > 0 or retry_403_count > 0:
                            self._log("info", f"重试成功！")
                        else:
                            self._log("debug", "缓存成功")
                        
                        # 异步清理（带错误处理）
                        asyncio.create_task(self._safe_cleanup())
                        return cache_path
                        
            except Exception as e:
                if outer_retry < MAX_OUTER_RETRY - 1:
                    self._log("warning", f"下载异常: {e}，外层重试 ({outer_retry+1}/{MAX_OUTER_RETRY})...")
                    await asyncio.sleep(0.5)
                    continue
                
                self._log("error", f"下载失败: {e}（已重试{outer_retry}次）")
                return None
        
        return None

    def get_cached(self, file_path: str) -> Optional[Path]:
        """获取已缓存的文件"""
        path = self._get_path(file_path)
        return path if path.exists() else None

    async def _safe_cleanup(self):
        """安全清理（捕获异常）"""
        try:
            await self.cleanup()
        except Exception as e:
            self._log("error", f"后台清理失败: {e}")

    async def cleanup(self):
        """清理超限缓存"""
        if self._cleanup_lock.locked():
            return
        
        async with self._cleanup_lock:
            try:
                max_mb = setting.global_config.get(f"{self.cache_type}_cache_max_size_mb", 500)
                max_bytes = max_mb * 1024 * 1024

                # 获取文件信息 (path, size, mtime)
                files = [(f, (s := f.stat()).st_size, s.st_mtime) 
                        for f in self.cache_dir.glob("*") if f.is_file()]
                total = sum(size for _, size, _ in files)

                if total <= max_bytes:
                    return

                self._log("info", f"清理缓存 {total/1024/1024:.1f}MB -> {max_mb}MB")
                
                # 删除最旧的文件
                for path, size, _ in sorted(files, key=lambda x: x[2]):
                    if total <= max_bytes:
                        break
                    path.unlink()
                    total -= size
                
                self._log("info", f"清理完成: {total/1024/1024:.1f}MB")
            except Exception as e:
                self._log("error", f"清理失败: {e}")


class ImageCache(CacheService):
    """图片缓存服务"""

    def __init__(self):
        super().__init__("image", timeout=30.0)

    async def download_image(self, path: str, token: str) -> Optional[Path]:
        """下载图片"""
        return await self.download(path, token)

    @staticmethod
    def to_base64(image_path: Path) -> Optional[str]:
        """图片转base64"""
        try:
            if not image_path.exists():
                logger.error(f"[ImageCache] 文件不存在: {image_path}")
                return None

            data = base64.b64encode(image_path.read_bytes()).decode()
            mime = MIME_TYPES.get(image_path.suffix.lower(), DEFAULT_MIME)
            return f"data:{mime};base64,{data}"
        except Exception as e:
            logger.error(f"[ImageCache] 转换失败: {e}")
            return None

    async def download_base64(self, path: str, token: str) -> Optional[str]:
        """下载并转为base64（自动删除临时文件）"""
        try:
            cache_path = await self.download(path, token)
            if not cache_path:
                return None

            result = self.to_base64(cache_path)
            
            # 清理临时文件
            try:
                cache_path.unlink()
            except Exception as e:
                logger.warning(f"[ImageCache] 删除临时文件失败: {e}")

            return result
        except Exception as e:
            logger.error(f"[ImageCache] 下载base64失败: {e}")
            return None


class VideoCache(CacheService):
    """视频缓存服务"""

    def __init__(self):
        super().__init__("video", timeout=60.0)

    async def download_video(self, path: str, token: str) -> Optional[Path]:
        """下载视频"""
        return await self.download(path, token)


# 全局实例
image_cache_service = ImageCache()
video_cache_service = VideoCache()
