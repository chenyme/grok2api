package cli

import (
	"encoding/json"
	"fmt"
	"strings"
)

func normalizeResponsesTools(payload map[string]json.RawMessage) (*responsesToolCompatibility, error) {
	compatibility := newResponsesToolCompatibility()
	tools, hasTools, err := decodeOptionalArray(payload["tools"], "tools")
	if err != nil {
		return nil, err
	}
	if hasTools {
		compatibility.visibleTools = cloneJSONArray(tools)
	}
	clientSearch, err := inspectToolSearch(tools)
	if err != nil {
		return nil, err
	}
	if err := compatibility.normalizeClientToolSearchParallel(payload, clientSearch); err != nil {
		return nil, err
	}

	normalizedTools := make([]any, 0, len(tools))
	for index, rawTool := range tools {
		converted, convertErr := compatibility.normalizeTool(rawTool, "", clientSearch, false, fmt.Sprintf("tools[%d]", index))
		if convertErr != nil {
			return nil, convertErr
		}
		normalizedTools = append(normalizedTools, converted...)
	}

	if rawInput := payload["input"]; !isEmptyJSON(rawInput) {
		var input any
		if err := json.Unmarshal(rawInput, &input); err != nil {
			return nil, &responsesRequestError{Message: "input 必须是字符串或数组", Param: "input", Code: "invalid_parameter"}
		}
		if items, ok := input.([]any); ok {
			rewritten, loadedTools, visibleTools, rewriteErr := compatibility.normalizeInputItems(items)
			if rewriteErr != nil {
				return nil, rewriteErr
			}
			normalizedTools = append(normalizedTools, loadedTools...)
			compatibility.visibleTools = append(compatibility.visibleTools, visibleTools...)
			payload["input"] = mustJSON(rewritten)
		}
	}

	if compatibility.clientSearchTool != nil {
		searchTool, searchErr := compatibility.buildClientSearchFunction()
		if searchErr != nil {
			return nil, searchErr
		}
		normalizedTools = append(normalizedTools, searchTool)
	}
	normalizedTools = dedupeNormalizedTools(normalizedTools)
	if len(normalizedTools) > 0 {
		payload["tools"] = mustJSON(normalizedTools)
	} else if hasTools {
		delete(payload, "tools")
		if _, exists := payload["parallel_tool_calls"]; exists {
			delete(payload, "parallel_tool_calls")
			compatibility.addWarning("parallel_tool_calls_without_tools_ignored")
		}
		compatibility.changed = true
	}
	if err := compatibility.normalizeToolChoice(payload, normalizedTools); err != nil {
		return nil, err
	}
	if !compatibility.changed {
		return nil, nil
	}
	return compatibility, nil
}

// normalizeClientToolSearchParallel 保证搜索函数先独立完成，再由客户端选择并回传工具定义。
func (c *responsesToolCompatibility) normalizeClientToolSearchParallel(payload map[string]json.RawMessage, clientSearch bool) error {
	if !clientSearch {
		return nil
	}
	raw, exists := payload["parallel_tool_calls"]
	if !exists || isEmptyJSON(raw) {
		payload["parallel_tool_calls"] = mustJSON(false)
		c.changed = true
		return nil
	}
	var parallel bool
	if err := json.Unmarshal(raw, &parallel); err != nil {
		return &responsesRequestError{
			Message: "parallel_tool_calls 必须是布尔值",
			Param:   "parallel_tool_calls", Code: "invalid_parameter",
		}
	}
	if parallel {
		payload["parallel_tool_calls"] = mustJSON(false)
		c.changed = true
		c.addWarning("client_tool_search_forced_serial")
	}
	return nil
}

func decodeOptionalArray(raw json.RawMessage, param string) ([]any, bool, error) {
	if isEmptyJSON(raw) {
		return nil, false, nil
	}
	var values []any
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, false, &responsesRequestError{Message: param + " 必须是数组", Param: param, Code: "invalid_parameter"}
	}
	return values, true, nil
}

