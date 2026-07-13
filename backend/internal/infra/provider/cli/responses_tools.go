package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	maxBuildToolAliasLength       = 128
	maxToolSearchDescriptionBytes = 16 << 10
)

type responsesToolKind uint8

const (
	responsesFunctionTool responsesToolKind = iota
	responsesCustomTool
	responsesToolSearch
	responsesApplyPatchTool
)

type responsesToolIdentity struct {
	Kind      responsesToolKind
	Namespace string
	Name      string
}

func (i responsesToolIdentity) key() string {
	return fmt.Sprintf("%d\x00%s\x00%s", i.Kind, i.Namespace, i.Name)
}

// responsesToolCompatibility 保存一次请求内的工具别名，避免跨请求共享可变协议状态。
type responsesToolCompatibility struct {
	aliases           map[string]responsesToolIdentity
	identityAliases   map[string]string
	visibleTools      []any
	deferredSurfaces  []string
	clientSearchTool  map[string]any
	clientSearchParam string
	streamCalls       map[string]*responsesStreamCall
	legacyLocalShell  bool
	nativeShell       bool
	webSearchDisabled bool
	warnings          []string
	warningSet        map[string]struct{}
	changed           bool
}

// responsesRequestError 表示可直接映射为 OpenAI 错误结构的 Provider 请求错误。
type responsesRequestError struct {
	Message string
	Param   string
	Code    string
}

func (e *responsesRequestError) Error() string { return e.Message }

func newResponsesToolCompatibility() *responsesToolCompatibility {
	return &responsesToolCompatibility{
		aliases:         make(map[string]responsesToolIdentity),
		identityAliases: make(map[string]string),
		streamCalls:     make(map[string]*responsesStreamCall),
		warningSet:      make(map[string]struct{}),
	}
}

