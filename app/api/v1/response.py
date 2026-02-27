"""
Responses API 路由 (OpenAI compatible).
"""

from typing import Any, Dict, List, Optional, Union

from fastapi import APIRouter
from fastapi.responses import JSONResponse, StreamingResponse
from pydantic import BaseModel, Field

from app.core.exceptions import ValidationException
from app.services.grok.services.responses import ResponsesService


router = APIRouter(tags=["Responses"])


class ResponseCreateRequest(BaseModel):
    model: str = Field(..., description="Model name")
    input: Optional[Any] = Field(None, description="Input content")
    instructions: Optional[str] = Field(None, description="System instructions")
    stream: Optional[bool] = Field(False, description="Stream response")
    max_output_tokens: Optional[int] = Field(None, description="Max output tokens")
    temperature: Optional[float] = Field(None, description="Sampling temperature")
    top_p: Optional[float] = Field(None, description="Nucleus sampling")
    tools: Optional[List[Dict[str, Any]]] = Field(None, description="Tool definitions")
    tool_choice: Optional[Union[str, Dict[str, Any]]] = Field(None, description="Tool choice")
    parallel_tool_calls: Optional[bool] = Field(True, description="Allow parallel tool calls")
    reasoning: Optional[Dict[str, Any]] = Field(None, description="Reasoning options")
    metadata: Optional[Dict[str, Any]] = Field(None, description="Metadata")
    user: Optional[str] = Field(None, description="User identifier")
    store: Optional[bool] = Field(None, description="Store response")
    previous_response_id: Optional[str] = Field(None, description="Previous response id")
    truncation: Optional[str] = Field(None, description="Truncation behavior")

    class Config:
        extra = "allow"


@router.post("/responses")
async def create_response(request: ResponseCreateRequest):
    if not request.model:
        raise ValidationException(message="model is required", param="model", code="invalid_request_error")

    if request.input is None:
        raise ValidationException(message="input is required", param="input", code="invalid_request_error")

    reasoning_effort = None
    if isinstance(request.reasoning, dict):
        reasoning_effort = request.reasoning.get("effort") or request.reasoning.get("reasoning_effort")

    result = await ResponsesService.create(
        model=request.model,
        input_value=request.input,
        instructions=request.instructions,
        stream=bool(request.stream),
        temperature=request.temperature,
        top_p=request.top_p,
        tools=request.tools,
        tool_choice=request.tool_choice,
        parallel_tool_calls=request.parallel_tool_calls,
        reasoning_effort=reasoning_effort,
        max_output_tokens=request.max_output_tokens,
        metadata=request.metadata,
        user=request.user,
        store=request.store,
        previous_response_id=request.previous_response_id,
        truncation=request.truncation,
    )

    if request.stream:
        return StreamingResponse(
            result,
            media_type="text/event-stream",
            headers={"Cache-Control": "no-cache", "Connection": "keep-alive"},
        )

    return JSONResponse(content=result)


__all__ = ["router"]
