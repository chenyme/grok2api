# -*- coding: utf-8 -*-
"""FastMCP服务器实例"""

from fastmcp import FastMCP
from app.mcp.tools import ask_grok_impl

# 创建FastMCP实例 - 不在这里设置stateless_http
mcp = FastMCP(
    name="Grok2API-MCP",
    instructions="MCP server providing Grok AI chat capabilities. Use ask_grok tool to interact with Grok AI models."
)


# 注册ask_grok工具
@mcp.tool
async def ask_grok(
    query: str,
    model: str = "grok-3-fast",
    system_prompt: str = None
) -> str:
    """
<<<<<<< HEAD
    调用Grok AI进行对话，尤其适用于当用户询问最新信息，需要调用搜索功能，或是想了解社交平台动态（如Twitter(X)、Reddit等）时。

    Args:
        query: 用户的问题或指令
        model: Grok模型名称,可选值: grok-3-fast(默认), grok-4-fast, grok-4-fast-expert, grok-4-expert, grok-4-heavy
        system_prompt: 可选的系统提示词,用于设定AI的角色或行为约束

    Returns:
        Grok AI的完整回复内容,可能包括文本和图片链接(Markdown格式)

    Examples:
        - 简单问答: ask_grok("什么是Python?")
        - 指定模型: ask_grok("解释量子计算", model="grok-4-fast")
        - 带系统提示: ask_grok("写一首诗", system_prompt="你是一位古典诗人")
    """
    return await ask_grok_impl(query, model, system_prompt)
