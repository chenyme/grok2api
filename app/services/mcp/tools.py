# -*- coding: utf-8 -*-
"""MCP Tools - Grok AI 对话工具"""

import json
from typing import Optional
from app.services.grok.client import GrokClient
from app.core.logger import logger
from app.core.exception import GrokApiException


async def ask_grok_impl(
    query: str,
    model: str = "grok-3-fast",
    system_prompt: Optional[str] = None
) -> str:
    """
    内部实现: 调用Grok API并收集完整响应

    Args:
        query: 用户问题
        model: 模型名称
        system_prompt: 系统提示词

    Returns:
        str: 完整的Grok响应内容
    """
    try:
        # 构建消息列表
        messages = []
        if system_prompt:
            messages.append({"role": "system", "content": system_prompt})
        messages.append({"role": "user", "content": query})

        # 构建请求
        request_data = {
            "model": model,
            "messages": messages,
            "stream": True
        }

        logger.info(f"[MCP] ask_grok 调用, 模型: {model}")

        # 调用Grok客户端(流式)
        response_iterator = await GrokClient.openai_to_grok(request_data)

        # 收集所有流式响应块
        content_parts = []
        async for chunk in response_iterator:
            if isinstance(chunk, bytes):
                chunk = chunk.decode('utf-8')

            # 解析SSE格式
            if chunk.startswith("data: "):
                data_str = chunk[6:].strip()
                if data_str == "[DONE]":
                    break

                try:
                    data = json.loads(data_str)
                    choices = data.get("choices", [])
                    if choices:
                        delta = choices[0].get("delta", {})
                        if content := delta.get("content"):
                            content_parts.append(content)
                except json.JSONDecodeError:
                    continue

        result = "".join(content_parts)
        logger.info(f"[MCP] ask_grok 完成, 响应长度: {len(result)}")
        return result

    except GrokApiException as e:
        logger.error(f"[MCP] Grok API错误: {str(e)}")
        raise Exception(f"Grok API调用失败: {str(e)}")
    except Exception as e:
        logger.error(f"[MCP] ask_grok异常: {str(e)}", exc_info=True)
        raise Exception(f"处理请求时出错: {str(e)}")
