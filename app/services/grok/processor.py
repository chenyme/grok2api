"""
OpenAI 响应格式处理器
"""
import time
import uuid
import random
import orjson
from typing import Any, AsyncGenerator, Optional, AsyncIterable, List

from app.core.config import get_config
from app.core.logger import logger
from app.services.grok.assets import DownloadService


ASSET_URL = "https://assets.grok.com/"


class BaseProcessor:
    """基础处理器"""
    
    def __init__(self, model: str, token: str = ""):
        self.model = model
        self.token = token
        self.created = int(time.time())
        self.app_url = get_config("app.app_url", "")
        self._dl_service: Optional[DownloadService] = None

    def _get_dl(self) -> DownloadService:
        """获取下载服务实例（复用）"""
        if self._dl_service is None:
            self._dl_service = DownloadService()
        return self._dl_service

    async def close(self):
        """释放下载服务资源"""
        if self._dl_service:
            await self._dl_service.close()
            self._dl_service = None

    async def process_url(self, path: str, media_type: str = "image") -> str:
        """处理资产 URL"""
        # 处理可能的绝对路径
        if path.startswith("http"):
            from urllib.parse import urlparse
            path = urlparse(path).path
            
        if not path.startswith("/"):
            path = f"/{path}"
            
        if self.app_url:
            dl_service = self._get_dl()
            await dl_service.download(path, self.token, media_type)
            return f"{self.app_url.rstrip('/')}/v1/files/{media_type}{path}"
        else:
            return f"{ASSET_URL.rstrip('/')}{path}"
            
    def _sse(self, content: str = "", role: str = None, finish: str = None) -> str:
        """构建 SSE 响应 (StreamProcessor 通用)"""
        if not hasattr(self, 'response_id'):
            self.response_id = None
        if not hasattr(self, 'fingerprint'):
            self.fingerprint = ""
            
        delta = {}
        if role:
            delta["role"] = role
            delta["content"] = ""
        elif content:
            delta["content"] = content
        
        chunk = {
            "id": self.response_id or f"chatcmpl-{uuid.uuid4().hex[:24]}",
            "object": "chat.completion.chunk",
            "created": self.created,
            "model": self.model,
            "system_fingerprint": self.fingerprint if hasattr(self, 'fingerprint') else "",
            "choices": [{"index": 0, "delta": delta, "logprobs": None, "finish_reason": finish}]
        }
        return f"data: {orjson.dumps(chunk).decode()}\n\n"


class StreamProcessor(BaseProcessor):
    """流式响应处理器"""
    
    def __init__(self, model: str, token: str = "", think: bool = None):
        super().__init__(model, token)
        self.response_id: Optional[str] = None
        self.fingerprint: str = ""
        self.think_opened: bool = False
        self.role_sent: bool = False
        self.filter_tags = get_config("grok.filter_tags", [])
        self.image_format = get_config("app.image_format", "url")
        
        if think is None:
            self.show_think = get_config("grok.thinking", False)
        else:
            self.show_think = think
    
    async def process(self, response: AsyncIterable[bytes]) -> AsyncGenerator[str, None]:
        """处理流式响应"""
        try:
            async for line in response:
                if not line:
                    continue
                try:
                    data = orjson.loads(line)
                except orjson.JSONDecodeError:
                    continue
                
                resp = data.get("result", {}).get("response", {})
                
                # 元数据
                if (llm := resp.get("llmInfo")) and not self.fingerprint:
                    self.fingerprint = llm.get("modelHash", "")
                if rid := resp.get("responseId"):
                    self.response_id = rid
                
                # 首次发送 role
                if not self.role_sent:
                    yield self._sse(role="assistant")
                    self.role_sent = True
                
                # 图像生成进度
                if img := resp.get("streamingImageGenerationResponse"):
                    if self.show_think:
                        if not self.think_opened:
                            yield self._sse("<think>\n")
                            self.think_opened = True
                        idx = img.get('imageIndex', 0) + 1
                        progress = img.get('progress', 0)
                        yield self._sse(f"正在生成第{idx}张图片中，当前进度{progress}%\n")
                    continue
                
                # modelResponse
                if mr := resp.get("modelResponse"):
                    if self.think_opened and self.show_think:
                        if msg := mr.get("message"):
                            yield self._sse(msg + "\n")
                        yield self._sse("</think>\n")
                        self.think_opened = False
                    
                    # 处理生成的图片
                    for url in mr.get("generatedImageUrls", []):
                        parts = url.split("/")
                        img_id = parts[-2] if len(parts) >= 2 else "image"
                        
                        if self.image_format == "base64":
                            dl_service = self._get_dl()
                            base64_data = await dl_service.to_base64(url, self.token, "image")
                            if base64_data:
                                yield self._sse(f"![{img_id}]({base64_data})\n")
                            else:
                                final_url = await self.process_url(url, "image")
                                yield self._sse(f"![{img_id}]({final_url})\n")
                        else:
                            final_url = await self.process_url(url, "image")
                            yield self._sse(f"![{img_id}]({final_url})\n")
                    
                    if (meta := mr.get("metadata", {})).get("llm_info", {}).get("modelHash"):
                        self.fingerprint = meta["llm_info"]["modelHash"]
                    continue
                
                # 普通 token
                if (token := resp.get("token")) is not None:
                    if token and not (self.filter_tags and any(t in token for t in self.filter_tags)):
                        yield self._sse(token)
                        
            if self.think_opened:
                yield self._sse("</think>\n")
            yield self._sse(finish="stop")
            yield "data: [DONE]\n\n"
        except Exception as e:
            logger.error(f"Stream processing error: {e}", extra={"model": self.model})
            raise
        finally:
            await self.close()


