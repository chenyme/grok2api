"""OpenAI-compatible request schemas (Pydantic models)."""

from typing import Any, Literal

from pydantic import BaseModel, Field


class MessageItem(BaseModel):
    role:         str
    content:      str | list[dict[str, Any]] | None = None
    tool_calls:   list[dict[str, Any]] | None       = None
    tool_call_id: str | None                        = None
    name:         str | None                        = None


class ImageConfig(BaseModel):
    n:               int | None = Field(1, ge=1, le=10)
    size:            str | None = "1024x1024"
    response_format: str | None = None


class VideoConfig(BaseModel):
    seconds: int | None = 6
    size: Literal["720x1280", "1280x720", "1024x1024", "1024x1792", "1792x1024"] | None = "720x1280"
    resolution_name: Literal["480p", "720p"] | None = None
    preset: Literal["fun", "normal", "spicy", "custom"] | None = None


class ChatCompletionRequest(BaseModel):
    model:               str
    messages:            list[MessageItem]
    stream:              bool | None                = None
    thinking:            bool | None                = None
    reasoning_effort:    str | None                 = None
    temperature:         float | None               = 0.8
    top_p:               float | None               = 0.95
    image_config:        ImageConfig | None         = None
    video_config:        VideoConfig | None         = None
    tools:               list[dict[str, Any]] | None = None
    tool_choice:         str | dict[str, Any] | None = None
    parallel_tool_calls: bool | None                = True
    max_tokens:          int | None                 = None
    deepsearch:          Literal["default", "deeper"] | None = Field(
        None, description="深度搜索预设: default/deeper"
    )


class ImageGenerationRequest(BaseModel):
    model:           str
    prompt:          str
    n:               int | None = Field(1, ge=1, le=10)
    size:            str | None = "1024x1024"
    response_format: str | None = "url"


class ImageEditRequest(BaseModel):
    model:           str
    prompt:          str
    image:           str | list[str]
    mask:            str | None = None
    n:               int | None = Field(1, ge=1, le=2)
    size:            str | None = "1024x1024"
    response_format: str | None = "url"


class ResponsesCreateRequest(BaseModel):
    """OpenAI Responses API — /v1/responses.

    Only model/input/instructions/stream/reasoning/temperature/top_p are acted on.
    All other fields are accepted and silently discarded.
    """
    model:                str
    input:                str | list[Any]
    instructions:         str | None           = None
    stream:               bool | None          = None
    reasoning:            dict[str, Any] | None = None
    temperature:          float | None         = None
    top_p:                float | None         = None
    # silently ignored
    max_output_tokens:    int | None            = None
    tools:                list[Any] | None      = None
    tool_choice:          Any | None            = None
    previous_response_id: str | None            = None
    store:                bool | None           = None
    metadata:             dict[str, Any] | None = None
    truncation:           str | None            = None
    parallel_tool_calls:  bool | None           = None
    include:              list[str] | None      = None
    background:           bool | None           = None

    class Config:
        extra = "ignore"


__all__ = [
    "MessageItem", "ImageConfig", "VideoConfig",
    "ChatCompletionRequest", "ImageGenerationRequest", "ImageEditRequest",
    "ResponsesCreateRequest",
]
