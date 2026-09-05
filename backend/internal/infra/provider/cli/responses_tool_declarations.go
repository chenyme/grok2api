package cli

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

func normalizeResponsesTools(payload map[string]json.RawMessage) (*responsesToolCompatibility, error) {
	if rawTools := payload["tools"]; len(rawTools) > 0 && strings.Contains(string(rawTools), "automation_update") {
		slog.Info("DEBUG_RAW_AUTOMATION_TOOLS", "tools", string(rawTools))
	}
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
	if rawTools := payload["tools"]; len(rawTools) > 0 && strings.Contains(string(rawTools), "automation_update") {
		slog.Info("DEBUG_NORMALIZED_AUTOMATION_TOOLS", "tools", string(rawTools))
	}
	if err := compatibility.normalizeToolChoice(payload, normalizedTools); err != nil {
		return nil, err
	}
	if !compatibility.changed && len(compatibility.functionSchemas) == 0 {
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
		if inputSchema, exists := converted["inputSchema"]; exists {
			converted["parameters"] = inputSchema
			delete(converted, "inputSchema")
			c.changed = true
		} else if inputSchema, exists := converted["input_schema"]; exists {
			converted["parameters"] = inputSchema
			delete(converted, "input_schema")
			c.changed = true
		}
		if parameters, exists := converted["parameters"]; exists {
			normalized, changed, normalizeErr := normalizeBuildFunctionParametersRoot(parameters, param+".parameters")
			if normalizeErr != nil {
				return nil, normalizeErr
			}
			if changed {
				converted["parameters"] = normalized
				c.changed = true
				c.addWarning("function_parameters_nullable_root_normalized")
			}
		}
		identity := responsesToolIdentity{Kind: responsesFunctionTool, Namespace: namespace, Name: name}
		alias := c.alias(identity)
		if parameters, exists := tool["parameters"]; exists && schemaContainsInteger(parameters) {
			c.functionSchemas[alias] = cloneJSONValue(parameters)
		}
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

// normalizeBuildFunctionParametersRoot removes root-level nullability from function schemas.
// Build requires the parameter root to be an object, while Codex can emit `object | null`
// for tools with an optional argument object. Nested nullable fields remain untouched.
func normalizeBuildFunctionParametersRoot(value any, param string) (any, bool, error) {
	schema, ok := value.(map[string]any)
	if !ok {
		return value, false, nil
	}
	normalized := cloneJSONObject(schema)
	changed := false

	if rawTypes, ok := normalized["type"].([]any); ok {
		filtered, removedNull := withoutNullSchemaTypes(rawTypes)
		if removedNull {
			changed = true
			switch len(filtered) {
			case 0:
				return nil, false, invalidBuildFunctionParametersRoot(param)
			case 1:
				if filtered[0] != "object" {
					return nil, false, invalidBuildFunctionParametersRoot(param)
				}
				normalized["type"] = "object"
			default:
				return nil, false, invalidBuildFunctionParametersRoot(param)
			}
		}
	}

	defs, _ := normalized["$defs"].(map[string]any)
	if defs == nil {
		defs, _ = normalized["definitions"].(map[string]any)
	}

	for _, keyword := range []string{"oneOf", "anyOf"} {
		rawBranches, exists := normalized[keyword]
		if !exists {
			continue
		}
		branches, ok := rawBranches.([]any)
		if !ok || len(branches) == 0 {
			continue
		}

		filtered := make([]any, 0, len(branches))
		removedNonObject := false
		for _, rawBranch := range branches {
			branch, branchOK := rawBranch.(map[string]any)
			if !branchOK || isNullOnlySchema(branch) || !isObjectRootSchema(branch, normalized, nil) {
				removedNonObject = true
				continue
			}
			filtered = append(filtered, rawBranch)
		}

		// 如果包含多分支，必须拍平消除顶层 keyword！
		if len(filtered) > 1 {
			changed = true
			delete(normalized, keyword)

			mergedProperties := make(map[string]any)
			if existingProps, ok := normalized["properties"].(map[string]any); ok {
				for k, v := range existingProps {
					mergedProperties[k] = v
				}
			}

			var mergedRequired []any
			if req, ok := normalized["required"].([]any); ok {
				mergedRequired = append(mergedRequired, req...)
			}

			var collectProps func(target any, depth int)
			collectProps = func(target any, depth int) {
				if depth > 10 || target == nil {
					return
				}
				obj, ok := target.(map[string]any)
				if !ok {
					return
				}
				if ref, ok := obj["$ref"].(string); ok && defs != nil {
					refKey := strings.TrimPrefix(ref, "#/$defs/")
					refKey = strings.TrimPrefix(refKey, "#/definitions/")
					if defObj, exists := defs[refKey]; exists {
						collectProps(defObj, depth+1)
					}
				}
				if props, ok := obj["properties"].(map[string]any); ok {
					for k, v := range props {
						if _, exists := mergedProperties[k]; !exists {
							mergedProperties[k] = v
						}
					}
				}
				if req, ok := obj["required"].([]any); ok {
					for _, r := range req {
						mergedRequired = append(mergedRequired, r)
					}
				}
				for _, subKw := range []string{"anyOf", "oneOf"} {
					if subBranches, exists := obj[subKw].([]any); exists {
						for _, b := range subBranches {
							collectProps(b, depth+1)
						}
					}
				}
			}

			for _, branch := range filtered {
				collectProps(branch, 0)
			}

			normalized["type"] = "object"
			normalized["properties"] = mergedProperties
			if len(mergedRequired) > 0 {
				normalized["required"] = dedupeSlice(mergedRequired)
			}
			continue
		}

		if !removedNonObject {
			continue
		}
		changed = true
		if len(filtered) == 0 {
			return nil, false, invalidBuildFunctionParametersRoot(param)
		}
		if len(filtered) == 1 {
			branch, ok := filtered[0].(map[string]any)
			if !ok || !isObjectRootSchema(branch, normalized, nil) {
				return nil, false, invalidBuildFunctionParametersRoot(param)
			}
			if len(normalized) == 1 {
				normalized = cloneJSONObject(branch)
				normalized["type"] = "object"
				continue
			}
			normalized[keyword] = cloneJSONArray(filtered)
			normalized["type"] = "object"
			continue
		}
	}

	if normalized["type"] != "object" {
		normalized["type"] = "object"
		changed = true
	}

	return normalized, changed, nil
}

func withoutNullSchemaTypes(types []any) ([]any, bool) {
	filtered := make([]any, 0, len(types))
	removed := false
	for _, value := range types {
		if value == "null" {
			removed = true
			continue
		}
		filtered = append(filtered, value)
	}
	return filtered, removed
}

func isNullOnlySchema(value any) bool {
	schema, ok := value.(map[string]any)
	if !ok {
		return false
	}
	if schema["type"] == "null" {
		return true
	}
	types, ok := schema["type"].([]any)
	if !ok || len(types) == 0 {
		return false
	}
	for _, value := range types {
		if value != "null" {
			return false
		}
	}
	return true
}

func isObjectRootSchema(schema, root map[string]any, visited map[string]struct{}) bool {
	rawType, hasType := schema["type"]
	if rawType == "object" {
		return true
	}
	if types, ok := rawType.([]any); ok && len(types) > 0 {
		for _, value := range types {
			if value != "object" {
				return false
			}
		}
		return true
	}
	if hasType {
		return false
	}
	if _, hasProperties := schema["properties"]; hasProperties {
		return true
	}
	ref, ok := schema["$ref"].(string)
	if !ok {
		return false
	}
	if visited == nil {
		visited = make(map[string]struct{})
	}
	if _, seen := visited[ref]; seen {
		return false
	}
	resolved, ok := resolveLocalSchemaRef(root, ref)
	if !ok {
		return false
	}
	visited[ref] = struct{}{}
	return isObjectRootSchema(resolved, root, visited)
}

func resolveLocalSchemaRef(root map[string]any, ref string) (map[string]any, bool) {
	if ref == "#" {
		return root, true
	}
	if !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	var current any = root
	for _, encoded := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		segment := strings.ReplaceAll(strings.ReplaceAll(encoded, "~1", "/"), "~0", "~")
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[segment]
		if !ok {
			return nil, false
		}
	}
	resolved, ok := current.(map[string]any)
	return resolved, ok
}

func invalidBuildFunctionParametersRoot(param string) error {
	return &responsesRequestError{
		Message: param + " 顶层必须是非 nullable 的 object schema",
		Param:   param,
		Code:    "invalid_parameter",
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

func dedupeSlice(slice []any) []any {
	seen := make(map[string]bool)
	var result []any
	for _, item := range slice {
		str, ok := item.(string)
		if ok {
			if !seen[str] {
				seen[str] = true
				result = append(result, str)
			}
		} else {
			result = append(result, item)
		}
	}
	return result
}
