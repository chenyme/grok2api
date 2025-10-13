"""Grok API 响应处理器模块"""

import json
import uuid
import time
from typing import Iterator

from app.core.config import setting
from app.core.exception import GrokApiException
from app.core.logger import logger
from app.models.openai_schema import (
    OpenAIChatCompletionResponse,
    OpenAIChatCompletionChoice,
    OpenAIChatCompletionMessage,
    OpenAIChatCompletionChunkResponse,
    OpenAIChatCompletionChunkChoice,
    OpenAIChatCompletionChunkMessage
)
from app.services.grok.cache import image_cache_service, video_cache_service


class GrokResponseProcessor:
    """Grok API 响应处理器"""

    @staticmethod
    async def process_normal(response, auth_token: str, model: str = None) -> OpenAIChatCompletionResponse:
        """处理非流式响应"""
        try:
            for chunk in response.iter_lines():
                if not chunk:
                    continue

                data = json.loads(chunk.decode("utf-8"))

                # 错误检查
                if error := data.get("error"):
                    raise GrokApiException(
                        f"API错误: {error.get('message', '未知错误')}",
                        "API_ERROR",
                        {"code": error.get("code")}
                    )

                # 提取响应数据
                grok_resp = data.get("result", {}).get("response", {})
                
                # 检查视频生成响应
                if video_resp := grok_resp.get("streamingVideoGenerationResponse"):
                    if video_url := video_resp.get("videoUrl"):
                        logger.debug(f"[Processor] 检测到视频生成: {video_url}")
                        full_video_url = f"https://assets.grok.com/{video_url}"
                        
                        # 下载并缓存视频
                        try:
                            cache_path = await video_cache_service.download_video(f"/{video_url}", auth_token)
                            if cache_path:
                                video_path = video_url.replace('/', '-')
                                base_url = setting.global_config.get("base_url", "")
                                local_video_url = f"{base_url}/images/{video_path}" if base_url else f"/images/{video_path}"
                                content = f'<video src="{local_video_url}" controls="controls" width="500" height="300"></video>'
                            else:
                                content = f'<video src="{full_video_url}" controls="controls" width="500" height="300"></video>'
                        except Exception as e:
                            logger.warning(f"[Processor] 缓存视频失败: {e}")
                            content = f'<video src="{full_video_url}" controls="controls" width="500" height="300"></video>'
                        
                        # 返回视频响应
                        result = OpenAIChatCompletionResponse(
                            id=f"chatcmpl-{uuid.uuid4()}",
                            object="chat.completion",
                            created=int(time.time()),
                            model=model or "grok-imagine-0.9",
                            choices=[OpenAIChatCompletionChoice(
                                index=0,
                                message=OpenAIChatCompletionMessage(
                                    role="assistant",
                                    content=content
                                ),
                                finish_reason="stop"
                            )],
                            usage=None
                        )
                        response.close()
                        return result

                # 提取模型响应
                model_response = grok_resp.get("modelResponse")
                if not model_response:
                    continue

                # 检查 modelResponse 中的错误
                if error_msg := model_response.get("error"):
                    raise GrokApiException(
                        f"模型响应错误: {error_msg}",
                        "MODEL_ERROR"
                    )

                # 构建响应内容
                model_name = model_response.get("model")
                content = model_response.get("message", "")

                # 添加生成的图片
                if images := model_response.get("generatedImageUrls"):
                    for img in images:
                        try:
                            cache_path = await image_cache_service.download_image(f"/{img}", auth_token)
                            if cache_path:
                                img_path = img.replace('/', '-')
                                base_url = setting.global_config.get("base_url", "")
                                img_url = f"{base_url}/images/{img_path}" if base_url else f"/images/{img_path}"
                                content += f"\n![Generated Image]({img_url})"
                            else:
                                content += f"\n![Generated Image](https://assets.grok.com/{img})"
                        except Exception as e:
                            logger.warning(f"[Processor] 缓存图片失败: {e}")
                            content += f"\n![Generated Image](https://assets.grok.com/{img})"

                # 返回OpenAI格式响应
                result = OpenAIChatCompletionResponse(
                    id=f"chatcmpl-{uuid.uuid4()}",
                    object="chat.completion",
                    created=int(time.time()),
                    model=model_name,
                    choices=[OpenAIChatCompletionChoice(
                        index=0,
                        message=OpenAIChatCompletionMessage(
                            role="assistant",
                            content=content
                        ),
                        finish_reason="stop"
                    )],
                    usage=None
                )
                response.close()
                return result

            raise GrokApiException("无响应数据", "NO_RESPONSE")

        except json.JSONDecodeError as e:
            raise GrokApiException(f"JSON解析失败: {e}", "JSON_ERROR") from e
        finally:
            # 确保响应对象被关闭
            if hasattr(response, 'close'):
                response.close()

    @staticmethod
    async def process_stream(response, auth_token: str) -> Iterator[str]:
        """处理流式响应"""
        is_image = False
        is_thinking = False
        thinking_finished = False
        chunk_index = 0
        model = None
        filtered_tags = setting.grok_config.get("filtered_tags", "").split(",")
        
        # 视频生成相关状态
        is_video = False
        video_url = None
        video_progress_started = False

        def make_chunk(content: str, finish: str = None):
            """生成OpenAI格式的响应块"""
            chunk_data = OpenAIChatCompletionChunkResponse(
                id=f"chatcmpl-{uuid.uuid4()}",
                created=int(time.time()),
                model=model or "grok-4-mini-thinking-tahoe",
                choices=[OpenAIChatCompletionChunkChoice(
                    index=chunk_index,
                    delta=OpenAIChatCompletionChunkMessage(
                        role="assistant",
                        content=content
                    ) if content else {},
                    finish_reason=finish
                )]
            ).model_dump()
            # SSE 格式返回
            return f"data: {json.dumps(chunk_data)}\n\n"

        try:
            for chunk in response.iter_lines():
                logger.debug(f"[Processor] 接收到数据块: {len(chunk)} bytes")
                if not chunk:
                    continue

                try:
                    data = json.loads(chunk.decode("utf-8"))

                    # 错误检查
                    if error := data.get("error"):
                        error_msg = error.get('message', '未知错误')
                        logger.error(f"[Processor] Grok API返回错误: {error_msg}")
                        yield make_chunk(f"Error: {error_msg}", "stop")
                        yield "data: [DONE]\n\n"
                        return

                    # 提取响应数据
                    grok_resp = data.get("result", {}).get("response", {})
                    logger.debug(f"[Processor] 解析响应数据: {len(grok_resp)} 字段")
                    if not grok_resp:
                        continue

                    # 更新模型名称
                    if user_resp := grok_resp.get("userResponse"):
                        if m := user_resp.get("model"):
                            model = m

                    # 检查视频生成响应
                    if video_resp := grok_resp.get("streamingVideoGenerationResponse"):
                        is_video = True
                        progress = video_resp.get("progress", 0)
                        
                        # 首次显示进度时添加 <think>
                        if not video_progress_started:
                            content = f"<think>视频已生成{progress}%\n"
                            video_progress_started = True
                        elif progress < 100:
                            content = f"视频已生成{progress}%\n"
                        else:
                            # 进度100%时关闭think标签
                            content = f"视频已生成{progress}%</think>\n"
                            # 保存视频URL
                            if v_url := video_resp.get("videoUrl"):
                                video_url = v_url
                                logger.debug(f"[Processor] 视频生成完成: {video_url}")
                        
                        yield make_chunk(content)
                        chunk_index += 1
                        continue

                    # 检查生成模式
                    if grok_resp.get("imageAttachmentInfo"):
                        is_image = True

                    # 获取token
                    token = grok_resp.get("token", "")

                    # 图片模式
                    if is_image:
                        if model_resp := grok_resp.get("modelResponse"):
                            # 生成图片链接并缓存
                            content = ""
                            for img in model_resp.get("generatedImageUrls", []):
                                try:
                                    # 异步下载并缓存图片
                                    await image_cache_service.download_image(f"/{img}", auth_token)
                                    # 使用本地缓存路径
                                    img_path = img.replace('/', '-')
                                    base_url = setting.global_config.get("base_url", "")
                                    img_url = f"{base_url}/images/{img_path}" if base_url else f"/images/{img_path}"
                                    content += f"![Generated Image]({img_url})\n"
                                except Exception as e:
                                    logger.warning(f"[Processor] 缓存图片失败: {e}")
                                    content += f"![Generated Image](https://assets.grok.com/{img})\n"
                            yield make_chunk(content.strip(), "stop")
                            return
                        elif token:
                            yield make_chunk(token)
                            chunk_index += 1

                    # 对话模式
                    else:
                        # 过滤 list 格式的 token
                        if isinstance(token, list):
                            continue

                        # 过滤特定标签
                        if any(tag in token for tag in filtered_tags if token):
                            continue

                        # 获取当前状态
                        current_is_thinking = grok_resp.get("isThinking", False)
                        message_tag = grok_resp.get("messageTag")

                        # 跳过后续的 thinking
                        if thinking_finished and current_is_thinking:
                            continue

                        # 检查 toolUsageCardId
                        if grok_resp.get("toolUsageCardId"):
                            if web_search := grok_resp.get("webSearchResults"):
                                if current_is_thinking:
                                    # 添加搜索结果到 token
                                    for result in web_search.get("results", []):
                                        title = result.get("title", "")
                                        url = result.get("url", "")
                                        preview = result.get("preview", "")
                                        preview_clean = preview.replace("\n", "") if isinstance(preview, str) else ""
                                        token += f'\n- [{title}]({url} "{preview_clean}")'
                                    token += "\n"
                                else:
                                    # 有 webSearchResults 但 isThinking 为 false
                                    continue
                            else:
                                # 没有 webSearchResults
                                continue

                        if token:
                            content = token

                            # header 在 token 后换行
                            if message_tag == "header":
                                content = f"\n\n{token}\n\n"

                            # is_thinking 状态切换
                            if not is_thinking and current_is_thinking:
                                content = f"<think>\n{content}"
                            elif is_thinking and not current_is_thinking:
                                content = f"\n</think>\n{content}"
                                thinking_finished = True

                            yield make_chunk(content)
                            chunk_index += 1
                            is_thinking = current_is_thinking

                except (json.JSONDecodeError, UnicodeDecodeError) as e:
                    logger.warning(f"[Processor] 解析chunk失败: {e}")
                    continue
                except Exception as e:
                    logger.warning(f"[Processor] 处理chunk出错: {e}")
                    continue

            # 处理视频生成完成后的输出
            if is_video and video_url:
                full_video_url = f"https://assets.grok.com/{video_url}"
                try:
                    # 下载并缓存视频
                    cache_path = await video_cache_service.download_video(f"/{video_url}", auth_token)
                    if cache_path:
                        video_path = video_url.replace('/', '-')
                        base_url = setting.global_config.get("base_url", "")
                        local_video_url = f"{base_url}/images/{video_path}" if base_url else f"/images/{video_path}"
                        content = f'<video src="{local_video_url}" controls="controls" width="500" height="300"></video>'
                    else:
                        content = f'<video src="{full_video_url}" controls="controls" width="500" height="300"></video>'
                except Exception as e:
                    logger.warning(f"[Processor] 缓存视频失败: {e}")
                    content = f'<video src="{full_video_url}" controls="controls" width="500" height="300"></video>'
                
                yield make_chunk(content, "stop")
            else:
                # 发送结束块
                yield make_chunk("", "stop")
            
            # 发送流结束标记
            yield "data: [DONE]\n\n"

        except Exception as e:
            logger.error(f"[Processor] 流式处理严重错误: {e}")
            yield make_chunk(f"处理错误: {e}", "error")
            # 发送流结束标记
            yield "data: [DONE]\n\n"