"""
Grok 文件资产服务
"""

import asyncio
import base64
import os
import time
import hashlib
import re
from pathlib import Path
from contextlib import asynccontextmanager

try:
    import fcntl
except ImportError:  # pragma: no cover - non-posix platforms
    fcntl = None
from typing import Tuple, List, Dict, Optional, Any
from urllib.parse import urlparse

import aiofiles
from curl_cffi.requests import AsyncSession

from app.core.logger import logger
from app.core.config import get_config
from app.core.exceptions import AppException, UpstreamException, ValidationException
from app.services.grok.utils.headers import apply_statsig, build_sso_cookie
from app.services.token.service import TokenService


# ==================== 常量 ====================

UPLOAD_API = "https://grok.com/rest/app-chat/upload-file"
LIST_API = "https://grok.com/rest/assets"
DELETE_API = "https://grok.com/rest/assets-metadata"
DOWNLOAD_API = "https://assets.grok.com"
LOCK_DIR = Path(__file__).parent.parent.parent.parent / "data" / ".locks"

TIMEOUT = 120
BROWSER = "chrome136"
DEFAULT_MIME = "application/octet-stream"

# 并发控制
DEFAULT_MAX_CONCURRENT = 25
DEFAULT_DELETE_BATCH_SIZE = 10
_ASSETS_SEMAPHORE = asyncio.Semaphore(DEFAULT_MAX_CONCURRENT)
_ASSETS_SEM_VALUE = DEFAULT_MAX_CONCURRENT


def _get_assets_semaphore() -> asyncio.Semaphore:
    global _ASSETS_SEMAPHORE, _ASSETS_SEM_VALUE
    value = get_config("performance.assets_max_concurrent", DEFAULT_MAX_CONCURRENT)
    try:
        value = int(value)
    except Exception:
        value = DEFAULT_MAX_CONCURRENT
    value = max(1, value)
    if value != _ASSETS_SEM_VALUE:
        _ASSETS_SEM_VALUE = value
        _ASSETS_SEMAPHORE = asyncio.Semaphore(value)
    return _ASSETS_SEMAPHORE


def _get_delete_batch_size() -> int:
    value = get_config(
        "performance.assets_delete_batch_size", DEFAULT_DELETE_BATCH_SIZE
    )
    try:
        value = int(value)
    except Exception:
        value = DEFAULT_DELETE_BATCH_SIZE
    return max(1, value)


@asynccontextmanager
async def _file_lock(name: str, timeout: int = 10):
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
            try:
                fd.close()
            except Exception:
                pass


MIME_TYPES = {
    # 图片
    ".jpg": "image/jpeg",
    ".jpeg": "image/jpeg",
    ".png": "image/png",
    ".gif": "image/gif",
    ".webp": "image/webp",
    ".bmp": "image/bmp",
    # 文档
    ".pdf": "application/pdf",
    ".txt": "text/plain",
    ".md": "text/markdown",
    ".doc": "application/msword",
    ".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
    ".rtf": "application/rtf",
    # 表格
    ".csv": "text/csv",
    ".xls": "application/vnd.ms-excel",
    ".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
    # 代码
    ".py": "text/x-python-script",
    ".js": "application/javascript",
    ".ts": "application/typescript",
    ".java": "text/x-java",
    ".cpp": "text/x-c++",
    ".c": "text/x-c",
    ".go": "text/x-go",
    ".rs": "text/x-rust",
    ".rb": "text/x-ruby",
    ".php": "text/x-php",
    ".sh": "application/x-sh",
    ".html": "text/html",
    ".css": "text/css",
    ".sql": "application/sql",
    # 数据
    ".json": "application/json",
    ".xml": "application/xml",
    ".yaml": "application/x-yaml",
    ".yml": "application/x-yaml",
    ".toml": "application/toml",
    ".ini": "text/plain",
    ".log": "text/plain",
    ".tmp": "application/octet-stream",
    # 其他
    ".graphql": "application/graphql",
    ".proto": "application/x-protobuf",
    ".latex": "application/x-latex",
    ".wiki": "text/plain",
    ".rst": "text/x-rst",
}

IMAGE_EXTS = {".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp"}
VIDEO_EXTS = {".mp4", ".mov", ".m4v", ".webm", ".avi", ".mkv"}