func inspectToolSearch(tools []any) (bool, error) {
	clientSearch := false
	serverSearch := false
	for index, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			return false, &responsesRequestError{Message: fmt.Sprintf("tools[%d] 必须是对象", index), Param: fmt.Sprintf("tools[%d]", index), Code: "invalid_parameter"}
		}
		if stringField(tool, "type") != "tool_search" {
			continue
		}
		param := fmt.Sprintf("tools[%d]", index)
		execution := strings.ToLower(strings.TrimSpace(stringField(tool, "execution")))
		if execution == "" || execution == "server" {
			if clientSearch {
				return false, &responsesRequestError{Message: "单次请求不能混用 client 与 server tool_search", Param: param + ".execution", Code: "invalid_parameter"}
			}
			serverSearch = true
			continue
		}
		if execution != "client" {
			return false, &responsesRequestError{Message: "tool_search.execution 必须是 client 或 server", Param: param + ".execution", Code: "invalid_parameter"}
		}
		if clientSearch {
			return false, &responsesRequestError{Message: "单次请求只能声明一个客户端 tool_search", Param: param, Code: "invalid_parameter"}
		}
		if serverSearch {
			return false, &responsesRequestError{Message: "单次请求不能混用 client 与 server tool_search", Param: param + ".execution", Code: "invalid_parameter"}
		}
		clientSearch = true
	}
	return clientSearch, nil
}

func (c *responsesToolCompatibility) normalizeTool(raw any, namespace string, clientSearch, force bool, param string) ([]any, error) {
	tool, ok := raw.(map[string]any)
	if !ok {
		return nil, &responsesRequestError{Message: param + " 必须是对象", Param: param, Code: "invalid_parameter"}
	}
	kind := strings.TrimSpace(stringField(tool, "type"))
	switch kind {
	case "function":
		name := strings.TrimSpace(stringField(tool, "name"))
		if name == "" {
			return nil, &responsesRequestError{Message: param + ".name 不能为空", Param: param + ".name", Code: "invalid_parameter"}
		}
		deferred, _ := tool["defer_loading"].(bool)
		if deferred && !clientSearch && !force {
			c.changed = true
			c.addWarning("orphan_deferred_tool_loaded")
		}
		if deferred && clientSearch && !force {
			c.changed = true
			if namespace == "" {
				c.deferredSurfaces = append(c.deferredSurfaces, describeDeferredTool(name, stringField(tool, "description")))
			}
			return nil, nil
		}
		converted := cloneJSONObject(tool)
		identity := responsesToolIdentity{Kind: responsesFunctionTool, Namespace: namespace, Name: name}
		alias := c.alias(identity)
		converted["name"] = alias
		if namespace != "" || alias != name {
			c.changed = true
		}
		if _, exists := converted["defer_loading"]; exists {
			delete(converted, "defer_loading")
			c.changed = true
		}
		return []any{converted}, nil
	case "namespace":
		name := strings.TrimSpace(stringField(tool, "name"))
		if name == "" {
			return nil, &responsesRequestError{Message: param + ".name 不能为空", Param: param + ".name", Code: "invalid_parameter"}
		}
		children, ok := tool["tools"].([]any)
		if !ok {
			return nil, &responsesRequestError{Message: param + ".tools 必须是数组", Param: param + ".tools", Code: "invalid_parameter"}
		}
		c.changed = true
		c.addWarning("namespace_flattened")
		if clientSearch && !force && namespaceHasDeferredFunctions(children) {
			c.deferredSurfaces = append(c.deferredSurfaces, describeDeferredTool(name, stringField(tool, "description")))
		}
		converted := make([]any, 0, len(children))
		for index, rawChild := range children {
			child, childOK := rawChild.(map[string]any)
			childParam := fmt.Sprintf("%s.tools[%d]", param, index)
			if !childOK {
				return nil, &responsesRequestError{Message: childParam + " 必须是对象", Param: childParam, Code: "invalid_parameter"}
			}
			if stringField(child, "type") != "function" {
				return nil, &responsesRequestError{Message: "namespace.tools 只能包含 function 工具", Param: childParam + ".type", Code: "invalid_parameter"}
			}
			items, err := c.normalizeTool(child, name, clientSearch, force, childParam)
			if err != nil {
				return nil, err
			}
			converted = append(converted, items...)
		}
		return converted, nil
	case "tool_search":
		if force {
			c.changed = true
			c.addWarning("nested_tool_search_ignored")
			return nil, nil
		}
		execution := strings.ToLower(strings.TrimSpace(stringField(tool, "execution")))
		if execution == "" || execution == "server" {
			// Build 上游没有服务端 Tool Search。将已声明的延迟工具提前展开，
			// 比让 Codex 因一个可选优化能力整次失败更符合兼容层语义。
			c.serverSearchEager = true
			c.changed = true
			c.addWarning("server_tool_search_eager_loaded")
			return nil, nil
		}
		c.changed = true
		c.addWarning("client_tool_search_emulated")
		c.clientSearchTool = cloneJSONObject(tool)
		c.clientSearchParam = param
		return nil, nil
	case "custom":
		return c.normalizeCustomTool(tool, namespace, param)
	case "web_search", "web_search_preview", "web_search_preview_2025_03_11", "web_search_2025_08_26":
		return c.normalizeWebSearchTool(tool, kind, param)
	case "mcp":
		return c.normalizeMCPTool(tool, clientSearch, force, param)
	case "shell":
		return c.normalizeShellTool(tool, param)
	case "local_shell":
		return c.normalizeLegacyLocalShellTool(tool, param)
	case "apply_patch":
		return c.normalizeApplyPatchTool(tool, param)
	case "x_search":
		return c.normalizeXSearchTool(tool, param)
	case "image_generation", "collections_search", "file_search", "code_execution", "code_interpreter":
		return c.normalizeNativeTool(tool, param)
	case "computer_use_preview":
		return nil, unsupportedBuildToolError(kind, param)
	default:
		if kind == "" {
			return nil, &responsesRequestError{Message: param + ".type 不能为空", Param: param + ".type", Code: "invalid_parameter"}
		}
		return nil, unsupportedBuildToolError(kind, param)
	}
}

