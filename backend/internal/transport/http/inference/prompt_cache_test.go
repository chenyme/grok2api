package inference

import (
	"net/http"
	"strings"
	"testing"
)

func TestExtractPromptCacheSeedSupportsClaudeCodeForms(t *testing.T) {
	tests := []struct {
		name    string
		headers http.Header
		body    string
		want    string
	}{
		{name: "header", headers: http.Header{"X-Claude-Code-Session-Id": {"header-session"}}, body: `{"metadata":{"session_id":"body-session"}}`, want: "header-session"},
		{name: "metadata snake case", body: `{"metadata":{"session_id":"snake-session"}}`, want: "snake-session"},
		{name: "metadata camel case", body: `{"metadata":{"sessionId":"camel-session"}}`, want: "camel-session"},
		{name: "embedded json user id", body: `{"metadata":{"user_id":"{\"device_id\":\"d1\",\"session_id\":\"embedded-session\"}"}}`, want: "embedded-session"},
		{name: "suffix user id", body: `{"metadata":{"user_id":"user_account_session_123e4567-e89b-12d3-a456-426614174000"}}`, want: "123e4567-e89b-12d3-a456-426614174000"},
		{name: "ordinary user id", body: `{"metadata":{"user_id":"user-123"}}`, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := extractPromptCacheSeed(test.headers, []byte(test.body)); got != test.want {
				t.Fatalf("seed = %q, want %q", got, test.want)
			}
		})
	}
}

func TestExtractPromptCacheSeedRejectsOversizedValues(t *testing.T) {
	headers := make(http.Header)
	headers.Set("X-Claude-Code-Session-Id", strings.Repeat("x", maxPromptCacheSeedBytes+1))
	if seed := extractPromptCacheSeed(headers, nil); seed != "" {
		t.Fatalf("oversized seed = %q", seed)
	}
}
