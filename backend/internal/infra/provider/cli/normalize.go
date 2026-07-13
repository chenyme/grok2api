package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// normalizeResponsesRequest 只改写路由字段和兼容别名，保留完整 Responses 输入项。
func normalizeResponsesRequest(body []byte, model string) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("解析 Responses 请求: %w", err)
	}
	payload["model"] = mustJSON(model)
	if responseFormat, exists := payload["response_format"]; exists {
		var text map[string]json.RawMessage
		if raw := payload["text"]; len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			if err := json.Unmarshal(raw, &text); err != nil {
				return nil, fmt.Errorf("解析 text: %w", err)
			}
		}
		if text == nil {
			text = make(map[string]json.RawMessage)
		}
		if isEmptyJSON(text["format"]) {
			formatted, err := normalizeResponseFormat(responseFormat)
			if err != nil {
				return nil, err
			}
			text["format"] = formatted
		}
		encoded, err := json.Marshal(text)
		if err != nil {
			return nil, err
		}
		payload["text"] = encoded
		delete(payload, "response_format")
	}
	return json.Marshal(payload)
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
	result := make(map[string]json.RawMessage, len(schema)+1)
	result["type"] = mustJSON("json_schema")
	for key, value := range schema {
		result[key] = value
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
