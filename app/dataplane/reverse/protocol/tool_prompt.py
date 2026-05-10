"""Tool call prompt injection — convert OpenAI tools definitions into a
system-level instruction block that guides the model to output tool calls
in a structured XML format we can reliably parse.
"""

from __future__ import annotations

import json
from typing import Any

# ---------------------------------------------------------------------------
# Instruction template
# ---------------------------------------------------------------------------

_TOOL_SYSTEM_HEADER = """\
You have access to the following tools.

AVAILABLE TOOLS:
{tool_definitions}

TOOL CALL FORMAT — follow these rules exactly:
- When calling a tool, output ONLY the XML block below. No text before or after it.
- <parameters> must be a single-line valid JSON object (no line breaks inside).
- Place multiple tool calls inside ONE <tool_calls> element.
- Do NOT use markdown code fences around the XML.
- Do NOT output any inner monologue or explanation alongside the XML.

<tool_calls>
  <tool_call>
    <tool_name>TOOL_NAME</tool_name>
    <parameters>{{"key": "value"}}</parameters>
  </tool_call>
</tool_calls>

WRONG (never do this):
```xml
<tool_calls>...</tool_calls>
```
I'll call the search tool now. <tool_calls>...</tool_calls>

{tool_choice_instruction}
NOTE: Even if you believe you cannot fulfill the request, you must still follow the WHEN TO CALL rule above.\
"""

_CODEX_TOOL_NOTE = """

CODEX TOOL RULES:
- If the user asks you to inspect local files, list directories, run shell/bash commands, edit files, apply patches, or verify tests, you MUST call the matching tool instead of describing what you would do.
- For shell/bash/terminal commands, use exec_command with a JSON object containing at least {"cmd": "..."}.
- For interactive or long-running shell sessions, use write_stdin with the existing session_id when you need to send input or poll output. Do not replace write_stdin with exec_command.
- For file edits, use exec_command to run apply_patch with a heredoc. Do not write files with echo, printf, cat, tee, sed -i, or Python.
- Example edit command: apply_patch <<'PATCH'\n*** Begin Patch\n*** Add File: example.txt\n+hello\n*** End Patch\nPATCH
- If apply_patch fails, inspect the target file and retry with a corrected, minimal patch. Do not repeat the same failed patch unchanged.
- Do not invent results from the filesystem, web, or tools. Call the tool and wait for the tool result.
- Do not mention unavailable tool names. Use exactly one of the AVAILABLE TOOLS names.
"""

_CHOICE_AUTO     = "WHEN TO CALL: Call a tool when it is clearly needed. Otherwise respond in plain text."
_CHOICE_NONE     = "WHEN TO CALL: Do NOT call any tools. Respond in plain text only."
_CHOICE_REQUIRED = "WHEN TO CALL: You MUST output a <tool_calls> XML block. Do NOT write any plain-text reply. If you are uncertain, still call the most relevant tool with your best guess at the parameters."
_CHOICE_FORCED   = "WHEN TO CALL: You MUST output a <tool_calls> XML block calling the tool named \"{name}\". Do NOT write any plain-text reply under any circumstances."


# ---------------------------------------------------------------------------
# Public API
# ---------------------------------------------------------------------------

def build_tool_system_prompt(
    tools: list[dict[str, Any]],
    tool_choice: Any = None,
) -> str:
    """Return the full system-level instruction block to inject into the prompt.

    Args:
        tools: OpenAI-format tool definitions (list of {type, function:{name,description,parameters}}).
        tool_choice: OpenAI tool_choice value — "auto" | "none" | "required" |
                     {"type": "function", "function": {"name": "..."}}
    """
    tool_defs = _format_tool_definitions(tools)
    choice_instruction = _build_choice_instruction(tools, tool_choice)
    prompt = _TOOL_SYSTEM_HEADER.format(
        tool_definitions=tool_defs,
        tool_choice_instruction=choice_instruction,
    )
    if _looks_like_codex_tools(tools):
        prompt += _CODEX_TOOL_NOTE
    return prompt


def extract_tool_names(tools: list[dict[str, Any]]) -> list[str]:
    """Return the list of function names from an OpenAI tools array."""
    names: list[str] = []
    for tool in tools:
        func = tool.get("function") or {}
        name = func.get("name", "").strip()
        if name:
            names.append(name)
    return names


def inject_into_message(message: str, system_prompt: str) -> str:
    """Prepend the tool system prompt to the flattened message string."""
    return (
        f"[system]: {system_prompt}\n\n"
        f"{message}\n\n"
        "[system]: If a tool is needed now, your next response must be only the "
        "<tool_calls> XML block. Do not say that you will call a tool; call it."
    )


def tool_calls_to_xml(tool_calls: list[dict[str, Any]]) -> str:
    """Convert an OpenAI tool_calls array back into the XML format we use in
    prompts, so multi-turn conversations reconstruct context correctly."""
    lines = ["<tool_calls>"]
    for tc in tool_calls:
        func = tc.get("function") or {}
        name = func.get("name", "")
        args = func.get("arguments", "{}")
        # Normalise to single-line JSON
        try:
            args = json.dumps(json.loads(args), ensure_ascii=False, separators=(",", ":"))
        except (json.JSONDecodeError, ValueError, TypeError):
            pass
        lines.append("  <tool_call>")
        lines.append(f"    <tool_name>{name}</tool_name>")
        lines.append(f"    <parameters>{args}</parameters>")
        lines.append("  </tool_call>")
    lines.append("</tool_calls>")
    return "\n".join(lines)


# ---------------------------------------------------------------------------
# Internals
# ---------------------------------------------------------------------------

def _format_tool_definitions(tools: list[dict[str, Any]]) -> str:
    parts: list[str] = []
    for tool in tools:
        func = tool.get("function") or {}
        name = func.get("name", "").strip()
        desc = (func.get("description") or "").strip()
        params = func.get("parameters")

        lines: list[str] = []
        lines.append(f"Tool: {name}")
        if desc:
            lines.append(f"Description: {desc}")
        if params:
            try:
                lines.append(f"Parameters: {json.dumps(params, ensure_ascii=False)}")
            except (TypeError, ValueError):
                lines.append(f"Parameters: {params}")
        parts.append("\n".join(lines))
    return "\n\n".join(parts)


def _build_choice_instruction(
    tools: list[dict[str, Any]],
    tool_choice: Any,
) -> str:
    if tool_choice is None or tool_choice == "auto":
        return _CHOICE_AUTO
    if tool_choice == "none":
        return _CHOICE_NONE
    if tool_choice == "required":
        return _CHOICE_REQUIRED
    # Object form: {"type": "function", "function": {"name": "..."}}
    if isinstance(tool_choice, dict):
        tc_type = tool_choice.get("type", "")
        if tc_type == "none":
            return _CHOICE_NONE
        if tc_type == "required":
            return _CHOICE_REQUIRED
        if tc_type == "function":
            forced_name = (tool_choice.get("function") or {}).get("name", "").strip()
            if forced_name:
                return _CHOICE_FORCED.format(name=forced_name)
    return _CHOICE_AUTO


def _looks_like_codex_tools(tools: list[dict[str, Any]]) -> bool:
    names = set(extract_tool_names(tools))
    return bool({"exec_command", "apply_patch", "write_stdin"} & names)
