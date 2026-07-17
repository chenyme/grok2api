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
		// Codex/OpenAI may omit type on role-bearing message items. Force a
		// known type before rewrite so untyped objects never reach ModelInput.
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
			converted, changed := sanitizeReasoningInput(item)
			if changed {
				c.changed = true
			}
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
				if c.stripExternal {
					rewritten = append(rewritten, c.externalHistoryBoundary(item, kind))
					continue
				}
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
	// CLIProxyAPI constructs function_call with only type/call_id/name/arguments.
	// Extra fields (id/status/namespace/metadata) break Grok's untagged ModelInput.
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

func sanitizeReasoningInput(item map[string]any) (map[string]any, bool) {
	// Grok Build's untagged ModelInput rejects null-valued fields and Codex
	// private metadata. Only keep portable non-null reasoning fields.
	//
	// Codex may emit reasoning items with summary text but null
	// encrypted_content (common when the model returns a summary-only item).
	// Replaying that shape 422s, so fall back to a developer boundary that
	// preserves the visible summary without claiming encrypted state.
	encrypted, hasEncrypted := nonNullJSONValue(item["encrypted_content"])
	if !hasEncrypted || strings.TrimSpace(fmt.Sprint(encrypted)) == "" {
		summary := reasoningSummaryText(item["summary"])
		text := "A prior model reasoning item was omitted because it has no portable encrypted_content for Grok Build."
		if summary != "" {
			text += "\nSummary:\n" + summary
		}
		return compatibilityBoundaryMessage(text), true
	}

	converted := map[string]any{"type": "reasoning", "encrypted_content": cloneJSONValue(encrypted)}
	for _, key := range []string{"id", "summary", "content", "status"} {
		if value, ok := nonNullJSONValue(item[key]); ok {
			converted[key] = cloneJSONValue(value)
		}
	}
	return converted, true
}

func nonNullJSONValue(value any) (any, bool) {
	if value == nil {
		return nil, false
	}
	return value, true
}

func reasoningSummaryText(raw any) string {
	items, ok := raw.([]any)
	if !ok {
		return ""
	}
	parts := make([]string, 0, len(items))
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		if text := strings.TrimSpace(stringField(item, "text")); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func hasPrivateInputFields(item map[string]any) bool {
	for _, key := range []string{"internal_chat_message_metadata_passthrough", "phase"} {
		if _, exists := item[key]; exists {
			return true
		}
	}
	return false
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
