package conversation

import (
	"fmt"
	"io"
	"strings"
	"testing"
)

// repeatSSE builds an SSE stream that emits the same delta count times using
// the given event name and payload template.
func repeatSSE(event, payloadTemplate string, count int, trailer ...string) string {
	lines := []string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-4.6","status":"in_progress"}}`, "",
	}
	for i := 0; i < count; i++ {
		lines = append(lines, "event: "+event, "data: "+payloadTemplate, "")
	}
	lines = append(lines, trailer...)
	lines = append(lines,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`, "", "")
	return strings.Join(lines, "\n")
}

// A visible-content loop is a real quota burn: it must be terminated near the
// content threshold and must not be allowed to run to the reasoning threshold.
func TestConvertResponsesStreamTerminatesContentDoomLoop(t *testing.T) {
	stream := repeatSSE("response.output_text.delta",
		`{"type":"response.output_text.delta","delta":"loop"}`,
		contentDoomLoopThreshold+8)
	_, err := io.ReadAll(ConvertResponseStream(io.NopCloser(strings.NewReader(stream)), OperationChat))
	if err == nil {
		t.Fatal("repeated visible content must terminate the stream")
	}
	if !strings.Contains(err.Error(), "model output loop detected") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Regression: high/xhigh effort reasoning legitimately repeats short tokens
// ("so", "hmm", "wait", bullet markers) far more often than visible output.
// A shared counter at the content threshold truncated valid deep-thinking
// answers, so reasoning must survive well past contentDoomLoopThreshold.
func TestConvertResponsesStreamKeepsRepeatedReasoningBelowThreshold(t *testing.T) {
	for _, event := range []string{"response.reasoning_text.delta", "response.reasoning_summary_text.delta"} {
		t.Run(event, func(t *testing.T) {
			// Sit between the two ceilings: this run must be fatal for visible
			// content but survivable for reasoning.
			repeats := (contentDoomLoopThreshold + reasoningDoomLoopThreshold) / 2
			if repeats <= contentDoomLoopThreshold || repeats >= reasoningDoomLoopThreshold {
				t.Fatalf("test invariant broken: %d must sit between %d and %d",
					repeats, contentDoomLoopThreshold, reasoningDoomLoopThreshold)
			}
			stream := repeatSSE(event,
				fmt.Sprintf(`{"type":%q,"item_id":"rs_1","delta":"hmm"}`, event),
				repeats,
				`event: response.output_text.delta`,
				`data: {"type":"response.output_text.delta","delta":"answer"}`, "")
			converted, err := io.ReadAll(ConvertResponseStream(io.NopCloser(strings.NewReader(stream)), OperationChat))
			if err != nil {
				t.Fatalf("deep reasoning must not be treated as a loop: %v", err)
			}
			if !strings.Contains(string(converted), `"content":"answer"`) {
				t.Fatalf("visible answer was lost: %s", converted)
			}
		})
	}
}

// The elevated reasoning threshold is a higher ceiling, not an exemption:
// a runaway reasoning loop still has to be terminated.
func TestConvertResponsesStreamTerminatesReasoningDoomLoop(t *testing.T) {
	for _, testCase := range []struct {
		event string
		want  string
	}{
		{"response.reasoning_text.delta", "model reasoning loop detected"},
		{"response.reasoning_summary_text.delta", "model reasoning summary loop detected"},
	} {
		t.Run(testCase.event, func(t *testing.T) {
			stream := repeatSSE(testCase.event,
				fmt.Sprintf(`{"type":%q,"item_id":"rs_1","delta":"hmm"}`, testCase.event),
				reasoningDoomLoopThreshold+8)
			_, err := io.ReadAll(ConvertResponseStream(io.NopCloser(strings.NewReader(stream)), OperationChat))
			if err == nil {
				t.Fatal("a runaway reasoning loop must still terminate the stream")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// Legitimate visible repetition must survive: markdown horizontal rules and
// ASCII table borders stream as long runs of an identical single-character
// delta. This is why the content threshold cannot sit near typical rule width.
func TestConvertResponsesStreamKeepsMarkdownRuleAndTableBorders(t *testing.T) {
	for _, testCase := range []struct{ name, delta string }{
		{"horizontal rule", "-"},
		{"table border", "="},
		{"empty table cells", " | "},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// A wide rule or table border comfortably exceeds 32 characters.
			stream := repeatSSE("response.output_text.delta",
				fmt.Sprintf(`{"type":"response.output_text.delta","delta":%q}`, testCase.delta),
				80)
			converted, err := io.ReadAll(ConvertResponseStream(io.NopCloser(strings.NewReader(stream)), OperationChat))
			if err != nil {
				t.Fatalf("legitimate repeated formatting must not be treated as a loop: %v", err)
			}
			if !strings.Contains(string(converted), "data: [DONE]") {
				t.Fatalf("stream did not complete: %s", converted)
			}
		})
	}
}

// Counters are per-channel and reset on change, so alternating deltas and
// interleaved reasoning must never accumulate into a false positive.
func TestConvertResponsesStreamDoomLoopCountersResetAndStaySeparate(t *testing.T) {
	lines := []string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-4.6","status":"in_progress"}}`, "",
	}
	// Alternating visible content never trips the content counter.
	for i := 0; i < contentDoomLoopThreshold*2; i++ {
		delta := "a"
		if i%2 == 1 {
			delta = "b"
		}
		lines = append(lines,
			`event: response.output_text.delta`,
			fmt.Sprintf(`data: {"type":"response.output_text.delta","delta":%q}`, delta), "")
	}
	// Reasoning repeats interleaved with distinct content must not share a
	// counter: the reasoning run alone exceeds the content threshold.
	for i := 0; i < contentDoomLoopThreshold*2; i++ {
		lines = append(lines,
			`event: response.reasoning_text.delta`,
			`data: {"type":"response.reasoning_text.delta","item_id":"rs_1","delta":"hmm"}`, "",
			`event: response.output_text.delta`,
			fmt.Sprintf(`data: {"type":"response.output_text.delta","delta":"tick%d"}`, i), "")
	}
	lines = append(lines,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`, "", "")
	stream := strings.Join(lines, "\n")
	converted, err := io.ReadAll(ConvertResponseStream(io.NopCloser(strings.NewReader(stream)), OperationChat))
	if err != nil {
		t.Fatalf("alternating deltas must not be treated as a loop: %v", err)
	}
	if !strings.Contains(string(converted), "data: [DONE]") {
		t.Fatalf("stream did not complete: %s", converted)
	}
}
