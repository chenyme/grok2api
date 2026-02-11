"""
Grok 文件资产服务
"""

import asyncio
import base64
import hashlib
import os
import re
import time
from contextlib import asynccontextmanager
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Dict, List, Optional, Tuple
from urllib.parse import urlparse

try:
    import fcntl
except ImportError:
    fcntl = None

import aiofiles
from curl_cffi.requests import AsyncSession

from app.core.config import get_config
from app.core.exceptions import AppException, UpstreamException, ValidationException
from app.core.logger import logger
from app.core.storage import DATA_DIR
from app.services.grok.utils.headers import apply_statsig, build_sso_cookie
from app.services.token.service import TokenService

# ==================== 常量 ====================

UPLOAD_API = "https://grok.com/rest/app-chat/upload-file"
LIST_API = "https://grok.com/rest/assets"
DELETE_API = "https://grok.com/rest/assets-metadata"
DOWNLOAD_API = "https://assets.grok.com"
LOCK_DIR = DATA_DIR / ".locks"

# 全局信号量（运行时动态初始化）
_ASSETS_SEMAPHORE = None
_ASSETS_SEM_VALUE = None

# 常用 MIME 类型（业务数据，非配置）
MIME_TYPES = {
    ".jpg": "image/jpeg",
    ".jpeg": "image/jpeg",
    ".png": "image/png",
    ".gif": "image/gif",
    ".webp": "image/webp",
    ".bmp": "image/bmp",
    ".pdf": "application/pdf",
    ".txt": "text/plain",
    ".md": "text/markdown",
    ".csv": "text/csv",
    ".json": "application/json",
    ".xml": "application/xml",
    ".py": "text/x-python-script",
    ".js": "application/javascript",
    ".html": "text/html",
    ".css": "text/css",
    ".mp4": "video/mp4",
    ".webm": "video/webm",
}

IMAGE_EXTS = {".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp"}
VIDEO_EXTS = {".mp4", ".mov", ".m4v", ".webm", ".avi", ".mkv"}

# ==================== 工具函数 ====================


def _get_assets_semaphore() -> asyncio.Semaphore:
    """获取全局并发控制信号量"""
    value = max(1, int(get_config("performance.assets_max_concurrent")))

    global _ASSETS_SEMAPHORE, _ASSETS_SEM_VALUE
    if _ASSETS_SEMAPHORE is None or value != _ASSETS_SEM_VALUE:
        _ASSETS_SEM_VALUE = value
        _ASSETS_SEMAPHORE = asyncio.Semaphore(value)
    return _ASSETS_SEMAPHORE


@asynccontextmanager
async def _file_lock(name: str, timeout: int = 10):
    """文件锁"""
    if fcntl is None:
        yield
        return

    LOCK_DIR.mkdir(parents=True, exist_ok=True)
    lock_path = LOCK_DIR / f"{name}.lock"
    fd = None
    locked = False
    start = time.monotonic()

    try:
        fd = open(lock_path, "a+")
        while True:
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                locked = True
                break
            except BlockingIOError:
                if time.monotonic() - start >= timeout:
                    break
                await asyncio.sleep(0.05)
        yield
    finally:
        if fd:
            if locked:
                try:
                    fcntl.flock(fd, fcntl.LOCK_UN)
                except Exception:
                    pass
            fd.close()


@dataclass
class ServiceConfig:
    """服务配置"""

    proxy: str
    timeout: int
    browser: str
    user_agent: str

    @classmethod
    def from_settings(cls, proxy: Optional[str] = None):
        return cls(
            proxy=proxy
            or get_config("network.asset_proxy_url")
            or get_config("network.base_proxy_url"),
            timeout=get_config("network.timeout"),
            browser=get_config("security.browser"),
            user_agent=get_config("security.user_agent"),
        )

    def get_proxies(self) -> Optional[dict]:
        return {"http": self.proxy, "https": self.proxy} if self.proxy else None


# ==================== 基础服务 ====================


