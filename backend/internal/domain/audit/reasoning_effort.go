package audit

import (
	"encoding/json"
	"strconv"
	"strings"
)

// ExtractReasoningEffort reads the client-facing reasoning setting used by
// Responses, Chat Completions, and Anthropic Messages requests.
func ExtractReasoningEffort(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var root map[string]any
	if json.Unmarshal(body, &root) != nil || root == nil {
		return ""
	}
	if value := normalizeReasoningEffort(root["reasoning_effort"]); value != "" {
		return value
	}
	if nested, ok := root["reasoning"].(map[string]any); ok {
		if value := normalizeReasoningEffort(nested["effort"]); value != "" {
			return value
		}
	}
	if nested, ok := root["output_config"].(map[string]any); ok {
		if value := normalizeReasoningEffort(nested["effort"]); value != "" {
			return value
		}
	}
	if nested, ok := root["thinking"].(map[string]any); ok {
		if value := normalizeReasoningEffort(nested["effort"]); value != "" {
			return value
		}
		typeName := strings.ToLower(strings.TrimSpace(reasoningString(nested["type"])))
		if typeName == "enabled" || typeName == "adaptive" {
			if budget, ok := reasoningNumber(nested["budget_tokens"]); ok && budget > 0 {
				return "thinking:" + strconv.FormatInt(int64(budget), 10)
			}
			return typeName
		}
		if typeName == "disabled" {
			return "none"
		}
	}
	return ""
}

func normalizeReasoningEffort(raw any) string {
	value := strings.ToLower(strings.TrimSpace(reasoningString(raw)))
	if value == "" || value == "null" {
		return ""
	}
	runes := []rune(value)
	if len(runes) > 32 {
		return string(runes[:32])
	}
	return value
}

func reasoningString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case json.Number:
		return typed.String()
	default:
		return ""
	}
}

func reasoningNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case json.Number:
		number, err := typed.Float64()
		return number, err == nil
	case string:
		number, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return number, err == nil
	default:
		return 0, false
	}
}
