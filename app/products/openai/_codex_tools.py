"""Codex-oriented Responses API tool fallback helpers."""

from __future__ import annotations

import re
import shlex
from typing import Any

import orjson

from app.dataplane.reverse.protocol.tool_parser import ParsedToolCall
from app.platform.logging.logger import logger


# ---------------------------------------------------------------------------
# Tool format normalisation
# ---------------------------------------------------------------------------

def _to_chat_tools(tools: list[dict]) -> list[dict]:
    """Normalise Responses API tool format → Chat Completions format.

    Responses API:  {type, name, description, parameters}       (flat)
    Chat Completions: {type, function: {name, description, parameters}}

    Already-wrapped tools are passed through unchanged so this is safe to
    call regardless of which format the caller used.
    """
    normalised = []
    for tool in tools:
        if not isinstance(tool, dict):
            continue
        if tool.get("type") == "function" and "function" not in tool and "name" in tool:
            normalised.append({
                "type": "function",
                "function": {
                    "name":        tool.get("name", ""),
                    "description": tool.get("description", ""),
                    "parameters":  tool.get("parameters"),
                },
            })
        elif tool.get("type") == "function":
            normalised.append(tool)
        elif "name" in tool and "parameters" in tool:
            # Some Responses clients send function-shaped tools with a custom
            # type.  Grok only sees the prompt, so normalize the callable
            # schema and ignore non-callable provider metadata.
            normalised.append({
                "type": "function",
                "function": {
                    "name":        tool.get("name", ""),
                    "description": tool.get("description", ""),
                    "parameters":  tool.get("parameters"),
                },
            })
        else:
            # Skip namespace/web_search/image tools here. They require native
            # Responses semantics; passing them as fake functions confuses the
            # model and Codex will not execute them as function_call items.
            continue
    return normalised


_LOCAL_TOOL_REQUEST_RE = re.compile(
    r"shell|bash|terminal|command|exec_command|apply_patch|write_stdin|"
    r"\bpwd\b|\bls\b|\brg\b|\bcat\b|\bsed\b|\bfind\b|"
    r"工具|调用|运行|执行|命令|终端|查看|读取|列出|文件|目录|修改|编辑",
    re.IGNORECASE,
)
_WRITE_STDIN_REQUEST_RE = re.compile(
    r"write_stdin|stdin|send input|send .* to session|poll output|session_id|"
    r"输入到.*会话|发送.*会话|轮询|长进程|交互",
    re.IGNORECASE,
)
_TOOL_RESULT_RE = re.compile(
    r"\[tool result|function_call_output|tool_call_id|session_id|patch:\s*(failed|completed)",
    re.IGNORECASE,
)
_CODEX_COMMAND_OUTPUT_RE = re.compile(
    r"command_execution|aggregated_output|exit_code|Process exited with code|"
    r"Chunk ID:|Wall time:|Original token count:",
    re.IGNORECASE,
)
_PATCH_COMPLETED_RE = re.compile(r"patch:\s*completed", re.IGNORECASE)
_PATCH_FAILURE_RE = re.compile(
    r"apply_patch|patch\b|补丁|diff",
    re.IGNORECASE,
)


def _looks_like_codex_tool_run(tool_names: list[str], message: str, text: str) -> bool:
    if not tool_names:
        return False
    if any(name and name in text for name in tool_names):
        return True
    if "exec_command" in tool_names and _LOCAL_TOOL_REQUEST_RE.search(message):
        return True
    return bool(_LOCAL_TOOL_REQUEST_RE.search(text))


def _forced_tool_choice(tool_names: list[str], message: str, text: str) -> Any:
    if "write_stdin" in tool_names and _WRITE_STDIN_REQUEST_RE.search(f"{message}\n{text}"):
        return {"type": "function", "function": {"name": "write_stdin"}}
    if "exec_command" in tool_names and (
        _LOCAL_TOOL_REQUEST_RE.search(message) or "exec_command" in text
    ):
        return {"type": "function", "function": {"name": "exec_command"}}
    if "apply_patch" in tool_names and (
        "apply_patch" in text or re.search(r"修改|编辑|patch|补丁", message, re.I)
    ):
        return {"type": "function", "function": {"name": "apply_patch"}}
    return "required"


