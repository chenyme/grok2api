package cli

import (
	"encoding/json"
	"fmt"
	"strings"
)

func (c *responsesToolCompatibility) normalizeToolChoice(payload map[string]json.RawMessage, normalizedTools []any) error {
	raw := payload["tool_choice"]
	if isEmptyJSON(raw) {
		return nil
	}
	if len(normalizedTools) == 0 {
		delete(payload, "tool_choice")
		c.changed = true
		c.addWarning("tool_choice_without_tools_ignored")
		if c.serverSearchEager {
			c.addWarning("server_tool_search_choice_downgraded")
		}
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
			if c.serverSearchEager {
				payload["tool_choice"] = mustJSON("auto")
				c.changed = true
				c.addWarning("server_tool_search_choice_downgraded")
				return nil
			}
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
		matching := toolsOfType(normalizedTools, normalizedKind)
		if len(matching) == 0 {
			return &responsesRequestError{Message: "tool_choice 引用了未声明的 hosted tool", Param: "tool_choice", Code: "invalid_parameter"}
		}
		if len(matching) != len(normalizedTools) {
			// 上游只支持 required，收窄本轮可见工具即可保持“指定该类工具”的语义。
			payload["tools"] = mustJSON(matching)
			c.addWarning("hosted_tool_choice_tools_narrowed")
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

func toolsOfType(tools []any, kind string) []any {
	matching := make([]any, 0)
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if ok && stringField(tool, "type") == kind {
			matching = append(matching, rawTool)
		}
	}
	return matching
}
