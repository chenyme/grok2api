# -*- coding: utf-8 -*-
"""FastMCP服务器实例"""

from fastmcp import FastMCP
from fastmcp.server.auth.providers.jwt import StaticTokenVerifier
from app.services.mcp.tools import ask_grok_impl
from app.core.config import setting


def create_mcp_server() -> FastMCP:
    """创建MCP服务器实例，如果配置了API密钥则启用认证"""
    # 检查是否配置了API密钥
    api_key = setting.grok_config.get("api_key")
    
    # 如果配置了API密钥，则启用静态token验证
    auth = None
    if api_key:
        auth = StaticTokenVerifier(
            tokens={
                api_key: {
                    "client_id": "grok2api-client",
                    "scopes": ["read", "write", "admin"]
                }
            },
            required_scopes=["read"]
        )
    
    # 创建FastMCP实例
    return FastMCP(
        name="Grok2API-MCP",
        instructions="MCP server providing Grok AI chat capabilities. Use ask_grok tool to interact with Grok AI models.",
        auth=auth
    )


# 创建全局MCP实例
mcp = create_mcp_server()


# 注册ask_grok工具
@mcp.tool
async def ask_grok(
    query: str,
    model: str = "grok-3-fast",
    system_prompt: str = None
) -> str:
    """
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