def _synthesize_codex_tool_call(
    tool_names: list[str],
    message: str,
    previous_text: str,
) -> list[ParsedToolCall]:
    if "write_stdin" in tool_names:
        stdin_args = _synthesize_write_stdin_args(message, previous_text)
        if stdin_args:
            return [ParsedToolCall.make("write_stdin", stdin_args)]
    if "exec_command" not in tool_names:
        return []
    intent = _latest_user_intent(message)
    if _has_prior_tool_result(intent) or _has_prior_command_output(message):
        return []
    patch_cmd = _synthesize_apply_patch_command(intent)
    if patch_cmd:
        return [ParsedToolCall.make("exec_command", {"cmd": patch_cmd})]
    cmd = _extract_requested_shell_command(intent, previous_text)
    if not cmd:
        return []
    return [ParsedToolCall.make("exec_command", {"cmd": cmd})]


def _normalize_codex_tool_calls(
    calls: list[ParsedToolCall],
    *,
    tool_names: list[str],
    message: str,
) -> list[ParsedToolCall]:
    if "exec_command" not in tool_names:
        return calls
    normalized: list[ParsedToolCall] = []
    for call in calls:
        if call.name != "exec_command":
            normalized.append(call)
            continue
        args = _json_args(call.arguments)
        cmd = str(args.get("cmd", "")).strip()
        if _is_duplicate_completed_patch(cmd, message):
            logger.info("responses suppressed duplicate completed apply_patch command")
            continue
        if _looks_like_direct_file_write(cmd) and _looks_like_edit_request(message):
            patch_cmd = _command_to_apply_patch(cmd) or _synthesize_apply_patch_command(message)
            if patch_cmd:
                args["cmd"] = patch_cmd
                normalized.append(ParsedToolCall(call.call_id, call.name, _json_dumps(args)))
                continue
        normalized.append(call)
    return normalized


def _has_prior_tool_result(message: str) -> bool:
    return bool(_TOOL_RESULT_RE.search(message))


def _has_prior_command_output(message: str) -> bool:
    if not _CODEX_COMMAND_OUTPUT_RE.search(message):
        return False
    latest_user_idx = message.lower().rfind("[user]:")
    if latest_user_idx < 0:
        return True
    return bool(_CODEX_COMMAND_OUTPUT_RE.search(message[latest_user_idx:]))


def _is_duplicate_completed_patch(cmd: str, message: str) -> bool:
    if not cmd or "apply_patch" not in cmd or not _PATCH_COMPLETED_RE.search(message):
        return False
    target = _extract_patch_target(cmd)
    if not target:
        return True
    return target in message


def _extract_patch_target(cmd: str) -> str | None:
    m = re.search(r"^\*\*\* (?:Add|Update|Delete) File:\s+(.+?)\s*$", cmd, re.M)
    if not m:
        return None
    path = m.group(1).strip()
    return path if _valid_patch_path(path) else None


def _json_args(arguments: str) -> dict[str, Any]:
    try:
        parsed = orjson.loads(arguments)
        return parsed if isinstance(parsed, dict) else {}
    except Exception:
        return {}


def _json_dumps(value: Any) -> str:
    return orjson.dumps(value).decode()


def _looks_like_edit_request(message: str) -> bool:
    return bool(re.search(r"apply_patch|patch|修改|编辑|创建|新增|写入|内容为|create file|edit file|write file", _latest_user_intent(message), re.I))


def _looks_like_direct_file_write(cmd: str) -> bool:
    return bool(re.search(r"(^|\s)(echo|printf|cat|tee)\b[\s\S]*(>|>>|\btee\b)", cmd))


def _command_to_apply_patch(cmd: str) -> str | None:
    parsed = _parse_simple_write_command(cmd)
    if not parsed:
        return None
    path, content, append = parsed
    if append:
        return None
    return _apply_patch_add_file_command(path, content)


