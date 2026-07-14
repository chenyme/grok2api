package conversation

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ResponseOptions 保留无法直接交给 Responses 上游执行的下游协议语义。
type ResponseOptions struct {
	AnthropicThinking bool
	StopSequences     []string
}

type responseEnvelope struct {
	ID        string         `json:"id"`
	Model     string         `json:"model"`
	Status    string         `json:"status"`
	CreatedAt int64          `json:"created_at"`
	Output    []responseItem `json:"output"`
	Usage     responseUsage  `json:"usage"`
	Error     any            `json:"error"`
}

type responseItem struct {
	ID        string            `json:"id"`
	Type      string            `json:"type"`
	Role      string            `json:"role"`
	Status    string            `json:"status"`
	Content   []responseContent `json:"content"`
	Summary   []responseContent `json:"summary"`
	CallID    string            `json:"call_id"`
	Name      string            `json:"name"`
	Arguments string            `json:"arguments"`
	Encrypted string            `json:"encrypted_content"`
}

type responseContent struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal"`
}

type responseUsage struct {
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	TotalTokens        int64 `json:"total_tokens"`
	InputTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

type parsedResponse struct {
	ID           string
	Model        string
	CreatedAt    int64
	Text         string
	Reasoning    string
	Signature    string
	Refusal      string
	Calls        []responseItem
	Usage        responseUsage
	Status       string
	StopSequence string
}

// ConvertResponseJSON 将 Responses 非流式结果转换为 Chat Completions 或 Anthropic Messages。
func ConvertResponseJSON(body []byte, operation string) ([]byte, error) {
	return ConvertResponseJSONWithOptions(body, operation, ResponseOptions{})
}

// ConvertResponseJSONWithOptions 按原始 Messages 请求选项恢复 thinking 与 stop sequence。
func ConvertResponseJSONWithOptions(body []byte, operation string, options ResponseOptions) ([]byte, error) {
	if operation == OperationResponses {
		return body, nil
	}
	var envelope responseEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("解析 Responses 响应: %w", err)
	}
	if envelope.Error != nil {
		if operation == OperationMessages {
			return anthropicErrorJSON(envelope.Error), nil
		}
		return body, nil
	}
	parsed := parseResponse(envelope)
	if operation == OperationMessages {
		parsed.Text, parsed.StopSequence = applyAnthropicStopSequences(parsed.Text, options.StopSequences)
	}
	var result any
	if operation == OperationMessages {
		result = messagesResponse(parsed, options)
	} else {
		result = chatResponse(parsed)
	}
	return json.Marshal(result)
}

func parseResponse(value responseEnvelope) parsedResponse {
	parsed := parsedResponse{ID: value.ID, Model: value.Model, CreatedAt: value.CreatedAt, Usage: value.Usage, Status: value.Status}
	if parsed.CreatedAt == 0 {
		parsed.CreatedAt = time.Now().Unix()
	}
	for _, item := range value.Output {
		switch item.Type {
		case "message":
			for _, content := range item.Content {
				switch content.Type {
				case "output_text":
					parsed.Text += content.Text
				case "refusal":
					parsed.Refusal += content.Refusal
				}
			}
		case "reasoning":
			for _, summary := range item.Summary {
				parsed.Reasoning += summary.Text
			}
			if item.Encrypted != "" {
				parsed.Signature = item.Encrypted
			}
		case "function_call":
			parsed.Calls = append(parsed.Calls, item)
		}
	}
	return parsed
}

func chatResponse(value parsedResponse) map[string]any {
	message := map[string]any{"role": "assistant", "content": value.Text}
	if value.Reasoning != "" {
		message["reasoning_content"] = value.Reasoning
	}
	finishReason := "stop"
	if len(value.Calls) > 0 {
		finishReason = "tool_calls"
		if value.Text == "" {
			message["content"] = nil
		}
		calls := make([]any, 0, len(value.Calls))
		for _, call := range value.Calls {
			calls = append(calls, map[string]any{
				"id": call.CallID, "type": "function",
				"function": map[string]any{"name": call.Name, "arguments": call.Arguments},
			})
		}
		message["tool_calls"] = calls
	} else if value.Status == "incomplete" {
		finishReason = "length"
	}
	if value.Refusal != "" {
		message["refusal"] = value.Refusal
	}
	id := strings.Replace(value.ID, "resp_", "chatcmpl_", 1)
	return map[string]any{
		"id": id, "object": "chat.completion", "created": value.CreatedAt, "model": value.Model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finishReason}},
		"usage":   chatUsage(value.Usage),
	}
}

