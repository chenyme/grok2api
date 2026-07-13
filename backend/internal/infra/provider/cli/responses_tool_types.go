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

// normalizeWebSearchTool 将旧 discriminator 映射为 web_search；0.2.99 未确认的控制字段不会被静默删除。
func (c *responsesToolCompatibility) normalizeWebSearchTool(tool map[string]any, kind, param string) ([]any, error) {
	for key := range tool {
		if key == "type" {
			continue
		}
		return nil, &responsesRequestError{
			Message: "Grok Build 0.2.99 的 web_search 尚未确认支持可选控制字段",
			Param:   param + "." + key, Code: "unsupported_parameter",
		}
	}
	if kind == "web_search" {
		return []any{cloneJSONValue(tool)}, nil
	}
	c.changed = true
	return []any{map[string]any{"type": "web_search"}}, nil
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
