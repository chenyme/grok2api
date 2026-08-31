package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

func TestClassifyQualityHold(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		sig  QualityStreamSignals
		want QualityVerdict
	}{
		{name: "thinking delivers", sig: QualityStreamSignals{HasThinking: true, HasReasoningDelta: true, VisibleTokens: 10}, want: QualityDeliver},
		{name: "usage reasoning tokens alone withhold", sig: QualityStreamSignals{ReasoningTokens: 40, VisibleTokens: 80, Terminal: true}, want: QualityWithhold},
		{name: "visible 32 no think withhold", sig: QualityStreamSignals{VisibleTokens: 32, Terminal: true}, want: QualityWithhold},
		{name: "output 40 no think withhold", sig: QualityStreamSignals{OutputTokens: 40, Terminal: true}, want: QualityWithhold},
		{name: "short visible output ignores inflated total", sig: QualityStreamSignals{VisibleTokens: 1, OutputTokens: 80, Terminal: true}, want: QualityDeliver},
		{name: "short no think delivers", sig: QualityStreamSignals{VisibleTokens: 10, Terminal: true}, want: QualityDeliver},
		{name: "empty terminal waits for transport handling", sig: QualityStreamSignals{Terminal: true}, want: QualityWait},
		{name: "midstream enough content withhold", sig: QualityStreamSignals{VisibleTokens: 64}, want: QualityWithhold},
		{name: "stub midstream waits even with enough visible", sig: QualityStreamSignals{ReasoningStarted: true, VisibleTokens: 64}, want: QualityWait},
		{name: "stub hold expiry with enough visible withholds", sig: QualityStreamSignals{ReasoningStarted: true, VisibleTokens: 64, HoldExpired: true}, want: QualityWithhold},
		{name: "stub-only hold expiry keeps waiting", sig: QualityStreamSignals{ReasoningStarted: true, HoldExpired: true}, want: QualityWait},
		{name: "stub terminal enough withhold", sig: QualityStreamSignals{ReasoningStarted: true, VisibleTokens: 64, Terminal: true}, want: QualityWithhold},
		{name: "wait for more", sig: QualityStreamSignals{VisibleTokens: 8}, want: QualityWait},
		{name: "hold expired short delivers", sig: QualityStreamSignals{VisibleTokens: 8, HoldExpired: true}, want: QualityDeliver},
		{name: "hold expired empty waits", sig: QualityStreamSignals{HoldExpired: true}, want: QualityWait},
		{name: "hold expired zero tokens waits", sig: QualityStreamSignals{VisibleTokens: 0, OutputTokens: 0, HoldExpired: true}, want: QualityWait},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := ClassifyQualityHold(test.sig, 32); got != test.want {
				t.Fatalf("ClassifyQualityHold() = %s, want %s", got, test.want)
			}
		})
	}
}

func TestClassifyQualityHoldBurst(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		sig  QualityStreamSignals
		want QualityVerdict
	}{
		{
			name: "terminal hold-expired hello dump withholds",
			sig:  QualityStreamSignals{HasThinking: true, VisibleTokens: 2, ReasoningTokens: 954, EncryptedBytes: 4000, EncryptedFloor: 3816, UsageReported: true, FirstVisible: true, HoldExpired: true, Terminal: true},
			want: QualityWithhold,
		},
		{
			name: "hold-expired long answer delivers",
			sig:  QualityStreamSignals{HasThinking: true, VisibleTokens: 200, ReasoningTokens: 954, EncryptedBytes: 8000, EncryptedFloor: 3816, UsageReported: true, FirstVisible: true, HoldExpired: true},
			want: QualityDeliver,
		},
		{
			name: "non-terminal barely-over-floor flush keeps observing",
			sig:  QualityStreamSignals{HasThinking: true, VisibleTokens: 50, ReasoningTokens: 60, EncryptedBytes: 300, EncryptedFloor: 256, UsageReported: true, FirstVisible: true, VisibleFlushMS: 100},
			want: QualityWait,
		},
		{
			name: "terminal barely-over-floor flush withholds",
			sig:  QualityStreamSignals{HasThinking: true, VisibleTokens: 50, ReasoningTokens: 60, EncryptedBytes: 300, EncryptedFloor: 256, UsageReported: true, FirstVisible: true, VisibleFlushMS: 100, Terminal: true},
			want: QualityWithhold,
		},
		{
			name: "large cipher flush delivers",
			sig:  QualityStreamSignals{HasThinking: true, VisibleTokens: 50, ReasoningTokens: 60, EncryptedBytes: 2000, EncryptedFloor: 256, UsageReported: true, FirstVisible: true, VisibleFlushMS: 100},
			want: QualityDeliver,
		},
		{
			name: "non-terminal floor-met short burst keeps observing",
			sig:  QualityStreamSignals{HasThinking: true, VisibleTokens: 3, ReasoningTokens: 1371, EncryptedBytes: 8000, EncryptedFloor: 5484, UsageReported: true, FirstVisible: true, VisibleFlushMS: 200},
			want: QualityWait,
		},
		{
			name: "floor-met short visible fast dump withholds",
			sig:  QualityStreamSignals{HasThinking: true, VisibleTokens: 3, ReasoningTokens: 1371, EncryptedBytes: 8000, EncryptedFloor: 5484, UsageReported: true, FirstVisible: true, VisibleFlushMS: 200, Terminal: true},
			want: QualityWithhold,
		},
		{
			name: "floor-met 8 visible 140 reasoning in under 1s withholds",
			sig:  QualityStreamSignals{HasThinking: true, VisibleTokens: 8, ReasoningTokens: 140, EncryptedBytes: 2000, EncryptedFloor: 560, UsageReported: true, FirstVisible: true, VisibleFlushMS: 660, Terminal: true},
			want: QualityWithhold,
		},
		{
			name: "floor-met long visible fast flush delivers",
			sig:  QualityStreamSignals{HasThinking: true, VisibleTokens: 80, ReasoningTokens: 140, EncryptedBytes: 2000, EncryptedFloor: 560, UsageReported: true, FirstVisible: true, VisibleFlushMS: 200, Terminal: true},
			want: QualityDeliver,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := ClassifyQualityHold(test.sig, 8); got != test.want {
				t.Fatalf("ClassifyQualityHold() = %s, want %s (%#v)", got, test.want, test.sig)
			}
		})
	}
}

func TestClassifyQualityHoldBurstUsesConfiguredFloor(t *testing.T) {
	t.Parallel()
	configuredHigh := QualityStreamSignals{
		HasThinking: true, VisibleTokens: 40, ReasoningTokens: 100,
		EncryptedBytes: 1100, EncryptedFloor: 1024, UsageReported: true,
		FirstVisible: true, VisibleFlushMS: 100, Terminal: true,
	}
	if got := ClassifyQualityHold(configuredHigh, 8); got != QualityWithhold {
		t.Fatalf("configured high floor verdict = %s, want withhold", got)
	}
	configuredLow := QualityStreamSignals{
		HasThinking: true, VisibleTokens: 40, ReasoningTokens: 60,
		EncryptedBytes: 300, EncryptedFloor: 64, UsageReported: true,
		FirstVisible: true, VisibleFlushMS: 100, Terminal: true,
	}
	if got := ClassifyQualityHold(configuredLow, 8); got != QualityDeliver {
		t.Fatalf("configured low floor verdict = %s, want deliver", got)
	}
}

func TestEncryptedThinkingFloorSaturates(t *testing.T) {
	t.Parallel()
	if got := encryptedThinkingFloor(256, 16, math.MaxInt64); got != math.MaxInt64 {
		t.Fatalf("overflowing floor = %d, want %d", got, int64(math.MaxInt64))
	}
}

func TestDecideQualityRetry(t *testing.T) {
	t.Parallel()
	if got := DecideQualityRetry(QualityDeliver, 0, 2, qualityRetryFailOpen); got != QualityActionDeliver {
		t.Fatalf("deliver verdict: %s", got)
	}
	if got := DecideQualityRetry(QualityWithhold, 0, 2, qualityRetryFailOpen); got != QualityActionRetry {
		t.Fatalf("first withhold: %s", got)
	}
	if got := DecideQualityRetry(QualityWithhold, 1, 2, qualityRetryFailOpen); got != QualityActionDeliverLast {
		t.Fatalf("last fail-open: %s", got)
	}
	if got := DecideQualityRetry(QualityWithhold, 1, 2, qualityRetryFailClosed); got != QualityActionReject {
		t.Fatalf("last fail-closed: %s", got)
	}
	if got := DecideQualityRetry(QualityWithhold, 0, 1, qualityRetryFailOpen); got != QualityActionDeliverLast {
		t.Fatalf("max 1 fail-open: %s", got)
	}
	if got := DecideQualityRetry(QualityWithhold, 5, 0, ""); got != QualityActionReject {
		t.Fatalf("zero-value policy must use fail-closed default: %s", got)
	}
}

func TestDecideQualityRetryLastWithholdIsMaxAttemptsMinusOne(t *testing.T) {
	t.Parallel()
	for _, maxAttempts := range []int{1, 2, 3, 6} {
		last := maxAttempts - 1
		if got := DecideQualityRetry(QualityWithhold, last, maxAttempts, qualityRetryFailOpen); got != QualityActionDeliverLast {
			t.Fatalf("fail-open last withhold max=%d index=%d got %s", maxAttempts, last, got)
		}
		if got := DecideQualityRetry(QualityWithhold, last, maxAttempts, qualityRetryFailClosed); got != QualityActionReject {
			t.Fatalf("fail-closed last withhold max=%d index=%d got %s", maxAttempts, last, got)
		}
		if last > 0 {
			if got := DecideQualityRetry(QualityWithhold, last-1, maxAttempts, qualityRetryFailOpen); got != QualityActionRetry {
				t.Fatalf("pre-last should retry max=%d index=%d got %s", maxAttempts, last-1, got)
			}
		}
	}
}