class BaseService:
    """基础服务类"""

    def __init__(self, proxy: Optional[str] = None):
        self.config = ServiceConfig.from_settings(proxy)
        self._session: Optional[AsyncSession] = None

    def _build_headers(
        self, token: str, referer: str = "https://grok.com/", download: bool = False
    ) -> dict:
        """构建请求头"""
        if download:
            headers = {
                "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7",
                "Sec-Fetch-Dest": "document",
                "Sec-Fetch-Mode": "navigate",
                "Sec-Fetch-Site": "same-site",
                "Sec-Fetch-User": "?1",
                "Referer": referer,
                "User-Agent": self.config.user_agent,
            }
        else:
            headers = {
                "Accept": "*/*",
                "Content-Type": "application/json",
                "Origin": "https://grok.com",
                "Referer": referer,
                "User-Agent": self.config.user_agent,
            }
            apply_statsig(headers)

        headers["Cookie"] = build_sso_cookie(token)
        return headers

    async def _get_session(self) -> AsyncSession:
        """获取复用 Session"""
        if self._session is None:
            self._session = AsyncSession()
        return self._session

    async def close(self):
        """关闭 Session"""
        if self._session:
            await self._session.close()
            self._session = None

    @staticmethod
    def is_url(s: str) -> bool:
        """检查是否为 URL"""
        try:
            r = urlparse(s)
            return bool(r.scheme and r.netloc and r.scheme in ["http", "https"])
        except Exception:
            return False

    @staticmethod
    async def fetch(url: str) -> Tuple[str, str, str]:
        """获取远程资源并转 Base64"""
        try:
            async with AsyncSession() as session:
                response = await session.get(url, timeout=10)
                if response.status_code >= 400:
                    raise UpstreamException(
                        message=f"Failed to fetch: {response.status_code}",
                        details={"url": url, "status": response.status_code},
                    )

                filename = url.split("/")[-1].split("?")[0] or "download"
                content_type = response.headers.get(
                    "content-type", "application/octet-stream"
                ).split(";")[0]
                b64 = base64.b64encode(response.content).decode()

                logger.debug(f"Fetched: {url}")
                return filename, b64, content_type
        except Exception as e:
            if isinstance(e, AppException):
                raise
            logger.error(f"Fetch failed: {url} - {e}")
            raise UpstreamException(f"Fetch failed: {str(e)}", details={"url": url})

    @staticmethod
    def parse_b64(data_uri: str) -> Tuple[str, str, str]:
        """解析 Base64 数据"""
        if not data_uri.startswith("data:"):
            return "file.bin", data_uri, "application/octet-stream"

        try:
            header, b64 = data_uri.split(",", 1)
        except ValueError:
            return "file.bin", data_uri, "application/octet-stream"

        if ";base64" not in header:
            return "file.bin", data_uri, "application/octet-stream"

        mime = header[5:].split(";", 1)[0] or "application/octet-stream"
        b64 = re.sub(r"\s+", "", b64)
        ext = mime.split("/")[-1] if "/" in mime else "bin"
        return f"file.{ext}", b64, mime

    @staticmethod
    def to_b64(file_path: Path, mime_type: str) -> str:
        """文件转 base64 data URI"""
        try:
            if not file_path.exists():
                logger.warning(f"File not found for base64 conversion: {file_path}")
                raise AppException(
                    f"File not found: {file_path}", code="file_not_found"
                )

            if not file_path.is_file():
                logger.warning(f"Path is not a file: {file_path}")
                raise AppException(
                    f"Invalid file path: {file_path}", code="invalid_file_path"
                )

            b64_data = base64.b64encode(file_path.read_bytes()).decode()
            return f"data:{mime_type};base64,{b64_data}"
        except AppException:
            raise
        except Exception as e:
            logger.error(f"File to base64 failed: {file_path} - {e}")
            raise AppException(
                f"Failed to read file: {file_path}", code="file_read_error"
            )


# ==================== 上传服务 ====================


class UploadService(BaseService):
    """文件上传服务"""

    async def upload(self, file_input: str, token: str) -> Tuple[str, str]:
        """
        上传文件到 Grok

        Returns:
            (file_id, file_uri)
        """
        async with _get_assets_semaphore():
            # 处理输入
            if self.is_url(file_input):
                filename, b64, mime = await self.fetch(file_input)
            else:
                filename, b64, mime = self.parse_b64(file_input)

            logger.debug(
                f"Upload prepare: filename={filename}, type={mime}, size={len(b64)}"
            )

            if not b64:
                raise ValidationException("Invalid file input: empty content")

            # 执行上传
            session = await self._get_session()
            response = await session.post(
                UPLOAD_API,
                headers=self._build_headers(token),
                json={"fileName": filename, "fileMimeType": mime, "content": b64},
                impersonate=self.config.browser,
                timeout=self.config.timeout,
                proxies=self.config.get_proxies(),
            )

            # 处理响应
            if response.status_code == 200:
                result = response.json()
                file_id = result.get("fileMetadataId", "")
                file_uri = result.get("fileUri", "")
                logger.info(f"Upload success: {filename} -> {file_id}")
                return file_id, file_uri

            # 认证失败
            if response.status_code in (401, 403):
                logger.warning(f"Upload auth failed: {response.status_code}")
                try:
                    await TokenService.record_fail(
                        token, response.status_code, "upload_auth_failed"
                    )
                except Exception as e:
                    logger.error(f"Failed to record token failure: {e}")

                raise UpstreamException(
                    message=f"Upload authentication failed: {response.status_code}",
                    details={"status": response.status_code, "token_invalidated": True},
                )

            # 其他错误
            logger.error(f"Upload failed: {filename} - {response.status_code}")
            raise UpstreamException(
                message=f"Upload failed: {response.status_code}",
                details={"status": response.status_code},
            )


