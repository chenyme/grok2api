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
	resetDurationPattern           = regexp.MustCompile(`(?i)(\d+)\s*([dhms])`)
	consoleRateLimitUsagePattern   = regexp.MustCompile(`(?i)\bRequests?\s+per\s+(Second|Minute)\s*\(\s*actual\s*/\s*limit\s*\)\s*:\s*(\d+)\s*/\s*(\d+)`)
	consoleRateLimitTeamPattern    = regexp.MustCompile(`(?i)\bteam\s+([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\b`)
	consoleRateLimitModelPattern   = regexp.MustCompile(`(?i)\bmodel\s+["']?([A-Za-z0-9][A-Za-z0-9._:/-]*)`)
	consoleRateLimitModelTrimChars = ".,;"
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
	if spec.SearchTools {
		mergeSearchTools(payload)
	}
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
		reasoning = make(map[string]any)
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
			result = append(result, clean)
		case "x_search":
			clean := map[string]any{"type": "x_search", "enable_video_understanding": true}
			if enabled, ok := tool["enable_video_understanding"].(bool); ok {
				clean["enable_video_understanding"] = enabled
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
		return false
	}
	payload["tools"] = result
	return retainedClientTools
}

func mergeSearchTools(payload map[string]any) {
	defaults := []any{
		map[string]any{"type": "web_search", "enable_image_understanding": true},
		map[string]any{"type": "x_search", "enable_video_understanding": true},
	}
	positions := map[string]int{"web_search": 0, "x_search": 1}
	result := append([]any(nil), defaults...)
	if value, exists := payload["tools"]; exists && value != nil {
		tools, _ := value.([]any)
		for _, tool := range tools {
			identity := toolIdentity(tool)
			if index, exists := positions[identity]; identity != "" && exists {
				result[index] = tool
				continue
			}
			if identity != "" {
				positions[identity] = len(result)
			}
			result = append(result, tool)
		}
	}
	payload["tools"] = result
	if _, exists := payload["tool_choice"]; !exists {
		payload["tool_choice"] = "auto"
	}
}

func normalizeConsoleToolChoice(payload map[string]any, retainedClientTools bool) {
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
	for _, text := range consoleRateLimitTexts(body) {
		metadata := parseConsoleRateLimitText(text)
		if metadata != nil {
			return metadata
		}
	}
	return nil
}

func consoleRateLimitTexts(body []byte) []string {
	texts := []string{string(body)}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return texts
	}
	collectConsoleRateLimitTexts(value, &texts)
	return texts
}

func collectConsoleRateLimitTexts(value any, texts *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		if message, ok := typed["message"].(string); ok {
			appendConsoleRateLimitText(message, texts)
		}
		for _, nested := range typed {
			collectConsoleRateLimitTexts(nested, texts)
		}
	case []any:
		for _, nested := range typed {
			collectConsoleRateLimitTexts(nested, texts)
		}
	case string:
		appendConsoleRateLimitText(typed, texts)
	}
}

func appendConsoleRateLimitText(text string, texts *[]string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	*texts = append(*texts, text)
}

func parseConsoleRateLimitText(text string) *provider.RateLimitMetadata {
	match := consoleRateLimitUsagePattern.FindStringSubmatch(text)
	if match == nil {
		return nil
	}
	actual, actualErr := strconv.Atoi(match[2])
	limit, limitErr := strconv.Atoi(match[3])
	if actualErr != nil || limitErr != nil {
		return nil
	}
	scope := provider.RateLimitScopeRPM
	retryAfter := time.Minute
	if strings.EqualFold(match[1], "second") {
		scope = provider.RateLimitScopeRPS
		retryAfter = 2 * time.Second
	}
	if parsed := consoleRetryAfter([]byte(text)); parsed > 0 {
		retryAfter = parsed
		if scope == provider.RateLimitScopeRPS && retryAfter < 2*time.Second {
			retryAfter = 2 * time.Second
		}
	}
	return &provider.RateLimitMetadata{
		Scope:      scope,
		TeamID:     consoleRateLimitTeamID(text),
		Model:      consoleRateLimitModel(text),
		Actual:     actual,
		Limit:      limit,
		RetryAfter: retryAfter,
	}
}

func consoleRateLimitTeamID(text string) string {
	match := consoleRateLimitTeamPattern.FindStringSubmatch(text)
	if match == nil {
		return ""
	}
	return match[1]
}

func consoleRateLimitModel(text string) string {
	match := consoleRateLimitModelPattern.FindStringSubmatch(text)
	if match == nil {
		return ""
	}
	return strings.TrimRight(match[1], consoleRateLimitModelTrimChars)
}