func TestCommitQualityHold(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		verdict        QualityVerdict
		qualityAttempt int
		maxAttempts    int
		hasNext        bool
		onExhausted    string
		wantAction     QualityRetryAction
		wantAudit      bool
		wantKeep       bool
	}{
		{
			name:    "first withhold + hasNext → Retry+Audit",
			verdict: QualityWithhold, qualityAttempt: 0, maxAttempts: 2, hasNext: true, onExhausted: qualityRetryFailOpen,
			wantAction: QualityActionRetry, wantAudit: true, wantKeep: false,
		},
		{
			name:    "last withhold fail-open → DeliverLast+KeepBody",
			verdict: QualityWithhold, qualityAttempt: 1, maxAttempts: 2, hasNext: true, onExhausted: qualityRetryFailOpen,
			wantAction: QualityActionDeliverLast, wantAudit: false, wantKeep: true,
		},
		{
			name:    "last withhold fail-closed → Reject+Audit",
			verdict: QualityWithhold, qualityAttempt: 1, maxAttempts: 2, hasNext: true, onExhausted: qualityRetryFailClosed,
			wantAction: QualityActionReject, wantAudit: true, wantKeep: false,
		},
		{
			name:    "routing exhausted even at qualityAttempt=0 → not Retry",
			verdict: QualityWithhold, qualityAttempt: 0, maxAttempts: 2, hasNext: false, onExhausted: qualityRetryFailOpen,
			wantAction: QualityActionDeliverLast, wantAudit: false, wantKeep: true,
		},
		{
			name:    "thinking delivers keep body",
			verdict: QualityDeliver, qualityAttempt: 0, maxAttempts: 2, hasNext: true, onExhausted: qualityRetryFailOpen,
			wantAction: QualityActionDeliver, wantAudit: false, wantKeep: true,
		},
		{
			name:    "switch 5 times: attempt 4 of 6 still retries",
			verdict: QualityWithhold, qualityAttempt: 4, maxAttempts: 6, hasNext: true, onExhausted: qualityRetryFailClosed,
			wantAction: QualityActionRetry, wantAudit: true, wantKeep: false,
		},
		{
			name:    "switch 5 times: attempt 5 of 6 fail-closed rejects no body",
			verdict: QualityWithhold, qualityAttempt: 5, maxAttempts: 6, hasNext: true, onExhausted: qualityRetryFailClosed,
			wantAction: QualityActionReject, wantAudit: true, wantKeep: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := CommitQualityHold(test.verdict, test.qualityAttempt, test.maxAttempts, test.hasNext, test.onExhausted)
			if got.Action != test.wantAction || got.Audit != test.wantAudit || got.KeepBody != test.wantKeep {
				t.Fatalf("CommitQualityHold() = %+v, want action=%s audit=%t keep=%t", got, test.wantAction, test.wantAudit, test.wantKeep)
			}
			if got.Action == QualityActionRetry && !test.hasNext {
				t.Fatal("routing exhausted must not Retry")
			}
		})
	}
}

func TestBoundQualityRetryWhenRoutingExhausted(t *testing.T) {
	t.Parallel()
	if got := BoundQualityRetry(QualityActionRetry, true, qualityRetryFailOpen); got != QualityActionRetry {
		t.Fatalf("has next: %s", got)
	}
	if got := BoundQualityRetry(QualityActionRetry, false, qualityRetryFailOpen); got != QualityActionDeliverLast {
		t.Fatalf("no next fail-open: %s", got)
	}
	if got := BoundQualityRetry(QualityActionRetry, false, qualityRetryFailClosed); got != QualityActionReject {
		t.Fatalf("no next fail-closed: %s", got)
	}
	if got := BoundQualityRetry(QualityActionDeliverLast, false, qualityRetryFailOpen); got != QualityActionDeliverLast {
		t.Fatalf("already last: %s", got)
	}
}

func TestSelectionSessionHasAvailableCandidate(t *testing.T) {
	t.Parallel()
	session := &selectionSession{
		values: []accountdomain.RoutingCandidate{
			{Credential: accountdomain.Credential{ID: 1}},
			{Credential: accountdomain.Credential{ID: 2}},
			{Credential: accountdomain.Credential{ID: 3}},
		},
		normalCandidates: []int{0, 1},
		probeCandidates:  []int{2},
		staleCandidates:  make(map[uint64]bool),
	}
	if !session.hasAvailableCandidate(map[uint64]bool{1: true}, false) {
		t.Fatal("second normal account should be available")
	}
	if session.hasAvailableCandidate(map[uint64]bool{1: true, 2: true}, false) {
		t.Fatal("routing attempt budget must not invent another normal account")
	}
	if !session.hasAvailableCandidate(map[uint64]bool{1: true, 2: true}, true) {
		t.Fatal("quota probe should be available when allowed")
	}
	if session.hasAvailableCandidate(map[uint64]bool{1: true, 2: true, 3: true}, true) {
		t.Fatal("fully excluded session should be exhausted")
	}
}