class CollectProcessor(BaseProcessor):
    """非流式响应处理器"""
    
    def __init__(self, model: str, token: str = ""):
        super().__init__(model, token)
        self.image_format = get_config("app.image_format", "url")
    
    async def process(self, response: AsyncIterable[bytes]) -> dict[str, Any]:
        """处理并收集完整响应"""
        response_id = ""
        fingerprint = ""
        content = ""
        
        try:
            async for line in response:
                if not line:
                    continue
                try:
                    data = orjson.loads(line)
                except orjson.JSONDecodeError:
                    continue
                
                resp = data.get("result", {}).get("response", {})
                
                if (llm := resp.get("llmInfo")) and not fingerprint:
                    fingerprint = llm.get("modelHash", "")
                
                if mr := resp.get("modelResponse"):
                    response_id = mr.get("responseId", "")
                    content = mr.get("message", "")
                    
                    if urls := mr.get("generatedImageUrls"):
                        content += "\n"
                        for url in urls:
                            parts = url.split("/")
                            img_id = parts[-2] if len(parts) >= 2 else "image"
                            
                            if self.image_format == "base64":
                                dl_service = self._get_dl()
                                base64_data = await dl_service.to_base64(url, self.token, "image")
                                if base64_data:
                                    content += f"![{img_id}]({base64_data})\n"
                                else:
                                    final_url = await self.process_url(url, "image")
                                    content += f"![{img_id}]({final_url})\n"
                            else:
                                final_url = await self.process_url(url, "image")
                                content += f"![{img_id}]({final_url})\n"
                    
                    if (meta := mr.get("metadata", {})).get("llm_info", {}).get("modelHash"):
                        fingerprint = meta["llm_info"]["modelHash"]
                            
        except Exception as e:
            logger.error(f"Collect processing error: {e}", extra={"model": self.model})
        finally:
            await self.close()
        
        return {
            "id": response_id,
            "object": "chat.completion",
            "created": self.created,
            "model": self.model,
            "system_fingerprint": fingerprint,
            "choices": [{
                "index": 0,
                "message": {"role": "assistant", "content": content, "refusal": None, "annotations": []},
                "finish_reason": "stop"
            }],
            "usage": {
                "prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0,
                "prompt_tokens_details": {"cached_tokens": 0, "text_tokens": 0, "audio_tokens": 0, "image_tokens": 0},
                "completion_tokens_details": {"text_tokens": 0, "audio_tokens": 0, "reasoning_tokens": 0}
            }
        }


