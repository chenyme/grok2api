"""
Responses API bridge service (OpenAI-compatible).
"""

import time
import uuid
from typing import Any, AsyncGenerator, Dict, List, Optional

import orjson

from app.services.grok.services.chat import ChatService
from app.services.grok.utils import process as proc_base


_TOOL_OUTPUT_TYPES = {
    "tool_output",
    "function_call_output",
    "tool_call_output",
    "input_tool_output",
}

_BUILTIN_TOOL_TYPES = {
    "web_search",
    "web_search_2025_08_26",
    "file_search",
    "code_interpreter",
}


def _now_ts() -> int:
    return int(time.time())


def _new_response_id() -> str:
    return f"resp_{uuid.uuid4().hex[:24]}"


def _new_message_id() -> str:
    return f"msg_{uuid.uuid4().hex[:24]}"


def _new_tool_call_id() -> str:
    return f"call_{uuid.uuid4().hex[:24]}"


def _new_function_call_id() -> str:
    return f"fc_{uuid.uuid4().hex[:24]}"


def _normalize_tool_choice(tool_choice: Any) -> Any:
    if isinstance(tool_choice, dict):
        t_type = tool_choice.get("type")
        if t_type and t_type != "function":
            return {"type": "function", "function": {"name": t_type}}
    return tool_choice


def _normalize_tools_for_chat(tools: Optional[List[Dict[str, Any]]]) -> Optional[List[Dict[str, Any]]]:
    if not tools:
        return None
    normalized: List[Dict[str, Any]] = []
    for tool in tools:
        if not isinstance(tool, dict):
            continue
        tool_type = tool.get("type")
        if tool_type == "function":
            normalized.append(tool)
            continue
        if tool_type in _BUILTIN_TOOL_TYPES:
            if tool_type.startswith("web_search"):
                normalized.append(
                    {
                        "type": "function",
                        "function": {
                            "name": tool_type,
                            "description": "Search the web for information and return results.",
                            "parameters": {
                                "type": "object",
                                "properties": {"query": {"type": "string"}},
                                "required": ["query"],
                            },
                        },
                    }
                )
            elif tool_type == "file_search":
                normalized.append(
                    {
                        "type": "function",
                        "function": {
                            "name": tool_type,
                            "description": "Search provided files for relevant information.",
                            "parameters": {
                                "type": "object",
                                "properties": {"query": {"type": "string"}},
                                "required": ["query"],
                            },
                        },
                    }
                )
            elif tool_type == "code_interpreter":
                normalized.append(
                    {
                        "type": "function",
                        "function": {
                            "name": tool_type,
                            "description": "Execute code to solve tasks and return results.",
                            "parameters": {
                                "type": "object",
                                "properties": {"code": {"type": "string"}},
                                "required": ["code"],
                            },
                        },
                    }
                )
    return normalized or None


def _content_item_from_input(item: Dict[str, Any]) -> Optional[Dict[str, Any]]:
    if not isinstance(item, dict):
        return None
    item_type = item.get("type")

    if item_type in {"input_text", "text", "output_text"}:
        text = item.get("text") or item.get("content") or ""
        return {"type": "text", "text": text}

    if item_type in {"input_image", "image", "image_url", "output_image"}:
        image_url = item.get("image_url")
        url = ""
        detail = None
        if isinstance(image_url, dict):
            url = image_url.get("url") or ""
            detail = image_url.get("detail")
        elif isinstance(image_url, str):
            url = image_url
        else:
            url = item.get("url") or item.get("image") or ""

        if not url:
            return None
        image_payload = {"url": url}
        if detail:
            image_payload["detail"] = detail
        return {"type": "image_url", "image_url": image_payload}

    if item_type in {"input_file", "file"}:
        file_data = item.get("file_data")
        file_id = item.get("file_id")
        if not file_data and isinstance(item.get("file"), dict):
            file_data = item["file"].get("file_data")
            file_id = item["file"].get("file_id")
        file_payload: Dict[str, Any] = {}
        if file_data:
            file_payload["file_data"] = file_data
        if file_id:
            file_payload["file_id"] = file_id
        if not file_payload:
            return None
        return {"type": "file", "file": file_payload}

    if item_type in {"input_audio", "audio"}:
        audio = item.get("audio") or {}
        data = audio.get("data") or item.get("data")
        if not data:
            return None
        return {"type": "input_audio", "input_audio": {"data": data}}

    return None


