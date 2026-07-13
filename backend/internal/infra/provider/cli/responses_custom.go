package cli

import (
	"encoding/json"
	"strings"
)

// normalizeCustomTool 将任意字符串输入包装为普通函数；grammar 约束无法等价表达时明确拒绝。
func (c *responsesToolCompatibility) normalizeCustomTool(tool map[string]any, namespace, param string) ([]any, error) {
	name := strings.TrimSpace(stringField(tool, "name"))
	if name == "" {
		return nil, &responsesRequestError{Message: param + ".name 不能为空", Param: param + ".name", Code: "invalid_parameter"}
	}
	if format, exists := tool["format"]; exists {
		formatObject, ok := format.(map[string]any)
		if !ok || (stringField(formatObject, "type") != "" && stringField(formatObject, "type") != "text") {
			return nil, &responsesRequestError{
				Message: "Grok Build 无法等价模拟 custom tool grammar",
				Param:   param + ".format", Code: "unsupported_parameter",
			}
		}
	}
	identity := responsesToolIdentity{Kind: responsesCustomTool, Namespace: namespace, Name: name}
	description := strings.TrimSpace(stringField(tool, "description"))
	if description != "" {
		description += "\n"
	}
	description += "Provide the custom tool input in the input string field."
	c.changed = true
	c.addWarning("custom_tool_emulated")
	return []any{map[string]any{
		"type": "function", "name": c.alias(identity), "description": description,
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"input": map[string]any{"type": "string"}},
			"required":   []any{"input"}, "additionalProperties": false,
		},
	}}, nil
}

func encodeCustomToolArguments(value any) (string, error) {
	input, ok := value.(string)
	if !ok {
		return "", &responsesRequestError{Message: "custom_tool_call.input 必须是字符串", Code: "invalid_parameter"}
	}
	encoded, err := json.Marshal(map[string]any{"input": input})
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func decodeCustomToolInput(value any) string {
	text, _ := value.(string)
	var wrapper map[string]any
	if json.Unmarshal([]byte(text), &wrapper) == nil {
		if input, ok := wrapper["input"].(string); ok {
			return input
		}
	}
	return text
}
