package conversation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	OperationResponses = "responses"
	OperationChat      = "chat"
	OperationMessages  = "messages"
)

// ConvertRequest 将下游对话协议转换为 Responses 请求，作为 Provider 的统一上游协议。
func ConvertRequest(body []byte, model, operation string) ([]byte, error) {
	switch operation {
	case OperationChat:
		return convertChatRequest(body, model)
	case OperationMessages:
		return convertMessagesRequest(body, model)
	default:
		return replaceModel(body, model)
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

func convertChatRequest(body []byte, model string) ([]byte, error) {
	var source map[string]json.RawMessage
	if err := json.Unmarshal(body, &source); err != nil {
		return nil, fmt.Errorf("解析 Chat Completions 请求: %w", err)
	}
	var messages []chatMessage
	if err := json.Unmarshal(source["messages"], &messages); err != nil || len(messages) == 0 {
		return nil, errors.New("messages 必须是非空数组")
	}
	input, err := convertChatMessages(messages)
	if err != nil {
		return nil, err
	}
	target := map[string]json.RawMessage{"model": mustJSON(model), "input": mustJSON(input)}
	copyFields(target, source, "stream", "temperature", "top_p", "presence_penalty", "frequency_penalty", "seed", "user", "parallel_tool_calls", "metadata", "store", "service_tier", "stop")
	if raw := firstJSON(source["max_completion_tokens"], source["max_tokens"]); !isEmptyJSON(raw) {
		target["max_output_tokens"] = raw
	}
	if raw := source["response_format"]; !isEmptyJSON(raw) {
		format, err := convertResponseFormat(raw)
		if err != nil {
			return nil, err
		}
		target["text"] = mustJSON(map[string]json.RawMessage{"format": format})
	}
	if raw := source["reasoning_effort"]; !isEmptyJSON(raw) {
		target["reasoning"] = mustJSON(map[string]json.RawMessage{"effort": raw})
	}
	if raw := source["tools"]; !isEmptyJSON(raw) {
		tools, err := convertChatTools(raw)
		if err != nil {
			return nil, err
		}
		target["tools"] = mustJSON(tools)
	}
	if raw := source["tool_choice"]; !isEmptyJSON(raw) {
		choice, err := convertChatToolChoice(raw)
		if err != nil {
			return nil, err
		}
		target["tool_choice"] = choice
	}
	return json.Marshal(target)
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
		if strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Function.Name) == "" || !json.Valid([]byte(call.Function.Arguments)) {
			return nil, errors.New("assistant.tool_calls 缺少有效 id、name 或 arguments")
		}
		result = append(result, map[string]any{"type": "function_call", "call_id": call.ID, "name": call.Function.Name, "arguments": call.Function.Arguments})
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
			result = append(result, value)
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

func convertMessagesRequest(body []byte, model string) ([]byte, error) {
	var request anthropicRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, fmt.Errorf("解析 Messages 请求: %w", err)
	}
	if len(request.Messages) == 0 {
		return nil, errors.New("messages 必须是非空数组")
	}
	input, err := convertAnthropicMessages(request.Messages)
	if err != nil {
		return nil, err
	}
	target := map[string]any{
		"model": model, "input": input, "stream": request.Stream,
		"max_output_tokens": request.MaxTokens,
	}
	if system, err := anthropicSystemText(request.System); err != nil {
		return nil, err
	} else if system != "" {
		target["instructions"] = system
	}
	copyOptionalNumber(target, "temperature", request.Temperature)
	copyOptionalNumber(target, "top_p", request.TopP)
	if len(request.StopSequences) > 0 {
		target["stop"] = request.StopSequences
	}
	if request.Metadata != nil {
		target["metadata"] = request.Metadata
	}
	if request.OutputConfig != nil && request.OutputConfig.Format != nil {
		target["text"] = map[string]any{"format": map[string]any{"type": "json_schema", "name": "anthropic_output", "schema": request.OutputConfig.Format.Schema}}
	}
	if request.Thinking != nil && request.Thinking.Type != "disabled" {
		effort := "high"
		if request.OutputConfig != nil && request.OutputConfig.Effort != "" {
			effort = request.OutputConfig.Effort
		}
		target["reasoning"] = map[string]any{"effort": effort, "summary": "auto"}
	}
	if len(request.Tools) > 0 {
		tools, err := convertAnthropicTools(request.Tools)
		if err != nil {
			return nil, err
		}
		target["tools"] = tools
	}
	if request.ToolChoice != nil {
		choice, parallel, err := convertAnthropicToolChoice(*request.ToolChoice)
		if err != nil {
			return nil, err
		}
		target["tool_choice"] = choice
		target["parallel_tool_calls"] = parallel
	}
	return json.Marshal(target)
}

type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	Messages      []anthropicMessage `json:"messages"`
	System        json.RawMessage    `json:"system"`
	Stream        bool               `json:"stream"`
	Temperature   *float64           `json:"temperature"`
	TopP          *float64           `json:"top_p"`
	StopSequences []string           `json:"stop_sequences"`
	Metadata      map[string]any     `json:"metadata"`
	Thinking      *struct {
		Type string `json:"type"`
	} `json:"thinking"`
	OutputConfig *struct {
		Effort string `json:"effort"`
		Format *struct {
			Type   string         `json:"type"`
			Schema map[string]any `json:"schema"`
		} `json:"format"`
	} `json:"output_config"`
	Tools      []map[string]json.RawMessage `json:"tools"`
	ToolChoice *anthropicToolChoice         `json:"tool_choice"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
}

func convertAnthropicMessages(messages []anthropicMessage) ([]any, error) {
	input := make([]any, 0, len(messages))
	for _, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role != "user" && role != "assistant" {
			return nil, fmt.Errorf("Messages API 不支持 role=%q", message.Role)
		}
		var text string
		if json.Unmarshal(message.Content, &text) == nil {
			input = append(input, map[string]any{"type": "message", "role": role, "content": text})
			continue
		}
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(message.Content, &blocks) != nil {
			return nil, errors.New("Messages content 必须是字符串或内容块数组")
		}
		messageParts := make([]any, 0, len(blocks))
		flushMessage := func() {
			if len(messageParts) > 0 {
				input = append(input, map[string]any{"type": "message", "role": role, "content": messageParts})
				messageParts = nil
			}
		}
		for _, block := range blocks {
			var typeName string
			_ = json.Unmarshal(block["type"], &typeName)
			switch typeName {
			case "text":
				var value string
				if json.Unmarshal(block["text"], &value) != nil {
					return nil, errors.New("text block 无效")
				}
				messageParts = append(messageParts, map[string]any{"type": "input_text", "text": value})
			case "image":
				imageURL, err := anthropicImageURL(block["source"])
				if err != nil {
					return nil, err
				}
				messageParts = append(messageParts, map[string]any{"type": "input_image", "image_url": imageURL})
			case "tool_use":
				flushMessage()
				var value struct {
					ID    string         `json:"id"`
					Name  string         `json:"name"`
					Input map[string]any `json:"input"`
				}
				if encoded, _ := json.Marshal(block); json.Unmarshal(encoded, &value) != nil || value.ID == "" || value.Name == "" {
					return nil, errors.New("tool_use block 无效")
				}
				arguments, _ := json.Marshal(value.Input)
				input = append(input, map[string]any{"type": "function_call", "call_id": value.ID, "name": value.Name, "arguments": string(arguments)})
			case "tool_result":
				flushMessage()
				var toolUseID string
				_ = json.Unmarshal(block["tool_use_id"], &toolUseID)
				if toolUseID == "" {
					return nil, errors.New("tool_result 缺少 tool_use_id")
				}
				output, err := anthropicToolResult(block["content"])
				if err != nil {
					return nil, err
				}
				input = append(input, map[string]any{"type": "function_call_output", "call_id": toolUseID, "output": output})
			default:
				return nil, fmt.Errorf("当前不支持 Anthropic content.type=%q", typeName)
			}
		}
		flushMessage()
	}
	return input, nil
}

func anthropicSystemText(raw json.RawMessage) (string, error) {
	if isEmptyJSON(raw) {
		return "", nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return "", errors.New("system 必须是字符串或 text block 数组")
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Type != "text" {
			return "", fmt.Errorf("system 不支持 type=%q", block.Type)
		}
		parts = append(parts, block.Text)
	}
	return strings.Join(parts, "\n\n"), nil
}

func anthropicImageURL(raw json.RawMessage) (string, error) {
	var source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	}
	if json.Unmarshal(raw, &source) != nil {
		return "", errors.New("image.source 无效")
	}
	switch source.Type {
	case "base64":
		if source.MediaType == "" || source.Data == "" {
			return "", errors.New("base64 image 缺少 media_type 或 data")
		}
		return "data:" + source.MediaType + ";base64," + source.Data, nil
	case "url":
		if strings.TrimSpace(source.URL) == "" {
			return "", errors.New("url image 缺少 url")
		}
		return source.URL, nil
	default:
		return "", fmt.Errorf("不支持 image.source.type=%q", source.Type)
	}
}

func anthropicToolResult(raw json.RawMessage) (string, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return "", errors.New("tool_result.content 无效")
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		var typeName string
		_ = json.Unmarshal(block["type"], &typeName)
		if typeName != "text" {
			return "", fmt.Errorf("tool_result 暂不支持 type=%q", typeName)
		}
		var value string
		_ = json.Unmarshal(block["text"], &value)
		parts = append(parts, value)
	}
	return strings.Join(parts, "\n"), nil
}

func convertAnthropicTools(tools []map[string]json.RawMessage) ([]any, error) {
	result := make([]any, 0, len(tools))
	for _, tool := range tools {
		var typeName string
		_ = json.Unmarshal(tool["type"], &typeName)
		if typeName != "" && typeName != "custom" {
			return nil, fmt.Errorf("当前不支持 Anthropic server tool type=%q", typeName)
		}
		var name, description string
		_ = json.Unmarshal(tool["name"], &name)
		_ = json.Unmarshal(tool["description"], &description)
		if strings.TrimSpace(name) == "" {
			return nil, errors.New("Anthropic tool 缺少 name")
		}
		var schema any = map[string]any{"type": "object", "properties": map[string]any{}}
		if raw := tool["input_schema"]; !isEmptyJSON(raw) {
			if json.Unmarshal(raw, &schema) != nil {
				return nil, fmt.Errorf("tool %q 的 input_schema 无效", name)
			}
		}
		result = append(result, map[string]any{"type": "function", "name": name, "description": description, "parameters": schema})
	}
	return result, nil
}

func convertAnthropicToolChoice(choice anthropicToolChoice) (any, bool, error) {
	parallel := !choice.DisableParallelToolUse
	switch choice.Type {
	case "auto", "none":
		return choice.Type, parallel, nil
	case "any":
		return "required", parallel, nil
	case "tool":
		if strings.TrimSpace(choice.Name) == "" {
			return nil, false, errors.New("tool_choice.tool 缺少 name")
		}
		return map[string]any{"type": "function", "name": choice.Name}, parallel, nil
	default:
		return nil, false, fmt.Errorf("不支持 tool_choice.type=%q", choice.Type)
	}
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
