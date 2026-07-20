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
		{name: "claude header", headers: http.Header{"X-Claude-Code-Session-Id": {"claude-session"}, "X-Session-Id": {"generic-session"}}, body: `{"metadata":{"session_id":"body-session"}}`, want: "claude-session"},
		{name: "generic header", headers: http.Header{"X-Session-Id": {"generic-session"}}, want: "generic-session"},
		{name: "codex hyphen header", headers: http.Header{"Session-Id": {"codex-session"}}, want: "codex-session"},
		{name: "codex underscore header", headers: http.Header{"Session_id": {"legacy-codex-session"}}, want: "legacy-codex-session"},
		{name: "metadata snake case", body: `{"metadata":{"session_id":"snake-session"}}`, want: "snake-session"},
		{name: "metadata camel case", body: `{"metadata":{"sessionId":"camel-session"}}`, want: "camel-session"},
		{name: "embedded json user id", body: `{"metadata":{"user_id":"{\"device_id\":\"d1\",\"session_id\":\"embedded-session\"}"}}`, want: "embedded-session"},
		{name: "suffix user id", body: `{"metadata":{"user_id":"user_account_session_123e4567-e89b-12d3-a456-426614174000"}}`, want: "123e4567-e89b-12d3-a456-426614174000"},
		{name: "conversation snake case", body: `{"conversation_id":"conversation-session"}`, want: "conversation-session"},
		{name: "conversation camel case", body: `{"conversationId":"camel-conversation"}`, want: "camel-conversation"},
		{name: "body prompt_cache_key", body: `{"prompt_cache_key":"sub2api-session"}`, want: "sub2api-session"},
		// http.Header canonical key is Session-Id for "session_id"
		{name: "sub2api session_id via Set", headers: func() http.Header { h := make(http.Header); h.Set("session_id", "sub2-header-session"); return h }(), want: "sub2-header-session"},
		{name: "grok conv header", headers: http.Header{"X-Grok-Conv-Id": {"grok-conv-session"}}, want: "grok-conv-session"},
		{name: "per request id ignored", headers: http.Header{"X-Client-Request-Id": {"request-123"}}, want: ""},
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
