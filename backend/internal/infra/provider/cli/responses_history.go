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
		switch stringField(item, "type") {
		case "function_call":
			namespace := strings.TrimSpace(stringField(item, "namespace"))
			if namespace == "" {
				rewritten = append(rewritten, cloneJSONValue(item))
				continue
			}
			name := strings.TrimSpace(stringField(item, "name"))
			if name == "" {
				return nil, nil, nil, &responsesRequestError{Message: param + ".name 不能为空", Param: param + ".name", Code: "invalid_parameter"}
			}
			converted := cloneJSONObject(item)
			converted["name"] = c.alias(responsesToolIdentity{Kind: responsesFunctionTool, Namespace: namespace, Name: name})
			delete(converted, "namespace")
			c.changed = true
			rewritten = append(rewritten, converted)
		case "tool_search_call":
			callID := strings.TrimSpace(stringField(item, "call_id"))
			if callID == "" {
				return nil, nil, nil, &responsesRequestError{Message: param + ".call_id 不能为空", Param: param + ".call_id", Code: "invalid_parameter"}
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
			if execution != "client" {
				return nil, nil, nil, &responsesRequestError{Message: "Grok Build 只兼容客户端 tool_search_output", Param: param + ".execution", Code: "unsupported_parameter"}
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
			rewritten = append(rewritten, map[string]any{
				"type": "function_call_output", "call_id": callID,
				"output": fmt.Sprintf("Tool search completed; %d selected tool definitions are now available.", len(tools)),
			})
		case "custom_tool_call":
			name := strings.TrimSpace(stringField(item, "name"))
			if name == "" {
				return nil, nil, nil, &responsesRequestError{Message: param + ".name 不能为空", Param: param + ".name", Code: "invalid_parameter"}
			}
			input, ok := item["input"].(string)
			if !ok {
				return nil, nil, nil, &responsesRequestError{Message: param + ".input 必须是字符串", Param: param + ".input", Code: "invalid_parameter"}
			}
			arguments, err := encodeCustomToolArguments(input)
			if err != nil {
				return nil, nil, nil, err
			}
			namespace := strings.TrimSpace(stringField(item, "namespace"))
			converted := cloneJSONObject(item)
			converted["type"] = "function_call"
			converted["name"] = c.alias(responsesToolIdentity{Kind: responsesCustomTool, Namespace: namespace, Name: name})
			converted["arguments"] = arguments
			delete(converted, "input")
			delete(converted, "namespace")
			c.changed = true
			rewritten = append(rewritten, converted)
		case "custom_tool_call_output":
			converted := cloneJSONObject(item)
			converted["type"] = "function_call_output"
			delete(converted, "name")
			delete(converted, "namespace")
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
			rewritten = append(rewritten, cloneJSONValue(item))
		}
	}
	return rewritten, loadedTools, visibleTools, nil
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