func sse(frames ...string) string {
	var b strings.Builder
	for _, frame := range frames {
		b.WriteString(frame)
		if !strings.HasSuffix(frame, "\n") {
			b.WriteByte('\n')
		}
		if !strings.HasSuffix(frame, "\n\n") {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func TestObserveQualityChunkThinkingChat(t *testing.T) {
	t.Parallel()
	state := qualityScanState{protocol: qualityProtocolChat}
	ObserveQualityChunk(&state, []byte(sse(
		": grok2api-reasoning-start",
		`data: {"choices":[{"delta":{"thinking_content":"plan the game"}}]}`,
		`data: {"choices":[{"delta":{"content":"here is a game"}}]}`,
		`data: {"usage":{"completion_tokens":80,"completion_tokens_details":{"reasoning_tokens":40}}}`,
		"data: [DONE]",
	)))
	sig := state.signals()
	if !sig.HasThinking || !sig.Terminal || sig.ReasoningTokens != 40 {
		t.Fatalf("thinking fixture signals = %#v", sig)
	}
	if ClassifyQualityHold(sig, 32) != QualityDeliver {
		t.Fatalf("thinking fixture withheld")
	}
}

func TestObserveQualityChunkNoThinkEnoughChat(t *testing.T) {
	t.Parallel()
	state := qualityScanState{protocol: qualityProtocolChat}
	content := strings.Repeat("word ", 40) // 200 runes → 50 tokens
	ObserveQualityChunk(&state, []byte(sse(
		`data: {"choices":[{"delta":{"content":"`+content+`"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: {"usage":{"completion_tokens":50,"completion_tokens_details":{"reasoning_tokens":0}}}`,
		"data: [DONE]",
	)))
	sig := state.signals()
	if sig.HasThinking || !sig.Terminal || sig.VisibleTokens < 32 {
		t.Fatalf("no-think fixture signals = %#v", sig)
	}
	if ClassifyQualityHold(sig, 32) != QualityWithhold {
		t.Fatalf("no-think enough should withhold, got %s (%#v)", ClassifyQualityHold(sig, 32), sig)
	}
}

func TestObserveQualityChunkShortNoThink(t *testing.T) {
	t.Parallel()
	state := qualityScanState{protocol: qualityProtocolChat}
	ObserveQualityChunk(&state, []byte(sse(
		`data: {"choices":[{"delta":{"content":"ok"}}]}`,
		"data: [DONE]",
	)))
	sig := state.signals()
	if ClassifyQualityHold(sig, 32) != QualityDeliver {
		t.Fatalf("short no-think should deliver, got %s (%#v)", ClassifyQualityHold(sig, 32), sig)
	}
}

func TestObserveQualityChunkShortNoThinkIgnoresFakeReasoningUsage(t *testing.T) {
	t.Parallel()
	state := qualityScanState{protocol: qualityProtocolChat}
	ObserveQualityChunk(&state, []byte(sse(
		": grok2api-reasoning-start",
		`data: {"choices":[{"delta":{"content":"OK"}}]}`,
		`data: {"usage":{"completion_tokens":80,"completion_tokens_details":{"reasoning_tokens":79}}}`,
		"data: [DONE]",
	)))
	sig := state.signals()
	if sig.VisibleTokens >= 32 || sig.OutputTokens != 80 || sig.ReasoningTokens != 79 {
		t.Fatalf("fake-usage short reply signals = %#v", sig)
	}
	if got := ClassifyQualityHold(sig, 32); got != QualityDeliver {
		t.Fatalf("short visible reply must not be withheld by inflated usage: %s (%#v)", got, sig)
	}
}

func TestObserveQualityConvertedEncryptedThinking(t *testing.T) {
	t.Parallel()
	cipher := strings.Repeat("A", defaultMinEncryptedBytes*8)
	source := sse(
		`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-4.6"}}`,
		`data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`,
		`data: {"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"`+cipher+`"}}`,
		`data: {"type":"response.output_text.delta","delta":"`+strings.Repeat("word ", 40)+`"}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"grok-4.6","usage":{"output_tokens":90,"output_tokens_details":{"reasoning_tokens":60}}}}`,
	)
	tests := []struct {
		name      string
		operation string
		protocol  string
		options   conversation.ResponseOptions
	}{
		{name: "chat", operation: conversation.OperationChat, protocol: qualityProtocolChat},
		{name: "messages", operation: conversation.OperationMessages, protocol: qualityProtocolAnthropic, options: conversation.ResponseOptions{AnthropicThinking: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			converted, err := io.ReadAll(conversation.ConvertResponseStreamWithOptions(
				io.NopCloser(strings.NewReader(source)), test.operation, test.options,
			))
			if err != nil {
				t.Fatal(err)
			}
			state := qualityScanState{protocol: test.protocol}
			ObserveQualityChunk(&state, converted)
			sig := state.signals()
			if !sig.HasThinking {
				t.Fatalf("converted encrypted thinking evidence was lost:\n%s", converted)
			}
			if got := ClassifyQualityHold(sig, 32); got != QualityDeliver {
				t.Fatalf("converted encrypted thinking verdict = %s (%#v)", got, sig)
			}
		})
	}
}

func TestObserveQualityConvertedShortEncryptedStubDoesNotBypassFloor(t *testing.T) {
	t.Parallel()
	const stub = "gAAAA-cipher"
	source := sse(
		`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-4.6"}}`,
		`data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`,
		`data: {"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"`+stub+`"}}`,
		`data: {"type":"response.output_text.delta","delta":"`+strings.Repeat("word ", 40)+`"}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"grok-4.6","usage":{"output_tokens":90,"output_tokens_details":{"reasoning_tokens":60}}}}`,
	)
	for _, test := range []struct {
		name      string
		operation string
		protocol  string
		options   conversation.ResponseOptions
	}{
		{name: "chat", operation: conversation.OperationChat, protocol: qualityProtocolChat},
		{name: "messages", operation: conversation.OperationMessages, protocol: qualityProtocolAnthropic, options: conversation.ResponseOptions{AnthropicThinking: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			converted, err := io.ReadAll(conversation.ConvertResponseStreamWithOptions(
				io.NopCloser(strings.NewReader(source)), test.operation, test.options,
			))
			if err != nil {
				t.Fatal(err)
			}
			state := qualityScanState{protocol: test.protocol}
			ObserveQualityChunk(&state, converted)
			sig := state.signals()
			if sig.HasThinking || sig.EncryptedBytes != len(stub) || !sig.ReasoningStarted {
				t.Fatalf("converted short stub signals = %#v\n%s", sig, converted)
			}
			if got := ClassifyQualityHold(sig, 32); got != QualityWithhold {
				t.Fatalf("converted short stub verdict = %s, want withhold", got)
			}
		})
	}
}

func TestObserveQualityChunkWhitespaceIsNotThinking(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		protocol string
		fixture  string
	}{
		{name: "chat", protocol: qualityProtocolChat, fixture: sse(`data: {"choices":[{"delta":{"reasoning_content":" \n\t"}}]}`)},
		{name: "responses", protocol: qualityProtocolResponses, fixture: sse(`data: {"type":"response.reasoning_summary_text.delta","delta":" \n\t"}`)},
		{name: "messages", protocol: qualityProtocolAnthropic, fixture: sse(`data: {"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":" \n\t"}}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := qualityScanState{protocol: test.protocol}
			ObserveQualityChunk(&state, []byte(test.fixture))
			if sig := state.signals(); sig.HasThinking {
				t.Fatalf("whitespace-only reasoning counted as thinking: %#v", sig)
			}
		})
	}
}

func TestObserveQualityChunkAnthropicSignatureIsThinking(t *testing.T) {
	t.Parallel()
	short := qualityScanState{protocol: qualityProtocolAnthropic}
	ObserveQualityChunk(&short, []byte(sse(
		`data: {"type":"content_block_delta","delta":{"type":"signature_delta","signature":"gAAAA-cipher"}}`,
	)))
	if sig := short.signals(); sig.HasThinking || !sig.ReasoningStarted || sig.EncryptedBytes == 0 {
		t.Fatalf("short Anthropic signature stub must not count as thinking: %#v", sig)
	}

	cipher := strings.Repeat("A", defaultMinEncryptedBytes)
	state := qualityScanState{protocol: qualityProtocolAnthropic}
	ObserveQualityChunk(&state, []byte(sse(
		`data: {"type":"content_block_delta","delta":{"type":"signature_delta","signature":"`+cipher+`"}}`,
	)))
	if sig := state.signals(); !sig.HasThinking || !sig.ReasoningStarted {
		t.Fatalf("floor-sized Anthropic signature must count as encrypted thinking: %#v", sig)
	}
}

func TestObserveQualityChunkResponsesReasoningItem(t *testing.T) {
	t.Parallel()
	fake := qualityScanState{protocol: qualityProtocolResponses}
	ObserveQualityChunk(&fake, []byte(sse(
		`data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`,
		`data: {"type":"response.output_text.delta","delta":"`+strings.Repeat("hello ", 40)+`"}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"output_tokens":90,"output_tokens_details":{"reasoning_tokens":60}}}}`,
	)))
	fakeSig := fake.signals()
	if fakeSig.HasThinking {
		t.Fatalf("usage-only reasoning tokens must not count as thinking: %#v", fakeSig)
	}
	if !fakeSig.ReasoningStarted || fakeSig.ReasoningTokens != 60 {
		t.Fatalf("usage-only signals = %#v", fakeSig)
	}
	if ClassifyQualityHold(fakeSig, 32) != QualityWithhold {
		t.Fatalf("usage-only reasoning with no deltas must withhold: %#v", fakeSig)
	}

	real := qualityScanState{protocol: qualityProtocolResponses}
	ObserveQualityChunk(&real, []byte(sse(
		`data: {"type":"response.reasoning_summary_text.delta","delta":"plan the fix"}`,
		`data: {"type":"response.output_text.delta","delta":"hello"}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"output_tokens":90,"output_tokens_details":{"reasoning_tokens":60}}}}`,
	)))
	realSig := real.signals()
	if !realSig.HasThinking || realSig.ReasoningTokens != 60 {
		t.Fatalf("streamed summary must count as thinking: %#v", realSig)
	}
	if ClassifyQualityHold(realSig, 32) != QualityDeliver {
		t.Fatalf("streamed reasoning summary should deliver: %#v", realSig)
	}

	short := qualityScanState{protocol: qualityProtocolResponses}
	ObserveQualityChunk(&short, []byte(sse(
		`data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`,
		`data: {"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"gAAAA-cipher"}}`,
		`data: {"type":"response.output_text.delta","delta":"`+strings.Repeat("word ", 40)+`"}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"output_tokens":90,"output_tokens_details":{"reasoning_tokens":60}}}}`,
	)))
	shortSig := short.signals()
	if shortSig.HasThinking {
		t.Fatalf("short encrypted stub must not count as thinking: %#v", shortSig)
	}
	if shortSig.EncryptedBytes == 0 || !shortSig.ReasoningStarted || shortSig.ReasoningTokens != 60 {
		t.Fatalf("short encrypted stub signals = %#v", shortSig)
	}
	if ClassifyQualityHold(shortSig, 32) != QualityWithhold {
		t.Fatalf("short encrypted stub must withhold: %#v", shortSig)
	}

	cipher := strings.Repeat("A", defaultMinEncryptedBytes*8)
	encrypted := qualityScanState{protocol: qualityProtocolResponses}
	ObserveQualityChunk(&encrypted, []byte(sse(
		`data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`,
		`data: {"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"`+cipher+`"}}`,
		`data: {"type":"response.output_text.delta","delta":"`+strings.Repeat("word ", 40)+`"}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"output_tokens":90,"output_tokens_details":{"reasoning_tokens":60}}}}`,
	)))
	encSig := encrypted.signals()
	if !encSig.HasThinking || encSig.ReasoningTokens != 60 || encSig.EncryptedBytes < defaultMinEncryptedBytes {
		t.Fatalf("encrypted reasoning item must count as thinking: %#v", encSig)
	}
	if ClassifyQualityHold(encSig, 32) != QualityDeliver {
		t.Fatalf("encrypted thinking should deliver: %#v", encSig)
	}

	floorCipher := strings.Repeat("A", defaultMinEncryptedBytes)
	undersized := qualityScanState{protocol: qualityProtocolResponses}
	ObserveQualityChunk(&undersized, []byte(sse(
		`data: {"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"`+floorCipher+`"}}`,
		`data: {"type":"response.output_text.delta","delta":"`+strings.Repeat("word ", 40)+`"}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"output_tokens":1200,"output_tokens_details":{"reasoning_tokens":1000}}}}`,
	)))
	underSig := undersized.signals()
	if underSig.HasThinking {
		t.Fatalf("256B cipher must not satisfy 1000 reasoning tokens: %#v", underSig)
	}
	if ClassifyQualityHold(underSig, 32) != QualityWithhold {
		t.Fatalf("undersized cipher vs usage must withhold: %#v", underSig)
	}
}