def _message_from_item(item: Dict[str, Any]) -> Optional[Dict[str, Any]]:
    if not isinstance(item, dict):
        return None

    if item.get("type") == "message":
        role = item.get("role") or "user"
        content = item.get("content")
        return {"role": role, "content": _coerce_content(content)}

    if "role" in item and "content" in item:
        return {"role": item.get("role") or "user", "content": _coerce_content(item.get("content"))}

    return None


def _coerce_content(content: Any) -> Any:
    if content is None:
        return ""
    if isinstance(content, str):
        return content
    if isinstance(content, dict):
        content = [content]
    if isinstance(content, list):
        blocks: List[Dict[str, Any]] = []
        for item in content:
            if isinstance(item, dict) and item.get("type") in {"input_text", "output_text"}:
                blocks.append({"type": "text", "text": item.get("text", "")})
                continue
            block = _content_item_from_input(item) if isinstance(item, dict) else None
            if block:
                blocks.append(block)
        return blocks if blocks else ""
    return str(content)


def _coerce_input_to_messages(input_value: Any) -> List[Dict[str, Any]]:
    if input_value is None:
        return []
    if isinstance(input_value, str):
        return [{"role": "user", "content": input_value}]

    if isinstance(input_value, dict):
        msg = _message_from_item(input_value)
        if msg:
            return [msg]
        content_item = _content_item_from_input(input_value)
        if content_item:
            return [{"role": "user", "content": [content_item]}]
        return []

    if not isinstance(input_value, list):
        return [{"role": "user", "content": str(input_value)}]

    messages: List[Dict[str, Any]] = []
    pending_blocks: List[Dict[str, Any]] = []

    def _flush_pending():
        nonlocal pending_blocks
        if pending_blocks:
            messages.append({"role": "user", "content": pending_blocks})
            pending_blocks = []

    for item in input_value:
        if isinstance(item, dict):
            msg = _message_from_item(item)
            if msg:
                _flush_pending()
                messages.append(msg)
                continue

            item_type = item.get("type")
            if item_type in _TOOL_OUTPUT_TYPES:
                _flush_pending()
                call_id = (
                    item.get("call_id")
                    or item.get("tool_call_id")
                    or item.get("id")
                    or _new_tool_call_id()
                )
                output = item.get("output") or item.get("content") or ""
                messages.append({"role": "tool", "tool_call_id": call_id, "content": output})
                continue

            block = _content_item_from_input(item)
            if block:
                pending_blocks.append(block)
                continue

        if isinstance(item, str):
            pending_blocks.append({"type": "text", "text": item})

    _flush_pending()
    return messages


def _build_output_message(
    text: str,
    *,
    message_id: Optional[str] = None,
    status: str = "completed",
) -> Dict[str, Any]:
    message_id = message_id or _new_message_id()
    return {
        "id": message_id,
        "type": "message",
        "role": "assistant",
        "status": status,
        "content": [
            {
                "type": "output_text",
                "text": text,
                "annotations": [],
            }
        ],
    }


def _build_output_tool_call(
    tool_call: Dict[str, Any],
    *,
    item_id: Optional[str] = None,
    status: str = "completed",
) -> Dict[str, Any]:
    fn = tool_call.get("function") or {}
    call_id = tool_call.get("id") or _new_tool_call_id()
    item_id = item_id or _new_function_call_id()
    return {
        "id": item_id,
        "type": "function_call",
        "status": status,
        "call_id": call_id,
        "name": fn.get("name"),
        "arguments": fn.get("arguments"),
    }


