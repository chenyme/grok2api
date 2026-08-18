package gateway

import (
	"context"
	"fmt"
	"io"
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
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

func TestClassifyQualityHold(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		sig  QualityStreamSignals
		want QualityVerdict
	}{
		{name: "thinking delivers", sig: QualityStreamSignals{HasThinking: true, VisibleTokens: 10}, want: QualityDeliver},
		{name: "reasoning tokens deliver", sig: QualityStreamSignals{ReasoningTokens: 40, VisibleTokens: 80, Terminal: true}, want: QualityDeliver},
		{name: "visible 32 no think withhold", sig: QualityStreamSignals{VisibleTokens: 32, Terminal: true}, want: QualityWithhold},
		{name: "output 40 no think withhold", sig: QualityStreamSignals{OutputTokens: 40, Terminal: true}, want: QualityWithhold},
		{name: "short no think delivers", sig: QualityStreamSignals{VisibleTokens: 10, Terminal: true}, want: QualityDeliver},
		{name: "midstream enough content withhold", sig: QualityStreamSignals{VisibleTokens: 64}, want: QualityWithhold},
		{name: "wait for more", sig: QualityStreamSignals{VisibleTokens: 8}, want: QualityWait},
		{name: "hold expired short delivers", sig: QualityStreamSignals{VisibleTokens: 8, HoldExpired: true}, want: QualityDeliver},
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

func TestObserveQualityChunkResponsesReasoningItem(t *testing.T) {
	t.Parallel()
	state := qualityScanState{protocol: qualityProtocolResponses}
	ObserveQualityChunk(&state, []byte(sse(
		`data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`,
		`data: {"type":"response.output_text.delta","delta":"hello"}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"output_tokens":90,"output_tokens_details":{"reasoning_tokens":60}}}}`,
	)))
	if ClassifyQualityHold(state.signals(), 32) != QualityDeliver {
		t.Fatalf("responses reasoning item should deliver: %#v", state.signals())
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

func TestPeekQualityStreamHoldTimeoutCoversBlockedFirstByte(t *testing.T) {
	t.Parallel()
	reader, writer := io.Pipe()
	continueWrite := make(chan struct{})
	writeErr := make(chan error, 1)
	go func() {
		select {
		case <-continueWrite:
		case <-time.After(500 * time.Millisecond):
		}
		_, err := io.WriteString(writer, sse(
			`data: {"choices":[{"delta":{"content":"late first byte"}}]}`,
			"data: [DONE]",
		))
		if closeErr := writer.Close(); err == nil {
			err = closeErr
		}
		writeErr <- err
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
		t.Fatalf("first-byte hold returned after %s, want the 30ms timeout", elapsed)
	}
	if verdict != QualityDeliver {
		t.Fatalf("empty timed-out prefix verdict = %s", verdict)
	}
	body, err := io.ReadAll(replay)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "late first byte") {
		t.Fatalf("replay lost post-timeout first byte: %q", body)
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
	if shouldHoldQualityStream(input, &owned, route, audit.OperationChat, cfg) {
		t.Fatal("pinned response must not hold")
	}
	if shouldHoldQualityStream(input, nil, route, audit.OperationImage, cfg) {
		t.Fatal("image must not hold")
	}
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "chat reasoning none", body: `{"reasoning_effort":"none"}`},
		{name: "responses reasoning none", body: `{"reasoning":{"effort":"none"}}`},
		{name: "messages thinking disabled", body: `{"thinking":{"type":"disabled"}}`},
		{name: "messages zero thinking budget", body: `{"thinking":{"type":"enabled","budget_tokens":0}}`},
		{name: "client tools", body: `{"tools":[{"type":"function","function":{"name":"charge"}}]}`},
		{name: "legacy functions", body: `{"functions":[{"name":"charge"}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := input
			request.Body = []byte(test.body)
			if shouldHoldQualityStream(request, nil, route, audit.OperationChat, cfg) {
				t.Fatal("explicitly disabled reasoning and tool requests must not be held")
			}
		})
	}
	toolCache := input
	toolCache.Body = []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	toolCache.AllowClientToolCacheRoute = true
	if !shouldHoldQualityStream(toolCache, nil, route, audit.OperationChat, cfg) {
		t.Fatal("client identity/cache compatibility alone must not disable the hold")
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

	credentials := make([]accountdomain.Credential, 0, 2)
	for index, name := range []string{"quality-a", "quality-b"} {
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
		credentials[0].ID: {{status: http.StatusOK, body: noThink}},
		credentials[1].ID: {{status: http.StatusOK, body: thinking}},
	}}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 3)
	service.UpdateQualityRetry(QualityRetryRuntime{Enabled: true, MaxAttempts: 2, MinOutputTokens: 32, OnExhausted: qualityRetryFailOpen, HoldTimeout: time.Second})

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
	if len(attempts) != 2 || attempts[0] != credentials[0].ID || attempts[1] != credentials[1].ID {
		t.Fatalf("expected account exclude+retry, attempts=%#v want [%d %d]", attempts, credentials[0].ID, credentials[1].ID)
	}
	logs, total, err := auditRepo.List(ctx, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	var degraded, delivered bool
	for _, rec := range logs {
		if rec.ErrorCode == ErrorQualityDegraded && rec.AccountID != nil && *rec.AccountID == credentials[0].ID {
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
	if !got.Enabled || got.MaxAttempts != 6 || got.MinOutputTokens != 32 || got.OnExhausted != qualityRetryFailClosed || got.HoldTimeout != 3*time.Second {
		t.Fatalf("defaults = %#v", got)
	}
}
