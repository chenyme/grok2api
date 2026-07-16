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
	if value, exists := payload["store"]; exists {
		store, ok := value.(bool)
		if !ok {
			return nil, fmt.Errorf("Console store 必须是布尔值")
		}
		if store {
			return nil, fmt.Errorf("Grok Console 不支持 store: true；请使用无状态 Responses 输入回放")
		}
	}
	if value, exists := payload["previous_response_id"]; exists && value != nil {
		previousID, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("Console previous_response_id 必须是字符串")
		}
		if strings.TrimSpace(previousID) != "" {
			return nil, fmt.Errorf("Grok Console 不支持 previous_response_id；请回放完整输入")
		}
		delete(payload, "previous_response_id")
	}
	payload["model"] = spec.UpstreamModel
	payload["store"] = false
	delete(payload, "metadata")
	if _, exists := payload["max_output_tokens"]; !exists && spec.MaxOutputTokens > 0 {
		payload["max_output_tokens"] = spec.MaxOutputTokens
	}
	normalizeReasoning(payload, spec)
	if spec.SearchTools {
		if err := mergeSearchTools(payload); err != nil {
			return nil, err
		}
	}
	return json.Marshal(payload)
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
	case "minimal", "low":
		return "low"
	case "medium":
		return "medium"
	case "high", "xhigh":
		return "high"
	default:
		return ""
	}
}

func mergeSearchTools(payload map[string]any) error {
	defaults := []any{
		map[string]any{"type": "web_search", "enable_image_understanding": true},
		map[string]any{"type": "x_search", "enable_video_understanding": true},
	}
	positions := map[string]int{"web_search": 0, "x_search": 1}
	result := append([]any(nil), defaults...)
	if value, exists := payload["tools"]; exists && value != nil {
		tools, ok := value.([]any)
		if !ok {
			return fmt.Errorf("Console tools 必须是数组")
		}
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
	return nil
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
