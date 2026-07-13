package conversation

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

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
	ID        string
	Model     string
	CreatedAt int64
	Text      string
	Reasoning string
	Refusal   string
	Calls     []responseItem
	Usage     responseUsage
	Status    string
}

// ConvertResponseJSON 将 Responses 非流式结果转换为 Chat Completions 或 Anthropic Messages。
func ConvertResponseJSON(body []byte, operation string) ([]byte, error) {
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
	var result any
	if operation == OperationMessages {
		result = messagesResponse(parsed)
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

func messagesResponse(value parsedResponse) map[string]any {
	content := make([]any, 0, len(value.Calls)+1)
	if value.Text != "" || len(value.Calls) == 0 {
		content = append(content, map[string]any{"type": "text", "text": value.Text})
	}
	for _, call := range value.Calls {
		var input any = map[string]any{}
		if json.Unmarshal([]byte(call.Arguments), &input) != nil {
			input = map[string]any{}
		}
		content = append(content, map[string]any{"type": "tool_use", "id": call.CallID, "name": call.Name, "input": input})
	}
	stopReason := "end_turn"
	if len(value.Calls) > 0 {
		stopReason = "tool_use"
	} else if value.Status == "incomplete" {
		stopReason = "max_tokens"
	} else if value.Refusal != "" {
		stopReason = "refusal"
	}
	return map[string]any{
		"id": strings.Replace(value.ID, "resp_", "msg_", 1), "type": "message", "role": "assistant",
		"model": value.Model, "content": content, "stop_reason": stopReason, "stop_sequence": nil,
		"usage": anthropicUsage(value.Usage),
	}
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
	if object, ok := value.(map[string]any); ok {
		if text, ok := object["message"].(string); ok && text != "" {
			message = text
		}
	}
	data, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": message}})
	return data
}