def _parse_simple_write_command(cmd: str) -> tuple[str, str, bool] | None:
    # echo 'ok' > file
    m = re.match(r"""echo\s+(['"])(.*?)\1\s*(>>?)\s*([^\s]+)\s*$""", cmd, re.S)
    if m:
        return m.group(4), m.group(2) + "\n", m.group(3) == ">>"

    # printf 'ok\n' > file
    m = re.match(r"""printf\s+(['"])(.*?)\1\s*(>>?)\s*([^\s]+)\s*$""", cmd, re.S)
    if m:
        content = bytes(m.group(2), "utf-8").decode("unicode_escape")
        return m.group(4), content, m.group(3) == ">>"

    # cat > file <<'EOF' ... EOF
    m = re.match(
        r"""cat\s*>\s*([^\s]+)\s*<<['"]?([A-Za-z0-9_]+)['"]?\n([\s\S]*)\n\2\s*$""",
        cmd,
    )
    if m:
        return m.group(1), m.group(3) + "\n", False

    # cat <<'EOF' > file ... EOF
    m = re.match(
        r"""cat\s*<<['"]?([A-Za-z0-9_]+)['"]?\s*>\s*([^\s]+)\n([\s\S]*)\n\1\s*$""",
        cmd,
    )
    if m:
        return m.group(2), m.group(3) + "\n", False

    # tee file <<'EOF' ... EOF
    m = re.match(
        r"""tee\s+([^\s]+)\s*<<['"]?([A-Za-z0-9_]+)['"]?\n([\s\S]*)\n\2\s*$""",
        cmd,
    )
    if m:
        return m.group(1), m.group(3) + "\n", False
    return None


def _synthesize_apply_patch_command(message: str) -> str | None:
    intent = _latest_user_intent(message)
    replace_cmd = _synthesize_simple_replace_patch(intent)
    if replace_cmd:
        return replace_cmd
    if not _looks_like_simple_create_request(intent):
        return None
    path = _extract_target_filename(intent)
    content = _extract_requested_file_content(intent)
    if not path or content is None:
        return None
    return _apply_patch_add_file_command(path, content)


def _synthesize_simple_replace_patch(intent: str) -> str | None:
    if not intent or len(intent) > 1200:
        return None
    if not re.search(r"修改|编辑|替换|改成|replace|change", intent, re.I):
        return None
    path = _extract_target_filename(intent)
    if not path:
        return None
    pair = _extract_replacement_pair(intent)
    if not pair:
        return None
    old, new = pair
    return _apply_patch_replace_line_command(path, old, new)


def _extract_replacement_pair(intent: str) -> tuple[str, str] | None:
    patterns = (
        r"(?:把|将)\s*(?:文件\s*)?`?[A-Za-z0-9_./-]+\.[A-Za-z0-9_-]+`?\s*(?:里|中的|里面的)?(?:的)?\s*[`'\"]?([^`'\"\n。；;，,\s]+)[`'\"]?\s*(?:改成|替换成|换成)\s*[`'\"]?([^`'\"\n。；;，,\s]+)[`'\"]?",
        r"(?:replace|change)\s+[`'\"]?([^`'\"\n]+?)[`'\"]?\s+(?:with|to)\s+[`'\"]?([^`'\"\n]+?)[`'\"]?\s+(?:in|inside)\s+`?[A-Za-z0-9_./-]+\.[A-Za-z0-9_-]+`?",
    )
    for pattern in patterns:
        m = re.search(pattern, intent, re.I)
        if m:
            old = m.group(1).strip()
            new = m.group(2).strip()
            if _valid_patch_line(old) and _valid_patch_line(new):
                return old, new
    return None


def _latest_user_intent(message: str) -> str:
    """Return the latest user-facing request, excluding injected tool schemas.

    The fallback synthesizer must be conservative: the full prompt contains
    tool descriptions, model names and schema fragments, all of which can look
    like filenames.  Only use explicit user/conversation blocks when present.
    """
    blocks = re.findall(
        r"\[(?:user|conversation)\]:\s*([\s\S]*?)(?=\n\[(?:system|assistant|tool|conversation|user)\]:|\Z)",
        message,
        re.IGNORECASE,
    )
    if blocks:
        return blocks[-1].strip()

    # Drop the final injected reminder and the leading tool prompt when this is
    # a flattened prompt built by inject_into_message().
    text = re.split(r"\n\n\[system\]:\s*If a tool is needed now", message, maxsplit=1, flags=re.I)[0]
    if text.startswith("[system]:") and "AVAILABLE TOOLS:" in text:
        for marker in (
            "Do not mention unavailable tool names. Use exactly one of the AVAILABLE TOOLS names.",
            "NOTE: Even if you believe you cannot fulfill the request, you must still follow the WHEN TO CALL rule above.",
        ):
            if marker in text:
                text = text.rsplit(marker, 1)[1]
                break
        else:
            parts = text.split("\n\n", 1)
            text = parts[1] if len(parts) > 1 else ""
    return text.strip()


