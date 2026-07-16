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

// webSearchCompatibilityFields 是 Codex/OpenAI 新版声明中已知、但 0.2.101 上游会以
// "Argument not supported" 拒绝的控制字段。Build 只能降级为其原生最小搜索工具。
var webSearchCompatibilityFields = map[string]struct{}{
	"external_web_access":  {},
	"indexed_web_access":   {},
	"search_content_types": {},
	"search_context_size":  {},
	"user_location":        {},
	"max_search_results":   {},
	"safe_search":          {},
}

// normalizeNativeTool 保留 0.2.101 已确认支持的工具，并拒绝只属于 Tool Search 的延迟字段。
func (c *responsesToolCompatibility) normalizeNativeTool(tool map[string]any, _ string) ([]any, error) {
	converted := cloneJSONObject(tool)
	if _, exists := converted["defer_loading"]; exists {
		delete(converted, "defer_loading")
		c.changed = true
		c.addWarning("orphan_deferred_tool_loaded")
	}
	return []any{converted}, nil
}

// normalizeWebSearchTool 保留 0.2.101 原生支持的 allowed_domains 约束，
// 并将无法等价表达的新版控制字段安全降级。
func (c *responsesToolCompatibility) normalizeWebSearchTool(tool map[string]any, kind, param string) ([]any, error) {
	if external, exists := tool["external_web_access"]; exists {
		enabled, ok := external.(bool)
		if !ok {
			return nil, &responsesRequestError{Message: "external_web_access 必须是布尔值", Param: param + ".external_web_access", Code: "invalid_parameter"}
		}
		if !enabled {
			// 0.2.101 不能表达“只允许索引、禁止外网”。发送最小 web_search
			// 会扩大客户端授权，因此直接移除该搜索工具，形成安全的能力子集。
			c.webSearchDisabled = true
			c.changed = true
			c.addWarning("web_search_disabled_no_external_access")
			return nil, nil
		}
	}
	filters, err := c.normalizeWebSearchFilters(tool, param)
	if err != nil {
		return nil, err
	}
	if contentTypes, exists := tool["search_content_types"]; exists {
		values, ok := contentTypes.([]any)
		if !ok {
			return nil, &responsesRequestError{Message: "search_content_types 必须是数组", Param: param + ".search_content_types", Code: "invalid_parameter"}
		}
		for _, value := range values {
			if value != "text" {
				c.changed = true
				c.addWarning("web_search_non_text_content_ignored")
			}
		}
	}
	for key := range tool {
		if key == "type" {
			continue
		}
		if key == "filters" {
			continue
		}
		if key == "allowed_domains" {
			c.changed = true
			c.addWarning("web_search_allowed_domains_normalized")
			continue
		}
		if _, compatible := webSearchCompatibilityFields[key]; !compatible {
			c.changed = true
			c.addWarning("web_search_unknown_controls_ignored")
			continue
		}
		c.changed = true
		c.addWarning("web_search_controls_downgraded")
	}
	if kind == "web_search" && len(tool) == 1 {
		return []any{cloneJSONValue(tool)}, nil
	}
	converted := map[string]any{"type": "web_search"}
	if filters != nil {
		converted["filters"] = filters
	}
	if kind != "web_search" || len(tool) != len(converted) {
		c.changed = true
	}
	return []any{converted}, nil
}

func (c *responsesToolCompatibility) normalizeWebSearchFilters(tool map[string]any, param string) (map[string]any, error) {
	var nestedDomains []any
	if rawFilters, exists := tool["filters"]; exists && rawFilters != nil {
		filters, ok := rawFilters.(map[string]any)
		if !ok {
			return nil, &responsesRequestError{Message: "web_search filters 必须是对象", Param: param + ".filters", Code: "invalid_parameter"}
		}
		for key := range filters {
			if key != "allowed_domains" {
				c.changed = true
				c.addWarning("web_search_filters_downgraded")
			}
		}
		if rawDomains, exists := filters["allowed_domains"]; exists {
			values, err := normalizeAllowedDomains(rawDomains, param+".filters.allowed_domains")
			if err != nil {
				return nil, err
			}
			nestedDomains = values
		}
	}

	var topLevelDomains []any
	if rawDomains, exists := tool["allowed_domains"]; exists {
		values, err := normalizeAllowedDomains(rawDomains, param+".allowed_domains")
		if err != nil {
			return nil, err
		}
		topLevelDomains = values
	}
	if len(nestedDomains) > 0 && len(topLevelDomains) > 0 && !sameStringValues(nestedDomains, topLevelDomains) {
		return nil, &responsesRequestError{Message: "web_search allowed_domains 声明冲突", Param: param + ".allowed_domains", Code: "invalid_parameter"}
	}
	domains := nestedDomains
	if len(domains) == 0 {
		domains = topLevelDomains
	}
	if len(domains) == 0 {
		return nil, nil
	}
	return map[string]any{"allowed_domains": cloneJSONValue(domains)}, nil
}

func normalizeAllowedDomains(value any, param string) ([]any, error) {
	values, ok := value.([]any)
	if !ok {
		return nil, &responsesRequestError{Message: "allowed_domains 必须是字符串数组", Param: param, Code: "invalid_parameter"}
	}
	for index, value := range values {
		domain, ok := value.(string)
		if !ok || strings.TrimSpace(domain) == "" {
			return nil, &responsesRequestError{Message: "allowed_domains 必须包含有效域名", Param: fmt.Sprintf("%s[%d]", param, index), Code: "invalid_parameter"}
		}
	}
	return values, nil
}

func sameStringValues(left, right []any) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// normalizeMCPTool 支持客户端 Tool Search 延迟加载整个 MCP server 定义。
func (c *responsesToolCompatibility) normalizeMCPTool(tool map[string]any, clientSearch, force bool, param string) ([]any, error) {
	deferred, _ := tool["defer_loading"].(bool)
	if deferred && !clientSearch && !force {
		c.changed = true
		c.addWarning("orphan_deferred_tool_loaded")
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
		Message: fmt.Sprintf("Grok Build 0.2.101 不支持 tools.type=%q", kind),
		Param:   param + ".type", Code: "unsupported_parameter",
	}
}

func normalizeHostedToolChoiceKind(kind string) string {
	return nativeHostedToolChoiceTypes[kind]
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
