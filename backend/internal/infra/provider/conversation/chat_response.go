package conversation

import "strings"

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

func chatUsage(value responseUsage) map[string]any {
	return map[string]any{
		"prompt_tokens": value.InputTokens, "completion_tokens": value.OutputTokens,
		"total_tokens":              value.InputTokens + value.OutputTokens,
		"prompt_tokens_details":     map[string]any{"cached_tokens": value.InputTokensDetails.CachedTokens},
		"completion_tokens_details": map[string]any{"reasoning_tokens": value.OutputTokensDetails.ReasoningTokens},
	}
}
