# -*- coding: utf-8 -*-
"""FastMCP Server"""

from typing import Optional
from fastmcp import FastMCP
from fastmcp.server.auth.providers.jwt import StaticTokenVerifier
from app.services.mcp.tools import (
    ask_grok_impl, generate_image_impl, edit_image_impl, 
    generate_video_impl, list_models_impl
)
from app.core.config import setting


def create_mcp_server() -> FastMCP:
    api_key = setting.grok_config.get("api_key")
    auth = None
    if api_key:
        auth = StaticTokenVerifier(tokens={api_key: {"client_id": "grok2api", "scopes": ["read"]}}, required_scopes=["read"])
    return FastMCP(name="Grok2API-MCP", instructions="MCP server for Grok AI", auth=auth)


mcp = create_mcp_server()


@mcp.tool
async def ask_grok(query: str, model: str = "grok-4", system_prompt: str = None) -> str:
    return await ask_grok_impl(query, model, system_prompt)


@mcp.tool
async def generate_image(prompt: str, n: int = None, size: str = None, model: str = "grok-imagine-1.0") -> str:
    return await generate_image_impl(prompt, n, size, model)


@mcp.tool
async def edit_image(prompt: str, image_source: str, n: int = None, size: str = None, 
                     model: str = "grok-imagine-1.0-edit") -> str:
    return await edit_image_impl(prompt, image_source, n, size, model)


@mcp.tool
async def generate_video(prompt: str, image_url: str, aspect_ratio: str = None, video_length: int = None,
                        resolution: str = None, preset: str = None, model: str = "grok-imagine-1.0-video") -> str:
    return await generate_video_impl(prompt, image_url, aspect_ratio, video_length, resolution, preset, model)


@mcp.tool
async def list_models() -> str:
    return await list_models_impl()
