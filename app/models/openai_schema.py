"""OpenAI 请求-响应模型定义"""

from fastapi import HTTPException
from typing import Optional, List, Union, Dict, Any
from pydantic import BaseModel, Field, field_validator

from app.models.grok_models import Models


class OpenAIChatRequest(BaseModel):
    """OpenAI聊天请求"""

    model: str = Field(..., description="模型名称", min_length=1)
    messages: List[Dict[str, Any]] = Field(..., description="消息列表", min_length=1)
    stream: bool = Field(False, description="流式响应")
    temperature: Optional[float] = Field(0.7, ge=0, le=2, description="采样温度")
    max_tokens: Optional[int] = Field(None, ge=1, le=100000, description="最大Token数")
    top_p: Optional[float] = Field(1.0, ge=0, le=1, description="采样参数")

    @classmethod
    @field_validator('messages')
    def validate_messages(cls, v):
        """验证消息格式"""
        if not v:
            raise HTTPException(status_code=400, detail="消息列表不能为空")

        for msg in v:
            if not isinstance(msg, dict):
                raise HTTPException(status_code=400, detail="每个消息必须是字典")
            if 'role' not in msg:
                raise HTTPException(status_code=400, detail="消息缺少 'role' 字段")
            if 'content' not in msg:
                raise HTTPException(status_code=400, detail="消息缺少 'content' 字段")
            if msg['role'] not in ['system', 'user', 'assistant']:
                raise HTTPException(
                    status_code=400,
                    detail=f"无效角色 '{msg['role']}', 必须是 system/user/assistant"
                )

        return v

    @classmethod
    @field_validator('model')
    def validate_model(cls, v):
        """验证模型名称"""
        if not Models.is_valid_model(v):
            supported = Models.get_all_model_names()
            raise HTTPException(
                status_code=400,
                detail=f"不支持的模型 '{v}', 支持: {', '.join(supported)}"
            )
        return v


class OpenAIChatCompletionMessage(BaseModel):
    """聊天完成消息"""
    role: str = Field(..., description="角色")
    content: str = Field(..., description="内容")
    reference_id: Optional[str] = Field(default=None, description="参考ID")
    annotations: Optional[List[str]] = Field(default=None, description="注释")


class OpenAIChatCompletionChoice(BaseModel):
    """聊天完成选项"""
    index: int = Field(..., description="索引")
    message: OpenAIChatCompletionMessage = Field(..., description="消息")
    logprobs: Optional[float] = Field(default=None, description="对数概率")
    finish_reason: str = Field(default="stop", description="完成原因")


class OpenAIChatCompletionResponse(BaseModel):
    """聊天完成响应"""
    id: str = Field(..., description="响应ID")
    object: str = Field("chat.completion", description="对象类型")
    created: int = Field(..., description="创建时间戳")
    model: str = Field(..., description="模型")
    choices: List[OpenAIChatCompletionChoice] = Field(..., description="选项")
    usage: Optional[Dict[str, Any]] = Field(None, description="令牌使用")


class OpenAIChatCompletionChunkMessage(BaseModel):
    """流式消息片段"""
    role: str = Field(..., description="角色")
    content: str = Field(..., description="内容")


class OpenAIChatCompletionChunkChoice(BaseModel):
    """流式选项"""
    index: int = Field(..., description="索引")
    delta: Optional[Union[Dict[str, Any], OpenAIChatCompletionChunkMessage]] = Field(
        None, description="Delta数据"
    )
    finish_reason: Optional[str] = Field(None, description="完成原因")


class OpenAIChatCompletionChunkResponse(BaseModel):
    """流式聊天响应"""
    id: str = Field(..., description="响应ID")
    object: str = Field(default="chat.completion.chunk", description="对象类型")
    created: int = Field(..., description="创建时间戳")
    model: str = Field(..., description="模型")
    system_fingerprint: Optional[str] = Field(default=None, description="系统指纹")
    choices: List[OpenAIChatCompletionChunkChoice] = Field(..., description="选项")