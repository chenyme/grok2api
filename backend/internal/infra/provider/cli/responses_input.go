package cli

import (
	"encoding/json"
	"strings"
)

// normalizeAgentMessageInput 将 inter-agent 历史保留为 developer 消息；不透明内容保留边界标记而不泄露密文。
func normalizeAgentMessageInput(item map[string]any, _ string) (map[string]any, error) {
	content, ok := textInputContent(item["content"])
	if !ok {
		return compatibilityBoundaryMessage("An encrypted inter-agent message occurred here but is not portable to the Grok Build account."), nil
	}
	author := strings.TrimSpace(stringField(item, "author"))
	if author == "" {
		author = "agent"
	}
	recipient := strings.TrimSpace(stringField(item, "recipient"))
	if recipient == "" {
		recipient = "recipient"
	}
	return map[string]any{
		"type": "message", "role": "developer",
		"content": []any{map[string]any{"type": "input_text", "text": "Agent message (" + author + " -> " + recipient + "):\n" + content}},
	}, nil
}

// normalizeLocalShellInput 将本地执行记录降级为可见 assistant 历史，避免伪造可再次执行的 hosted shell call。
func normalizeLocalShellInput(item map[string]any, param string) (map[string]any, error) {
	action, err := json.Marshal(item["action"])
	if err != nil {
		return nil, &responsesRequestError{Message: "local_shell_call.action 无法编码", Param: param + ".action", Code: "invalid_parameter"}
	}
	status := strings.TrimSpace(stringField(item, "status"))
	if status == "" {
		status = "unknown"
	}
	return map[string]any{
		"type": "message", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": "Local shell call (" + status + "): " + string(action)}},
	}, nil
}

// normalizeMCPOutputInput 将无法关联到上游 MCP 状态的输出保留为 developer 文本历史。
func normalizeMCPOutputInput(item map[string]any, param string) (map[string]any, error) {
	output, err := json.Marshal(item["output"])
	if err != nil {
		return nil, &responsesRequestError{Message: "mcp_tool_call_output.output 无法编码", Param: param + ".output", Code: "invalid_parameter"}
	}
	callID := strings.TrimSpace(stringField(item, "call_id"))
	if callID == "" {
		callID = "unknown"
	}
	return map[string]any{
		"type": "message", "role": "developer",
		"content": []any{map[string]any{"type": "input_text", "text": "MCP tool output for call " + callID + ": " + string(output)}},
	}, nil
}

func textInputContent(raw any) (string, bool) {
	if text, ok := raw.(string); ok {
		return text, true
	}
	items, ok := raw.([]any)
	if !ok {
		return "", false
	}
	parts := make([]string, 0, len(items))
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			return "", false
		}
		switch stringField(item, "type") {
		case "input_text", "output_text", "text":
			parts = append(parts, stringField(item, "text"))
		default:
			return "", false
		}
	}
	return strings.Join(parts, "\n"), true
}