# ==================== 列表服务 ====================


class ListService(BaseService):
    """文件列表查询服务"""

    async def iter_assets(self, token: str):
        """分页迭代资产列表"""
        headers = self._build_headers(token, referer="https://grok.com/files")
        params = {
            "pageSize": 50,
            "orderBy": "ORDER_BY_LAST_USE_TIME",
            "source": "SOURCE_ANY",
            "isLatest": "true",
        }
        page_token = None
        seen_tokens = set()

        async with AsyncSession() as session:
            while True:
                if page_token:
                    if page_token in seen_tokens:
                        logger.warning("Pagination stopped: repeated page token")
                        break
                    seen_tokens.add(page_token)
                    params["pageToken"] = page_token
                else:
                    params.pop("pageToken", None)

                response = await session.get(
                    LIST_API,
                    headers=headers,
                    params=params,
                    impersonate=self.config.browser,
                    timeout=self.config.timeout,
                    proxies=self.config.get_proxies(),
                )

                if response.status_code != 200:
                    raise UpstreamException(
                        message=f"List failed: {response.status_code}",
                        details={"status": response.status_code},
                    )

                result = response.json()
                page_assets = result.get("assets", [])
                yield page_assets

                page_token = result.get("nextPageToken")
                if not page_token:
                    break

    async def list(self, token: str) -> List[Dict]:
        """查询文件列表"""
        assets = []
        async for page_assets in self.iter_assets(token):
            assets.extend(page_assets)
        logger.info(f"List success: {len(assets)} files")
        return assets

    async def count(self, token: str) -> int:
        """统计资产数量"""
        total = 0
        async for page_assets in self.iter_assets(token):
            total += len(page_assets)
        logger.debug(f"Asset count: {total}")
        return total


# ==================== 删除服务 ====================


class DeleteService(BaseService):
    """文件删除服务"""

    async def delete(self, token: str, asset_id: str) -> bool:
        """删除单个文件"""
        async with _get_assets_semaphore():
            session = await self._get_session()
            response = await session.delete(
                f"{DELETE_API}/{asset_id}",
                headers=self._build_headers(token, referer="https://grok.com/files"),
                impersonate=self.config.browser,
                timeout=self.config.timeout,
                proxies=self.config.get_proxies(),
            )

            if response.status_code == 200:
                logger.debug(f"Deleted: {asset_id}")
                return True

            logger.error(f"Delete failed: {asset_id} - {response.status_code}")
            raise UpstreamException(
                message=f"Delete failed: {asset_id}",
                details={"status": response.status_code},
            )

    async def delete_all(self, token: str) -> Dict[str, int]:
        """删除所有文件"""
        total = success = failed = 0
        list_service = ListService(self.config.proxy)

        try:
            async for assets in list_service.iter_assets(token):
                if not assets:
                    continue

                total += len(assets)
                batch_result = await self._delete_batch(token, assets)
                success += batch_result["success"]
                failed += batch_result["failed"]

            if total == 0:
                logger.info("No assets to delete")
                return {"total": 0, "success": 0, "failed": 0, "skipped": True}
        finally:
            await list_service.close()

        logger.info(f"Delete all: total={total}, success={success}, failed={failed}")
        return {"total": total, "success": success, "failed": failed}

    async def _delete_batch(self, token: str, assets: List[Dict]) -> Dict[str, int]:
        """批量删除"""
        batch_size = max(1, int(get_config("performance.assets_delete_batch_size")))
        success = failed = 0

        for i in range(0, len(assets), batch_size):
            batch = assets[i : i + batch_size]
            results = await asyncio.gather(
                *[
                    self._delete_one(token, asset, idx)
                    for idx, asset in enumerate(batch)
                ],
                return_exceptions=True,
            )
            success += sum(1 for r in results if r is True)
            failed += sum(1 for r in results if r is not True)

        return {"success": success, "failed": failed}

    async def _delete_one(self, token: str, asset: Dict, index: int) -> bool:
        """删除单个资产（带延迟）"""
        await asyncio.sleep(0.01 * index)
        asset_id = asset.get("assetId", "")
        if not asset_id:
            return False
        try:
            return await self.delete(token, asset_id)
        except Exception:
            return False