// normalizeResponsesTools 将 namespace 和客户端 Tool Search 收敛为 Grok Build 支持的普通函数协议。
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
	} else if hasTools && compatibility.webSearchDisabled {
		delete(payload, "tools")
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
		return &responsesRequestError{
			Message: "客户端 tool_search 暂不支持并行工具调用",
			Param:   "parallel_tool_calls", Code: "unsupported_parameter",
		}
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
			return false, &responsesRequestError{
				Message: "Grok Build 上游不支持服务端 tool_search；请使用 execution: \"client\"",
				Param:   param + ".execution", Code: "unsupported_parameter",
			}
		}
		if execution != "client" {
			return false, &responsesRequestError{Message: "tool_search.execution 只支持 client", Param: param + ".execution", Code: "unsupported_parameter"}
		}
		if clientSearch {
			return false, &responsesRequestError{Message: "单次请求只能声明一个客户端 tool_search", Param: param, Code: "invalid_parameter"}
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
			return nil, &responsesRequestError{
				Message: "defer_loading: true 需要 execution: \"client\" 的 tool_search",
				Param:   param + ".defer_loading", Code: "invalid_parameter",
			}
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
				return nil, &responsesRequestError{Message: "namespace 内只支持 function 工具", Param: childParam + ".type", Code: "unsupported_parameter"}
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
			return nil, &responsesRequestError{Message: "tool_search_output.tools 不能再次声明 tool_search", Param: param, Code: "unsupported_parameter"}
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
		return c.normalizeApplyPatchTool(tool, namespace, param)
	case "x_search", "image_generation", "collections_search", "file_search", "code_execution", "code_interpreter":
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
			converted := map[string]any{
				"type": "function_call", "call_id": callID,
				"name": c.alias(responsesToolIdentity{Kind: responsesToolSearch, Name: "tool_search"}), "arguments": arguments,
			}
			c.changed = true
			rewritten = append(rewritten, converted)
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

func (c *responsesToolCompatibility) normalizeToolChoice(payload map[string]json.RawMessage, normalizedTools []any) error {
	raw := payload["tool_choice"]
	if isEmptyJSON(raw) {
		return nil
	}
	var choice any
	if err := json.Unmarshal(raw, &choice); err != nil {
		return &responsesRequestError{Message: "tool_choice 格式无效", Param: "tool_choice", Code: "invalid_parameter"}
	}
	object, ok := choice.(map[string]any)
	if !ok {
		if value, isString := choice.(string); isString && (value == "auto" || value == "required") && c.webSearchDisabled && len(normalizedTools) == 0 {
			payload["tool_choice"] = mustJSON("none")
			c.changed = true
			c.addWarning("web_search_tool_choice_disabled")
		}
		return nil
	}
	kind := stringField(object, "type")
	if c.webSearchDisabled && normalizeHostedToolChoiceKind(kind) == "web_search" && !hasToolType(normalizedTools, "web_search") {
		payload["tool_choice"] = mustJSON("none")
		c.changed = true
		c.addWarning("web_search_tool_choice_disabled")
		return nil
	}
	if kind == "tool_search" {
		if c.clientSearchTool == nil {
			return &responsesRequestError{Message: "tool_choice 引用了未声明的 tool_search", Param: "tool_choice", Code: "invalid_parameter"}
		}
		object = map[string]any{
			"type": "function",
			"name": c.alias(responsesToolIdentity{Kind: responsesToolSearch, Name: "tool_search"}),
		}
		c.changed = true
		payload["tool_choice"] = mustJSON(object)
		return nil
	}
	if kind == "custom" {
		name := strings.TrimSpace(stringField(object, "name"))
		namespace := strings.TrimSpace(stringField(object, "namespace"))
		if name == "" {
			return &responsesRequestError{Message: "tool_choice.name 不能为空", Param: "tool_choice.name", Code: "invalid_parameter"}
		}
		identity := responsesToolIdentity{Kind: responsesCustomTool, Namespace: namespace, Name: name}
		alias, exists := c.identityAliases[identity.key()]
		if !exists {
			return &responsesRequestError{Message: "tool_choice 引用了未声明的 custom 工具", Param: "tool_choice.name", Code: "invalid_parameter"}
		}
		object["type"] = "function"
		object["name"] = alias
		delete(object, "namespace")
		c.changed = true
		payload["tool_choice"] = mustJSON(object)
		return nil
	}
	if kind == "apply_patch" {
		identity := responsesToolIdentity{Kind: responsesApplyPatchTool, Name: "apply_patch"}
		alias, exists := c.identityAliases[identity.key()]
		if !exists {
			return &responsesRequestError{Message: "tool_choice 引用了未声明的 apply_patch 工具", Param: "tool_choice", Code: "invalid_parameter"}
		}
		payload["tool_choice"] = mustJSON(map[string]any{"type": "function", "name": alias})
		c.changed = true
		return nil
	}
	if normalizedKind := normalizeHostedToolChoiceKind(kind); normalizedKind != "" {
		if !hasSingleToolType(normalizedTools, normalizedKind) {
			return &responsesRequestError{
				Message: "Grok Build 仅能在请求中只有一个匹配工具时兼容 hosted tool_choice",
				Param:   "tool_choice", Code: "unsupported_parameter",
			}
		}
		payload["tool_choice"] = mustJSON("required")
		c.changed = true
		return nil
	}
	if kind != "function" {
		return &responsesRequestError{Message: fmt.Sprintf("Grok Build 不支持 tool_choice.type=%q", kind), Param: "tool_choice.type", Code: "unsupported_parameter"}
	}
	name := strings.TrimSpace(stringField(object, "name"))
	namespace := strings.TrimSpace(stringField(object, "namespace"))
	if function, nested := object["function"].(map[string]any); nested {
		name = strings.TrimSpace(stringField(function, "name"))
		namespace = strings.TrimSpace(stringField(function, "namespace"))
		if name != "" && namespace != "" {
			identity := responsesToolIdentity{Kind: responsesFunctionTool, Namespace: namespace, Name: name}
			alias, exists := c.identityAliases[identity.key()]
			if !exists {
				return &responsesRequestError{Message: "tool_choice 引用了未声明的 namespace 函数", Param: "tool_choice.function.name", Code: "invalid_parameter"}
			}
			function["name"] = alias
			delete(function, "namespace")
			c.changed = true
			payload["tool_choice"] = mustJSON(object)
		}
		return nil
	}
	if name == "" || namespace == "" {
		return nil
	}
	identity := responsesToolIdentity{Kind: responsesFunctionTool, Namespace: namespace, Name: name}
	alias, exists := c.identityAliases[identity.key()]
	if !exists {
		return &responsesRequestError{Message: "tool_choice 引用了未声明的 namespace 函数", Param: "tool_choice.name", Code: "invalid_parameter"}
	}
	object["name"] = alias
	delete(object, "namespace")
	c.changed = true
	payload["tool_choice"] = mustJSON(object)
	return nil
}

func (c *responsesToolCompatibility) alias(identity responsesToolIdentity) string {
	key := identity.key()
	if alias, exists := c.identityAliases[key]; exists {
		return alias
	}
	base := identity.Name
	if identity.Kind == responsesToolSearch {
		base = "grok2api_tool_search"
	} else if identity.Kind == responsesApplyPatchTool {
		base = "grok2api_apply_patch"
	} else if identity.Namespace != "" {
		separator := "__"
		if strings.HasSuffix(identity.Namespace, separator) {
			separator = ""
		}
		base = identity.Namespace + separator + identity.Name
	}
	alias := truncateToolAlias(base, key)
	if existing, collision := c.aliases[alias]; collision && existing.key() != key {
		alias = hashedToolAlias(base, key)
	}
	c.aliases[alias] = identity
	c.identityAliases[key] = alias
	return alias
}

func truncateToolAlias(base, key string) string {
	if len(base) <= maxBuildToolAliasLength {
		return base
	}
	return hashedToolAlias(base, key)
}

func hashedToolAlias(base, key string) string {
	suffix := "__" + shortToolHash(key)
	limit := maxBuildToolAliasLength - len(suffix)
	if len(base) > limit {
		base = base[:limit]
	}
	return base + suffix
}

func shortToolHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:9]
}

func stringField(value map[string]any, key string) string {
	text, _ := value[key].(string)
	return text
}

func cloneJSONArray(values []any) []any {
	cloned := make([]any, len(values))
	for index, value := range values {
		cloned[index] = cloneJSONValue(value)
	}
	return cloned
}

func cloneJSONObject(value map[string]any) map[string]any {
	cloned := make(map[string]any, len(value))
	for key, item := range value {
		cloned[key] = cloneJSONValue(item)
	}
	return cloned
}

func cloneJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneJSONObject(typed)
	case []any:
		return cloneJSONArray(typed)
	default:
		return value
	}
}