func namespaceHasDeferredFunctions(children []any) bool {
	for _, rawChild := range children {
		child, ok := rawChild.(map[string]any)
		if !ok || stringField(child, "type") != "function" {
			continue
		}
		if deferred, _ := child["defer_loading"].(bool); deferred {
			return true
		}
	}
	return false
}

func describeDeferredTool(name, description string) string {
	description = strings.TrimSpace(description)
	if description == "" {
		return name
	}
	if len(description) > 240 {
		description = description[:240]
	}
	return name + ": " + description
}

func (c *responsesToolCompatibility) buildClientSearchFunction() (map[string]any, error) {
	identity := responsesToolIdentity{Kind: responsesToolSearch, Name: "tool_search"}
	description := strings.TrimSpace(stringField(c.clientSearchTool, "description"))
	if description == "" {
		description = "Search for tools needed to continue the task."
	}
	if len(c.deferredSurfaces) > 0 {
		description += "\nDeferred tool surfaces available to search:\n- " + strings.Join(c.deferredSurfaces, "\n- ")
	}
	if len(description) > maxToolSearchDescriptionBytes {
		description = description[:maxToolSearchDescriptionBytes]
	}
	parameters, exists := c.clientSearchTool["parameters"]
	if !exists {
		parameters = map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": true}
	} else if _, ok := parameters.(map[string]any); !ok {
		return nil, &responsesRequestError{Message: "tool_search.parameters 必须是对象", Param: c.clientSearchParam + ".parameters", Code: "invalid_parameter"}
	}
	return map[string]any{
		"type": "function", "name": c.alias(identity), "description": description,
		"parameters": cloneJSONValue(parameters),
	}, nil
}
