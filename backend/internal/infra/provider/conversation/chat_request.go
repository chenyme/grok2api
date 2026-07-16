package conversation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// convertChatRequest 将 Chat Completions 请求完整转换为标准 Responses 输入。
func convertChatRequest(body []byte, model string) ([]byte, ResponseOptions, error) {
	var source map[string]json.RawMessage
	if err := json.Unmarshal(body, &source); err != nil {
		return nil, ResponseOptions{}, fmt.Errorf("解析 Chat Completions 请求: %w", err)
	}
	var messages []chatMessage
	if err := json.Unmarshal(source["messages"], &messages); err != nil || len(messages) == 0 {
		return nil, ResponseOptions{}, errors.New("messages 必须是非空数组")
	}
	input, err := convertChatMessages(messages)
	if err != nil {
		return nil, ResponseOptions{}, err
	}
	target := map[string]json.RawMessage{"model": mustJSON(model), "input": mustJSON(input)}
	copyFields(target, source, "stream", "temperature", "top_p", "parallel_tool_calls", "metadata", "store", "service_tier")
	if raw := source["user"]; !isEmptyJSON(raw) {
		var user string
		if json.Unmarshal(raw, &user) != nil || strings.TrimSpace(user) == "" {
			return nil, ResponseOptions{}, errors.New("user 必须是非空字符串")
		}
		target["safety_identifier"] = mustJSON(strings.TrimSpace(user))
	}
	stopSequences, err := parseChatStopSequences(source["stop"])
	if err != nil {
		return nil, ResponseOptions{}, err
	}
	if raw := firstJSON(source["max_completion_tokens"], source["max_tokens"]); !isEmptyJSON(raw) {
		target["max_output_tokens"] = raw
	}
	if raw := source["response_format"]; !isEmptyJSON(raw) {
		format, err := convertResponseFormat(raw)
		if err != nil {
			return nil, ResponseOptions{}, err
		}
		target["text"] = mustJSON(map[string]json.RawMessage{"format": format})
	}
	if raw := source["reasoning_effort"]; !isEmptyJSON(raw) {
		target["reasoning"] = mustJSON(map[string]json.RawMessage{"effort": raw})
	}
	var tools []any
	if raw := source["tools"]; !isEmptyJSON(raw) {
		tools, err = convertChatTools(raw)
		if err != nil {
			return nil, ResponseOptions{}, err
		}
	}
	if !isEmptyJSON(source["web_search_options"]) && !containsToolType(tools, "web_search") {
		tools = append(tools, map[string]any{"type": "web_search"})
	}
	if len(tools) > 0 {
		target["tools"] = mustJSON(tools)
	}
	if raw := source["tool_choice"]; !isEmptyJSON(raw) {
		choice, err := convertChatToolChoice(raw)
		if err != nil {
			return nil, ResponseOptions{}, err
		}
		target["tool_choice"] = choice
	}
	converted, err := json.Marshal(target)
	return converted, ResponseOptions{StopSequences: stopSequences}, err
}