def _looks_like_simple_create_request(intent: str) -> bool:
    if not intent or len(intent) > 1200:
        return False
    has_create = re.search(r"创建|新增|create (?:a )?file|add (?:a )?file|write (?:a )?file", intent, re.I)
    has_content = re.search(r"内容为|内容是|content(?:\s+is|\s+为)?|with content", intent, re.I)
    return bool(has_create and has_content and _extract_target_filename(intent))


def _extract_target_filename(message: str) -> str | None:
    patterns = (
        r"(?:文件|file)\s+`?([A-Za-z0-9_./-]+\.[A-Za-z0-9_-]+)`?",
        r"`([A-Za-z0-9_./-]+\.[A-Za-z0-9_-]+)`",
        r"\b((?:\.?/)?(?:[A-Za-z0-9_-]+/)*[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+)\b",
    )
    for pattern in patterns:
        m = re.search(pattern, message, re.I)
        if m:
            path = m.group(1).strip()
            if _valid_patch_path(path):
                return path
    return None


def _extract_requested_file_content(message: str) -> str | None:
    m = re.search(r"(?:内容为|内容是|content(?:\s+is|\s+为)?|with content)\s*```(?:[A-Za-z0-9_-]+)?\n([\s\S]*?)\n```", message, re.I)
    if m:
        return m.group(1).rstrip("\n") + "\n"
    m = re.search(r"(?:内容为|内容是|content(?:\s+is|\s+为)?|with content)\s*[`'\"]?([^`'\"\n。；;，,]+)[`'\"]?", message, re.I)
    if not m:
        return None
    return m.group(1).strip() + "\n"


def _valid_patch_path(path: str) -> bool:
    if not path or path.startswith(("/", "~")) or ".." in path.split("/"):
        return False
    if path in {"gpt-5.5", "gpt-5.4", "gpt-5.3", "text"}:
        return False
    return bool(re.match(r"^[A-Za-z0-9_./-]+\.[A-Za-z0-9_-]+$", path))


def _apply_patch_add_file_command(path: str, content: str) -> str:
    lines = ["apply_patch <<'PATCH'", "*** Begin Patch", f"*** Add File: {path}"]
    for line in content.splitlines():
        lines.append(f"+{line}")
    if content.endswith("\n") and not content.splitlines():
        lines.append("+")
    lines.extend(["*** End Patch", "PATCH"])
    return "\n".join(lines)


def _apply_patch_replace_line_command(path: str, old: str, new: str) -> str:
    return "\n".join(
        [
            "apply_patch <<'PATCH'",
            "*** Begin Patch",
            f"*** Update File: {path}",
            "@@",
            f"-{old}",
            f"+{new}",
            "*** End Patch",
            "PATCH",
        ]
    )


def _valid_patch_line(value: str) -> bool:
    if not value or len(value) > 300:
        return False
    return "\n" not in value and "\r" not in value


def _synthesize_write_stdin_args(message: str, previous_text: str) -> dict[str, Any] | None:
    haystack = f"{_latest_user_intent(message)}\n{previous_text}"
    if not _WRITE_STDIN_REQUEST_RE.search(haystack):
        return None
    sid_match = re.search(r"(?:session_id|session|会话)\s*[:=#]?\s*([0-9]{1,12})", haystack, re.I)
    if not sid_match:
        return None
    chars = ""
    for pattern in (
        r"(?:send|write|输入|发送)\s+[`'\"]([^`'\"]{0,500})[`'\"]",
        r"(?:chars|input|内容)\s*[:=]\s*[`'\"]([^`'\"]{0,500})[`'\"]",
    ):
        m = re.search(pattern, haystack, re.I)
        if m:
            chars = m.group(1)
            break
    return {
        "session_id": int(sid_match.group(1)),
        "chars": chars,
    }