func TestObserveQualityChunkEmptyReasoningStubIsNotThinking(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("word ", 40)
	chat := qualityScanState{protocol: qualityProtocolChat}
	ObserveQualityChunk(&chat, []byte(sse(
		": grok2api-reasoning-start",
		`data: {"choices":[{"delta":{"content":"`+content+`"}}]}`,
		`data: {"usage":{"completion_tokens":45,"completion_tokens_details":{"reasoning_tokens":0}}}`,
		"data: [DONE]",
	)))
	chatSig := chat.signals()
	if chatSig.HasThinking {
		t.Fatalf("chat SSE stub must not count as thinking: %#v", chatSig)
	}
	if !chatSig.ReasoningStarted || !chatSig.Terminal || chatSig.ReasoningTokens != 0 {
		t.Fatalf("chat stub signals = %#v", chatSig)
	}
	if ClassifyQualityHold(chatSig, 32) != QualityWithhold {
		t.Fatalf("chat stub + 0 reasoning must withhold, got %s (%#v)", ClassifyQualityHold(chatSig, 32), chatSig)
	}

	mid := qualityScanState{protocol: qualityProtocolChat}
	ObserveQualityChunk(&mid, []byte(sse(
		": grok2api-reasoning-start",
		`data: {"choices":[{"delta":{"content":"`+content+`"}}]}`,
	)))
	midSig := mid.signals()
	if midSig.HasThinking || !midSig.ReasoningStarted || midSig.Terminal {
		t.Fatalf("midstream stub signals = %#v", midSig)
	}
	if ClassifyQualityHold(midSig, 32) != QualityWait {
		t.Fatalf("midstream stub must wait for usage, got %s (%#v)", ClassifyQualityHold(midSig, 32), midSig)
	}

	responses := qualityScanState{protocol: qualityProtocolResponses}
	ObserveQualityChunk(&responses, []byte(sse(
		`data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`,
		`data: {"type":"response.output_text.delta","delta":"`+content+`"}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"output_tokens":45,"output_tokens_details":{"reasoning_tokens":0}}}}`,
	)))
	respSig := responses.signals()
	if respSig.HasThinking {
		t.Fatalf("empty responses reasoning item must not count as thinking: %#v", respSig)
	}
	if ClassifyQualityHold(respSig, 32) != QualityWithhold {
		t.Fatalf("empty reasoning item + 0 tokens must withhold, got %s (%#v)", ClassifyQualityHold(respSig, 32), respSig)
	}
}