# ==================== 下载服务 ====================


class DownloadService(BaseService):
    """文件下载服务"""

    def __init__(self, proxy: Optional[str] = None):
        super().__init__(proxy)
        self.base_dir = DATA_DIR / "tmp"
        self.image_dir = self.base_dir / "image"
        self.video_dir = self.base_dir / "video"
        self.image_dir.mkdir(parents=True, exist_ok=True)
        self.video_dir.mkdir(parents=True, exist_ok=True)
        self._cleanup_running = False

    def _cache_path(self, file_path: str, media_type: str) -> Path:
        """获取缓存路径"""
        cache_dir = self.image_dir if media_type == "image" else self.video_dir
        filename = file_path.lstrip("/").replace("/", "-")
        return cache_dir / filename

    def _get_mime(self, cache_path: Path, response=None) -> str:
        """获取 MIME 类型"""
        if response:
            return response.headers.get(
                "content-type", "application/octet-stream"
            ).split(";")[0]
        return MIME_TYPES.get(cache_path.suffix.lower(), "application/octet-stream")

    async def download(
        self, file_path: str, token: str, media_type: str = "image"
    ) -> Tuple[Optional[Path], str]:
        """下载文件到本地"""
        async with _get_assets_semaphore():
            cache_path = self._cache_path(file_path, media_type)

            # 检查缓存
            if cache_path.exists():
                logger.debug(f"Cache hit: {cache_path}")
                return cache_path, self._get_mime(cache_path)

            # 文件锁防止并发下载
            lock_name = f"dl_{media_type}_{hashlib.sha1(str(cache_path).encode()).hexdigest()[:16]}"
            async with _file_lock(lock_name, timeout=10):
                # 双重检查
                if cache_path.exists():
                    return cache_path, self._get_mime(cache_path)

                # 执行下载
                mime = await self._download_file(file_path, token, cache_path)
                logger.info(f"Downloaded: {file_path}")

                # 异步检查缓存限制
                asyncio.create_task(self.check_limit())

                return cache_path, mime

    async def _download_file(self, file_path: str, token: str, cache_path: Path) -> str:
        """执行下载"""
        if not file_path.startswith("/"):
            file_path = f"/{file_path}"

        url = f"{DOWNLOAD_API}{file_path}"
        headers = self._build_headers(token, download=True)

        session = await self._get_session()
        response = await session.get(
            url,
            headers=headers,
            proxies=self.config.get_proxies(),
            timeout=self.config.timeout,
            allow_redirects=True,
            impersonate=self.config.browser,
            stream=True,
        )

        if response.status_code != 200:
            raise UpstreamException(
                message=f"Download failed: {response.status_code}",
                details={"path": file_path, "status": response.status_code},
            )

        # 保存文件
        tmp_path = cache_path.with_suffix(cache_path.suffix + ".tmp")
        try:
            async with aiofiles.open(tmp_path, "wb") as f:
                # 尝试流式写入
                if hasattr(response, "aiter_content"):
                    async for chunk in response.aiter_content():
                        if chunk:
                            await f.write(chunk)
                else:
                    await f.write(response.content)
            os.replace(tmp_path, cache_path)
        finally:
            if tmp_path.exists() and not cache_path.exists():
                try:
                    tmp_path.unlink()
                except Exception:
                    pass

        return self._get_mime(cache_path, response)

    async def to_base64(
        self, file_path: str, token: str, media_type: str = "image"
    ) -> str:
        """下载并转 base64"""
        try:
            cache_path, mime = await self.download(file_path, token, media_type)
            if not cache_path or not cache_path.exists():
                logger.warning(f"Download failed for {file_path}: invalid path")
                raise AppException(
                    "Download failed: invalid path", code="download_failed"
                )

            data_uri = self.to_b64(cache_path, mime)

            # 删除临时文件
            if data_uri:
                try:
                    cache_path.unlink()
                except Exception as e:
                    logger.debug(f"Failed to cleanup temp file {cache_path}: {e}")

            return data_uri
        except Exception as e:
            logger.error(f"Failed to convert {file_path} to base64: {e}")
            raise

    def get_stats(self, media_type: str = "image") -> Dict[str, Any]:
        """获取缓存统计"""
        cache_dir = self.image_dir if media_type == "image" else self.video_dir
        if not cache_dir.exists():
            return {"count": 0, "size_mb": 0.0}

        allowed = IMAGE_EXTS if media_type == "image" else VIDEO_EXTS
        files = [
            f
            for f in cache_dir.glob("*")
            if f.is_file() and f.suffix.lower() in allowed
        ]
        total_size = sum(f.stat().st_size for f in files)
        return {"count": len(files), "size_mb": round(total_size / 1024 / 1024, 2)}

    def list_files(
        self, media_type: str = "image", page: int = 1, page_size: int = 1000
    ) -> Dict[str, Any]:
        """列出缓存文件"""
        cache_dir = self.image_dir if media_type == "image" else self.video_dir
        if not cache_dir.exists():
            return {"total": 0, "page": page, "page_size": page_size, "items": []}

        allowed = IMAGE_EXTS if media_type == "image" else VIDEO_EXTS
        files = [
            f
            for f in cache_dir.glob("*")
            if f.is_file() and f.suffix.lower() in allowed
        ]

        # 构建文件列表
        items = []
        for f in files:
            try:
                stat = f.stat()
                items.append(
                    {
                        "name": f.name,
                        "size_bytes": stat.st_size,
                        "mtime_ms": int(stat.st_mtime * 1000),
                    }
                )
            except Exception:
                continue

        items.sort(key=lambda x: x["mtime_ms"], reverse=True)

        # 分页
        total = len(items)
        start = max(0, (page - 1) * page_size)
        paged = items[start : start + page_size]

        # 添加 URL
        for item in paged:
            item["view_url"] = f"/v1/files/{media_type}/{item['name']}"

        return {"total": total, "page": page, "page_size": page_size, "items": paged}

    def delete_file(self, media_type: str, name: str) -> Dict[str, Any]:
        """删除缓存文件"""
        cache_dir = self.image_dir if media_type == "image" else self.video_dir
        file_path = cache_dir / name.replace("/", "-")

        if file_path.exists():
            try:
                file_path.unlink()
                return {"deleted": True}
            except Exception:
                pass
        return {"deleted": False}

    def clear(self, media_type: str = "image") -> Dict[str, Any]:
        """清空缓存"""
        cache_dir = self.image_dir if media_type == "image" else self.video_dir
        if not cache_dir.exists():
            return {"count": 0, "size_mb": 0.0}

        files = list(cache_dir.glob("*"))
        total_size = sum(f.stat().st_size for f in files if f.is_file())
        count = 0

        for f in files:
            if f.is_file():
                try:
                    f.unlink()
                    count += 1
                except Exception:
                    pass

        return {"count": count, "size_mb": round(total_size / 1024 / 1024, 2)}

    async def check_limit(self):
        """检查并清理缓存"""
        if self._cleanup_running or not get_config("cache.enable_auto_clean"):
            return

        self._cleanup_running = True
        try:
            async with _file_lock("cache_cleanup", timeout=5):
                limit_mb = get_config("cache.limit_mb")
                all_files, total_size = self._collect_files()
                current_mb = total_size / 1024 / 1024

                if current_mb <= limit_mb:
                    return

                # 清理到 80%
                logger.info(
                    f"Cache limit exceeded ({current_mb:.2f}MB > {limit_mb}MB), cleaning..."
                )
                all_files.sort(key=lambda x: x[1])  # 按时间排序

                deleted_count = 0
                deleted_size = 0
                target_mb = limit_mb * 0.8

                for f, _, size in all_files:
                    try:
                        f.unlink()
                        deleted_count += 1
                        deleted_size += size
                        total_size -= size
                        if (total_size / 1024 / 1024) <= target_mb:
                            break
                    except Exception:
                        pass

                logger.info(
                    f"Cache cleanup: {deleted_count} files ({deleted_size / 1024 / 1024:.2f}MB)"
                )
        finally:
            self._cleanup_running = False

    def _collect_files(self) -> Tuple[List[Tuple[Path, float, int]], int]:
        """收集所有缓存文件"""
        total_size = 0
        all_files = []

        for d in [self.image_dir, self.video_dir]:
            if d.exists():
                for f in d.glob("*"):
                    if f.is_file():
                        try:
                            stat = f.stat()
                            total_size += stat.st_size
                            all_files.append((f, stat.st_mtime, stat.st_size))
                        except Exception:
                            pass

        return all_files, total_size


__all__ = [
    "BaseService",
    "UploadService",
    "ListService",
    "DeleteService",
    "DownloadService",
]