def _build_response_object(
    *,
    model: str,
    output_text: Optional[str] = None,
    tool_calls: Optional[List[Dict[str, Any]]] = None,
    response_id: Optional[str] = None,
    usage: Optional[Dict[str, Any]] = None,
    created_at: Optional[int] = None,
    completed_at: Optional[int] = None,
    status: str = "completed",
    instructions: Optional[str] = None,
    max_output_tokens: Optional[int] = None,
    parallel_tool_calls: Optional[bool] = None,
    previous_response_id: Optional[str] = None,
    reasoning_effort: Optional[str] = None,
    store: Optional[bool] = None,
    temperature: Optional[float] = None,
    tool_choice: Optional[Any] = None,
    tools: Optional[List[Dict[str, Any]]] = None,
    top_p: Optional[float] = None,
    truncation: Optional[str] = None,
    user: Optional[str] = None,
    metadata: Optional[Dict[str, Any]] = None,
) -> Dict[str, Any]:
    response_id = response_id or _new_response_id()
    created_at = created_at or _now_ts()
    if status == "completed" and completed_at is None:
        completed_at = _now_ts()

    output: List[Dict[str, Any]] = []
    if output_text is not None:
        output.append(_build_output_message(output_text))

    if tool_calls:
        for call in tool_calls:
            output.append(_build_output_tool_call(call))

    return {
        "id": response_id,
        "object": "response",
        "created_at": created_at,
        "completed_at": completed_at,
        "status": status,
        "error": None,
        "incomplete_details": None,
        "instructions": instructions,
        "max_output_tokens": max_output_tokens,
        "model": model,
        "output": output,
        "parallel_tool_calls": True if parallel_tool_calls is None else parallel_tool_calls,
        "previous_response_id": previous_response_id,
        "reasoning": {"effort": reasoning_effort, "summary": None},
        "store": True if store is None else store,
        "temperature": 1.0 if temperature is None else temperature,
        "text": {"format": {"type": "text"}},
        "tool_choice": tool_choice or "auto",
        "tools": tools or [],
        "top_p": 1.0 if top_p is None else top_p,
        "truncation": truncation or "disabled",
        "usage": usage,
        "user": user,
        "metadata": metadata or {},
    }


