package gateway

import (
	"bytes"
	"encoding/json"
	"strings"
)

func maybeInjectAutoWebSearch(body []byte) []byte {
	var source map[string]json.RawMessage
	if json.Unmarshal(body, &source) != nil {
		return body
	}
	if toolChoiceDisablesTools(source["tool_choice"]) {
		return body
	}
	if len(bytes.TrimSpace(source["web_search_options"])) > 0 && string(bytes.TrimSpace(source["web_search_options"])) != "null" {
		return body
	}
	tools, ok := decodeChatTools(source["tools"])
	if !ok {
		return body
	}
	if containsWebSearchTool(tools) {
		return body
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return body
	}
	payload["tools"] = append(tools, map[string]any{"type": "web_search"})
	encoded, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return encoded
}

func decodeChatTools(raw json.RawMessage) ([]any, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, true
	}
	var tools []any
	if json.Unmarshal(trimmed, &tools) != nil {
		return nil, false
	}
	return tools, true
}

func containsWebSearchTool(tools []any) bool {
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := tool["type"].(string)
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(kind)), "web_search") {
			return true
		}
	}
	return false
}

func toolChoiceDisablesTools(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return false
	}
	var asString string
	if json.Unmarshal(trimmed, &asString) == nil {
		return strings.EqualFold(strings.TrimSpace(asString), "none")
	}
	var asObject map[string]any
	if json.Unmarshal(trimmed, &asObject) != nil {
		return false
	}
	kind, _ := asObject["type"].(string)
	return strings.EqualFold(strings.TrimSpace(kind), "none")
}
