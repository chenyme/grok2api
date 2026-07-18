package cli

import (
	"encoding/json"
	"fmt"
	"strings"
)

// normalizeInputItems 将 Codex/Responses 扩展历史降级为 Grok Build 可接受的结构，
// 同时收集 tool_search 或 additional_tools 动态加载的工具定义。
func (c *responsesToolCompatibility) normalizeInputItems(items []any) ([]any, []any, []any, error) {
	rewritten := make([]any, 0, len(items))
	loadedTools := make([]any, 0)
	visibleTools := make([]any, 0)
	for index, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			rewritten = append(rewritten, rawItem)
			continue
		}
		param := fmt.Sprintf("input[%d]", index)
		itemType := strings.TrimSpace(stringField(item, "type"))
		// Codex/OpenAI 可能省略带 role 消息的 type；在重建前补成明确消息，
		// 避免无类型对象直接进入 ModelInput。
		if itemType == "" && strings.TrimSpace(stringField(item, "role")) != "" {
			itemType = "message"
		}
		switch itemType {
		case "message":
			converted, err := normalizeMessageInput(item, param)
			if err != nil {
				return nil, nil, nil, err
			}
			c.changed = true
			rewritten = append(rewritten, converted)
		case "function_call":
			converted, err := c.normalizeFunctionCallInput(item, param)
			if err != nil {
				return nil, nil, nil, err
			}
			c.changed = true
			rewritten = append(rewritten, converted)
		case "function_call_output":
			converted, err := normalizeFunctionCallOutputInput(item, param)
			if err != nil {
				return nil, nil, nil, err
			}
			c.changed = true
			rewritten = append(rewritten, converted)
		case "reasoning":
			converted := sanitizeReasoningInput(item)
			c.changed = true
			rewritten = append(rewritten, converted)
		case "file_search_call", "web_search_call", "image_generation_call", "code_interpreter_call",
			"shell_call", "mcp_list_tools", "mcp_approval_request", "mcp_approval_response", "mcp_call", "compaction":
			// 这些类型已进入 Grok Build 0.2.102 的 Responses InputItem 契约。
			// 仅清理 Codex 私有字段和 null，不能把原生调用降级成文本边界。
			converted := sanitizeNativeHistoryInput(item, itemType)
			c.changed = true
			rewritten = append(rewritten, converted)
		case "tool_search_call":
			callID := strings.TrimSpace(stringField(item, "call_id"))
			if callID == "" {
				return nil, nil, nil, &responsesRequestError{Message: param + ".call_id 不能为空", Param: param + ".call_id", Code: "invalid_parameter"}
			}
			execution := strings.ToLower(strings.TrimSpace(stringField(item, "execution")))
			if execution == "" || execution == "server" {
				c.serverSearchEager = true
				c.changed = true
				c.addWarning("server_tool_search_history_approximated")
				rewritten = append(rewritten, compatibilityBoundaryMessage("A server-side tool search occurred here; selected tools are made available directly."))
				continue
			}
			if execution != "client" {
				return nil, nil, nil, &responsesRequestError{Message: "tool_search_call.execution 只支持 client 或 server", Param: param + ".execution", Code: "invalid_parameter"}
			}
			arguments, err := encodeFunctionArguments(item["arguments"])
			if err != nil {
				return nil, nil, nil, &responsesRequestError{Message: param + ".arguments 无法编码", Param: param + ".arguments", Code: "invalid_parameter"}
			}
			rewritten = append(rewritten, map[string]any{
				"type": "function_call", "call_id": callID,
				"name": c.alias(responsesToolIdentity{Kind: responsesToolSearch, Name: "tool_search"}), "arguments": arguments,
			})
			c.changed = true
		case "tool_search_output":
			execution := strings.ToLower(strings.TrimSpace(stringField(item, "execution")))
			if execution != "" && execution != "client" && execution != "server" {
				return nil, nil, nil, &responsesRequestError{Message: "tool_search_output.execution 只支持 client 或 server", Param: param + ".execution", Code: "invalid_parameter"}
			}
			callID := strings.TrimSpace(stringField(item, "call_id"))
			if callID == "" {
				return nil, nil, nil, &responsesRequestError{Message: param + ".call_id 不能为空", Param: param + ".call_id", Code: "invalid_parameter"}
			}
			tools, ok := item["tools"].([]any)
			if !ok {
				return nil, nil, nil, &responsesRequestError{Message: param + ".tools 必须是数组", Param: param + ".tools", Code: "invalid_parameter"}
			}
			for toolIndex, rawTool := range tools {
				converted, err := c.normalizeTool(rawTool, "", false, true, fmt.Sprintf("%s.tools[%d]", param, toolIndex))
				if err != nil {
					return nil, nil, nil, err
				}
				loadedTools = append(loadedTools, converted...)
			}
			visibleTools = append(visibleTools, cloneJSONArray(tools)...)
			c.changed = true
			message := fmt.Sprintf("Tool search completed; %d selected tool definitions are now available.", len(tools))
			if execution == "client" {
				rewritten = append(rewritten, map[string]any{"type": "function_call_output", "call_id": callID, "output": message})
			} else {
				c.serverSearchEager = true
				c.addWarning("server_tool_search_history_approximated")
				rewritten = append(rewritten, compatibilityBoundaryMessage(message))
			}
		case "custom_tool_call":
			converted, err := c.normalizeCustomToolCallInput(item, param)
			if err != nil {
				return nil, nil, nil, err
			}
			c.changed = true
			rewritten = append(rewritten, converted)
		case "custom_tool_call_output":
			converted, err := normalizeFunctionCallOutputInput(item, param)
			if err != nil {
				return nil, nil, nil, err
			}
			c.changed = true
			rewritten = append(rewritten, converted)
		case "apply_patch_call":
			converted, err := c.normalizeApplyPatchCallInput(item, param)
			if err != nil {
				return nil, nil, nil, err
			}
			c.changed = true
			rewritten = append(rewritten, converted)
		case "apply_patch_call_output":
			converted, err := normalizeApplyPatchOutputInput(item, param)
			if err != nil {
				return nil, nil, nil, err
			}
			c.changed = true
			rewritten = append(rewritten, converted)
		case "agent_message":
			if _, visible := textInputContent(item["content"]); !visible {
				c.addWarning("opaque_agent_message_redacted")
			}
			converted, err := normalizeAgentMessageInput(item, param)
			if err != nil {
				return nil, nil, nil, err
			}
			c.changed = true
			rewritten = append(rewritten, converted)
		case "local_shell_call":
			converted, err := normalizeLegacyLocalShellCallInput(item, param)
			if err != nil {
				return nil, nil, nil, err
			}
			c.changed = true
			rewritten = append(rewritten, converted)
		case "local_shell_call_output":
			converted, err := normalizeLegacyLocalShellOutputInput(item, param)
			if err != nil {
				return nil, nil, nil, err
			}
			c.changed = true
			rewritten = append(rewritten, converted)
		case "shell_call_output":
			converted, err := normalizeShellCallOutputInput(item, param)
			if err != nil {
				return nil, nil, nil, err
			}
			c.changed = true
			rewritten = append(rewritten, converted)
		case "mcp_tool_call_output":
			converted, err := normalizeMCPOutputInput(item, param)
			if err != nil {
				return nil, nil, nil, err
			}
			c.changed = true
			rewritten = append(rewritten, converted)
		case "compaction_trigger":
			c.changed = true
			c.addWarning("compaction_boundary_preserved")
			rewritten = append(rewritten, compatibilityBoundaryMessage("Codex context compaction boundary reached."))
		case "additional_tools":
			marker, additional, visible, err := c.normalizeAdditionalToolsInput(item, param)
			if err != nil {
				return nil, nil, nil, err
			}
			loadedTools = append(loadedTools, additional...)
			visibleTools = append(visibleTools, visible...)
			c.changed = true
			rewritten = append(rewritten, marker)
		default:
			if kind := strings.TrimSpace(stringField(item, "type")); kind != "" {
				c.changed = true
				c.addWarning("unsupported_input_history_omitted")
				rewritten = append(rewritten, unsupportedInputHistoryBoundary(item, kind))
				continue
			}
			rewritten = append(rewritten, cloneJSONValue(item))
		}
	}
	return rewritten, loadedTools, visibleTools, nil
}

