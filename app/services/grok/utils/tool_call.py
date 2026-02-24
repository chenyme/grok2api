"""
Tool call utilities for OpenAI-compatible function calling.

Provides prompt-based emulation of tool calls by injecting tool definitions
into the system prompt and parsing structured responses.
"""

import json
import re
import uuid
from typing import Any, Dict, List, Optional, Tuple


def build_tool_prompt(
    tools: List[Dict[str, Any]],
    tool_choice: Optional[Any] = None,
    parallel_tool_calls: bool = True,
) -> str:
    """Generate a system prompt block describing available tools.

    Args:
        tools: List of OpenAI-format tool definitions.
        tool_choice: "auto", "required", "none", or {"type":"function","function":{"name":"..."}}.
        parallel_tool_calls: Whether multiple tool calls are allowed.

    Returns:
        System prompt string to prepend to the conversation.
    """
    if not tools:
        return ""

    # tool_choice="none" means don't mention tools at all
    if tool_choice == "none":
        return ""

    lines = [
        "# Available Tools",
        "",
        "You have access to the following tools. To call a tool, output a <tool_call> block with a JSON object containing \"name\" and \"arguments\".",
        "",
        "Format:",
        "<tool_call>",
        '{"name": "function_name", "arguments": {"param": "value"}}',
        "</tool_call>",
        "",
    ]

    if parallel_tool_calls:
        lines.append("You may make multiple tool calls in a single response by using multiple <tool_call> blocks.")
        lines.append("")

    # Describe each tool
    lines.append("## Tool Definitions")
    lines.append("")
    for tool in tools:
        if tool.get("type") != "function":
            continue
        func = tool.get("function", {})
        name = func.get("name", "")
        desc = func.get("description", "")
        params = func.get("parameters", {})

        lines.append(f"### {name}")
        if desc:
            lines.append(f"{desc}")
        if params:
            lines.append(f"Parameters: {json.dumps(params, ensure_ascii=False)}")
        lines.append("")

    # Handle tool_choice directives
    if tool_choice == "required":
        lines.append("IMPORTANT: You MUST call at least one tool in your response. Do not respond with only text.")
    elif isinstance(tool_choice, dict):
        func_info = tool_choice.get("function", {})
        forced_name = func_info.get("name", "")
        if forced_name:
            lines.append(f"IMPORTANT: You MUST call the tool \"{forced_name}\" in your response.")
    else:
        # "auto" or default
        lines.append("Decide whether to call a tool based on the user's request. If you don't need a tool, respond normally with text only.")

    lines.append("")
    lines.append("When you call a tool, you may include text before or after the <tool_call> blocks, but the tool call blocks must be valid JSON.")

    return "\n".join(lines)


_TOOL_CALL_RE = re.compile(
    r"<tool_call>\s*(.*?)\s*</tool_call>",
    re.DOTALL,
)


def parse_tool_calls(
    content: str,
    tools: Optional[List[Dict[str, Any]]] = None,
) -> Tuple[Optional[str], Optional[List[Dict[str, Any]]]]:
    """Parse tool call blocks from model output.

    Detects ``<tool_call>...</tool_call>`` blocks, parses JSON from each block,
    and returns OpenAI-format tool call objects.

    Args:
        content: Raw model output text.
        tools: Optional list of tool definitions for name validation.

    Returns:
        Tuple of (text_content, tool_calls_list).
        - text_content: text outside <tool_call> blocks (None if empty).
        - tool_calls_list: list of OpenAI tool call dicts, or None if no calls found.
    """
    if not content:
        return content, None

    matches = list(_TOOL_CALL_RE.finditer(content))
    if not matches:
        return content, None

    # Build set of valid tool names for validation
    valid_names = set()
    if tools:
        for tool in tools:
            func = tool.get("function", {})
            name = func.get("name")
            if name:
                valid_names.add(name)

    tool_calls = []
    for match in matches:
        raw_json = match.group(1).strip()
        try:
            parsed = json.loads(raw_json)
        except json.JSONDecodeError:
            continue

        if not isinstance(parsed, dict):
            continue

        name = parsed.get("name")
        arguments = parsed.get("arguments", {})

        if not name:
            continue

        # Validate against known tools if provided
        if valid_names and name not in valid_names:
            continue

        # Ensure arguments is a JSON string (OpenAI format)
        if isinstance(arguments, dict):
            arguments_str = json.dumps(arguments, ensure_ascii=False)
        elif isinstance(arguments, str):
            arguments_str = arguments
        else:
            arguments_str = json.dumps(arguments, ensure_ascii=False)

        tool_calls.append({
            "id": f"call_{uuid.uuid4().hex[:24]}",
            "type": "function",
            "function": {
                "name": name,
                "arguments": arguments_str,
            },
        })

    if not tool_calls:
        return content, None

    # Extract text outside of tool_call blocks
    text_parts = []
    last_end = 0
    for match in matches:
        before = content[last_end:match.start()]
        if before.strip():
            text_parts.append(before.strip())
        last_end = match.end()
    trailing = content[last_end:]
    if trailing.strip():
        text_parts.append(trailing.strip())

    text_content = "\n".join(text_parts) if text_parts else None

    return text_content, tool_calls


def build_tool_overrides(tools: List[Dict[str, Any]]) -> Dict[str, Any]:
    """Convert OpenAI tool format to Grok's toolOverrides format (experimental).

    Best-effort mapping for passthrough mode.

    Args:
        tools: List of OpenAI-format tool definitions.

    Returns:
        Dict suitable for the toolOverrides field in Grok API payload.
    """
    if not tools:
        return {}

    tool_overrides = {}
    for tool in tools:
        if tool.get("type") != "function":
            continue
        func = tool.get("function", {})
        name = func.get("name", "")
        if not name:
            continue
        tool_overrides[name] = {
            "enabled": True,
            "description": func.get("description", ""),
            "parameters": func.get("parameters", {}),
        }

    return tool_overrides


def format_tool_history(messages: List[Dict[str, Any]]) -> List[Dict[str, Any]]:
    """Convert assistant messages with tool_calls and tool role messages into text format.

    Since Grok's web API only accepts a single message string, this converts
    tool-related messages back to a text representation for multi-turn conversations.

    Args:
        messages: List of OpenAI-format messages that may contain tool_calls and tool roles.

    Returns:
        List of messages with tool content converted to text format.
    """
    result = []
    for msg in messages:
        role = msg.get("role", "")
        content = msg.get("content")
        tool_calls = msg.get("tool_calls")
        tool_call_id = msg.get("tool_call_id")
        name = msg.get("name")

        if role == "assistant" and tool_calls:
            # Convert assistant tool_calls to text representation
            parts = []
            if content:
                parts.append(content if isinstance(content, str) else str(content))
            for tc in tool_calls:
                func = tc.get("function", {})
                tc_name = func.get("name", "")
                tc_args = func.get("arguments", "{}")
                tc_id = tc.get("id", "")
                parts.append(f'<tool_call>{{"name":"{tc_name}","arguments":{tc_args}}}</tool_call>')
            result.append({
                "role": "assistant",
                "content": "\n".join(parts),
            })

        elif role == "tool":
            # Convert tool result to text format
            tool_name = name or "unknown"
            call_id = tool_call_id or ""
            content_str = content if isinstance(content, str) else json.dumps(content, ensure_ascii=False) if content else ""
            result.append({
                "role": "user",
                "content": f"tool ({tool_name}, {call_id}): {content_str}",
            })

        else:
            result.append(msg)

    return result


__all__ = [
    "build_tool_prompt",
    "parse_tool_calls",
    "build_tool_overrides",
    "format_tool_history",
]