def _extract_requested_shell_command(message: str, previous_text: str) -> str | None:
    haystack = f"{_latest_user_intent(message)}\n\n{previous_text}"

    # Commands in code spans are the cleanest signal.
    for m in re.finditer(r"`([^`\n]{1,300})`", haystack):
        candidate = m.group(1).strip()
        if _looks_safe_shell_snippet(candidate):
            return candidate

    patterns = (
        r"(?:run|execute|运行|执行)\s+(?:the\s+)?(?:shell\s+)?(?:command\s+|命令\s*)?([A-Za-z0-9_./ -]{1,160})",
        r"(?:使用|用)\s*(?:shell|bash|终端).*?(?:运行|执行)\s*([A-Za-z0-9_./ -]{1,160})",
    )
    for pattern in patterns:
        m = re.search(pattern, haystack, re.IGNORECASE)
        if not m:
            continue
        candidate = _clean_command_candidate(m.group(1))
        if _looks_safe_shell_snippet(candidate):
            return candidate

    lowered = haystack.lower()
    if re.search(r"\bpwd\b", lowered) or "当前路径" in haystack or "当前目录路径" in haystack:
        return "pwd"
    if (
        "desktop" in lowered
        or "桌面" in haystack
    ) and (
        "有哪些" in haystack
        or "列出" in haystack
        or "看看" in haystack
        or re.search(r"\b(list|show|see)\b", lowered)
    ):
        return "ls -la ~/Desktop | sed -n '1,40p'"
    if (
        "有哪些文件" in haystack
        or "列出" in haystack and "文件" in haystack
        or re.search(r"\blist\b.*\bfiles\b", lowered)
    ):
        return "find . -maxdepth 2 -print | sort"
    read_cmd = _synthesize_read_file_command(haystack)
    if read_cmd:
        return read_cmd
    search_cmd = _synthesize_search_command(haystack)
    if search_cmd:
        return search_cmd
    if re.search(r"\bls\b", lowered):
        return "ls -la"
    return None


def _synthesize_read_file_command(haystack: str) -> str | None:
    if not re.search(r"读取|查看|打开|读一下|read|show|cat", haystack, re.I):
        return None
    path = _extract_target_filename(haystack)
    if not path:
        return None
    return f"sed -n '1,200p' {shlex.quote(path)}"


def _synthesize_search_command(haystack: str) -> str | None:
    if not re.search(r"搜索|查找|包含|grep|rg|search|find", haystack, re.I):
        return None
    term = _extract_search_term(haystack)
    if not term:
        return None
    return f"rg -n -- {shlex.quote(term)} ."


def _extract_search_term(haystack: str) -> str | None:
    patterns = (
        r"(?:包含|含有)\s*[`'\"]?([^`'\"\n。；;，,\s]{1,120})[`'\"]?",
        r"(?:搜索|查找)\s*[`'\"]?([^`'\"\n。；;，,\s]{1,120})[`'\"]?",
        r"(?:search|find|grep)\s+(?:for\s+)?[`'\"]?([^`'\"\n]{1,120})[`'\"]?",
    )
    for pattern in patterns:
        m = re.search(pattern, haystack, re.I)
        if not m:
            continue
        term = m.group(1).strip()
        if term and not re.search(r"\s(?:的|文件|路径)$", term):
            return term
    return None


def _clean_command_candidate(candidate: str) -> str:
    candidate = candidate.strip()
    candidate = re.split(r"[\n\r。；;]", candidate, maxsplit=1)[0].strip()
    candidate = re.sub(r"^(?:就是|为|is|as)\s+", "", candidate, flags=re.I).strip()
    return candidate.strip("'\" ")


def _looks_safe_shell_snippet(candidate: str | None) -> bool:
    if not candidate:
        return False
    if len(candidate) > 300:
        return False
    # Reject prose-like captures and obvious shell control chains. Codex will
    # still enforce its own sandbox/approval policy after receiving the call.
    if any(token in candidate for token in ("\n", "\r", "&&", "||", ";", "| sh", "|sh")):
        return False
    return bool(re.match(r"^[A-Za-z0-9_./~:${}\\[\\]*?=,'\" -]+$", candidate))


__all__ = [
    "_PATCH_COMPLETED_RE",
    "_PATCH_FAILURE_RE",
    "_forced_tool_choice",
    "_looks_like_codex_tool_run",
    "_normalize_codex_tool_calls",
    "_synthesize_codex_tool_call",
    "_to_chat_tools",
]
