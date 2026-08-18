package gateway

import "testing"

const tuiCompactionPrompt = "Your task is to produce a faithful, concise summary of the conversation so far. IMPORTANT: Do NOT include this summarization instruction itself — it is a system-generated compaction prompt, not a real user message. Output the final summary inside a single <summary> block."

func TestIsResponsesCompactionRequest(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "remote v2", body: `{"input":[{"role":"user","content":"continue"},{"type":"compaction_trigger"}]}`, want: true},
		{name: "not terminal", body: `{"input":[{"type":"compaction_trigger"},{"role":"user","content":"continue"}]}`, want: true},
		{name: "tui last user string", body: `{"input":[{"role":"user","content":"continue"},{"role":"user","content":"` + tuiCompactionPrompt + `"}]}`, want: true},
		{name: "tui last user parts", body: `{"input":[{"role":"user","content":[{"type":"input_text","text":"` + tuiCompactionPrompt + `"}]}]}`, want: true},
		{name: "tui chat messages", body: `{"messages":[{"role":"user","content":"` + tuiCompactionPrompt + `"}]}`, want: true},
		{name: "prompt only in history", body: `{"input":[{"role":"user","content":"` + tuiCompactionPrompt + `"},{"role":"user","content":"continue the task"}]}`},
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
