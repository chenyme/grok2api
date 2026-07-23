package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// normalizeResponsesRequest 改写路由字段和兼容别名，并为上游不支持的新工具协议建立请求级映射。
func normalizeResponsesRequest(body []byte, model string) ([]byte, *responsesToolCompatibility, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, nil, fmt.Errorf("解析 Responses 请求: %w", err)
	}
	payload["model"] = mustJSON(model)
	normalizeBuildReasoningEffortPayload(payload)
	if responseFormat, exists := payload["response_format"]; exists {
		var text map[string]json.RawMessage
		if raw := payload["text"]; len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			if err := json.Unmarshal(raw, &text); err != nil {
				return nil, nil, fmt.Errorf("解析 text: %w", err)
			}
		}
		if text == nil {
			text = make(map[string]json.RawMessage)
		}
		if isEmptyJSON(text["format"]) {
			formatted, err := normalizeResponseFormat(responseFormat)
			if err != nil {
				return nil, nil, err
			}
			text["format"] = formatted
		}
		encoded, err := json.Marshal(text)
		if err != nil {
			return nil, nil, err
		}
		payload["text"] = encoded
		delete(payload, "response_format")
	}
	patchReasoningTextTypes(payload)
	compatibility, err := normalizeResponsesTools(payload)
	if err != nil {
		return nil, nil, err
	}
	normalized, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}
	return normalized, compatibility, nil
}

// normalizeBuildReasoningEffort maps client aliases to levels accepted by Grok Build.
func normalizeBuildReasoningEffort(body []byte) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("解析 Build reasoning 请求: %w", err)
	}
	if !normalizeBuildReasoningEffortPayload(payload) {
		return body, nil
	}
	return json.Marshal(payload)
}

func normalizeBuildReasoningEffortPayload(payload map[string]json.RawMessage) bool {
	raw, exists := payload["reasoning"]
	if !exists || isEmptyJSON(raw) {
		return false
	}
	var reasoning map[string]json.RawMessage
	if err := json.Unmarshal(raw, &reasoning); err != nil || reasoning == nil {
		return false
	}
	var effort string
	if err := json.Unmarshal(reasoning["effort"], &effort); err != nil {
		return false
	}
	var normalized string
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "max", "xhigh":
		normalized = "high"
	default:
		return false
	}
	if effort == normalized {
		return false
	}
	reasoning["effort"] = mustJSON(normalized)
	payload["reasoning"] = mustJSON(reasoning)
	return true
}

// patchReasoningTextTypes 对齐官方 CLI 的序列化后修补：Responses 上游要求
// reasoning.content[*] 必须携带 type=reasoning_text，即使部分客户端只发送 text。
func patchReasoningTextTypes(payload map[string]json.RawMessage) {
	raw := payload["input"]
	if isEmptyJSON(raw) {
		return
	}
	var items []any
	if json.Unmarshal(raw, &items) != nil {
		return // 字符串输入或其他合法简写不需要处理。
	}
	changed := false
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok || item["type"] != "reasoning" {
			continue
		}
		content, ok := item["content"].([]any)
		if !ok {
			continue
		}
		for _, rawContent := range content {
			value, ok := rawContent.(map[string]any)
			if !ok {
				continue
			}
			if _, exists := value["type"]; !exists {
				value["type"] = "reasoning_text"
				changed = true
			}
		}
	}
	if changed {
		payload["input"] = mustJSON(items)
	}
}

func normalizeResponseFormat(raw json.RawMessage) (json.RawMessage, error) {
	var format map[string]json.RawMessage
	if err := json.Unmarshal(raw, &format); err != nil {
		return nil, fmt.Errorf("解析 response_format: %w", err)
	}
	var formatType string
	_ = json.Unmarshal(format["type"], &formatType)
	if formatType != "json_schema" || isEmptyJSON(format["json_schema"]) {
		return raw, nil
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(format["json_schema"], &schema); err != nil {
		return nil, fmt.Errorf("解析 response_format.json_schema: %w", err)
	}
	result := make(map[string]json.RawMessage, len(schema))
	result["type"] = mustJSON("json_schema")
	for key, value := range schema {
		if key != "type" {
			result[key] = value
		}
	}
	return json.Marshal(result)
}

func isEmptyJSON(raw json.RawMessage) bool {
	value := bytes.TrimSpace(raw)
	return len(value) == 0 || bytes.Equal(value, []byte("null")) || bytes.Equal(value, []byte(`""`))
}

func mustJSON(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}