class ResponseStreamAdapter:
    def __init__(
        self,
        *,
        model: str,
        response_id: str,
        created_at: int,
        instructions: Optional[str],
        max_output_tokens: Optional[int],
        parallel_tool_calls: Optional[bool],
        previous_response_id: Optional[str],
        reasoning_effort: Optional[str],
        store: Optional[bool],
        temperature: Optional[float],
        tool_choice: Optional[Any],
        tools: Optional[List[Dict[str, Any]]],
        top_p: Optional[float],
        truncation: Optional[str],
        user: Optional[str],
        metadata: Optional[Dict[str, Any]],
    ):
        self.model = model
        self.response_id = response_id
        self.created_at = created_at
        self.instructions = instructions
        self.max_output_tokens = max_output_tokens
        self.parallel_tool_calls = parallel_tool_calls
        self.previous_response_id = previous_response_id
        self.reasoning_effort = reasoning_effort
        self.store = store
        self.temperature = temperature
        self.tool_choice = tool_choice
        self.tools = tools
        self.top_p = top_p
        self.truncation = truncation
        self.user = user
        self.metadata = metadata

        self.output_text_parts: List[str] = []
        self.tool_calls_by_index: Dict[int, Dict[str, Any]] = {}
        self.tool_items: Dict[int, Dict[str, Any]] = {}
        self.next_output_index = 0
        self.content_index = 0
        self.message_id = _new_message_id()
        self.message_started = False
        self.message_output_index: Optional[int] = None

    def _event(self, event_type: str, payload: Dict[str, Any]) -> str:
        return f"event: {event_type}\ndata: {orjson.dumps(payload).decode()}\n\n"

    def _response_payload(self, *, status: str, output_text: Optional[str], usage: Optional[Dict[str, Any]] = None) -> Dict[str, Any]:
        tool_calls = None
        if status == "completed" and self.tool_calls_by_index:
            tool_calls = [
                self.tool_calls_by_index[idx]
                for idx in sorted(self.tool_calls_by_index.keys())
            ]
        return _build_response_object(
            model=self.model,
            output_text=output_text,
            tool_calls=tool_calls,
            response_id=self.response_id,
            usage=usage,
            created_at=self.created_at,
            status=status,
            instructions=self.instructions,
            max_output_tokens=self.max_output_tokens,
            parallel_tool_calls=self.parallel_tool_calls,
            previous_response_id=self.previous_response_id,
            reasoning_effort=self.reasoning_effort,
            store=self.store,
            temperature=self.temperature,
            tool_choice=self.tool_choice,
            tools=self.tools,
            top_p=self.top_p,
            truncation=self.truncation,
            user=self.user,
            metadata=self.metadata,
        )

    def _alloc_output_index(self) -> int:
        idx = self.next_output_index
        self.next_output_index += 1
        return idx

    def created_event(self) -> str:
        payload = {
            "type": "response.created",
            "response": self._response_payload(status="in_progress", output_text=None, usage=None),
        }
        return self._event("response.created", payload)

    def in_progress_event(self) -> str:
        payload = {
            "type": "response.in_progress",
            "response": self._response_payload(status="in_progress", output_text=None, usage=None),
        }
        return self._event("response.in_progress", payload)

    def ensure_message_started(self) -> List[str]:
        if self.message_started:
            return []
        self.message_started = True
        self.message_output_index = self._alloc_output_index()
        item = _build_output_message("", message_id=self.message_id, status="in_progress")
        item["content"] = []
        events = [
            self._event(
                "response.output_item.added",
                {
                    "type": "response.output_item.added",
                    "response_id": self.response_id,
                    "output_index": self.message_output_index,
                    "item": item,
                },
            ),
            self._event(
                "response.content_part.added",
                {
                    "type": "response.content_part.added",
                    "response_id": self.response_id,
                    "item_id": self.message_id,
                    "output_index": self.message_output_index,
                    "content_index": self.content_index,
                    "part": {"type": "output_text", "text": "", "annotations": []},
                },
            ),
        ]
        return events

    def output_delta_event(self, delta: str) -> str:
        return self._event(
            "response.output_text.delta",
            {
                "type": "response.output_text.delta",
                "response_id": self.response_id,
                "item_id": self.message_id,
                "output_index": self.message_output_index,
                "content_index": self.content_index,
                "delta": delta,
            },
        )

    def output_done_events(self, text: str) -> List[str]:
        if self.message_output_index is None:
            return []
        return [
            self._event(
                "response.output_text.done",
                {
                    "type": "response.output_text.done",
                    "response_id": self.response_id,
                    "item_id": self.message_id,
                    "output_index": self.message_output_index,
                    "content_index": self.content_index,
                    "text": text,
                },
            ),
            self._event(
                "response.content_part.done",
                {
                    "type": "response.content_part.done",
                    "response_id": self.response_id,
                    "item_id": self.message_id,
                    "output_index": self.message_output_index,
                    "content_index": self.content_index,
                    "part": {"type": "output_text", "text": text, "annotations": []},
                },
            ),
            self._event(
                "response.output_item.done",
                {
                    "type": "response.output_item.done",
                    "response_id": self.response_id,
                    "output_index": self.message_output_index,
                    "item": _build_output_message(
                        text, message_id=self.message_id, status="completed"
                    ),
                },
            ),
        ]

    def ensure_tool_item(self, tool_index: int, call_id: str, name: Optional[str]) -> List[str]:
        if tool_index in self.tool_items:
            item = self.tool_items[tool_index]
            if name and not item.get("name"):
                item["name"] = name
            return []
        output_index = self._alloc_output_index()
        item_id = _new_function_call_id()
        self.tool_items[tool_index] = {
            "item_id": item_id,
            "output_index": output_index,
            "call_id": call_id,
            "name": name,
            "arguments": "",
        }
        tool_item = _build_output_tool_call(
            {"id": call_id, "function": {"name": name, "arguments": ""}},
            item_id=item_id,
            status="in_progress",
        )
        return [
            self._event(
                "response.output_item.added",
                {
                    "type": "response.output_item.added",
                    "response_id": self.response_id,
                    "output_index": output_index,
                    "item": tool_item,
                },
            )
        ]

    def tool_arguments_delta_event(self, tool_index: int, delta: str) -> Optional[str]:
        if not delta:
            return None
        item = self.tool_items.get(tool_index)
        if not item:
            return None
        item["arguments"] += delta
        return self._event(
            "response.function_call_arguments.delta",
            {
                "type": "response.function_call_arguments.delta",
                "response_id": self.response_id,
                "item_id": item["item_id"],
                "output_index": item["output_index"],
                "delta": delta,
            },
        )

    def tool_arguments_done_events(self) -> List[str]:
        events: List[str] = []
        for tool_index, item in sorted(
            self.tool_items.items(), key=lambda kv: kv[1]["output_index"]
        ):
            events.append(
                self._event(
                    "response.function_call_arguments.done",
                    {
                        "type": "response.function_call_arguments.done",
                        "response_id": self.response_id,
                        "item_id": item["item_id"],
                        "output_index": item["output_index"],
                        "arguments": item["arguments"],
                    },
                )
            )
            tool_item = _build_output_tool_call(
                {
                    "id": item["call_id"],
                    "function": {"name": item.get("name"), "arguments": item["arguments"]},
                },
                item_id=item["item_id"],
                status="completed",
            )
            events.append(
                self._event(
                    "response.output_item.done",
                    {
                        "type": "response.output_item.done",
                        "response_id": self.response_id,
                        "output_index": item["output_index"],
                        "item": tool_item,
                    },
                )
            )
        return events

    def record_tool_call(self, tool_index: int, call_id: str, name: Optional[str], arguments_delta: str) -> None:
        tool_call = self.tool_calls_by_index.get(tool_index)
        if not tool_call:
            tool_call = {
                "id": call_id or _new_tool_call_id(),
                "type": "function",
                "function": {"name": name, "arguments": ""},
            }
            self.tool_calls_by_index[tool_index] = tool_call
        if name and not tool_call["function"].get("name"):
            tool_call["function"]["name"] = name
        if arguments_delta:
            tool_call["function"]["arguments"] += arguments_delta

    def completed_event(self, usage: Optional[Dict[str, Any]] = None) -> str:
        response = self._response_payload(
            status="completed",
            output_text="".join(self.output_text_parts) if self.message_started else None,
            usage=usage
            or {"total_tokens": 0, "input_tokens": 0, "output_tokens": 0},
        )
        payload = {"type": "response.completed", "response": response}
        return self._event("response.completed", payload)