# ==================== 基础服务 ====================


class BaseService:
    """基础服务类"""

    def __init__(self, proxy: str = None):
        self.proxy = (
            proxy
            or get_config("grok.asset_proxy_url")
            or get_config("grok.base_proxy_url", "")
        )
        self.timeout = get_config("grok.timeout", TIMEOUT)
        self._session: Optional[AsyncSession] = None

    def _headers(self, token: str, referer: str = "https://grok.com/") -> dict:
        """构建请求头"""
        headers = {
            "Accept": "*/*",
            "Accept-Encoding": "gzip, deflate, br, zstd",
            "Accept-Language": "zh-CN,zh;q=0.9",
            "Baggage": "sentry-environment=production,sentry-release=d6add6fb0460641fd482d767a335ef72b9b6abb8,sentry-public_key=b311e0f2690c81f25e2c4cf6d4f7ce1c",
            "Cache-Control": "no-cache",
            "Content-Type": "application/json",
            "Origin": "https://grok.com",
            "Pragma": "no-cache",
            "Priority": "u=1, i",
            "Referer": referer,
            "Sec-Ch-Ua": '"Google Chrome";v="136", "Chromium";v="136", "Not(A:Brand";v="24"',
            "Sec-Ch-Ua-Arch": "arm",
            "Sec-Ch-Ua-Bitness": "64",
            "Sec-Ch-Ua-Mobile": "?0",
            "Sec-Ch-Ua-Model": "",
            "Sec-Ch-Ua-Platform": '"macOS"',
            "Sec-Fetch-Dest": "empty",
            "Sec-Fetch-Mode": "cors",
            "Sec-Fetch-Site": "same-origin",
            "User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36",
        }

        apply_statsig(headers)
        headers["Cookie"] = build_sso_cookie(token)

        return headers

    def _proxies(self) -> Optional[dict]:
        """构建代理配置"""
        return {"http": self.proxy, "https": self.proxy} if self.proxy else None

    def _dl_headers(self, token: str, file_path: str) -> dict:
        """构建下载请求头"""
        headers = {
            "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
            "Sec-Fetch-Dest": "document",
            "Sec-Fetch-Mode": "navigate",
            "Sec-Fetch-Site": "same-site",
            "Sec-Fetch-User": "?1",
            "Upgrade-Insecure-Requests": "1",
            "Referer": "https://grok.com/",
        }

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
    def is_url(input_str: str) -> bool:
        """检查是否为 URL"""
        try:
            result = urlparse(input_str)
            return all([result.scheme, result.netloc]) and result.scheme in [
                "http",
                "https",
            ]
        except Exception:
            return False

    @staticmethod
    async def fetch(url: str) -> Tuple[str, str, str]:
        """
        获取远程资源并转 Base64

        Raises:
            UpstreamException: 当获取失败时
        """
        try:
            async with AsyncSession() as session:
                response = await session.get(url, timeout=10)
                if response.status_code >= 400:
                    raise UpstreamException(
                        message=f"Failed to fetch resource: {response.status_code}",
                        details={"url": url, "status": response.status_code},
                    )

                filename = url.split("/")[-1].split("?")[0] or "download"
                content_type = response.headers.get("content-type", DEFAULT_MIME).split(
                    ";"
                )[0]
                b64 = base64.b64encode(response.content).decode()

                logger.debug(f"Fetched: {url} -> {filename}")
                return filename, b64, content_type
        except Exception as e:
            logger.error(f"Fetch failed: {url} - {e}")
            if isinstance(e, AppException):
                raise e
            raise UpstreamException(
                f"Resource fetch failed: {str(e)}", details={"url": url}
            )

    @staticmethod
    def parse_b64(data_uri: str) -> Tuple[str, str, str]:
        """解析 Base64 数据"""
        if data_uri.startswith("data:"):
            try:
                header, b64 = data_uri.split(",", 1)
            except ValueError:
                return "file.bin", data_uri, DEFAULT_MIME
            if ";base64" in header:
                mime = header[5:].split(";", 1)[0] or DEFAULT_MIME
                b64 = re.sub(r"\\s+", "", b64)
                ext = mime.split("/")[-1] if "/" in mime else "bin"
                return f"file.{ext}", b64, mime
        return "file.bin", data_uri, DEFAULT_MIME

    @staticmethod
    def to_b64(file_path: Path, mime_type: str) -> str:
        """将本地文件转为 base64 data URI"""
        try:
            b64_data = base64.b64encode(file_path.read_bytes()).decode()
            return f"data:{mime_type};base64,{b64_data}"
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

        Raises:
            ValidationException: 输入无效
            UpstreamException: 上传失败
        """
        async with _get_assets_semaphore():
            try:
                # 处理输入
                if self.is_url(file_input):
                    filename, b64, mime = await self.fetch(file_input)
                else:
                    filename, b64, mime = self.parse_b64(file_input)

                if not b64:
                    raise ValidationException("Invalid file input: empty content")

                # 构建请求
                headers = self._headers(token)
                payload = {
                    "fileName": filename,
                    "fileMimeType": mime,
                    "content": b64,
                }

                # 执行上传
                session = await self._get_session()
                response = await session.post(
                    UPLOAD_API,
                    headers=headers,
                    json=payload,
                    impersonate=BROWSER,
                    timeout=self.timeout,
                    proxies=self._proxies(),
                )

                if response.status_code == 200:
                    result = response.json()
                    file_id = result.get("fileMetadataId", "")
                    file_uri = result.get("fileUri", "")
                    logger.info(
                        f"Upload success: {filename} -> {file_id}",
                        extra={"file_id": file_id},
                    )
                    return file_id, file_uri

                # 401/403 表示 token 失效，记录失败并标记冷却
                if response.status_code in (401, 403):
                    logger.warning(
                        f"Upload token invalid: {response.status_code}, recording failure",
                        extra={"response": response.text[:200]},
                    )
                    try:
                        await TokenService.record_fail(
                            token,
                            status_code=response.status_code,
                            reason="upload_auth_failed",
                        )
                    except Exception as mark_err:
                        logger.error(f"Failed to record token failure: {mark_err}")

                    raise UpstreamException(
                        message=f"Upload authentication failed: {response.status_code}",
                        details={
                            "status": response.status_code,
                            "response": response.text[:200],
                            "token_invalidated": True,
                        },
                    )

                logger.error(
                    f"Upload failed: {filename} - {response.status_code}",
                    extra={"response": response.text[:200]},
                )
                raise UpstreamException(
                    message=f"Upload failed with status {response.status_code}",
                    details={
                        "status": response.status_code,
                        "response": response.text[:200],
                    },
                )

            except Exception as e:
                logger.error(f"Upload error: {e}")
                if isinstance(e, AppException):
                    raise e
                raise UpstreamException(f"Upload process error: {str(e)}")


# ==================== 列表服务 ====================


class ListService(BaseService):
    """文件列表查询服务"""

    async def iter_assets(self, token: str):
        """
        分页迭代资产列表
        """
        headers = self._headers(token, referer="https://grok.com/files")
        base_params = {
            "pageSize": 50,
            "orderBy": "ORDER_BY_LAST_USE_TIME",
            "source": "SOURCE_ANY",
            "isLatest": "true",
        }
        page_token = None
        seen_tokens = set()

        async with AsyncSession() as session:
            while True:
                params = dict(base_params)
                if page_token:
                    if page_token in seen_tokens:
                        logger.warning(
                            "List pagination stopped due to repeated page token"
                        )
                        break
                    seen_tokens.add(page_token)
                    params["pageToken"] = page_token

                response = await session.get(
                    LIST_API,
                    headers=headers,
                    params=params,
                    impersonate=BROWSER,
                    timeout=self.timeout,
                    proxies=self._proxies(),
                )

                if response.status_code != 200:
                    logger.error(f"List failed: {response.status_code}")
                    raise UpstreamException(
                        message=f"List assets failed: {response.status_code}",
                        details={"status": response.status_code},
                    )

                result = response.json()
                page_assets = result.get("assets", [])
                yield page_assets

                page_token = result.get("nextPageToken")
                if not page_token:
                    break

    async def list(self, token: str) -> List[Dict]:
        """
        查询文件列表

        Raises:
            UpstreamException: 查询失败
        """
        try:
            assets: List[Dict] = []
            async for page_assets in self.iter_assets(token):
                assets.extend(page_assets)

            logger.info(f"List success: {len(assets)} files")
            return assets

        except Exception as e:
            logger.error(f"List error: {e}")
            if isinstance(e, AppException):
                raise e
            raise UpstreamException(f"List assets error: {str(e)}")

    async def count(self, token: str) -> int:
        """
        统计资产数量（不保留明细）
        """
        try:
            total = 0
            async for page_assets in self.iter_assets(token):
                total += len(page_assets)
            return total
        except Exception as e:
            logger.error(f"List count error: {e}")
            if isinstance(e, AppException):
                raise e
            raise UpstreamException(f"List assets error: {str(e)}")


# ==================== 删除服务 ====================


class DeleteService(BaseService):
    """文件删除服务"""

    async def delete(self, token: str, asset_id: str) -> bool:
        """
        删除单个文件

        Raises:
            UpstreamException: 删除失败
        """
        async with _get_assets_semaphore():
            try:
                headers = self._headers(token, referer="https://grok.com/files")
                url = f"{DELETE_API}/{asset_id}"

                session = await self._get_session()
                response = await session.delete(
                    url,
                    headers=headers,
                    impersonate=BROWSER,
                    timeout=self.timeout,
                    proxies=self._proxies(),
                )

                if response.status_code == 200:
                    logger.debug(f"Delete success: {asset_id}")
                    return True

                logger.error(f"Delete failed: {asset_id} - {response.status_code}")
                #: Note: Returning False or raising Exception?
                #: Assuming caller handles Exception for stricter control, or False for loose control.
                #: Given "optimization" and "standardization", raising exceptions is better for API feedback.
                raise UpstreamException(
                    message=f"Delete failed: {asset_id}",
                    details={"status": response.status_code},
                )

            except Exception as e:
                logger.error(f"Delete error: {asset_id} - {e}")
                if isinstance(e, AppException):
                    raise e
                raise UpstreamException(f"Delete error: {str(e)}")

    async def delete_all(self, token: str) -> Dict[str, int]:
        """
        删除所有文件
        """
        total = 0
        success = 0
        failed = 0
        list_service = ListService(self.proxy)
        try:
            async for assets in list_service.iter_assets(token):
                if not assets:
                    continue
                total += len(assets)

                # 批量并发删除
                async def _delete_one(asset: Dict, index: int) -> bool:
                    await asyncio.sleep(0.01 * index)
                    asset_id = asset.get("assetId", "")
                    if asset_id:
                        try:
                            return await self.delete(token, asset_id)
                        except Exception:
                            return False
                    return False

                batch_size = _get_delete_batch_size()
                for i in range(0, len(assets), batch_size):
                    batch = assets[i : i + batch_size]
                    results = await asyncio.gather(
                        *[_delete_one(asset, idx) for idx, asset in enumerate(batch)]
                    )
                    success += sum(results)
                    failed += len(batch) - sum(results)

            if total == 0:
                logger.info("No assets to delete")
                return {"total": 0, "success": 0, "failed": 0, "skipped": True}
        except Exception as e:
            logger.error(f"Delete all failed during list: {e}")
            return {"total": total, "success": success, "failed": failed}
        finally:
            await list_service.close()

        logger.info(f"Delete all: total={total}, success={success}, failed={failed}")
        return {"total": total, "success": success, "failed": failed}


# ==================== 下载服务 ====================


class DownloadService(BaseService):
    """文件下载服务"""

    def __init__(self, proxy: str = None):
        super().__init__(proxy)
        # 创建缓存目录
        self.base_dir = Path(__file__).parent.parent.parent.parent / "data" / "tmp"
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

    async def download(
        self, file_path: str, token: str, media_type: str = "image"
    ) -> Tuple[Optional[Path], str]:
        """
        下载文件到本地

        Raises:
            UpstreamException: 下载失败
        """
        async with _get_assets_semaphore():
            try:
                cache_path = self._cache_path(file_path, media_type)

                # 如果已缓存
                if cache_path.exists():
                    logger.debug(f"Cache hit: {cache_path}")
                    mime_type = MIME_TYPES.get(cache_path.suffix.lower(), DEFAULT_MIME)
                    return cache_path, mime_type

                lock_name = f"download_{media_type}_{hashlib.sha1(str(cache_path).encode('utf-8')).hexdigest()[:16]}"
                async with _file_lock(lock_name, timeout=10):
                    # Double-check after lock
                    if cache_path.exists():
                        logger.debug(f"Cache hit after lock: {cache_path}")
                        mime_type = MIME_TYPES.get(
                            cache_path.suffix.lower(), DEFAULT_MIME
                        )
                        return cache_path, mime_type

                    # 下载文件
                    if not file_path.startswith("/"):
                        file_path = f"/{file_path}"

                    url = f"{DOWNLOAD_API}{file_path}"
                    headers = self._dl_headers(token, file_path)

                    session = await self._get_session()
                    response = await session.get(
                        url,
                        headers=headers,
                        proxies=self._proxies(),
                        timeout=self.timeout,
                        allow_redirects=True,
                        impersonate=BROWSER,
                        stream=True,
                    )

                    if response.status_code != 200:
                        raise UpstreamException(
                            message=f"Download failed: {response.status_code}",
                            details={"path": file_path, "status": response.status_code},
                        )

                    # 保存文件（分块写入，避免大文件占用内存）
                    tmp_path = cache_path.with_suffix(cache_path.suffix + ".tmp")
                    try:
                        async with aiofiles.open(tmp_path, "wb") as f:
                            if hasattr(response, "aiter_content"):
                                async for chunk in response.aiter_content():
                                    if chunk:
                                        await f.write(chunk)
                            elif hasattr(response, "aiter_bytes"):
                                async for chunk in response.aiter_bytes():
                                    if chunk:
                                        await f.write(chunk)
                            elif hasattr(response, "aiter_raw"):
                                async for chunk in response.aiter_raw():
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
                    mime_type = response.headers.get(
                        "content-type", DEFAULT_MIME
                    ).split(";")[0]

                    logger.info(f"Download success: {file_path}")

                    # 检查缓存限制
                    asyncio.create_task(self.check_limit())

                    return cache_path, mime_type

            except Exception as e:
                logger.error(f"Download failed: {file_path} - {e}")
                if isinstance(e, AppException):
                    raise e
                raise UpstreamException(f"Download error: {str(e)}")

    async def to_base64(
        self, file_path: str, token: str, media_type: str = "image"
    ) -> str:
        """
        下载文件并转为 base64
        """
        try:
            cache_path, mime_type = await self.download(file_path, token, media_type)

            if not cache_path or not cache_path.exists():
                raise AppException("File download returned invalid path")

            # 使用基础服务的工具方法转换
            data_uri = self.to_b64(cache_path, mime_type)

            # 删除临时文件
            if data_uri:
                try:
                    cache_path.unlink()
                except Exception as e:
                    logger.warning(f"Delete temp file failed: {e}")

            return data_uri

        except Exception as e:
            logger.error(f"To base64 failed: {file_path} - {e}")
            if isinstance(e, AppException):
                raise e
            raise AppException(f"Base64 conversion failed: {str(e)}")

    def get_stats(self, media_type: str = "image") -> Dict[str, Any]:
        """获取缓存统计"""
        cache_dir = self.image_dir if media_type == "image" else self.video_dir
        if not cache_dir.exists():
            return {"count": 0, "size_mb": 0.0}

        files = [f for f in cache_dir.glob("*") if f.is_file()]
        allowed_exts = IMAGE_EXTS if media_type == "image" else VIDEO_EXTS
        files = [f for f in files if f.suffix.lower() in allowed_exts]
        total_size = sum(f.stat().st_size for f in files)

        return {"count": len(files), "size_mb": round(total_size / 1024 / 1024, 2)}

    def list_files(
        self, media_type: str = "image", page: int = 1, page_size: int = 1000
    ) -> Dict[str, Any]:
        """列出本地缓存文件"""
        cache_dir = self.image_dir if media_type == "image" else self.video_dir
        if not cache_dir.exists():
            return {"total": 0, "page": page, "page_size": page_size, "items": []}

        files = [f for f in cache_dir.glob("*") if f.is_file()]
        allowed_exts = IMAGE_EXTS if media_type == "image" else VIDEO_EXTS
        files = [f for f in files if f.suffix.lower() in allowed_exts]
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
        total = len(items)
        start = max(0, (page - 1) * page_size)
        end = start + page_size
        paged = items[start:end]

        if media_type == "image":
            for item in paged:
                item["view_url"] = f"/v1/files/image/{item['name']}"
        else:
            preview_map = {}
            if self.image_dir.exists():
                for img in self.image_dir.glob("*"):
                    if img.is_file() and img.suffix.lower() in IMAGE_EXTS:
                        preview_map.setdefault(img.stem, img.name)
            for item in paged:
                item["view_url"] = f"/v1/files/video/{item['name']}"
                preview_name = preview_map.get(Path(item["name"]).stem)
                if preview_name:
                    item["preview_url"] = f"/v1/files/image/{preview_name}"

        return {"total": total, "page": page, "page_size": page_size, "items": paged}

    def delete_file(self, media_type: str, name: str) -> Dict[str, Any]:
        """删除单个缓存文件"""
        cache_dir = self.image_dir if media_type == "image" else self.video_dir
        safe_name = name.replace("/", "-")
        file_path = cache_dir / safe_name
        if not file_path.exists():
            return {"deleted": False}
        try:
            file_path.unlink()
            return {"deleted": True}
        except Exception:
            return {"deleted": False}

    def clear(self, media_type: str = "image") -> Dict[str, Any]:
        """清空缓存"""
        cache_dir = self.image_dir if media_type == "image" else self.video_dir
        if not cache_dir.exists():
            return {"count": 0, "size_mb": 0.0}

        files = list(cache_dir.glob("*"))
        total_size = sum(f.stat().st_size for f in files)
        count = 0

        for f in files:
            try:
                f.unlink()
                count += 1
            except Exception as e:
                logger.error(f"Failed to delete {f}: {e}")

        return {"count": count, "size_mb": round(total_size / 1024 / 1024, 2)}

    async def check_limit(self):
        """检查并清理缓存限制"""
        if self._cleanup_running:
            return
        self._cleanup_running = True
        try:
            async with _file_lock("cache_cleanup", timeout=5):
                if not get_config("cache.enable_auto_clean", True):
                    return

                limit_mb = get_config("cache.limit_mb", 1024)

                # 统计总大小
                total_size = 0
                all_files = []

                for d in [self.image_dir, self.video_dir]:
                    if d.exists():
                        for f in d.glob("*"):
                            try:
                                stat = f.stat()
                                total_size += stat.st_size
                                all_files.append((f, stat.st_mtime, stat.st_size))
                            except Exception:
                                pass

                current_mb = total_size / 1024 / 1024
                if current_mb <= limit_mb:
                    return

                # 需要清理
                logger.info(
                    f"Cache limit exceeded ({current_mb:.2f}MB > {limit_mb}MB), cleaning up..."
                )

                # 按时间排序
                all_files.sort(key=lambda x: x[1])

                deleted_count = 0
                deleted_size = 0
                target_mb = limit_mb * 0.8  # 清理到 80%

                for f, _, size in all_files:
                    try:
                        f.unlink()
                        deleted_count += 1
                        deleted_size += size
                        total_size -= size

                        if (total_size / 1024 / 1024) <= target_mb:
                            break
                    except Exception as e:
                        logger.error(f"Cleanup failed for {f}: {e}")

                logger.info(
                    f"Cache cleanup: deleted {deleted_count} files ({deleted_size / 1024 / 1024:.2f}MB)"
                )
        finally:
            self._cleanup_running = False

    def get_public_url(self, file_path: str) -> str:
        """
        获取文件的公共访问 URL

        如果配置了 app_url，则返回自托管 URL，否则返回 Grok 原始 URL
        """
        app_url = get_config("app.app_url", "")
        if not app_url:
            return f"{DOWNLOAD_API}{file_path if file_path.startswith('/') else '/' + file_path}"

        if not file_path.startswith("/"):
            file_path = f"/{file_path}"

        # 自动添加 /v1/files 前缀
        return f"{app_url.rstrip('/')}/v1/files{file_path}"


__all__ = [
    "BaseService",
    "UploadService",
    "ListService",
    "DeleteService",
    "DownloadService",
]