func messagesResponse(value parsedResponse, options ResponseOptions) map[string]any {
	content := make([]any, 0, len(value.Calls)+2)
	if options.AnthropicThinking && (value.Reasoning != "" || value.Signature != "") {
		if value.Reasoning == "" {
			content = append(content, map[string]any{"type": "redacted_thinking", "data": value.Signature})
		} else {
			thinking := map[string]any{"type": "thinking", "thinking": value.Reasoning}
			if value.Signature != "" {
				thinking["signature"] = value.Signature
			}
			content = append(content, thinking)
		}
	}
	if value.Text != "" || len(value.Calls) == 0 {
		content = append(content, map[string]any{"type": "text", "text": value.Text})
	}
	for _, call := range value.Calls {
		var input any = map[string]any{}
		if json.Unmarshal([]byte(call.Arguments), &input) != nil {
			input = map[string]any{}
		}
		content = append(content, map[string]any{"type": "tool_use", "id": anthropicToolUseID(call.CallID), "name": call.Name, "input": input})
	}
	stopReason := "end_turn"
	if len(value.Calls) > 0 {
		stopReason = "tool_use"
	} else if value.StopSequence != "" {
		stopReason = "stop_sequence"
	} else if value.Status == "incomplete" {
		stopReason = "max_tokens"
	} else if value.Refusal != "" {
		stopReason = "refusal"
	}
	return map[string]any{
		"id": anthropicMessageID(value.ID), "type": "message", "role": "assistant",
		"model": value.Model, "content": content, "stop_reason": stopReason, "stop_sequence": nullableAnthropicString(value.StopSequence),
		"usage": anthropicUsage(value.Usage),
	}
}

func applyAnthropicStopSequences(text string, sequences []string) (string, string) {
	matchAt := -1
	matched := ""
	for _, sequence := range sequences {
		if sequence == "" {
			continue
		}
		if index := strings.Index(text, sequence); index >= 0 && (matchAt < 0 || index < matchAt) {
			matchAt = index
			matched = sequence
		}
	}
	if matchAt < 0 {
		return text, ""
	}
	return text[:matchAt], matched
}

func anthropicMessageID(value string) string {
	if strings.HasPrefix(value, "msg_") {
		return value
	}
	if strings.HasPrefix(value, "resp_") {
		return "msg_" + strings.TrimPrefix(value, "resp_")
	}
	if value == "" {
		return fmt.Sprintf("msg_%d", time.Now().UnixNano())
	}
	return "msg_" + value
}

func anthropicToolUseID(value string) string {
	if strings.HasPrefix(value, "toolu_") {
		return value
	}
	if value == "" {
		return fmt.Sprintf("toolu_%d", time.Now().UnixNano())
	}
	return "toolu_" + value
}

func nullableAnthropicString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func chatUsage(value responseUsage) map[string]any {
	return map[string]any{
		"prompt_tokens": value.InputTokens, "completion_tokens": value.OutputTokens,
		"total_tokens":              value.InputTokens + value.OutputTokens,
		"prompt_tokens_details":     map[string]any{"cached_tokens": value.InputTokensDetails.CachedTokens},
		"completion_tokens_details": map[string]any{"reasoning_tokens": value.OutputTokensDetails.ReasoningTokens},
	}
}

func anthropicUsage(value responseUsage) map[string]any {
	return map[string]any{
		"input_tokens": value.InputTokens, "output_tokens": value.OutputTokens,
		"cache_creation_input_tokens": 0, "cache_read_input_tokens": value.InputTokensDetails.CachedTokens,
	}
}

func anthropicErrorJSON(value any) []byte {
	message := "Upstream request failed"
	errorType := "api_error"
	if object, ok := value.(map[string]any); ok {
		if text, ok := object["message"].(string); ok && text != "" {
			message = text
		}
		errorType = normalizeAnthropicErrorType(object)
	} else if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
		message = text
	}
	data, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": errorType, "message": message}})
	return data
}

func normalizeAnthropicErrorType(object map[string]any) string {
	for _, field := range []string{"type", "code"} {
		value, _ := object[field].(string)
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "invalid_request_error", "authentication_error", "billing_error", "permission_error",
			"not_found_error", "rate_limit_error", "timeout_error", "overloaded_error", "api_error":
			return strings.ToLower(strings.TrimSpace(value))
		case "unsupported_parameter", "invalid_parameter", "bad_request", "invalid_request":
			return "invalid_request_error"
		case "unauthorized", "authentication_failed":
			return "authentication_error"
		case "forbidden":
			return "permission_error"
		case "not_found":
			return "not_found_error"
		case "rate_limit_exceeded", "too_many_requests":
			return "rate_limit_error"
		case "request_timeout", "timeout":
			return "timeout_error"
		case "service_unavailable", "overloaded":
			return "overloaded_error"
		case "server_error", "internal_error":
			return "api_error"
		}
	}
	return "api_error"
}
