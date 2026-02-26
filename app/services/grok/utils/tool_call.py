"""
Tool call utilities for OpenAI-compatible function calling.

Provides prompt-based emulation of tool calls by injecting tool definitions
into the system prompt and parsing structured responses.
"""

import json
import re
import uuid
from typing import Any, Dict, List, Optional, Tuple


# ---------------------------------------------------------------------------
# JSON repair helpers
# ---------------------------------------------------------------------------

def _escape_string_whitespace(s: str) -> str:
    """Escape literal control characters within JSON string values."""
    result = []
    in_string = False
    escape_next = False
    for char in s:
        if escape_next:
            result.append(char)
            escape_next = False
        elif char == "\\":
            result.append(char)
            escape_next = True
        elif char == '"':
            result.append(char)
            in_string = not in_string
        elif in_string and char == "\n":
            result.append("\\n")
        elif in_string and char == "\r":
            result.append("\\r")
        elif in_string and char == "\t":
            result.append("\\t")
        else:
            result.append(char)
    return "".join(result)


def _complete_brackets(s: str) -> str:
    """Auto-complete mismatched opening brackets."""
    stack: List[str] = []
    in_string = False
    escape_next = False
    for char in s:
        if escape_next:
            escape_next = False
            continue
        if char == "\\" and in_string:
            escape_next = True
            continue
        if char == '"':
            in_string = not in_string
            continue
        if not in_string:
            if char in ("{", "["):
                stack.append(char)
            elif char == "}" and stack and stack[-1] == "{":
                stack.pop()
            elif char == "]" and stack and stack[-1] == "[":
                stack.pop()
    while stack:
        opener = stack.pop()
        s += "}" if opener == "{" else "]"
    return s


def repair_json(s: str) -> str:
    """Attempt to repair malformed JSON from model output.

    Applies six sequential strategies modelled after cc-proxy's repairJson:
    1. Extract content between first ``{`` and last ``}``
    2. Remove trailing commas before ``}`` / ``]``
    3. Escape literal newlines / carriage-returns / tabs inside string values
    4. Quote unquoted object keys
    5. Un-quote over-quoted boolean / null values
    6. Auto-complete mismatched brackets
    """
    fixed = s.strip()
    if not fixed:
        return fixed

    # Strategy 1: extract valid JSON range
    first_brace = fixed.find("{")
    last_brace = fixed.rfind("}")
    if first_brace != -1 and last_brace > first_brace:
        fixed = fixed[first_brace : last_brace + 1]

    # Strategy 2: trailing commas
    fixed = re.sub(r",\s*([\}\]])", r"\1", fixed)

    # Strategy 3: escape literal whitespace in strings
    fixed = _escape_string_whitespace(fixed)

    # Strategy 4: quote unquoted keys
    fixed = re.sub(r"([{,]\s*)([a-zA-Z_][a-zA-Z0-9_]*)\s*:", r'\1"\2":', fixed)

    # Strategy 5: un-quote boolean / null values
    fixed = re.sub(
        r':\s*"(true|false|null)"',
        lambda m: f": {m.group(1).lower()}",
        fixed,
        flags=re.IGNORECASE,
    )

    # Strategy 6: complete mismatched brackets
    fixed = _complete_brackets(fixed)

    return fixed


# ---------------------------------------------------------------------------
# Streaming tool-call parser
# ---------------------------------------------------------------------------