func parseChatStopSequences(raw json.RawMessage) ([]string, error) {
	if isEmptyJSON(raw) {
		return nil, nil
	}
	var single string
	if json.Unmarshal(raw, &single) == nil {
		if single == "" {
			return nil, errors.New("stop 不能为空")
		}
		return []string{single}, nil
	}
	var values []string
	if json.Unmarshal(raw, &values) != nil || len(values) == 0 {
		return nil, errors.New("stop 必须是字符串或非空字符串数组")
	}
	if len(values) > 4 {
		return nil, errors.New("stop 最多包含 4 个序列")
	}
	for index, value := range values {
		if value == "" {
			return nil, fmt.Errorf("stop[%d] 不能为空", index)
		}
	}
	return values, nil
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  json.RawMessage `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
	Name       string          `json:"name"`
}

func convertChatMessages(messages []chatMessage) ([]any, error) {
	input := make([]any, 0, len(messages))
	for _, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		switch role {
		case "system", "developer", "user", "assistant":
			if !isEmptyJSON(message.Content) && !bytes.Equal(bytes.TrimSpace(message.Content), []byte("null")) {
				content, err := convertChatContent(message.Content)
				if err != nil {
					return nil, fmt.Errorf("%s 消息内容无效: %w", role, err)
				}
				input = append(input, map[string]any{"type": "message", "role": role, "content": content})
			}
			if role == "assistant" && !isEmptyJSON(message.ToolCalls) {
				calls, err := convertAssistantToolCalls(message.ToolCalls)
				if err != nil {
					return nil, err
				}
				input = append(input, calls...)
			}
		case "tool":
			if strings.TrimSpace(message.ToolCallID) == "" {
				return nil, errors.New("tool 消息缺少 tool_call_id")
			}
			output, err := contentAsText(message.Content)
			if err != nil {
				return nil, err
			}
			input = append(input, map[string]any{"type": "function_call_output", "call_id": message.ToolCallID, "output": output})
		default:
			return nil, fmt.Errorf("不支持 messages.role=%q", message.Role)
		}
	}
	if len(input) == 0 {
		return nil, errors.New("messages 中没有可发送内容")
	}
	return input, nil
}

func convertChatContent(raw json.RawMessage) (any, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, errors.New("content 必须是字符串或内容数组")
	}
	result := make([]any, 0, len(parts))
	for _, part := range parts {
		var typeName string
		_ = json.Unmarshal(part["type"], &typeName)
		switch typeName {
		case "text", "input_text", "output_text":
			var value string
			if json.Unmarshal(part["text"], &value) != nil {
				return nil, errors.New("text 内容无效")
			}
			result = append(result, map[string]any{"type": "input_text", "text": value})
		case "image_url", "input_image":
			imageURL, err := parseImageURL(part)
			if err != nil {
				return nil, err
			}
			result = append(result, map[string]any{"type": "input_image", "image_url": imageURL})
		default:
			return nil, fmt.Errorf("不支持 content.type=%q", typeName)
		}
	}
	return result, nil
}

func parseImageURL(part map[string]json.RawMessage) (string, error) {
	raw := firstJSON(part["image_url"], part["url"])
	var value string
	if json.Unmarshal(raw, &value) == nil && strings.TrimSpace(value) != "" {
		return value, nil
	}
	var nested struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(raw, &nested) == nil && strings.TrimSpace(nested.URL) != "" {
		return nested.URL, nil
	}
	return "", errors.New("image_url 缺少有效 url")
}

func convertAssistantToolCalls(raw json.RawMessage) ([]any, error) {
	var calls []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &calls); err != nil {
		return nil, errors.New("assistant.tool_calls 格式无效")
	}
	result := make([]any, 0, len(calls))
	for _, call := range calls {
		if strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Function.Name) == "" {
			return nil, errors.New("assistant.tool_calls 缺少有效 id 或 name")
		}
		arguments := call.Function.Arguments
		if arguments == "" {
			arguments = "{}"
		}
		result = append(result, map[string]any{"type": "function_call", "call_id": call.ID, "name": call.Function.Name, "arguments": arguments})
	}
	return result, nil
}

func convertChatTools(raw json.RawMessage) ([]any, error) {
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, errors.New("tools 必须是数组")
	}
	result := make([]any, 0, len(tools))
	for _, tool := range tools {
		var typeName string
		_ = json.Unmarshal(tool["type"], &typeName)
		if typeName != "function" {
			var value any
			_ = json.Unmarshal(mustJSON(tool), &value)
			object, _ := value.(map[string]any)
			switch typeName {
			case "web_search", "web_search_preview", "web_search_preview_2025_03_11", "web_search_2025_08_26":
				converted := map[string]any{"type": "web_search"}
				if filters, ok := object["filters"].(map[string]any); ok {
					if domains, exists := filters["allowed_domains"]; exists {
						converted["filters"] = map[string]any{"allowed_domains": domains}
					}
				} else if domains, exists := object["allowed_domains"]; exists {
					converted["filters"] = map[string]any{"allowed_domains": domains}
				}
				result = append(result, converted)
			default:
				result = append(result, value)
			}
			continue
		}
		var function map[string]any
		if json.Unmarshal(tool["function"], &function) != nil {
			return nil, errors.New("function tool 格式无效")
		}
		function["type"] = "function"
		result = append(result, function)
	}
	return result, nil
}

func containsToolType(tools []any, kind string) bool {
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if ok && tool["type"] == kind {
			return true
		}
	}
	return false
}

func convertChatToolChoice(raw json.RawMessage) (json.RawMessage, error) {
	var value map[string]json.RawMessage
	if json.Unmarshal(raw, &value) != nil {
		return raw, nil
	}
	var typeName string
	_ = json.Unmarshal(value["type"], &typeName)
	if typeName != "function" {
		return raw, nil
	}
	var function struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(value["function"], &function) != nil || strings.TrimSpace(function.Name) == "" {
		return nil, errors.New("tool_choice.function.name 无效")
	}
	return mustJSON(map[string]any{"type": "function", "name": function.Name}), nil
}