func TestPeekQualityStreamThinkingDeliversRemainder(t *testing.T) {
	t.Parallel()
	body := io.NopCloser(strings.NewReader(sse(
		`data: {"choices":[{"delta":{"thinking_content":"think"}}]}`,
		`data: {"choices":[{"delta":{"content":"answer after think"}}]}`,
		"data: [DONE]",
	)))
	replay, verdict, _, _, err := peekQualityStream(context.Background(), body, qualityProtocolChat, QualityRetryRuntime{MinOutputTokens: 32, HoldTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	if verdict != QualityDeliver {
		t.Fatalf("verdict=%s", verdict)
	}
	got, _ := io.ReadAll(replay)
	if !strings.Contains(string(got), "answer after think") || !strings.Contains(string(got), "thinking_content") {
		t.Fatalf("replay lost frames: %s", got)
	}
}

func TestPeekQualityStreamCipherBurstWaitsAcrossEventSplits(t *testing.T) {
	t.Parallel()
	cipher := strings.Repeat("A", 400)
	createdAndCipher := sse(
		`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-4.6","usage":{"output_tokens":80,"output_tokens_details":{"reasoning_tokens":80}}}}`,
		`data: {"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"`+cipher+`"}}`,
	)
	visible := sse(`data: {"type":"response.output_text.delta","delta":"你好"}`)
	completed := sse(`data: {"type":"response.completed","response":{"id":"resp_1","model":"grok-4.6","usage":{"output_tokens":82,"output_tokens_details":{"reasoning_tokens":80}}}}`)
	cfg := QualityRetryRuntime{MinOutputTokens: 8, HoldTimeout: 2 * time.Second}

	coalesced, coalescedVerdict, _, _, err := peekQualityStream(
		context.Background(), io.NopCloser(strings.NewReader(createdAndCipher+visible+completed)), qualityProtocolResponses, cfg,
	)
	if err != nil {
		t.Fatal(err)
	}
	_ = coalesced.Close()
	if coalescedVerdict != QualityWithhold {
		t.Fatalf("coalesced verdict = %s, want withhold", coalescedVerdict)
	}

	reader, writer := io.Pipe()
	type peekResult struct {
		replay  io.ReadCloser
		verdict QualityVerdict
		err     error
	}
	done := make(chan peekResult, 1)
	go func() {
		replay, verdict, _, _, peekErr := peekQualityStream(context.Background(), reader, qualityProtocolResponses, cfg)
		done <- peekResult{replay: replay, verdict: verdict, err: peekErr}
	}()
	if _, err := io.WriteString(writer, createdAndCipher); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.replay != nil {
			_ = result.replay.Close()
		}
		t.Fatalf("ciphertext-only partial stream returned early: verdict=%s err=%v", result.verdict, result.err)
	case <-time.After(40 * time.Millisecond):
	}
	if _, err := io.WriteString(writer, visible); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.replay != nil {
			_ = result.replay.Close()
		}
		t.Fatalf("non-terminal burst was finalized early: verdict=%s err=%v", result.verdict, result.err)
	case <-time.After(40 * time.Millisecond):
	}
	if _, err := io.WriteString(writer, completed); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.replay != nil {
			_ = result.replay.Close()
		}
		if result.err != nil || result.verdict != QualityWithhold {
			t.Fatalf("split verdict=%s err=%v, want withhold", result.verdict, result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("split terminal stream did not finish")
	}
}

func TestPeekQualityStreamWithholdsNoThinkEnough(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("abcd", 16) // 64 runes → 16 tokens... need 32 tokens = 128 runes
	content = strings.Repeat("abcd", 40)  // 160 runes → 40 tokens
	body := io.NopCloser(strings.NewReader(sse(
		`data: {"choices":[{"delta":{"content":"`+content+`"}}]}`,
		`data: {"usage":{"completion_tokens":40,"completion_tokens_details":{"reasoning_tokens":0}}}`,
		"data: [DONE]",
	)))
	replay, verdict, usage, _, err := peekQualityStream(context.Background(), body, qualityProtocolChat, QualityRetryRuntime{MinOutputTokens: 32, HoldTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	if verdict != QualityWithhold {
		t.Fatalf("verdict=%s usage=%#v", verdict, usage)
	}
	if usage.ReasoningTokens != 0 || usage.OutputTokens < 32 {
		t.Fatalf("usage=%#v", usage)
	}
}

func TestPeekThenDecideQualityRetryBounded(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("abcd", 40)
	fixture := sse(
		`data: {"choices":[{"delta":{"content":"`+content+`"}}]}`,
		`data: {"usage":{"completion_tokens":40,"completion_tokens_details":{"reasoning_tokens":0}}}`,
		"data: [DONE]",
	)
	cfg := QualityRetryRuntime{MinOutputTokens: 32, MaxAttempts: 2, OnExhausted: qualityRetryFailOpen, HoldTimeout: time.Second}

	replay, verdict, usage, _, err := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader(fixture)), qualityProtocolChat, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	if verdict != QualityWithhold {
		t.Fatalf("first peek verdict=%s usage=%#v", verdict, usage)
	}
	if got := DecideQualityRetry(verdict, 0, cfg.MaxAttempts, cfg.OnExhausted); got != QualityActionRetry {
		t.Fatalf("first withhold action=%s", got)
	}

	replay2, verdict2, _, _, err := peekQualityStream(context.Background(), io.NopCloser(strings.NewReader(fixture)), qualityProtocolChat, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer replay2.Close()
	if verdict2 != QualityWithhold {
		t.Fatalf("second peek verdict=%s", verdict2)
	}
	action2 := DecideQualityRetry(verdict2, 1, cfg.MaxAttempts, cfg.OnExhausted)
	action2 = BoundQualityRetry(action2, false, cfg.OnExhausted)
	if action2 != QualityActionDeliverLast {
		t.Fatalf("second withhold fail-open action=%s", action2)
	}
	got, _ := io.ReadAll(replay2)
	if !strings.Contains(string(got), content) {
		t.Fatalf("fail-open must still deliver the last body, got %q", got)
	}
}

func TestPeekQualityStreamShortDelivers(t *testing.T) {
	t.Parallel()
	body := io.NopCloser(strings.NewReader(sse(
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		"data: [DONE]",
	)))
	replay, verdict, _, _, err := peekQualityStream(context.Background(), body, qualityProtocolChat, QualityRetryRuntime{MinOutputTokens: 32})
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	if verdict != QualityDeliver {
		t.Fatalf("short verdict=%s", verdict)
	}
}

func TestPeekQualityStreamHoldTimeoutInterruptsBlockedReadAndPreservesRemainder(t *testing.T) {
	t.Parallel()
	reader, writer := io.Pipe()
	first := sse(`data: {"choices":[{"delta":{"content":"hi"}}]}`)
	second := sse(`data: {"choices":[{"delta":{"content":" after timeout"}}]}`, "data: [DONE]")
	writeErr := make(chan error, 1)
	continueWrite := make(chan struct{})
	go func() {
		if _, err := io.WriteString(writer, first); err != nil {
			writeErr <- err
			return
		}
		select {
		case <-continueWrite:
		case <-time.After(500 * time.Millisecond):
		}
		if _, err := io.WriteString(writer, second); err != nil {
			writeErr <- err
			return
		}
		writeErr <- writer.Close()
	}()

	started := time.Now()
	replay, verdict, _, _, err := peekQualityStream(context.Background(), reader, qualityProtocolChat, QualityRetryRuntime{
		MinOutputTokens: 32,
		HoldTimeout:     30 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	close(continueWrite)
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond || elapsed > 200*time.Millisecond {
		t.Fatalf("peek returned after %s, want the 30ms hold timeout", elapsed)
	}
	if verdict != QualityDeliver {
		t.Fatalf("short timed-out response verdict = %s", verdict)
	}
	body, err := io.ReadAll(replay)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}
	if got := string(body); !strings.Contains(got, "hi") || !strings.Contains(got, "after timeout") {
		t.Fatalf("replay lost streamed bytes: %q", got)
	}
}

func TestPeekQualityStreamHoldTimeoutDeliversStartedReasoningAndPreservesLateEvidence(t *testing.T) {
	t.Parallel()
	reader, writer := io.Pipe()
	content := strings.Repeat("abcd", 40)
	first := sse(
		`data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`,
		`data: {"type":"response.output_text.delta","delta":"`+content+`"}`,
	)
	second := sse(
		`data: {"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"late-proof"}}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"output_tokens":40,"output_tokens_details":{"reasoning_tokens":20}}}}`,
	)
	writeErr := make(chan error, 1)
	continueWrite := make(chan struct{})
	go func() {
		if _, err := io.WriteString(writer, first); err != nil {
			writeErr <- err
			return
		}
		select {
		case <-continueWrite:
		case <-time.After(500 * time.Millisecond):
		}
		if _, err := io.WriteString(writer, second); err != nil {
			writeErr <- err
			return
		}
		writeErr <- writer.Close()
	}()

	started := time.Now()
	replay, verdict, _, _, err := peekQualityStream(context.Background(), reader, qualityProtocolResponses, QualityRetryRuntime{
		MinOutputTokens: 8,
		HoldTimeout:     30 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond || elapsed > 200*time.Millisecond {
		t.Fatalf("peek returned after %s, want the 30ms hold timeout", elapsed)
	}
	if verdict != QualityWithhold {
		t.Fatalf("started reasoning stub at hold timeout verdict = %s, want withhold", verdict)
	}
	close(continueWrite)
	body, err := io.ReadAll(replay)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}
	if got := string(body); !strings.Contains(got, content) || !strings.Contains(got, `"encrypted_content":"late-proof"`) {
		t.Fatalf("replay lost late reasoning evidence: %q", got)
	}
}

func TestPeekQualityStreamHoldTimeoutEmptyDoesNotFailOpen(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancelCause(context.Background())
	reader, writer := io.Pipe()
	defer writer.Close()
	done := make(chan struct{})
	var verdict QualityVerdict
	var peekErr error
	go func() {
		defer close(done)
		_, verdict, _, _, peekErr = peekQualityStream(ctx, reader, qualityProtocolChat, QualityRetryRuntime{
			MinOutputTokens: 32,
			HoldTimeout:     20 * time.Millisecond,
		})
	}()
	select {
	case <-done:
		t.Fatal("empty hold timeout must keep reading, not fail-open")
	case <-time.After(50 * time.Millisecond):
	}
	cancel(neterrorpkg.ErrUpstreamStreamIdleTimeout)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("peekQualityStream did not return after idle cancel")
	}
	if !neterrorpkg.IsUpstreamStreamIdleTimeout(peekErr) {
		t.Fatalf("peekErr = %v, want idle timeout", peekErr)
	}
	if verdict != QualityWait {
		t.Fatalf("verdict=%s, want wait so the loop does not fail-open", verdict)
	}
}

func TestPeekQualityStreamHoldTimeoutStubOnlyDoesNotFailOpen(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancelCause(context.Background())
	reader, writer := io.Pipe()
	defer writer.Close()
	writeDone := make(chan error, 1)
	go func() {
		_, err := io.WriteString(writer, sse(": grok2api-reasoning-start"))
		writeDone <- err
	}()
	done := make(chan struct{})
	var verdict QualityVerdict
	var peekErr error
	go func() {
		defer close(done)
		_, verdict, _, _, peekErr = peekQualityStream(ctx, reader, qualityProtocolChat, QualityRetryRuntime{
			MinOutputTokens: 8,
			HoldTimeout:     20 * time.Millisecond,
		})
	}()
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		t.Fatal("stub-only hold timeout must keep reading, not release an empty stream")
	case <-time.After(50 * time.Millisecond):
	}
	cancel(neterrorpkg.ErrUpstreamStreamIdleTimeout)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("peekQualityStream did not return after stub-only idle cancel")
	}
	if !neterrorpkg.IsUpstreamStreamIdleTimeout(peekErr) {
		t.Fatalf("peekErr = %v, want idle timeout", peekErr)
	}
	if verdict != QualityWait {
		t.Fatalf("verdict=%s, want wait so the loop retries as transport", verdict)
	}
}

type qualityOpenPeekResult struct {
	replay  io.ReadCloser
	verdict QualityVerdict
	err     error
}

func peekOpenQualityStreamForTest(t *testing.T, protocol, stream string) qualityOpenPeekResult {
	t.Helper()
	reader, writer := io.Pipe()
	done := make(chan qualityOpenPeekResult, 1)
	go func() {
		replay, verdict, _, _, err := peekQualityStream(
			context.Background(), reader, protocol,
			QualityRetryRuntime{MinOutputTokens: 32, HoldTimeout: 2 * time.Second},
		)
		done <- qualityOpenPeekResult{replay: replay, verdict: verdict, err: err}
	}()
	if _, err := io.WriteString(writer, stream); err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	select {
	case result := <-done:
		_ = writer.Close()
		return result
	case <-time.After(500 * time.Millisecond):
		_ = writer.CloseWithError(errors.New("test terminal stream timeout"))
		result := <-done
		if result.replay != nil {
			_ = result.replay.Close()
		}
		t.Fatal("terminal stream waited for hold/idle instead of finishing while the connection remained open")
		return qualityOpenPeekResult{}
	}
}

func TestPeekQualityStreamEmptyCompletedRetriesWithoutIdle(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		protocol string
		stream   string
	}{
		{
			name:     "responses completed",
			protocol: qualityProtocolResponses,
			stream: sse(
				`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"output_tokens":0}}}`,
			),
		},
		{
			name:     "responses completed with empty text node",
			protocol: qualityProtocolResponses,
			stream: sse(
				`data: {"type":"response.completed","response":{"id":"resp_1","output":[{"type":"message","content":[{"type":"output_text","text":""}]}],"usage":{"output_tokens":0}}}`,
			),
		},
		{name: "chat done", protocol: qualityProtocolChat, stream: sse("data: [DONE]")},
		{
			name:     "chat finish reason",
			protocol: qualityProtocolChat,
			stream:   sse(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`),
		},
		{name: "anthropic message stop", protocol: qualityProtocolAnthropic, stream: sse(`data: {"type":"message_stop"}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := peekOpenQualityStreamForTest(t, test.protocol, test.stream)
			if result.replay != nil {
				defer result.replay.Close()
			}
			if !errors.Is(result.err, errQualityEmptyStream) {
				t.Fatalf("peek error = %v, want empty stream", result.err)
			}
			if result.verdict != QualityWait {
				t.Fatalf("verdict = %s, want wait so the attempt loop retries as transport", result.verdict)
			}
		})
	}
}

func TestPeekQualityStreamTerminalSemanticOutputIsNotEmpty(t *testing.T) {
	t.Parallel()
	longText := strings.Repeat("word ", 40)
	shortText := strings.Repeat("a", 80)
	for _, test := range []struct {
		name        string
		protocol    string
		stream      string
		wantVerdict QualityVerdict
	}{
		{
			name:     "responses aggregate function call",
			protocol: qualityProtocolResponses,
			stream: sse(
				`data: {"type":"response.completed","response":{"id":"resp_1","output":[{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{}"}]}}`,
			),
			wantVerdict: QualityDeliver,
		},
		{
			name:     "responses streamed function call",
			protocol: qualityProtocolResponses,
			stream: sse(
				`data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_1","name":"read_file","arguments":""}}`,
				`data: {"type":"response.completed","response":{"id":"resp_1","output":[]}}`,
			),
			wantVerdict: QualityDeliver,
		},
		{
			name:     "chat tool call",
			protocol: qualityProtocolChat,
			stream: sse(
				`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
			),
			wantVerdict: QualityDeliver,
		},
		{
			name:     "anthropic tool use",
			protocol: qualityProtocolAnthropic,
			stream: sse(
				`data: {"type":"content_block_start","content_block":{"type":"tool_use","id":"tool_1","name":"read_file","input":{}}}`,
				`data: {"type":"message_stop"}`,
			),
			wantVerdict: QualityDeliver,
		},
		{
			name:     "responses aggregate long text",
			protocol: qualityProtocolResponses,
			stream: sse(
				`data: {"type":"response.completed","response":{"id":"resp_1","output":[{"type":"message","content":[{"type":"output_text","text":"` + longText + `"}]}]}}`,
			),
			wantVerdict: QualityWithhold,
		},
		{
			name:     "responses aggregate does not double count deltas",
			protocol: qualityProtocolResponses,
			stream: sse(
				`data: {"type":"response.output_text.delta","delta":"`+shortText+`"}`,
				`data: {"type":"response.completed","response":{"id":"resp_1","output":[{"type":"message","content":[{"type":"output_text","text":"`+shortText+`"}]}]}}`,
			),
			wantVerdict: QualityDeliver,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := peekOpenQualityStreamForTest(t, test.protocol, test.stream)
			if result.replay != nil {
				defer result.replay.Close()
			}
			if result.err != nil {
				t.Fatalf("peek error = %v, want semantic output", result.err)
			}
			if result.verdict != test.wantVerdict {
				t.Fatalf("verdict = %s, want %s", result.verdict, test.wantVerdict)
			}
		})
	}
}

func TestPeekQualityStreamEmptyEOFRequestsAnotherAccount(t *testing.T) {
	t.Parallel()
	replay, verdict, _, _, err := peekQualityStream(
		context.Background(),
		io.NopCloser(strings.NewReader("")),
		qualityProtocolResponses,
		QualityRetryRuntime{MinOutputTokens: 32, HoldTimeout: time.Second},
	)
	if replay != nil {
		defer replay.Close()
	}
	if !errors.Is(err, errQualityEmptyStream) {
		t.Fatalf("peek error = %v, want empty stream", err)
	}
	if verdict != QualityWait {
		t.Fatalf("verdict = %s, want wait", verdict)
	}
}

func TestPeekQualityStreamProcessesUnterminatedFinalEvent(t *testing.T) {
	t.Parallel()
	body := io.NopCloser(strings.NewReader(`data: {"type":"response.output_text.delta","delta":"ok"}`))
	replay, verdict, _, _, err := peekQualityStream(
		context.Background(), body, qualityProtocolResponses,
		QualityRetryRuntime{MinOutputTokens: 32, HoldTimeout: time.Second},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	if verdict != QualityDeliver {
		t.Fatalf("verdict = %s, want deliver for a real short response", verdict)
	}
}

func TestQualityPeekAbortErrorPrefersIdleCause(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(neterrorpkg.ErrUpstreamStreamIdleTimeout)
	got := qualityPeekAbortError(ctx, context.Canceled)
	if !neterrorpkg.IsUpstreamStreamIdleTimeout(got) {
		t.Fatalf("abort error = %v, want idle timeout", got)
	}
	if isClientRequestCancel(ctx, got) {
		t.Fatal("idle timeout must not look like a client cancel")
	}
	plain, plainCancel := context.WithCancel(context.Background())
	plainCancel()
	if !isClientRequestCancel(plain, context.Canceled) {
		t.Fatal("plain cancel must still be a client cancel")
	}
}

func TestShouldHoldQualityStreamGates(t *testing.T) {
	t.Parallel()
	cfg := QualityRetryRuntime{Enabled: true, MaxAttempts: 2, MinOutputTokens: 32}
	route := modeldomain.Route{Provider: accountdomain.ProviderBuild, UpstreamModel: "grok-4.6", PublicID: "grok-4.6"}
	input := Input{Streaming: true, PublicModel: "grok-4.6"}
	if !shouldHoldQualityStream(input, nil, route, audit.OperationChat, cfg) {
		t.Fatal("expected hold on thinking build chat")
	}
	off := cfg
	off.Enabled = false
	if shouldHoldQualityStream(input, nil, route, audit.OperationChat, off) {
		t.Fatal("disabled must not hold")
	}
	forced := input
	forced.ForcedEgressNodeID = 9
	if shouldHoldQualityStream(forced, nil, route, audit.OperationChat, cfg) {
		t.Fatal("forced egress must not hold")
	}
	owned := inferencedomain.ResponseOwnership{ResponseID: "r1", AccountID: 1}
	if !shouldHoldQualityStream(input, &owned, route, audit.OperationChat, cfg) {
		t.Fatal("pinned previous_response_id must still hold on missing thinking")
	}
	if shouldHoldQualityStream(input, nil, route, audit.OperationImage, cfg) {
		t.Fatal("image must not hold")
	}
	if shouldHoldQualityStream(input, nil, route, audit.OperationCompaction, cfg) {
		t.Fatal("compaction must not enter missing-thinking hold")
	}
	classified := input
	classified.skipQualityHold = true
	if shouldHoldQualityStream(classified, nil, route, audit.OperationResponses, cfg) {
		t.Fatal("explicit skipQualityHold must not hold")
	}
	tui := input
	tui.Body = []byte(`{"input":[{"role":"user","content":"` + tuiCompactionPrompt + `"}]}`)
	if shouldHoldQualityStream(tui, nil, route, audit.OperationResponses, cfg) {
		t.Fatal("tui compaction prompt must not enter missing-thinking hold")
	}
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "chat reasoning none", body: `{"reasoning_effort":"none"}`},
		{name: "responses reasoning none", body: `{"reasoning":{"effort":"none"}}`},
		{name: "messages thinking disabled", body: `{"thinking":{"type":"disabled"}}`},
		{name: "messages zero thinking budget", body: `{"thinking":{"type":"enabled","budget_tokens":0}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := input
			request.Body = []byte(test.body)
			if shouldHoldQualityStream(request, nil, route, audit.OperationChat, cfg) {
				t.Fatal("explicitly disabled reasoning must not be held")
			}
		})
	}
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "client tools schema", body: `{"tools":[{"type":"function","function":{"name":"charge"}}]}`},
		{name: "legacy functions schema", body: `{"functions":[{"name":"charge"}]}`},
		{name: "tui tools schema plus user input", body: `{"model":"grok-4.6","tools":[{"type":"function","name":"read_file"}],"input":[{"role":"user","content":"hello"}]}`},
		{name: "local shell declaration", body: `{"tools":[{"type":"local_shell"}]}`},
		{name: "local environment shell declaration", body: `{"tools":[{"type":"shell","environment":{"type":"local"}}]}`},
		{name: "apply patch declaration", body: `{"tools":[{"type":"apply_patch"}]}`},
		{name: "client namespace", body: `{"tools":[{"type":"namespace","name":"local","tools":[{"type":"function","name":"read_file"}]}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := input
			request.Body = []byte(test.body)
			if !shouldHoldQualityStream(request, nil, route, audit.OperationChat, cfg) {
				t.Fatal("tools schema alone must still hold so TUI thinking turns are classified")
			}
		})
	}
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "function_call_output", body: `{"input":[{"type":"function_call_output","call_id":"call_1","output":"done"}]}`},
		{name: "tool_result", body: `{"input":[{"type":"tool_result","tool_use_id":"tool_1","content":"done"}]}`},
		{name: "role tool", body: `{"messages":[{"role":"tool","content":"done"}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := input
			request.Body = []byte(test.body)
			if !shouldHoldQualityStream(request, nil, route, audit.OperationChat, cfg) {
				t.Fatal("in-flight tool results must still be held so 0-thinking agent turns are classified")
			}
		})
	}
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "responses web search", body: `{"tools":[{"type":"web_search"}]}`},
		{name: "versioned web search", body: `{"tools":[{"type":"web_search_2025_08_26"}]}`},
		{name: "x search", body: `{"tools":[{"type":"x_search"}]}`},
		{name: "image generation", body: `{"tools":[{"type":"image_generation"}]}`},
		{name: "file search", body: `{"tools":[{"type":"file_search"}]}`},
		{name: "collections search", body: `{"tools":[{"type":"collections_search"}]}`},
		{name: "code execution", body: `{"tools":[{"type":"code_execution"}]}`},
		{name: "code interpreter", body: `{"tools":[{"type":"code_interpreter"}]}`},
		{name: "hosted shell", body: `{"tools":[{"type":"shell","environment":{"type":"container_auto"}}]}`},
		{name: "future native tool skips replay", body: `{"tools":[{"type":"future_server_tool"}]}`},
		{name: "remote mcp", body: `{"tools":[{"type":"mcp","server_url":"https://example.com"}]}`},
		{name: "mixed client and hosted", body: `{"tools":[{"type":"function","name":"read_file"},{"type":"web_search"}]}`},
		{name: "chat web search options", body: `{"web_search_options":{}}`},
		{name: "messages web search", body: `{"tools":[{"type":"web_search_20250305","name":"web_search"}]}`},
		{name: "messages mcp servers", body: `{"mcp_servers":[{"type":"url","url":"https://example.com"}]}`},
		{name: "deferred hosted tool", body: `{"input":[{"type":"additional_tools","tools":[{"type":"mcp","server_url":"https://example.com"}]}]}`},
		{name: "nested hosted tool", body: `{"tools":[{"type":"namespace","name":"remote","tools":[{"type":"web_search"}]}]}`},
		{name: "after local tool with hosted declaration", body: `{"tools":[{"type":"web_search"}],"input":[{"type":"function_call_output","call_id":"call_1","output":"done"}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := input
			request.Body = []byte(test.body)
			if !qualityRequestHasReplayUnsafeHostedTools(request.Body) {
				t.Fatal("fixture must be classified as replay-unsafe hosted tooling")
			}
			if !shouldHoldQualityStream(request, nil, route, audit.OperationChat, cfg) {
				t.Fatal("hosted tools must still hold; TUI always declares them")
			}
		})
	}
	for _, body := range []string{
		`{"tools":[{"type":"function","name":"read_file","parameters":{"type":"object","properties":{"tools":{"type":"array"}}}}]}`,
		`{"mcp_servers":[]}`,
		`{"web_search_options":null}`,
	} {
		if qualityRequestHasReplayUnsafeHostedTools([]byte(body)) {
			t.Fatalf("safe or empty tool metadata classified as hosted: %s", body)
		}
	}
	toolCache := input
	toolCache.Body = []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	toolCache.AllowClientToolCacheRoute = true
	if !shouldHoldQualityStream(toolCache, nil, route, audit.OperationChat, cfg) {
		t.Fatal("client identity/cache compatibility alone must not disable the hold")
	}
}

func TestCanReplayQualityHoldAcrossAccounts(t *testing.T) {
	t.Parallel()
	plain := Input{Body: []byte(`{"input":"hello"}`)}
	if !canReplayQualityHoldAcrossAccounts(plain, nil) {
		t.Fatal("stateless text request should allow account retry")
	}
	owned := inferencedomain.ResponseOwnership{ResponseID: "resp_1", AccountID: 1}
	if canReplayQualityHoldAcrossAccounts(plain, &owned) {
		t.Fatal("stored response must remain pinned to its owner account")
	}
	hosted := Input{Body: []byte(`{"tools":[{"type":"web_search"}],"input":"hello"}`)}
	if canReplayQualityHoldAcrossAccounts(hosted, nil) {
		t.Fatal("hosted tool must not be replayed across accounts")
	}
	clientTool := Input{Body: []byte(`{"tools":[{"type":"function","name":"read_file"}],"input":"hello"}`)}
	if !canReplayQualityHoldAcrossAccounts(clientTool, nil) {
		t.Fatal("client-executed tool declaration is safe to retry before delivery")
	}
}

func TestAttemptLoopQualityHoldPreservesReplaySafety(t *testing.T) {
	tests := []struct {
		name        string
		previous    bool
		body        string
		onExhausted string
		wantError   bool
		empty       bool
		wantMissing bool
	}{
		{
			name:        "previous response remains account bound",
			previous:    true,
			body:        `{"model":"grok-4.6","previous_response_id":"resp-root","input":"continue","stream":true}`,
			onExhausted: qualityRetryFailClosed,
			wantError:   true,
			wantMissing: true,
		},
		{
			name:        "hosted tool fail closed executes once",
			body:        `{"model":"grok-4.6","tools":[{"type":"web_search"}],"input":"search","stream":true}`,
			onExhausted: qualityRetryFailClosed,
			wantError:   true,
			wantMissing: true,
		},
		{
			name:        "hosted tool fail open delivers same attempt",
			body:        `{"model":"grok-4.6","tools":[{"type":"web_search"}],"input":"search","stream":true}`,
			onExhausted: qualityRetryFailOpen,
			wantMissing: true,
		},
		{
			name:        "hosted tool empty stream executes once",
			body:        `{"model":"grok-4.6","tools":[{"type":"web_search"}],"input":"search","stream":true}`,
			onExhausted: qualityRetryFailClosed,
			wantError:   true,
			empty:       true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "quality-replay-safety.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			if err := database.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			accountRepo := relational.NewAccountRepository(database)
			modelRepo := relational.NewModelRepository(database)
			auditRepo := relational.NewAuditRepository(database)
			responseRepo := relational.NewResponseRepository(database)
			keyRepo := relational.NewClientKeyRepository(database)

			credentials := make([]accountdomain.Credential, 0, 2)
			for index, name := range []string{"quality-safe-a", "quality-safe-b"} {
				credential, _, createErr := accountRepo.UpsertByIdentity(ctx, accountdomain.Credential{
					Provider: accountdomain.ProviderBuild, Name: name, SourceKey: name,
					EncryptedAccessToken: name, EncryptedRefreshToken: "refresh-" + name,
					ExpiresAt: time.Now().Add(time.Hour), Enabled: true,
					AuthStatus: accountdomain.AuthStatusActive, Priority: 200 - index, MaxConcurrent: 1,
				})
				if createErr != nil {
					t.Fatal(createErr)
				}
				credentials = append(credentials, credential)
			}
			if err := modelRepo.UpsertDiscovered(ctx, accountdomain.ProviderBuild, []string{"grok-4.6"}); err != nil {
				t.Fatal(err)
			}
			for _, credential := range credentials {
				if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-4.6"}, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
			}
			key, err := keyRepo.Create(ctx, clientkey.Key{
				Name: "quality-safe-key", Prefix: "qsafe", SecretHash: strings.Repeat("8", 64),
				EncryptedSecret: "encrypted", Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
			})
			if err != nil {
				t.Fatal(err)
			}
			if test.previous {
				now := time.Now().UTC()
				if err := responseRepo.Save(ctx, inferencedomain.ResponseOwnership{
					ResponseID: "resp-root", AccountID: credentials[0].ID, ClientKeyID: key.ID,
					Provider: accountdomain.ProviderBuild, PromptCacheKey: "session-root",
					CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
				}); err != nil {
					t.Fatal(err)
				}
			}

			noThinking := sse(
				`data: {"type":"response.created","response":{"id":"resp-bad","model":"grok-4.6"}}`,
				`data: {"type":"response.output_text.delta","delta":"`+strings.Repeat("bad ", 20)+`"}`,
				`data: {"type":"response.completed","response":{"id":"resp-bad","model":"grok-4.6","usage":{"output_tokens":40,"output_tokens_details":{"reasoning_tokens":0}}}}`,
			)
			thinking := sse(
				`data: {"type":"response.created","response":{"id":"resp-good","model":"grok-4.6"}}`,
				`data: {"type":"response.reasoning_summary_text.delta","delta":"real reasoning"}`,
				`data: {"type":"response.output_text.delta","delta":"good answer"}`,
				`data: {"type":"response.completed","response":{"id":"resp-good","model":"grok-4.6","usage":{"output_tokens":40,"output_tokens_details":{"reasoning_tokens":20}}}}`,
			)
			firstBody := noThinking
			if test.empty {
				firstBody = ""
			}
			adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{
				credentials[0].ID: {{status: http.StatusOK, body: firstBody}},
				credentials[1].ID: {{status: http.StatusOK, body: thinking}},
			}}
			registry := provider.NewRegistry(adapter)
			sticky := memory.NewStickyStore()
			accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
			selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
			service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 3)
			service.UpdateQualityRetry(QualityRetryRuntime{
				Enabled: true, MaxAttempts: 2, MinOutputTokens: 8,
				OnExhausted: test.onExhausted, HoldTimeout: time.Second,
			})

			input := Input{
				RequestID: "req-quality-safe", ClientKey: key, PublicModel: "grok-4.6",
				Streaming: true, Body: []byte(test.body),
			}
			if test.previous {
				input.PreviousResponseID = "resp-root"
			}
			result, requestErr := service.CreateResponse(ctx, input)
			var responseBody []byte
			if result != nil {
				responseBody, _ = io.ReadAll(result.Body)
				result.Finalize(Usage{}, "", "")
				_ = result.Body.Close()
			}
			if (requestErr != nil) != test.wantError {
				t.Fatalf("request error = %v, wantError=%t", requestErr, test.wantError)
			}
			if !test.wantError && !strings.Contains(string(responseBody), "bad ") {
				t.Fatalf("fail-open did not deliver the held first attempt: %s", responseBody)
			}
			attempts := adapter.Attempts()
			if len(attempts) != 1 || attempts[0] != credentials[0].ID {
				t.Fatalf("request crossed its replay-safety boundary, attempts=%v", attempts)
			}
			cooled, err := accountRepo.Get(ctx, credentials[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if cooled.CooldownUntil == nil || (test.wantMissing && cooled.LastError != lastErrorMissingThinking) {
				t.Fatalf("degraded account was not penalized: %#v", cooled)
			}
		})
	}
}

func TestAttemptLoopQualityHold(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "quality-hold-loop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	responseRepo := relational.NewResponseRepository(database)
	keyRepo := relational.NewClientKeyRepository(database)

	credentials := make([]accountdomain.Credential, 0, 3)
	for index, name := range []string{"quality-empty", "quality-no-think", "quality-thinking"} {
		credential, _, createErr := accountRepo.UpsertByIdentity(ctx, accountdomain.Credential{
			Provider: accountdomain.ProviderBuild, Name: name, SourceKey: name, EncryptedAccessToken: name,
			EncryptedRefreshToken: "refresh-" + name, ExpiresAt: time.Now().Add(time.Hour),
			Enabled: true, AuthStatus: accountdomain.AuthStatusActive, Priority: 200 - index, MaxConcurrent: 1,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		credentials = append(credentials, credential)
	}
	if err := modelRepo.UpsertDiscovered(ctx, accountdomain.ProviderBuild, []string{"grok-4.6"}); err != nil {
		t.Fatal(err)
	}
	for _, credential := range credentials {
		if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-4.6"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "quality-loop-key", Prefix: "qhold", SecretHash: strings.Repeat("f", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}

	noThink := sse(
		`data: {"choices":[{"delta":{"content":"`+strings.Repeat("abcd", 40)+`"}}]}`,
		`data: {"usage":{"completion_tokens":40,"completion_tokens_details":{"reasoning_tokens":0}}}`,
		"data: [DONE]",
	)
	thinking := sse(
		`data: {"choices":[{"delta":{"thinking_content":"plan the game"}}]}`,
		`data: {"choices":[{"delta":{"content":"good game after retry"}}]}`,
		`data: {"usage":{"completion_tokens":80,"completion_tokens_details":{"reasoning_tokens":40}}}`,
		"data: [DONE]",
	)
	adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{
		credentials[0].ID: {{status: http.StatusOK, body: ""}},
		credentials[1].ID: {{status: http.StatusOK, body: noThink}},
		credentials[2].ID: {{status: http.StatusOK, body: thinking}},
	}}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 3)
	service.UpdateQualityRetry(QualityRetryRuntime{Enabled: true, MaxAttempts: 3, MinOutputTokens: 32, OnExhausted: qualityRetryFailOpen, HoldTimeout: time.Second})

	result, err := service.CreateChatCompletion(ctx, Input{
		RequestID: "req-quality-hold", ClientKey: clientKey, PublicModel: "grok-4.6", Streaming: true,
		Body: []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"write a game"}],"stream":true}`),
	})
	if err != nil {
		t.Fatalf("attempt loop should deliver after withhold retry, err=%v", err)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", result.StatusCode)
	}
	body, _ := io.ReadAll(result.Body)
	result.Finalize(Usage{Reported: true, OutputTokens: 80, ReasoningTokens: 40}, "chat-ok", "")
	_ = result.Body.Close()
	if !strings.Contains(string(body), "good game after retry") || !strings.Contains(string(body), "thinking_content") {
		t.Fatalf("client must receive the second attempt body, got %s", body)
	}
	if strings.Contains(string(body), strings.Repeat("abcd", 40)) {
		t.Fatal("first no-think body must not be delivered")
	}
	attempts := adapter.Attempts()
	if len(attempts) != 3 || attempts[0] != credentials[0].ID || attempts[1] != credentials[1].ID || attempts[2] != credentials[2].ID {
		t.Fatalf("expected empty+no-think account exclusion and retry, attempts=%#v", attempts)
	}
	emptyAccount, err := accountRepo.Get(ctx, credentials[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if emptyAccount.FailureCount != 1 || emptyAccount.CooldownUntil == nil {
		t.Fatalf("empty stream account was not cooled: %#v", emptyAccount)
	}
	if remaining := time.Until(*emptyAccount.CooldownUntil); remaining < 14*time.Minute || remaining > 15*time.Minute+time.Minute {
		t.Fatalf("empty stream cooldown = %s, want about 15m", remaining)
	}
	noThinkingAccount, err := accountRepo.Get(ctx, credentials[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !noThinkingAccount.Enabled || noThinkingAccount.LastError != lastErrorMissingThinking || noThinkingAccount.CooldownUntil == nil {
		t.Fatalf("missing-thinking account was not cooled: %#v", noThinkingAccount)
	}
	if remaining := time.Until(*noThinkingAccount.CooldownUntil); remaining < 11*time.Hour || remaining > 12*time.Hour+time.Minute {
		t.Fatalf("missing-thinking cooldown = %s, want about 12h", remaining)
	}
	logs, total, err := auditRepo.List(ctx, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	var degraded, delivered bool
	for _, rec := range logs {
		if rec.ErrorCode == ErrorQualityDegraded && rec.AccountID != nil && *rec.AccountID == credentials[1].ID {
			degraded = true
		}
		if rec.RequestID == "req-quality-hold" && rec.ErrorCode == "" && rec.StatusCode == http.StatusOK {
			delivered = true
		}
	}
	if !degraded {
		t.Fatalf("first withhold must write quality_degraded, audits=%d total=%d", len(logs), total)
	}
	if !delivered {
		t.Fatalf("final delivered attempt missing from audits, total=%d", total)
	}
}

func TestAttemptLoopQualityHoldFailOpenKeepsSingleAccountBody(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "quality-hold-single.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	responseRepo := relational.NewResponseRepository(database)
	keyRepo := relational.NewClientKeyRepository(database)

	credential, _, err := accountRepo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "quality-only", SourceKey: "quality-only",
		EncryptedAccessToken: "quality-only", EncryptedRefreshToken: "refresh-quality-only",
		ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
		Priority: 200, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertDiscovered(ctx, accountdomain.ProviderBuild, []string{"grok-4.6"}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-4.6"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "quality-single-key", Prefix: "qsingle", SecretHash: strings.Repeat("e", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}

	content := strings.Repeat("abcd", 40)
	noThink := sse(
		`data: {"choices":[{"delta":{"content":"`+content+`"}}]}`,
		`data: {"usage":{"completion_tokens":40,"completion_tokens_details":{"reasoning_tokens":0}}}`,
		"data: [DONE]",
	)
	adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{
		credential.ID: {{status: http.StatusOK, body: noThink}},
	}}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 999)
	service.UpdateQualityRetry(QualityRetryRuntime{
		Enabled: true, MaxAttempts: 6, MinOutputTokens: 32, OnExhausted: qualityRetryFailOpen, HoldTimeout: time.Second,
	})

	result, err := service.CreateChatCompletion(ctx, Input{
		RequestID: "req-quality-single", ClientKey: clientKey, PublicModel: "grok-4.6", Streaming: true,
		Body: []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"write a game"}],"stream":true}`),
	})
	if err != nil {
		t.Fatalf("fail-open should deliver the only account body, err=%v", err)
	}
	body, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatal(err)
	}
	result.Finalize(Usage{Reported: true, OutputTokens: 40}, "chat-single", "")
	_ = result.Body.Close()
	if !strings.Contains(string(body), content) {
		t.Fatalf("fail-open lost the held body: %s", body)
	}
	if attempts := adapter.Attempts(); len(attempts) != 1 || attempts[0] != credential.ID {
		t.Fatalf("single-account pool must not enter a fake retry, attempts=%#v", attempts)
	}
	cooled, err := accountRepo.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !cooled.Enabled || cooled.LastError != lastErrorMissingThinking || cooled.CooldownUntil == nil {
		t.Fatalf("delivered fail-open response must still cool the no-thinking account: %#v", cooled)
	}
}

func TestAttemptLoopQualityFailOpenFallbackAndTotalAttemptCap(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "quality-hold-fallback.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	responseRepo := relational.NewResponseRepository(database)
	keyRepo := relational.NewClientKeyRepository(database)

	credentials := make([]accountdomain.Credential, 0, 7)
	for index := 0; index < 7; index++ {
		name := fmt.Sprintf("quality-fallback-%d", index)
		credential, _, createErr := accountRepo.UpsertByIdentity(ctx, accountdomain.Credential{
			Provider: accountdomain.ProviderBuild, Name: name, SourceKey: name,
			EncryptedAccessToken: name, EncryptedRefreshToken: "refresh-" + name,
			ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
			Priority: 300 - index, MaxConcurrent: 1,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		credentials = append(credentials, credential)
	}
	if err := modelRepo.UpsertDiscovered(ctx, accountdomain.ProviderBuild, []string{"grok-4.6"}); err != nil {
		t.Fatal(err)
	}
	for _, credential := range credentials {
		if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-4.6"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "quality-fallback-key", Prefix: "qfallback", SecretHash: strings.Repeat("d", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}

	content := strings.Repeat("fallback", 24)
	responses := make(map[uint64][]scriptedBuildResponse, len(credentials))
	responses[credentials[0].ID] = []scriptedBuildResponse{{status: http.StatusOK, body: sse(
		`data: {"choices":[{"delta":{"content":"`+content+`"}}]}`,
		`data: {"usage":{"completion_tokens":48,"completion_tokens_details":{"reasoning_tokens":0}}}`,
		"data: [DONE]",
	)}}
	for _, credential := range credentials[1:] {
		responses[credential.ID] = []scriptedBuildResponse{{status: http.StatusInternalServerError, body: `{"error":"temporary"}`}}
	}
	adapter := &scriptedBuildAdapter{responses: responses}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 999)
	service.UpdateQualityRetry(QualityRetryRuntime{
		Enabled: true, MaxAttempts: 6, MinOutputTokens: 32, OnExhausted: qualityRetryFailOpen, HoldTimeout: time.Second,
	})

	result, err := service.CreateChatCompletion(ctx, Input{
		RequestID: "req-quality-fallback", ClientKey: clientKey, PublicModel: "grok-4.6", Streaming: true,
		Body: []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"write a game"}],"stream":true}`),
	})
	if err != nil {
		t.Fatalf("fail-open should return the retained response after later failures: %v", err)
	}
	body, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatal(err)
	}
	result.Finalize(Usage{Reported: true, OutputTokens: 48}, "chat-fallback", "")
	_ = result.Body.Close()
	if !strings.Contains(string(body), content) {
		t.Fatalf("retained fail-open response was lost: %s", body)
	}
	if attempts := adapter.Attempts(); len(attempts) != 6 || attempts[0] != credentials[0].ID {
		t.Fatalf("requestRetry must cap real account attempts at 6, attempts=%#v", attempts)
	}
	logs, _, err := auditRepo.List(ctx, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range logs {
		if record.ErrorCode == ErrorQualityDegraded && record.AccountID != nil && *record.AccountID == credentials[0].ID {
			t.Fatal("the delivered fallback must not be audited as a discarded quality attempt")
		}
	}
}

func TestNormalizeQualityRetryDefaults(t *testing.T) {
	t.Parallel()
	got := normalizeQualityRetry(QualityRetryRuntime{Enabled: true})
	if !got.Enabled || got.MaxAttempts != 6 || got.MinOutputTokens != 8 || got.OnExhausted != qualityRetryFailClosed || got.HoldTimeout != 30*time.Second || got.AccountCooldown != 12*time.Hour || got.IdleAccountCooldown != 15*time.Minute || got.MinEncryptedBytes != defaultMinEncryptedBytes || got.EncryptedBytesPerReasoningToken != defaultEncryptedBytesPerReasoningToken {
		t.Fatalf("defaults = %#v", got)
	}
}