func (c *responsesToolCompatibility) normalizeFunctionCallInput(item map[string]any, param string) (map[string]any, error) {
	name := strings.TrimSpace(stringField(item, "name"))
	if name == "" {
		return nil, &responsesRequestError{Message: param + ".name 不能为空", Param: param + ".name", Code: "invalid_parameter"}
	}
	callID := strings.TrimSpace(stringField(item, "call_id"))
	if callID == "" {
		return nil, &responsesRequestError{Message: param + ".call_id 不能为空", Param: param + ".call_id", Code: "invalid_parameter"}
	}
	arguments, err := encodeFunctionArguments(item["arguments"])
	if err != nil {
		return nil, &responsesRequestError{Message: param + ".arguments 无法编码", Param: param + ".arguments", Code: "invalid_parameter"}
	}
	namespace := strings.TrimSpace(stringField(item, "namespace"))
	if namespace != "" {
		name = c.alias(responsesToolIdentity{Kind: responsesFunctionTool, Namespace: namespace, Name: name})
	}
	// 按官方 Build 回放结构仅保留四个输入字段，避免输出态字段和私有元数据
	// 干扰 Grok 的 untagged ModelInput 反序列化。
	return map[string]any{"type": "function_call", "call_id": callID, "name": name, "arguments": arguments}, nil
}

