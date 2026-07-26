package provider

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	rateLimitUsagePattern   = regexp.MustCompile(`(?i)\bRequests?\s+per\s+(Second|Minute)\s*\(\s*actual\s*/\s*limit\s*\)\s*:\s*(\d+)\s*/\s*(\d+)`)
	rateLimitTeamPattern    = regexp.MustCompile(`(?i)\bteam\s+([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\b`)
	rateLimitModelPattern   = regexp.MustCompile(`(?i)\bmodel\s+["']?([A-Za-z0-9][A-Za-z0-9._:/-]*)`)
	rateLimitModelTrimChars = ".,;"
	rateLimitResetPattern   = regexp.MustCompile(`(?i)(\d+)\s*([dhms])`)
)

// ParseRateLimitMetadata extracts Team+Model RPS/RPM limit metadata from an upstream 429 body.
// It accepts both Console and Build CLI resource-exhausted text shapes.
func ParseRateLimitMetadata(body []byte) *RateLimitMetadata {
	for _, text := range rateLimitTexts(body) {
		if metadata := parseRateLimitText(text); metadata != nil {
			return metadata
		}
	}
	return nil
}

// RateLimitFromResponse derives RateLimitMetadata from a 429 status, headers, and body.
// When Retry-After is absent but the body contains a reset window, the header is updated.
func RateLimitFromResponse(status int, header http.Header, body []byte) *RateLimitMetadata {
	if status != http.StatusTooManyRequests {
		return nil
	}
	metadata := ParseRateLimitMetadata(body)
	if metadata == nil {
		return nil
	}
	if header != nil {
		if headerValue := header.Get("Retry-After"); headerValue != "" {
			if retryAfter := parseRetryAfterHeader(headerValue, time.Now().UTC()); retryAfter > 0 {
				metadata.RetryAfter = retryAfter
			}
		} else if metadata.RetryAfter > 0 {
			header.Set("Retry-After", strconv.FormatInt(int64(metadata.RetryAfter/time.Second), 10))
		}
	}
	return metadata
}

func rateLimitTexts(body []byte) []string {
	texts := []string{string(body)}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return texts
	}
	collectRateLimitTexts(value, &texts)
	return texts
}

func collectRateLimitTexts(value any, texts *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		if message, ok := typed["message"].(string); ok {
			appendRateLimitText(message, texts)
		}
		if errText, ok := typed["error"].(string); ok {
			appendRateLimitText(errText, texts)
		}
		for _, nested := range typed {
			collectRateLimitTexts(nested, texts)
		}
	case []any:
		for _, nested := range typed {
			collectRateLimitTexts(nested, texts)
		}
	case string:
		appendRateLimitText(typed, texts)
	}
}

func appendRateLimitText(text string, texts *[]string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	*texts = append(*texts, text)
}

func parseRateLimitText(text string) *RateLimitMetadata {
	match := rateLimitUsagePattern.FindStringSubmatch(text)
	if match == nil {
		return nil
	}
	actual, actualErr := strconv.Atoi(match[2])
	limit, limitErr := strconv.Atoi(match[3])
	if actualErr != nil || limitErr != nil {
		return nil
	}
	scope := RateLimitScopeRPM
	retryAfter := time.Minute
	if strings.EqualFold(match[1], "second") {
		scope = RateLimitScopeRPS
		retryAfter = 2 * time.Second
	}
	if parsed := rateLimitResetAfter(text); parsed > 0 {
		retryAfter = parsed
		if scope == RateLimitScopeRPS && retryAfter < 2*time.Second {
			retryAfter = 2 * time.Second
		}
	}
	return &RateLimitMetadata{
		Scope:      scope,
		TeamID:     rateLimitTeamID(text),
		Model:      rateLimitModel(text),
		Actual:     actual,
		Limit:      limit,
		RetryAfter: retryAfter,
	}
}

func rateLimitTeamID(text string) string {
	match := rateLimitTeamPattern.FindStringSubmatch(text)
	if match == nil {
		return ""
	}
	return match[1]
}

func rateLimitModel(text string) string {
	match := rateLimitModelPattern.FindStringSubmatch(text)
	if match == nil {
		return ""
	}
	return strings.TrimRight(match[1], rateLimitModelTrimChars)
}

func rateLimitResetAfter(body string) time.Duration {
	index := strings.Index(strings.ToLower(body), "resets in:")
	if index < 0 {
		return 0
	}
	text := body[index+len("resets in:"):]
	var total time.Duration
	for _, match := range rateLimitResetPattern.FindAllStringSubmatch(text, -1) {
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

func parseRetryAfterHeader(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}
