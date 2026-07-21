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
		{name: "claude header", headers: http.Header{"X-Claude-Code-Session-Id": {"claude-session"}, "X-Session-Id": {"generic-session"}}, body: `{"metadata":{"session_id":"body-session"}}`, want: "claude:claude-session:agent:main"},
		{name: "claude agent header", headers: http.Header{"X-Claude-Code-Session-Id": {"claude-session"}, "X-Claude-Code-Agent-Id": {"agent-7"}}, want: "claude:claude-session:agent:agent-7"},
		{name: "codex turn prompt cache", headers: http.Header{"X-Codex-Turn-Metadata": {`{"prompt_cache_key":"codex-cache","window_id":"window-1"}`}}, want: "codex-cache"},
		{name: "codex turn window", headers: http.Header{"X-Codex-Turn-Metadata": {`{"window_id":"window-1"}`}}, want: "codex:window:window-1"},
		{name: "codex window header", headers: http.Header{"X-Codex-Window-Id": {"window-2"}}, want: "codex:window:window-2"},
		{name: "generic header", headers: http.Header{"X-Session-Id": {"generic-session"}}, want: "generic-session"},
		{name: "codex hyphen header", headers: http.Header{"Session-Id": {"codex-session"}}, want: "codex-session"},
		{name: "underscore session header", headers: http.Header{"Session_id": {"underscore-session"}}, want: "underscore-session"},
		{name: "metadata snake case", body: `{"metadata":{"session_id":"snake-session"}}`, want: "snake-session"},
		{name: "metadata camel case", body: `{"metadata":{"sessionId":"camel-session"}}`, want: "camel-session"},
		{name: "embedded json user id", body: `{"metadata":{"user_id":"{\"device_id\":\"d1\",\"session_id\":\"embedded-session\"}"}}`, want: "claude:embedded-session:agent:main"},
		{name: "suffix user id", body: `{"metadata":{"user_id":"user_account_session_123e4567-e89b-12d3-a456-426614174000"}}`, want: "claude:123e4567-e89b-12d3-a456-426614174000:agent:main"},
		{name: "conversation snake case", body: `{"conversation_id":"conversation-session"}`, want: "conversation-session"},
		{name: "conversation camel case", body: `{"conversationId":"camel-conversation"}`, want: "camel-conversation"},
		{name: "body prompt_cache_key", body: `{"prompt_cache_key":"client-session"}`, want: "client-session"},
		{name: "body codex turn metadata", body: `{"client_metadata":{"x-codex-turn-metadata":"{\"prompt_cache_key\":\"body-codex-cache\"}"}}`, want: "body-codex-cache"},
		{name: "body codex turn metadata object", body: `{"client_metadata":{"x-codex-turn-metadata":{"prompt_cache_key":"body-codex-object"}}}`, want: "body-codex-object"},
		{name: "body codex window", body: `{"client_metadata":{"x-codex-window-id":"body-window"}}`, want: "codex:window:body-window"},
		// http.Header canonicalizes "session_id" to Session-Id.
		{name: "compatible client session_id via Set", headers: func() http.Header { h := make(http.Header); h.Set("session_id", "client-header-session"); return h }(), want: "client-header-session"},
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

func TestExtractPromptCacheSeedIsolatesClaudeCodeTitleRequest(t *testing.T) {
	headers := http.Header{"X-Claude-Code-Session-Id": {"claude-session"}}
	body := []byte(`{
		"system":[
			{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.170; cch=abcde;"},
			{"type":"text","text":"Generate a concise, sentence-case title (3-7 words) that captures the main topic or goal of this coding session."}
		],
		"messages":[{"role":"user","content":"<session>hello</session>"}]
	}`)
	if seed := extractPromptCacheSeed(headers, body); seed != "" {
		t.Fatalf("title request seed = %q, want empty explicit seed", seed)
	}
}

func TestExtractPromptCacheSeedRejectsOversizedValues(t *testing.T) {
	headers := make(http.Header)
	headers.Set("X-Claude-Code-Session-Id", strings.Repeat("x", maxPromptCacheSeedBytes+1))
	if seed := extractPromptCacheSeed(headers, nil); seed != "" {
		t.Fatalf("oversized seed = %q", seed)
	}
}

func TestAllowBuildClientToolCacheRouteUsesKnownCLISignals(t *testing.T) {
	tests := []struct {
		name    string
		headers http.Header
		want    bool
	}{
		{name: "claude", headers: http.Header{"X-Claude-Code-Session-Id": {"session"}}, want: true},
		{name: "codex metadata", headers: http.Header{"X-Codex-Turn-Metadata": {`{"window_id":"window-1"}`}}, want: true},
		{name: "codex user agent", headers: http.Header{"User-Agent": {"Codex Desktop/1.0"}}, want: true},
		{name: "generic", headers: http.Header{"User-Agent": {"custom-client/1.0"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := allowBuildClientToolCacheRoute(test.headers); got != test.want {
				t.Fatalf("allowed = %t, want %t", got, test.want)
			}
		})
	}
}