class ResponsesService:
    @staticmethod
    async def create(
        *,
        model: str,
        input_value: Any,
        instructions: Optional[str] = None,
        stream: bool = False,
        temperature: Optional[float] = None,
        top_p: Optional[float] = None,
        tools: Optional[List[Dict[str, Any]]] = None,
        tool_choice: Any = None,
        parallel_tool_calls: Optional[bool] = None,
        reasoning_effort: Optional[str] = None,
        max_output_tokens: Optional[int] = None,
        metadata: Optional[Dict[str, Any]] = None,
        user: Optional[str] = None,
        store: Optional[bool] = None,
        previous_response_id: Optional[str] = None,
        truncation: Optional[str] = None,
    ) -> Any:
        messages = _coerce_input_to_messages(input_value)
        if instructions:
            messages = [{"role": "system", "content": instructions}] + messages

        if not messages:
            raise ValueError("input is required")

        normalized_tools = _normalize_tools_for_chat(tools)
        normalized_tool_choice = _normalize_tool_choice(tool_choice)

        chat_kwargs: Dict[str, Any] = {
            "model": model,
            "messages": messages,
            "stream": stream,
        }
        if temperature is not None:
            chat_kwargs["temperature"] = temperature
        if top_p is not None:
            chat_kwargs["top_p"] = top_p
        if normalized_tools is not None:
            chat_kwargs["tools"] = normalized_tools
        if normalized_tool_choice is not None:
            chat_kwargs["tool_choice"] = normalized_tool_choice
        if parallel_tool_calls is not None:
            chat_kwargs["parallel_tool_calls"] = parallel_tool_calls
        if reasoning_effort is not None:
            chat_kwargs["reasoning_effort"] = reasoning_effort

        result = await ChatService.completions(**chat_kwargs)

        if not stream:
            if not isinstance(result, dict):
                raise ValueError("Unexpected stream response for non-stream request")
            choice = (result.get("choices") or [{}])[0]
            message = choice.get("message") or {}
            content = message.get("content") or ""
            tool_calls = message.get("tool_calls")
            return _build_response_object(
                model=model,
                output_text=content,
                tool_calls=tool_calls,
                usage=result.get("usage")
                or {"total_tokens": 0, "input_tokens": 0, "output_tokens": 0},
                status="completed",
                instructions=instructions,
                max_output_tokens=max_output_tokens,
                parallel_tool_calls=parallel_tool_calls,
                previous_response_id=previous_response_id,
                reasoning_effort=reasoning_effort,
                store=store,
                temperature=temperature,
                tool_choice=tool_choice,
                tools=tools,
                top_p=top_p,
                truncation=truncation,
                user=user,
                metadata=metadata,
            )

        if not hasattr(result, "__aiter__"):
            raise ValueError("Unexpected non-stream response for stream request")

        created_at = _now_ts()
        response_id = _new_response_id()
        adapter = ResponseStreamAdapter(
            model=model,
            response_id=response_id,
            created_at=created_at,
            instructions=instructions,
            max_output_tokens=max_output_tokens,
            parallel_tool_calls=parallel_tool_calls,
            previous_response_id=previous_response_id,
            reasoning_effort=reasoning_effort,
            store=store,
            temperature=temperature,
            tool_choice=tool_choice,
            tools=tools,
            top_p=top_p,
            truncation=truncation,
            user=user,
            metadata=metadata,
        )

        async def _stream() -> AsyncGenerator[str, None]:
            yield adapter.created_event()
            yield adapter.in_progress_event()
            async for chunk in result:
                line = proc_base._normalize_line(chunk)
                if not line:
                    continue
                try:
                    data = orjson.loads(line)
                except orjson.JSONDecodeError:
                    continue

                if data.get("object") == "chat.completion.chunk":
                    delta = (data.get("choices") or [{}])[0].get("delta") or {}
                    if "content" in delta and delta["content"]:
                        for event in adapter.ensure_message_started():
                            yield event
                        adapter.output_text_parts.append(delta["content"])
                        yield adapter.output_delta_event(delta["content"])
                    tool_calls = delta.get("tool_calls")
                    if isinstance(tool_calls, list):
                        for tool in tool_calls:
                            if not isinstance(tool, dict):
                                continue
                            tool_index = tool.get("index", 0)
                            call_id = tool.get("id") or _new_tool_call_id()
                            fn = tool.get("function") or {}
                            name = fn.get("name")
                            args_delta = fn.get("arguments") or ""
                            adapter.record_tool_call(
                                tool_index, call_id, name, args_delta
                            )
                            for event in adapter.ensure_tool_item(
                                tool_index, call_id, name
                            ):
                                yield event
                            delta_event = adapter.tool_arguments_delta_event(
                                tool_index, args_delta
                            )
                            if delta_event:
                                yield delta_event

            full_text = "".join(adapter.output_text_parts)
            if full_text and adapter.message_started:
                for event in adapter.output_done_events(full_text):
                    yield event
            for event in adapter.tool_arguments_done_events():
                yield event
            yield adapter.completed_event()

        return _stream()


__all__ = ["ResponsesService"]
