package gateway

import "testing"

func TestIsResponsesCompactionRequest(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "remote v2", body: `{"input":[{"role":"user","content":"continue"},{"type":"compaction_trigger"}]}`, want: true},
		{name: "not terminal", body: `{"input":[{"type":"compaction_trigger"},{"role":"user","content":"continue"}]}`, want: true},
		{name: "normal responses", body: `{"input":[{"role":"user","content":"continue"}]}`},
		{name: "string input", body: `{"input":"continue"}`},
		{name: "invalid json", body: `{`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isResponsesCompactionRequest([]byte(test.body)); got != test.want {
				t.Fatalf("isResponsesCompactionRequest() = %t, want %t", got, test.want)
			}
		})
	}
}
