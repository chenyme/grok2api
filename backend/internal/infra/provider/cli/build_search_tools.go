package cli

import (
	"encoding/json"
	"fmt"
	"strings"
)

// injectBuildSearchTools ensures cli-chat-proxy native web_search + x_search tools
// are present on a Responses-style body.
//
// Free Grok Build OAuth often lands on a path that keeps cached_tokens at 0 unless
// these native tools are declared (see CLIProxyAPI #4213). Existing tools of the
// same type are left untouched; only missing types are appended.
func injectBuildSearchTools(body []byte) ([]byte, bool, error) {
	if len(bytesTrimSpace(body)) == 0 {
		return body, false, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false, fmt.Errorf("解析请求以注入 build search tools: %w", err)
	}
	tools, hasTools, err := decodeToolsArray(payload["tools"])
	if err != nil {
		return nil, false, err
	}
	haveWebSearch, haveXSearch := scanNativeSearchTools(tools)
	if haveWebSearch && haveXSearch {
		return body, false, nil
	}
	if !hasTools {
		tools = make([]any, 0, 2)
	}
	changed := false
	if !haveWebSearch {
		tools = append(tools, map[string]any{"type": "web_search"})
		changed = true
	}
	if !haveXSearch {
		tools = append(tools, map[string]any{"type": "x_search"})
		changed = true
	}
	if !changed {
		return body, false, nil
	}
	payload["tools"] = tools
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, false, fmt.Errorf("编码注入后的 build search tools: %w", err)
	}
	return encoded, true, nil
}

func decodeToolsArray(raw any) ([]any, bool, error) {
	if raw == nil {
		return nil, false, nil
	}
	switch value := raw.(type) {
	case []any:
		return value, true, nil
	case json.RawMessage:
		if len(bytesTrimSpace(value)) == 0 || string(bytesTrimSpace(value)) == "null" {
			return nil, false, nil
		}
		var tools []any
		if err := json.Unmarshal(value, &tools); err != nil {
			return nil, false, fmt.Errorf("tools 必须是数组: %w", err)
		}
		return tools, true, nil
	default:
		// Re-encode unknown JSON-ish values so map[string]json.RawMessage paths work.
		encoded, err := json.Marshal(raw)
		if err != nil {
			return nil, false, fmt.Errorf("tools 必须是数组")
		}
		if len(bytesTrimSpace(encoded)) == 0 || string(bytesTrimSpace(encoded)) == "null" {
			return nil, false, nil
		}
		var tools []any
		if err := json.Unmarshal(encoded, &tools); err != nil {
			return nil, false, fmt.Errorf("tools 必须是数组: %w", err)
		}
		return tools, true, nil
	}
}

func scanNativeSearchTools(tools []any) (haveWebSearch, haveXSearch bool) {
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(stringField(tool, "type")))
		switch {
		case kind == "web_search" || strings.HasPrefix(kind, "web_search_"):
			haveWebSearch = true
		case kind == "x_search":
			haveXSearch = true
		}
	}
	return haveWebSearch, haveXSearch
}

func bytesTrimSpace(value []byte) []byte {
	return []byte(strings.TrimSpace(string(value)))
}
