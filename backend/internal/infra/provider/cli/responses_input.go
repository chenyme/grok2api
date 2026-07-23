package cli

import (
	"encoding/json"
	"fmt"
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

func (c *responsesToolCompatibility) normalizeMessageInput(item map[string]any, param string) (map[string]any, error) {
	role := strings.TrimSpace(stringField(item, "role"))
	if role == "" {
		role = "assistant"
	}
	if role == "model" {
		role = "assistant"
	}
	content, err := c.normalizeMessageContent(item["content"], role, param+".content")
	if err != nil {
		return nil, err
	}
	// 仅按白名单重建，避免 Codex 的 phase、metadata、status、id 等
	// 非输入字段进入 Grok ModelInput。
	return map[string]any{"type": "message", "role": role, "content": content}, nil
}

func (c *responsesToolCompatibility) normalizeMessageContent(value any, role, param string) (any, error) {
	if text, ok := value.(string); ok {
		return text, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, &responsesRequestError{Message: param + " 必须是字符串或数组", Param: param, Code: "invalid_parameter"}
	}
	// 官方 Grok CLI 会把 assistant 的多个输出文本合并为 EasyInputMessage
	// 字符串，既保留语义，也绕开 OutputMessage 对 id/status 的输入限制。
	if role == "assistant" {
		texts := make([]string, 0, len(items))
		for _, raw := range items {
			item, isObject := raw.(map[string]any)
			if !isObject {
				texts = nil
				break
			}
			switch stringField(item, "type") {
			case "text", "input_text", "output_text":
				texts = append(texts, stringField(item, "text"))
			case "refusal":
				texts = append(texts, stringField(item, "refusal"))
			default:
				texts = nil
			}
			if texts == nil {
				break
			}
		}
		if texts != nil {
			return strings.Join(texts, "\n"), nil
		}
	}

	textPartType := "input_text"
	if role == "assistant" {
		textPartType = "output_text"
	}
	normalized := make([]any, 0, len(items))
	for index, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, &responsesRequestError{Message: param + "[] 必须是对象", Param: fmt.Sprintf("%s[%d]", param, index), Code: "invalid_parameter"}
		}
		switch stringField(item, "type") {
		case "text", "input_text", "output_text":
			// 按角色重建文本类型，不能把 assistant 的 output_text 改成 input_text。
			normalized = append(normalized, map[string]any{"type": textPartType, "text": stringField(item, "text")})
		case "refusal":
			normalized = append(normalized, map[string]any{"type": textPartType, "text": stringField(item, "refusal")})
		case "input_image":
			converted, err := c.normalizeInputImagePart(item, fmt.Sprintf("%s[%d]", param, index))
			if err != nil {
				return nil, err
			}
			normalized = append(normalized, converted)
		case "input_file":
			normalized = append(normalized, normalizeInputFilePart(item))
		default:
			return nil, &responsesRequestError{Message: "Grok Build 0.2.110 不支持该 message.content 类型", Param: fmt.Sprintf("%s[%d].type", param, index), Code: "unsupported_parameter"}
		}
	}
	return normalized, nil
}

func (c *responsesToolCompatibility) normalizeInputImagePart(item map[string]any, param string) (map[string]any, error) {
	detail := "auto"
	if raw, exists := item["detail"]; exists && raw != nil {
		value, ok := raw.(string)
		if !ok {
			return nil, &responsesRequestError{Message: param + ".detail 必须是字符串", Param: param + ".detail", Code: "invalid_parameter"}
		}
		detail = strings.TrimSpace(value)
		if detail == "" {
			detail = "auto"
		}
	}
	switch detail {
	case "auto", "low", "high":
	case "original":
		// OpenAI accepts original, while Grok Build 0.2.110 supports up to high.
		detail = "high"
		c.addWarning("image_detail_original_downgraded")
	default:
		return nil, &responsesRequestError{Message: param + ".detail 只支持 auto、low、high 或 original", Param: param + ".detail", Code: "invalid_parameter"}
	}

	converted := map[string]any{"type": "input_image", "detail": detail}
	if value, exists := item["image_url"]; exists && value != nil {
		converted["image_url"] = cloneJSONValue(value)
	} else if value, exists := item["url"]; exists && value != nil {
		converted["image_url"] = cloneJSONValue(value)
	}
	if value, exists := item["file_id"]; exists && value != nil {
		converted["file_id"] = cloneJSONValue(value)
	}
	return converted, nil
}

func normalizeInputFilePart(item map[string]any) map[string]any {
	converted := map[string]any{"type": "input_file"}
	for _, key := range []string{"file_data", "file_id", "filename", "file_url"} {
		if value, exists := item[key]; exists && value != nil {
			converted[key] = cloneJSONValue(value)
		}
	}
	return converted
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
