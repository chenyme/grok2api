package console

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

var (
	resetDurationPattern = regexp.MustCompile(`(?i)(\d+)\s*([dhms])`)
)

func normalizeRequest(body []byte, spec ModelSpec) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("解析 Console Responses 请求: %w", err)
	}
	payload["model"] = spec.UpstreamModel
	// Console is stateless. Replay the supplied input and silently discard
	// stateful client hints instead of rejecting an otherwise valid request.
	payload["store"] = false
	for _, field := range []string{
		"metadata", "previous_response_id", "service_tier", "prompt_cache_key",
		"background", "conversation",
	} {
		delete(payload, field)
	}
	normalizeConsoleResponseFormat(payload)
	patchConsoleInput(payload)
	if _, exists := payload["max_output_tokens"]; !exists && spec.MaxOutputTokens > 0 {
		payload["max_output_tokens"] = spec.MaxOutputTokens
	}
	normalizeReasoning(payload, spec)
	ensureReasoningInclude(payload)
	retainedClientTools := normalizeConsoleTools(payload)
	normalizeConsoleToolChoice(payload, retainedClientTools)
	return json.Marshal(payload)
}

func normalizeConsoleResponseFormat(payload map[string]any) {
	raw, exists := payload["response_format"]
	if !exists {
		return
	}
	delete(payload, "response_format")
	format, ok := raw.(map[string]any)
	if !ok {
		return
	}
	if typeName, _ := format["type"].(string); typeName == "json_schema" {
		if nested, ok := format["json_schema"].(map[string]any); ok {
			flattened := map[string]any{"type": "json_schema"}
			for key, value := range nested {
				if key != "type" {
					flattened[key] = value
				}
			}
			format = flattened
		}
	}
	text, _ := payload["text"].(map[string]any)
	if text == nil {
		text = make(map[string]any)
	}
	if _, exists := text["format"]; !exists {
		text["format"] = format
	}
	payload["text"] = text
}

func patchConsoleInput(payload map[string]any) {
	items, ok := payload["input"].([]any)
	if !ok {
		return
	}
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		if item["type"] == "reasoning" {
			patchConsoleReasoningContent(item)
			continue
		}
		content, ok := item["content"].([]any)
		if !ok {
			continue
		}
		for _, rawPart := range content {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			typeName, _ := part["type"].(string)
			switch typeName {
			case "text", "output_text":
				part["type"] = "input_text"
			case "image_url":
				if image, ok := part["image_url"].(map[string]any); ok {
					if url, _ := image["url"].(string); strings.TrimSpace(url) != "" {
						part["type"] = "input_image"
						part["image_url"] = url
					}
				}
			}
		}
	}
}

func patchConsoleReasoningContent(item map[string]any) {
	content, ok := item["content"].([]any)
	if !ok {
		return
	}
	for _, rawPart := range content {
		part, ok := rawPart.(map[string]any)
		if !ok {
			continue
		}
		if _, exists := part["type"]; !exists {
			if _, hasText := part["text"]; hasText {
				part["type"] = "reasoning_text"
			}
		}
	}
}

func normalizeReasoning(payload map[string]any, spec ModelSpec) {
	if !spec.SupportsReasoning {
		delete(payload, "reasoning")
		return
	}
	reasoning, _ := payload["reasoning"].(map[string]any)
	if reasoning == nil {
		if spec.DefaultReasoningEffort == "" {
			delete(payload, "reasoning")
			return
		}
		reasoning = make(map[string]any)
	}
	if !spec.SupportsReasoningEffort {
		delete(reasoning, "effort")
		if len(reasoning) == 0 {
			delete(payload, "reasoning")
		} else {
			payload["reasoning"] = reasoning
		}
		return
	}
	effort, _ := reasoning["effort"].(string)
	effort = normalizeEffort(effort)
	if effort == "" {
		effort = spec.DefaultReasoningEffort
	}
	if effort != "" {
		reasoning["effort"] = effort
	}
	payload["reasoning"] = reasoning
}

func normalizeEffort(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "none":
		return "none"
	case "minimal", "low":
		return "low"
	case "medium":
		return "medium"
	case "high":
		return "high"
	case "xhigh", "max":
		return "xhigh"
	default:
		return ""
	}
}