class VideoStreamProcessor(BaseProcessor):
    """视频流式响应处理器"""
    
    def __init__(self, model: str, token: str = "", think: bool = None):
        super().__init__(model, token)
        self.response_id: Optional[str] = None
        self.think_opened: bool = False
        self.role_sent: bool = False
        self.video_format = get_config("app.video_format", "url")
        
        if think is None:
            self.show_think = get_config("grok.thinking", False)
        else:
            self.show_think = think
    
    def _build_video_html(self, video_url: str, thumbnail_url: str = "") -> str:
        """构建视频 HTML 标签"""
        poster_attr = f' poster="{thumbnail_url}"' if thumbnail_url else ""
        return f'''<video id="video" controls="" preload="none"{poster_attr}>
  <source id="mp4" src="{video_url}" type="video/mp4">
</video>'''
    
    async def process(self, response: AsyncIterable[bytes]) -> AsyncGenerator[str, None]:
        """处理视频流式响应"""
        try:
            async for line in response:
                if not line:
                    continue
                try:
                    data = orjson.loads(line)
                except orjson.JSONDecodeError:
                    continue
                
                resp = data.get("result", {}).get("response", {})
                
                if rid := resp.get("responseId"):
                    self.response_id = rid
                
                # 首次发送 role
                if not self.role_sent:
                    yield self._sse(role="assistant")
                    self.role_sent = True
                
                # 视频生成进度
                if video_resp := resp.get("streamingVideoGenerationResponse"):
                    progress = video_resp.get("progress", 0)
                    
                    if self.show_think:
                        if not self.think_opened:
                            yield self._sse("<think>\n")
                            self.think_opened = True
                        yield self._sse(f"正在生成视频中，当前进度{progress}%\n")
                    
                    if progress == 100:
                        video_url = video_resp.get("videoUrl", "")
                        thumbnail_url = video_resp.get("thumbnailImageUrl", "")
                        
                        if self.think_opened and self.show_think:
                            yield self._sse("</think>\n")
                            self.think_opened = False
                        
                        if video_url:
                            final_video_url = await self.process_url(video_url, "video")
                            final_thumbnail_url = ""
                            if thumbnail_url:
                                final_thumbnail_url = await self.process_url(thumbnail_url, "image")
                            
                            video_html = self._build_video_html(final_video_url, final_thumbnail_url)
                            yield self._sse(video_html)
                            
                            logger.info(f"Video generated: {video_url}")
                    continue
                        
            if self.think_opened:
                yield self._sse("</think>\n")
            yield self._sse(finish="stop")
            yield "data: [DONE]\n\n"
        except Exception as e:
            logger.error(f"Video stream processing error: {e}", extra={"model": self.model})
        finally:
            await self.close()


