package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
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

// webSearchCompatibilityFields lists newer Codex/OpenAI controls that the verified Build wire contract rejects.
// The compatibility layer can only reduce them to Build's native minimal search tool.
var webSearchCompatibilityFields = map[string]struct{}{
	"external_web_access":  {},
	"indexed_web_access":   {},
	"search_content_types": {},
	"search_context_size":  {},
	"user_location":        {},
	"max_search_results":   {},
	"safe_search":          {},
}

const maxWebSearchDomains = conversation.MaxWebSearchDomains

// normalizeNativeTool preserves native Build tools and removes Tool Search-only fields.
func (c *responsesToolCompatibility) normalizeNativeTool(tool map[string]any, _ string) ([]any, error) {
	converted := cloneJSONObject(tool)
	if _, exists := converted["defer_loading"]; exists {
		delete(converted, "defer_loading")
		c.changed = true
		c.addWarning("orphan_deferred_tool_loaded")
	}
	return []any{converted}, nil
}

func (c *responsesToolCompatibility) normalizeXSearchTool(tool map[string]any, param string) ([]any, error) {
	converted := cloneJSONObject(tool)
	var fromDate, toDate time.Time
	var hasFromDate, hasToDate bool
	for _, field := range []string{"from_date", "to_date"} {
		value, exists := converted[field]
		if !exists {
			continue
		}
		if value == nil {
			delete(converted, field)
			c.changed = true
			continue
		}
		text, ok := value.(string)
		date, err := time.Parse("2006-01-02", text)
		if !ok || err != nil || len(text) != len("2006-01-02") || date.Year() < 1 || date.Format("2006-01-02") != text {
			return nil, &responsesRequestError{
				Message: field + " 必须使用 YYYY-MM-DD 格式",
				Param:   param + "." + field,
				Code:    "invalid_parameter",
			}
		}
		if field == "from_date" {
			fromDate, hasFromDate = date, true
		} else {
			toDate, hasToDate = date, true
		}
	}
	if hasFromDate && hasToDate && fromDate.After(toDate) {
		return nil, &responsesRequestError{
			Message: "from_date 不得晚于 to_date",
			Param:   param + ".from_date",
			Code:    "invalid_parameter",
		}
	}
	return c.normalizeNativeTool(converted, param)
}

// normalizeWebSearchTool preserves Build's allowed/excluded domain constraints and safely
// reduces newer controls that cannot be represented with equivalent semantics.
func (c *responsesToolCompatibility) normalizeWebSearchTool(tool map[string]any, kind, param string) ([]any, error) {
	if external, exists := tool["external_web_access"]; exists {
		enabled, ok := external.(bool)
		if !ok {
			return nil, &responsesRequestError{Message: "external_web_access 必须是布尔值", Param: param + ".external_web_access", Code: "invalid_parameter"}
		}
		if !enabled {
			// Build cannot express indexed-only search without external access. Sending a minimal
			// web_search would broaden the client's authorization, so remove the tool instead.
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
		if key == "excluded_domains" {
			c.changed = true
			c.addWarning("web_search_excluded_domains_normalized")
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
	nested := make(map[string][]any, 2)
	if rawFilters, exists := tool["filters"]; exists && rawFilters != nil {
		filters, ok := rawFilters.(map[string]any)
		if !ok {
			return nil, &responsesRequestError{Message: "web_search filters 必须是对象", Param: param + ".filters", Code: "invalid_parameter"}
		}
		for key := range filters {
			if key != "allowed_domains" && key != "excluded_domains" {
				c.changed = true
				c.addWarning("web_search_filters_downgraded")
			}
		}
		for _, field := range []string{"allowed_domains", "excluded_domains"} {
			rawDomains, exists := filters[field]
			if !exists {
				continue
			}
			values, err := normalizeWebSearchDomains(rawDomains, field, param+".filters."+field)
			if err != nil {
				return nil, err
			}
			nested[field] = values
		}
	}

	topLevel := make(map[string][]any, 2)
	for _, field := range []string{"allowed_domains", "excluded_domains"} {
		if rawDomains, exists := tool[field]; exists {
			values, err := normalizeWebSearchDomains(rawDomains, field, param+"."+field)
			if err != nil {
				return nil, err
			}
			topLevel[field] = values
		}
	}

	result := make(map[string]any, 2)
	for _, field := range []string{"allowed_domains", "excluded_domains"} {
		nestedDomains := nested[field]
		topLevelDomains := topLevel[field]
		if len(nestedDomains) > 0 && len(topLevelDomains) > 0 && !sameStringValues(nestedDomains, topLevelDomains) {
			return nil, &responsesRequestError{Message: "web_search " + field + " 声明冲突", Param: param + "." + field, Code: "invalid_parameter"}
		}
		domains := nestedDomains
		if len(domains) == 0 {
			domains = topLevelDomains
		}
		if len(domains) > 0 {
			result[field] = cloneJSONValue(domains)
		}
	}
	if _, hasAllowed := result["allowed_domains"]; hasAllowed {
		if _, hasExcluded := result["excluded_domains"]; hasExcluded {
			return nil, &responsesRequestError{Message: "web_search 不能同时设置 allowed_domains 和 excluded_domains", Param: param + ".filters", Code: "invalid_parameter"}
		}
	}
	if len(result) == 0 {
		return nil, nil
	}
	return result, nil
}

func normalizeWebSearchDomains(value any, field, param string) ([]any, error) {
	if value == nil {
		return nil, nil
	}
	values, ok := value.([]any)
	if !ok {
		return nil, &responsesRequestError{Message: field + " 必须是字符串数组", Param: param, Code: "invalid_parameter"}
	}
	if len(values) > maxWebSearchDomains {
		return nil, &responsesRequestError{Message: fmt.Sprintf("%s 不能超过 %d 个域名", field, maxWebSearchDomains), Param: param, Code: "invalid_parameter"}
	}
	for index, value := range values {
		domain, ok := value.(string)
		if !ok || strings.TrimSpace(domain) == "" {
			return nil, &responsesRequestError{Message: field + " 必须包含有效域名", Param: fmt.Sprintf("%s[%d]", param, index), Code: "invalid_parameter"}
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
		Message: fmt.Sprintf("Grok Build 不支持 tools.type=%q", kind),
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
