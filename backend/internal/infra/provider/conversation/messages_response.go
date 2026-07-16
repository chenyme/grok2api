package conversation

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func messagesResponse(value parsedResponse, options ResponseOptions) map[string]any {
	webSearch := value.WebSearch
	if options.AnthropicWebSearchRequired && len(webSearch) == 0 {
		webSearch = []webSearchCall{unavailableWebSearchCall(options.AnthropicWebSearchQuery)}
	}
	// Avoid capacity arithmetic on untrusted upstream lengths (CodeQL size overflow).
	// appendServerWebSearchContent already hard-caps search blocks.
	content := make([]any, 0)
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
	// Hosted web search completes in-process; emit Anthropic server tool blocks first.
	content = appendServerWebSearchContent(content, webSearch)
	if value.Text != "" || (len(value.Calls) == 0 && len(webSearch) == 0) {
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
		// Client tools still pause the turn. Hosted web_search does not.
		stopReason = "tool_use"
	} else if value.StopSequence != "" {
		stopReason = "stop_sequence"
	} else if value.Refusal != "" {
		stopReason = "refusal"
	} else if value.Status == "incomplete" {
		stopReason = "max_tokens"
	}
	return map[string]any{
		"id": anthropicMessageID(value.ID), "type": "message", "role": "assistant",
		"model": value.Model, "content": content, "stop_reason": stopReason, "stop_sequence": nullableAnthropicString(value.StopSequence),
		"usage": anthropicUsage(value.Usage, webSearchRequestCount(webSearch)),
	}
}

func applyStopSequences(text string, sequences []string) (string, string) {
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

func anthropicUsage(value responseUsage, webSearchRequests int) map[string]any {
	usage := map[string]any{
		"input_tokens": value.InputTokens, "output_tokens": value.OutputTokens,
		"cache_creation_input_tokens": 0, "cache_read_input_tokens": value.InputTokensDetails.CachedTokens,
		"cost_in_usd_ticks":          value.CostInUSDTicks,
		"num_sources_used":           value.NumSourcesUsed,
		"num_server_side_tools_used": value.NumServerSideToolsUsed,
		"context_details": map[string]any{
			"input_tokens": value.ContextDetails.InputTokens, "output_tokens": value.ContextDetails.OutputTokens,
		},
	}
	if webSearchRequests > 0 {
		usage["server_tool_use"] = map[string]any{"web_search_requests": webSearchRequests}
	}
	return usage
}

func anthropicErrorJSON(value any) []byte {
	data, _ := json.Marshal(map[string]any{"type": "error", "error": normalizeAnthropicError(value)})
	return data
}

func normalizeAnthropicError(value any) map[string]any {
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
	return map[string]any{"type": errorType, "message": message}
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