class VideoCollectProcessor(BaseProcessor):
    """视频非流式响应处理器"""
    
    def __init__(self, model: str, token: str = ""):
        super().__init__(model, token)
        self.video_format = get_config("app.video_format", "url")
    
    def _build_video_html(self, video_url: str, thumbnail_url: str = "") -> str:
        poster_attr = f' poster="{thumbnail_url}"' if thumbnail_url else ""
        return f'''<video id="video" controls="" preload="none"{poster_attr}>
  <source id="mp4" src="{video_url}" type="video/mp4">
</video>'''
    
    async def process(self, response: AsyncIterable[bytes]) -> dict[str, Any]:
        """处理并收集视频响应"""
        response_id = ""
        content = ""
        
        try:
            async for line in response:
                if not line:
                    continue
                try:
                    data = orjson.loads(line)
                except orjson.JSONDecodeError:
                    continue
                
                resp = data.get("result", {}).get("response", {})
                
                if video_resp := resp.get("streamingVideoGenerationResponse"):
                    if video_resp.get("progress") == 100:
                        response_id = resp.get("responseId", "")
                        video_url = video_resp.get("videoUrl", "")
                        thumbnail_url = video_resp.get("thumbnailImageUrl", "")
                        
                        if video_url:
                            final_video_url = await self.process_url(video_url, "video")
                            final_thumbnail_url = ""
                            if thumbnail_url:
                                final_thumbnail_url = await self.process_url(thumbnail_url, "image")
                            
                            content = self._build_video_html(final_video_url, final_thumbnail_url)
                            logger.info(f"Video generated: {video_url}")
                            
        except Exception as e:
            logger.error(f"Video collect processing error: {e}", extra={"model": self.model})
        finally:
            await self.close()
        
        return {
            "id": response_id,
            "object": "chat.completion",
            "created": self.created,
            "model": self.model,
            "choices": [{
                "index": 0,
                "message": {"role": "assistant", "content": content, "refusal": None},
                "finish_reason": "stop"
            }],
            "usage": {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
        }


class ImageStreamProcessor(BaseProcessor):
    """图片生成流式响应处理器"""
    
    def __init__(self, model: str, token: str = "", n: int = 1):
        super().__init__(model, token)
        self.partial_index = 0
        self.n = n
        self.target_index = random.randint(0, 1) if n == 1 else None
    
    def _sse(self, event: str, data: dict) -> str:
        """构建 SSE 响应 (覆盖基类)"""
        return f"event: {event}\ndata: {orjson.dumps(data).decode()}\n\n"
    
    async def process(self, response: AsyncIterable[bytes]) -> AsyncGenerator[str, None]:
        """处理流式响应"""
        final_images = []
        
        try:
            async for line in response:
                if not line:
                    continue
                try:
                    data = orjson.loads(line)
                except orjson.JSONDecodeError:
                    continue
                
                resp = data.get("result", {}).get("response", {})
                
                # 图片生成进度
                if img := resp.get("streamingImageGenerationResponse"):
                    image_index = img.get("imageIndex", 0)
                    progress = img.get("progress", 0)
                    
                    if self.n == 1 and image_index != self.target_index:
                        continue
                    
                    out_index = 0 if self.n == 1 else image_index
                    
                    yield self._sse("image_generation.partial_image", {
                        "type": "image_generation.partial_image",
                        "b64_json": "",
                        "index": out_index,
                        "progress": progress
                    })
                    continue
                
                # modelResponse
                if mr := resp.get("modelResponse"):
                    if urls := mr.get("generatedImageUrls"):
                        for url in urls:
                            dl_service = self._get_dl()
                            base64_data = await dl_service.to_base64(url, self.token, "image")
                            if base64_data:
                                if "," in base64_data:
                                    b64 = base64_data.split(",", 1)[1]
                                else:
                                    b64 = base64_data
                                final_images.append(b64)
                    continue
                    
            for index, b64 in enumerate(final_images):
                if self.n == 1:
                    if index != self.target_index:
                        continue
                    out_index = 0
                else:
                    out_index = index
                
                yield self._sse("image_generation.completed", {
                    "type": "image_generation.completed",
                    "b64_json": b64,
                    "index": out_index,
                    "usage": {
                        "total_tokens": 50,
                        "input_tokens": 25,
                        "output_tokens": 25,
                        "input_tokens_details": {"text_tokens": 5, "image_tokens": 20}
                    }
                })
        except Exception as e:
            logger.error(f"Image stream processing error: {e}")
            raise
        finally:
            await self.close()


class ImageCollectProcessor(BaseProcessor):
    """图片生成非流式响应处理器"""
    
    def __init__(self, model: str, token: str = ""):
        super().__init__(model, token)
    
    async def process(self, response: AsyncIterable[bytes]) -> List[str]:
        """处理并收集图片"""
        images = []
        
        try:
            async for line in response:
                if not line:
                    continue
                try:
                    data = orjson.loads(line)
                except orjson.JSONDecodeError:
                    continue
                
                resp = data.get("result", {}).get("response", {})
                
                if mr := resp.get("modelResponse"):
                    if urls := mr.get("generatedImageUrls"):
                        for url in urls:
                            dl_service = self._get_dl()
                            base64_data = await dl_service.to_base64(url, self.token, "image")
                            if base64_data:
                                if "," in base64_data:
                                    b64 = base64_data.split(",", 1)[1]
                                else:
                                    b64 = base64_data
                                images.append(b64)
                                
        except Exception as e:
            logger.error(f"Image collect processing error: {e}")
        finally:
            await self.close()
        
        return images


__all__ = [
    "StreamProcessor",
    "CollectProcessor",
    "VideoStreamProcessor",
    "VideoCollectProcessor",
    "ImageStreamProcessor",
    "ImageCollectProcessor",
]