class StreamingToolParser:
    """True streaming parser for ``<tool_call>`` blocks.

    Implements a two-state machine (TEXT → TOOL_BUFFERING) so that:

    * Text *outside* tool-call blocks is yielded to the client immediately.
    * Only the content *inside* ``<tool_call>…</tool_call>`` is buffered.

    Feed chunks via :meth:`feed` and call :meth:`flush` at end-of-stream.
    Each returns a list of events::

        {"type": "text",      "content": "..."}
        {"type": "tool_call", "data":    {...}}   # OpenAI tool-call dict
    """

    TOOL_START = "<tool_call>"
    TOOL_END = "</tool_call>"

    def __init__(self, tools: Optional[List[Dict[str, Any]]] = None) -> None:
        self._state = "TEXT"   # TEXT | TOOL_BUFFERING
        self._pending = ""     # Unprocessed input
        self._tool_buf = ""    # Accumulates current tool call body
        self._valid_names: Optional[set] = None
        if tools:
            self._valid_names = {
                t["function"]["name"]
                for t in tools
                if t.get("type") == "function" and t.get("function", {}).get("name")
            }

    def feed(self, chunk: str) -> List[Dict[str, Any]]:
        """Feed a text chunk; returns a list of events."""
        events: List[Dict[str, Any]] = []
        self._pending += chunk
        self._process(events)
        return events

    def flush(self) -> List[Dict[str, Any]]:
        """Flush remaining buffer at end of stream; returns remaining events."""
        events: List[Dict[str, Any]] = []
        if self._state == "TOOL_BUFFERING":
            # Incomplete tool call — surface as raw text
            raw = f"{self.TOOL_START}{self._tool_buf}{self._pending}"
            if raw:
                events.append({"type": "text", "content": raw})
        elif self._pending:
            events.append({"type": "text", "content": self._pending})
        self._pending = ""
        self._tool_buf = ""
        self._state = "TEXT"
        return events

    def _process(self, events: List[Dict[str, Any]]) -> None:
        while True:
            if self._state == "TEXT":
                idx = self._pending.find(self.TOOL_START)
                if idx == -1:
                    # No marker; emit everything except the last (marker-1) chars
                    safe = len(self._pending) - (len(self.TOOL_START) - 1)
                    if safe > 0:
                        events.append({"type": "text", "content": self._pending[:safe]})
                        self._pending = self._pending[safe:]
                    break
                # Marker found
                if idx > 0:
                    events.append({"type": "text", "content": self._pending[:idx]})
                self._pending = self._pending[idx + len(self.TOOL_START):]
                self._state = "TOOL_BUFFERING"
                self._tool_buf = ""

            else:  # TOOL_BUFFERING
                idx = self._pending.find(self.TOOL_END)
                if idx == -1:
                    safe = len(self._pending) - (len(self.TOOL_END) - 1)
                    if safe > 0:
                        self._tool_buf += self._pending[:safe]
                        self._pending = self._pending[safe:]
                    break
                # End marker found
                self._tool_buf += self._pending[:idx]
                self._pending = self._pending[idx + len(self.TOOL_END):]
                self._state = "TEXT"

                tool_call = self._parse(self._tool_buf.strip())
                if tool_call:
                    events.append({"type": "tool_call", "data": tool_call})
                else:
                    raw = f"{self.TOOL_START}{self._tool_buf}{self.TOOL_END}"
                    events.append({"type": "text", "content": raw})
                self._tool_buf = ""

    def _parse(self, raw_json: str) -> Optional[Dict[str, Any]]:
        """Parse a tool call JSON blob, falling back to repair_json on failure."""
        parsed = None
        try:
            parsed = json.loads(raw_json)
        except (json.JSONDecodeError, ValueError):
            repaired = repair_json(raw_json)
            try:
                parsed = json.loads(repaired)
            except (json.JSONDecodeError, ValueError):
                return None

        if not isinstance(parsed, dict):
            return None

        name = parsed.get("name")
        if not name:
            return None
        if self._valid_names and name not in self._valid_names:
            return None

        arguments = parsed.get("arguments", {})
        if isinstance(arguments, dict):
            arguments_str = json.dumps(arguments, ensure_ascii=False)
        elif isinstance(arguments, str):
            arguments_str = arguments
        else:
            arguments_str = json.dumps(arguments, ensure_ascii=False)

        return {
            "id": f"call_{uuid.uuid4().hex[:24]}",
            "type": "function",
            "function": {"name": name, "arguments": arguments_str},
        }


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
        except (json.JSONDecodeError, ValueError):
            repaired = repair_json(raw_json)
            try:
                parsed = json.loads(repaired)
            except (json.JSONDecodeError, ValueError):
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
    "repair_json",
    "StreamingToolParser",
    "build_tool_prompt",
    "parse_tool_calls",
    "build_tool_overrides",
    "format_tool_history",
]
