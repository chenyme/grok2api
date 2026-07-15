package conversation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	OperationResponses = "responses"
	OperationChat      = "chat"
	OperationMessages  = "messages"
)

// ConvertRequest 将下游对话协议转换为 Responses 请求，作为 Provider 的统一上游协议。
func ConvertRequest(body []byte, model, operation string) ([]byte, error) {
	converted, _, err := ConvertRequestWithOptions(body, model, operation)
	return converted, err
}

// ConvertRequestWithOptions 同时返回下游协议特有的响应语义，供 JSON/SSE 转换阶段使用。
func ConvertRequestWithOptions(body []byte, model, operation string) ([]byte, ResponseOptions, error) {
	switch operation {
	case OperationChat:
		converted, err := convertChatRequest(body, model)
		return converted, ResponseOptions{}, err
	case OperationMessages:
		return convertMessagesRequest(body, model)
	default:
		converted, err := replaceModel(body, model)
		return converted, ResponseOptions{}, err
	}
}

func replaceModel(body []byte, model string) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("解析 Responses 请求: %w", err)
	}
	payload["model"] = mustJSON(model)
	return json.Marshal(payload)
}

func convertResponseFormat(raw json.RawMessage) (json.RawMessage, error) {
	var format map[string]json.RawMessage
	if json.Unmarshal(raw, &format) != nil {
		return nil, errors.New("response_format 无效")
	}
	var typeName string
	_ = json.Unmarshal(format["type"], &typeName)
	if typeName != "json_schema" || isEmptyJSON(format["json_schema"]) {
		return raw, nil
	}
	var schema map[string]json.RawMessage
	if json.Unmarshal(format["json_schema"], &schema) != nil {
		return nil, errors.New("response_format.json_schema 无效")
	}
	result := map[string]json.RawMessage{"type": mustJSON("json_schema")}
	for key, value := range schema {
		result[key] = value
	}
	return mustJSON(result), nil
}

func contentAsText(raw json.RawMessage) (string, error) {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value, nil
	}
	var arbitrary any
	if json.Unmarshal(raw, &arbitrary) != nil {
		return "", errors.New("tool content 无效")
	}
	encoded, _ := json.Marshal(arbitrary)
	return string(encoded), nil
}

func copyFields(target, source map[string]json.RawMessage, names ...string) {
	for _, name := range names {
		if raw := source[name]; !isEmptyJSON(raw) {
			target[name] = raw
		}
	}
}

func copyOptionalNumber(target map[string]any, name string, value *float64) {
	if value != nil {
		target[name] = *value
	}
}

func firstJSON(values ...json.RawMessage) json.RawMessage {
	for _, value := range values {
		if !isEmptyJSON(value) {
			return value
		}
	}
	return nil
}

func isEmptyJSON(raw json.RawMessage) bool {
	value := bytes.TrimSpace(raw)
	return len(value) == 0 || bytes.Equal(value, []byte("null"))
}

func mustJSON(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}