func ensureReasoningInclude(payload map[string]any) {
	value, _ := payload["include"].([]any)
	seen := make(map[string]struct{})
	result := make([]any, 0)
	for _, item := range value {
		name, ok := item.(string)
		if !ok || strings.TrimSpace(name) == "" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	if _, exists := seen["reasoning.encrypted_content"]; !exists {
		result = append(result, "reasoning.encrypted_content")
	}
	payload["include"] = result
}

func normalizeConsoleTools(payload map[string]any) bool {
	value, exists := payload["tools"]
	if !exists || value == nil {
		delete(payload, "tools")
		delete(payload, "tool_choice")
		return false
	}
	tools, ok := value.([]any)
	if !ok {
		delete(payload, "tools")
		delete(payload, "tool_choice")
		return false
	}
	result := make([]any, 0, len(tools))
	retainedClientTools := false
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		typeName, _ := tool["type"].(string)
		switch strings.ToLower(strings.TrimSpace(typeName)) {
		case "web_search", "web_search_preview", "web_search_preview_2025_03_11", "web_search_2025_08_26":
			clean := map[string]any{"type": "web_search", "enable_image_understanding": true}
			if enabled, ok := tool["enable_image_understanding"].(bool); ok {
				clean["enable_image_understanding"] = enabled
			}
			// Forward the image-search toggle (enable_image_search) so clients
			// can explicitly enable/disable it; absent when not requested.
			if enabled, ok := tool["enable_image_search"].(bool); ok {
				clean["enable_image_search"] = enabled
			}
			result = append(result, clean)
		case "x_search":
			clean := map[string]any{"type": "x_search", "enable_video_understanding": true}
			if enabled, ok := tool["enable_video_understanding"].(bool); ok {
				clean["enable_video_understanding"] = enabled
			}
			// Forward the X-search time bounds (from_date/to_date, YYYY-MM-DD).
			// Invalid formats and empty strings are dropped; if from_date is
			// later than to_date both are dropped to avoid an upstream 400.
			for _, field := range []string{"from_date", "to_date"} {
				text, ok := tool[field].(string)
				if !ok || text == "" {
					continue
				}
				if date, err := time.Parse("2006-01-02", text); err == nil && date.Format("2006-01-02") == text {
					clean[field] = text
				}
			}
			from, hasFrom := clean["from_date"].(string)
			to, hasTo := clean["to_date"].(string)
			if hasFrom && hasTo {
				fromDate, _ := time.Parse("2006-01-02", from)
				toDate, _ := time.Parse("2006-01-02", to)
				if fromDate.After(toDate) {
					delete(clean, "from_date")
					delete(clean, "to_date")
				}
			}
			result = append(result, clean)
		case "function":
			name, _ := tool["name"].(string)
			if strings.TrimSpace(name) == "" {
				continue
			}
			clean := map[string]any{"type": "function", "name": strings.TrimSpace(name)}
			for _, field := range []string{"description", "parameters", "strict"} {
				if fieldValue, exists := tool[field]; exists {
					clean[field] = fieldValue
				}
			}
			result = append(result, clean)
			retainedClientTools = true
		case "mcp", "shell", "image_generation", "collections_search", "file_search", "code_execution", "code_interpreter":
			// These are native xAI Responses tool variants. Keep their payloads,
			// while namespace/tool_search remain client-side abstractions and are
			// intentionally omitted instead of causing an upstream 400.
			result = append(result, tool)
			retainedClientTools = true
		}
	}
	if len(result) == 0 {
		delete(payload, "tools")
		delete(payload, "tool_choice")
		return false
	}
	payload["tools"] = result
	return retainedClientTools
}

func normalizeConsoleToolChoice(payload map[string]any, retainedClientTools bool) {
	if _, exists := payload["tools"]; !exists {
		delete(payload, "tool_choice")
		return
	}
	choice, exists := payload["tool_choice"]
	if !exists {
		payload["tool_choice"] = "auto"
		return
	}
	if value, ok := choice.(string); ok {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "none", "auto":
			payload["tool_choice"] = strings.ToLower(strings.TrimSpace(value))
		case "required":
			if !retainedClientTools {
				payload["tool_choice"] = "auto"
			}
		default:
			payload["tool_choice"] = "auto"
		}
		return
	}
	object, ok := choice.(map[string]any)
	if !ok {
		payload["tool_choice"] = "auto"
		return
	}
	typeName, _ := object["type"].(string)
	if typeName != "function" || !retainedClientTools {
		payload["tool_choice"] = "auto"
		return
	}
	name, _ := object["name"].(string)
	if strings.TrimSpace(name) == "" {
		if function, ok := object["function"].(map[string]any); ok {
			name, _ = function["name"].(string)
		}
	}
	if strings.TrimSpace(name) == "" {
		payload["tool_choice"] = "auto"
		return
	}
	payload["tool_choice"] = map[string]any{"type": "function", "name": strings.TrimSpace(name)}
}

func toolIdentity(value any) string {
	tool, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	typeName, _ := tool["type"].(string)
	if typeName != "function" {
		return typeName
	}
	name, _ := tool["name"].(string)
	return typeName + ":" + name
}

func consoleRetryAfter(body []byte) time.Duration {
	text := string(body)
	index := strings.Index(strings.ToLower(text), "resets in:")
	if index < 0 {
		return 0
	}
	text = text[index+len("resets in:"):]
	var total time.Duration
	for _, match := range resetDurationPattern.FindAllStringSubmatch(text, -1) {
		value, _ := strconv.Atoi(match[1])
		switch strings.ToLower(match[2]) {
		case "d":
			total += time.Duration(value) * 24 * time.Hour
		case "h":
			total += time.Duration(value) * time.Hour
		case "m":
			total += time.Duration(value) * time.Minute
		case "s":
			total += time.Duration(value) * time.Second
		}
	}
	return total
}

func parseConsoleRetryAfterHeader(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}

func parseConsoleRateLimitMetadata(body []byte) *provider.RateLimitMetadata {
	return provider.ParseRateLimitMetadata(body)
}
