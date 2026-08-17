package audit

import (
	"strings"
	"testing"
)

func TestExtractReasoningEffort(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "chat", body: `{"model":"grok-4.6","reasoning_effort":"XHIGH"}`, want: "xhigh"},
		{name: "responses", body: `{"reasoning":{"effort":"high"}}`, want: "high"},
		{name: "messages output config", body: `{"output_config":{"effort":"medium"},"thinking":{"type":"adaptive"}}`, want: "medium"},
		{name: "thinking budget", body: `{"thinking":{"type":"enabled","budget_tokens":32000}}`, want: "thinking:32000"},
		{name: "thinking disabled", body: `{"thinking":{"type":"disabled"}}`, want: "none"},
		{name: "missing", body: `{"model":"grok-4.6"}`, want: ""},
		{name: "malformed", body: `{`, want: ""},
		{name: "bounded", body: `{"reasoning_effort":"` + strings.Repeat("x", 40) + `"}`, want: strings.Repeat("x", 32)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ExtractReasoningEffort([]byte(test.body)); got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
}
