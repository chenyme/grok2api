package cli

import (
	"fmt"
	"strings"
)

var nativeHostedToolChoiceTypes = map[string]string{
	"web_search":                    "web_search",
	"web_search_preview":            "web_search",
	"web_search_preview_2025_03_11": "web_search",
	"web_search_2025_08_26":         "web_search",
	"x_search":                      "x_search",
	"image_generation":              "image_generation",
	"collections_search":            "collections_search",
	"file_search":                   "file_search",
	"code_execution":                "code_execution",
	"code_interpreter":              "code_interpreter",
	"mcp":                           "mcp",
	"shell":                         "shell",
	"local_shell":                   "shell",
}

// webSearchCompatibilityFields 是 Codex/OpenAI 新版声明中已知、但 0.2.99 上游会以
// "Argument not supported" 拒绝的控制字段。Build 只能降级为其原生最小搜索工具。
var webSearchCompatibilityFields = map[string]struct{}{
	"external_web_access":  {},
	"indexed_web_access":   {},
	"search_content_types": {},
	"search_context_size":  {},
	"user_location":        {},
	"filters":              {},
	"allowed_domains":      {},
	"max_search_results":   {},
	"safe_search":          {},
}

// normalizeNativeTool 保留 0.2.99 已确认支持的工具，并拒绝只属于 Tool Search 的延迟字段。
func (c *responsesToolCompatibility) normalizeNativeTool(tool map[string]any, param string) ([]any, error) {
	if _, exists := tool["defer_loading"]; exists {
		return nil, &responsesRequestError{
			Message: "该工具类型不支持 defer_loading",
			Param:   param + ".defer_loading", Code: "unsupported_parameter",
		}
	}
	return []any{cloneJSONValue(tool)}, nil
}

// normalizeWebSearchTool 将 Codex/OpenAI 新版搜索声明降级为 0.2.99 支持的最小 web_search。
func (c *responsesToolCompatibility) normalizeWebSearchTool(tool map[string]any, kind, param string) ([]any, error) {
	if external, exists := tool["external_web_access"]; exists {
		enabled, ok := external.(bool)
		if !ok {
			return nil, &responsesRequestError{Message: "external_web_access 必须是布尔值", Param: param + ".external_web_access", Code: "invalid_parameter"}
		}
		if !enabled {
			// 0.2.99 不能表达“只允许索引、禁止外网”。发送最小 web_search
			// 会扩大客户端授权，因此直接移除该搜索工具，形成安全的能力子集。
			c.webSearchDisabled = true
			c.changed = true
			c.addWarning("web_search_disabled_no_external_access")
			return nil, nil
		}
	}
	if filters, exists := tool["filters"]; exists && hasNonEmptyWebSearchConstraint(filters) {
		return nil, &responsesRequestError{
			Message: "Grok Build 0.2.99 无法保证 web_search filters 约束",
			Param:   param + ".filters", Code: "unsupported_parameter",
		}
	}
	if domains, exists := tool["allowed_domains"]; exists && hasNonEmptyWebSearchConstraint(domains) {
		return nil, &responsesRequestError{
			Message: "Grok Build 0.2.99 无法保证 allowed_domains 约束",
			Param:   param + ".allowed_domains", Code: "unsupported_parameter",
		}
	}
	if contentTypes, exists := tool["search_content_types"]; exists {
		values, ok := contentTypes.([]any)
		if !ok {
			return nil, &responsesRequestError{Message: "search_content_types 必须是数组", Param: param + ".search_content_types", Code: "invalid_parameter"}
		}
		for _, value := range values {
			if value != "text" {
				return nil, &responsesRequestError{
					Message: "Grok Build 0.2.99 只能兼容文本 Web Search",
					Param:   param + ".search_content_types", Code: "unsupported_parameter",
				}
			}
		}
	}
	for key := range tool {
		if key == "type" {
			continue
		}
		if _, compatible := webSearchCompatibilityFields[key]; !compatible {
			return nil, &responsesRequestError{
				Message: "Grok Build 0.2.99 不支持该 web_search 字段",
				Param:   param + "." + key, Code: "unsupported_parameter",
			}
		}
		c.changed = true
		c.addWarning("web_search_controls_downgraded")
	}
	if kind == "web_search" && len(tool) == 1 {
		return []any{cloneJSONValue(tool)}, nil
	}
	c.changed = true
	return []any{map[string]any{"type": "web_search"}}, nil
}

func hasNonEmptyWebSearchConstraint(value any) bool {
	if value == nil {
		return false
	}
	switch typed := value.(type) {
	case []any:
		return len(typed) > 0
	case map[string]any:
		for _, item := range typed {
			if hasNonEmptyWebSearchConstraint(item) {
				return true
			}
		}
		return false
	case string:
		return strings.TrimSpace(typed) != ""
	case bool:
		return typed
	default:
		return true
	}
}

// normalizeMCPTool 支持客户端 Tool Search 延迟加载整个 MCP server 定义。
func (c *responsesToolCompatibility) normalizeMCPTool(tool map[string]any, clientSearch, force bool, param string) ([]any, error) {
	deferred, _ := tool["defer_loading"].(bool)
	if deferred && !clientSearch && !force {
		return nil, &responsesRequestError{
			Message: "MCP defer_loading: true 需要 execution: \"client\" 的 tool_search",
			Param:   param + ".defer_loading", Code: "invalid_parameter",
		}
	}
	if deferred && clientSearch && !force {
		label := strings.TrimSpace(stringField(tool, "server_label"))
		if label == "" {
			label = strings.TrimSpace(stringField(tool, "name"))
		}
		if label == "" {
			return nil, &responsesRequestError{Message: "延迟 MCP 工具缺少 server_label", Param: param + ".server_label", Code: "invalid_parameter"}
		}
		c.deferredSurfaces = append(c.deferredSurfaces, describeDeferredTool(label, stringField(tool, "description")))
		c.changed = true
		return nil, nil
	}
	converted := cloneJSONObject(tool)
	if _, exists := converted["defer_loading"]; exists {
		delete(converted, "defer_loading")
		c.changed = true
	}
	return []any{converted}, nil
}

func unsupportedBuildToolError(kind, param string) error {
	return &responsesRequestError{
		Message: fmt.Sprintf("Grok Build 0.2.99 不支持 tools.type=%q", kind),
		Param:   param + ".type", Code: "unsupported_parameter",
	}
}

func normalizeHostedToolChoiceKind(kind string) string {
	return nativeHostedToolChoiceTypes[kind]
}

func hasSingleToolType(tools []any, kind string) bool {
	if len(tools) != 1 {
		return false
	}
	tool, ok := tools[0].(map[string]any)
	return ok && stringField(tool, "type") == kind
}

func hasToolType(tools []any, kind string) bool {
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if ok && stringField(tool, "type") == kind {
			return true
		}
	}
	return false
}