func (c *responsesToolCompatibility) normalizeCustomToolCallInput(item map[string]any, param string) (map[string]any, error) {
	name := strings.TrimSpace(stringField(item, "name"))
	if name == "" {
		return nil, &responsesRequestError{Message: param + ".name 不能为空", Param: param + ".name", Code: "invalid_parameter"}
	}
	input, ok := item["input"].(string)
	if !ok {
		return nil, &responsesRequestError{Message: param + ".input 必须是字符串", Param: param + ".input", Code: "invalid_parameter"}
	}
	arguments, err := encodeCustomToolArguments(input)
	if err != nil {
		return nil, err
	}
	callID := strings.TrimSpace(stringField(item, "call_id"))
	if callID == "" {
		return nil, &responsesRequestError{Message: param + ".call_id 不能为空", Param: param + ".call_id", Code: "invalid_parameter"}
	}
	namespace := strings.TrimSpace(stringField(item, "namespace"))
	return map[string]any{
		"type": "function_call", "call_id": callID,
		"name":      c.alias(responsesToolIdentity{Kind: responsesCustomTool, Namespace: namespace, Name: name}),
		"arguments": arguments,
	}, nil
}

func sanitizeReasoningInput(item map[string]any) map[string]any {
	// 官方 Grok Build 回放 reasoning 时会删除 output-only status，但会保留
	// id、summary、content 和可选 encrypted_content。密文不是回放的前置条件。
	converted := copyNonNullHistoryFields(item, "id", "summary", "content", "encrypted_content")
	converted["type"] = "reasoning"
	if !hasPortableReasoningContent(converted) {
		return compatibilityBoundaryMessage("A prior model reasoning item was omitted because it has no portable content for Grok Build.")
	}
	return converted
}

func hasPortableReasoningContent(item map[string]any) bool {
	if encrypted, ok := item["encrypted_content"].(string); ok && strings.TrimSpace(encrypted) != "" {
		return true
	}
	for _, key := range []string{"summary", "content"} {
		if values, ok := item[key].([]any); ok && len(values) > 0 {
			return true
		}
	}
	return false
}

// sanitizeNativeHistoryInput 按 Grok Build 0.2.102 的原生 InputItem 字段重建历史，
// 避免 Codex 扩展元数据干扰 Rust untagged enum 的反序列化。
func sanitizeNativeHistoryInput(item map[string]any, itemType string) map[string]any {
	var fields []string
	switch itemType {
	case "file_search_call":
		fields = []string{"id", "queries", "status", "results"}
	case "web_search_call":
		fields = []string{"action", "id", "status"}
	case "image_generation_call":
		fields = []string{"id", "result", "status"}
	case "code_interpreter_call":
		fields = []string{"code", "container_id", "id", "outputs", "status"}
	case "shell_call":
		fields = []string{"id", "call_id", "action", "status", "environment"}
	case "mcp_list_tools":
		fields = []string{"id", "server_label", "tools", "error"}
	case "mcp_approval_request":
		fields = []string{"arguments", "id", "name", "server_label"}
	case "mcp_approval_response":
		fields = []string{"approval_request_id", "approve", "id", "reason"}
	case "mcp_call":
		fields = []string{"arguments", "id", "name", "server_label", "approval_request_id", "error", "output", "status"}
	case "compaction":
		fields = []string{"id", "encrypted_content"}
	}
	converted := copyNonNullHistoryFields(item, fields...)
	converted["type"] = itemType
	return converted
}

func copyNonNullHistoryFields(item map[string]any, fields ...string) map[string]any {
	converted := make(map[string]any, len(fields)+1)
	for _, key := range fields {
		if value, ok := sanitizeHistoryJSONValue(item[key]); ok {
			converted[key] = value
		}
	}
	return converted
}

func sanitizeHistoryJSONValue(value any) (any, bool) {
	switch typed := value.(type) {
	case nil:
		return nil, false
	case map[string]any:
		cleaned := make(map[string]any, len(typed))
		for key, nested := range typed {
			if key == "phase" || key == "internal_chat_message_metadata_passthrough" {
				continue
			}
			if normalized, ok := sanitizeHistoryJSONValue(nested); ok {
				cleaned[key] = normalized
			}
		}
		return cleaned, true
	case []any:
		cleaned := make([]any, 0, len(typed))
		for _, nested := range typed {
			if normalized, ok := sanitizeHistoryJSONValue(nested); ok {
				cleaned = append(cleaned, normalized)
			}
		}
		return cleaned, true
	default:
		return cloneJSONValue(value), true
	}
}

func unsupportedInputHistoryBoundary(item map[string]any, kind string) map[string]any {
	parts := []string{"A prior Responses history item was omitted because Grok Build cannot deserialize this Codex item type.", "Type: " + kind}
	for _, key := range []string{"id", "call_id", "name", "status"} {
		if value := strings.TrimSpace(stringField(item, key)); value != "" {
			parts = append(parts, strings.ReplaceAll(key, "_", " ")+": "+value)
		}
	}
	return compatibilityBoundaryMessage(strings.Join(parts, "\n"))
}

func encodeFunctionArguments(value any) (string, error) {
	if text, ok := value.(string); ok {
		return text, nil
	}
	if value == nil {
		return "{}", nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
