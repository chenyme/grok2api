package gateway

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/requestmeta"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestQueueAccountModelSyncDeduplicatesConcurrentETagRefresh(t *testing.T) {
	resolver := &etagSyncResolver{started: make(chan uint64, 2), release: make(chan struct{})}
	service := &Service{models: resolver, logger: slog.Default(), modelSyncing: make(map[uint64]struct{})}
	service.queueAccountModelSync(42)
	service.queueAccountModelSync(42)
	select {
	case accountID := <-resolver.started:
		if accountID != 42 {
			t.Fatalf("account id = %d", accountID)
		}
	case <-time.After(time.Second):
		t.Fatal("模型 ETag 刷新未启动")
	}
	if calls := resolver.calls.Load(); calls != 1 {
		t.Fatalf("concurrent sync calls = %d", calls)
	}
	close(resolver.release)
}

func TestVoiceWebSocketAuditOutcomeUsesLogicalSuccessStatus(t *testing.T) {
	if status, code := voiceWebSocketAuditOutcome(VoiceWebSocketOutcome{}); status != http.StatusOK || code != "" {
		t.Fatalf("successful outcome = status %d code %q", status, code)
	}
	if status, code := voiceWebSocketAuditOutcome(VoiceWebSocketOutcome{ErrorCode: " upstream_stream_interrupted "}); status != http.StatusBadGateway || code != "upstream_stream_interrupted" {
		t.Fatalf("failed outcome = status %d code %q", status, code)
	}
}

func TestStreamingSTTPricingUsesCompletedDuration(t *testing.T) {
	result, ok := audit.EstimateOfficialSTTCost(3.45, true)
	if !ok || result.Model != "grok-stt-streaming" || result.CostInUSDTicks != 1_916_667 {
		t.Fatalf("streaming STT pricing = %#v, ok = %t", result, ok)
	}
}

func TestVoiceErrorResponseDoesNotExposeUnclassifiedErrors(t *testing.T) {
	response, err := voiceErrorResponse(testVoiceStatusError{status: http.StatusBadGateway, message: "access_token=secret"})
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(body), "access_token") || strings.Contains(string(body), "secret") {
		t.Fatalf("unclassified provider error leaked through public response: %s", body)
	}
}

type testVoiceStatusError struct {
	status  int
	message string
}

func (e testVoiceStatusError) Error() string       { return e.message }
func (e testVoiceStatusError) HTTPStatusCode() int { return e.status }

func TestFormatSTTResponseHonorsOpenAIFormats(t *testing.T) {
	speaker := 2
	result := provider.STTResult{
		Text: "hello", Language: "en", Duration: 1.25,
		Words:   []provider.STTWord{{Text: "hello", Start: 0.1, End: 0.8, Speaker: &speaker}},
		RawJSON: []byte(`{"text":"native","provider_field":true}`),
	}
	for _, test := range []struct {
		format      string
		contentType string
		contains    []string
	}{
		{format: "text", contentType: "text/plain; charset=utf-8", contains: []string{"hello"}},
		{format: "json", contentType: "application/json", contains: []string{`"text":"hello"`}},
		{format: "verbose_json", contentType: "application/json", contains: []string{`"task":"transcribe"`, `"word":"hello"`, `"speaker":2`}},
		{format: "", contentType: "application/json", contains: []string{`"provider_field":true`}},
	} {
		response := formatSTTResponse(result, test.format)
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if response.Header.Get("Content-Type") != test.contentType {
			t.Fatalf("format %q content type = %q", test.format, response.Header.Get("Content-Type"))
		}
		for _, expected := range test.contains {
			if !strings.Contains(string(body), expected) {
				t.Fatalf("format %q body %q missing %q", test.format, body, expected)
			}
		}
	}
}

type etagSyncResolver struct {
	calls   atomic.Int64
	started chan uint64
	release chan struct{}
}

func (r *etagSyncResolver) SyncAccount(_ context.Context, accountID uint64) (int, error) {
	r.calls.Add(1)
	r.started <- accountID
	<-r.release
	return 1, nil
}

func (r *etagSyncResolver) Get(context.Context, uint64) (modeldomain.Route, error) {
	return modeldomain.Route{}, repository.ErrNotFound
}

func (r *etagSyncResolver) GetByPublicID(context.Context, string) (modeldomain.Route, error) {
	return modeldomain.Route{}, repository.ErrNotFound
}

func (r *etagSyncResolver) GetByPublicIDCandidates(context.Context, string) ([]modeldomain.Route, error) {
	return nil, repository.ErrNotFound
}

func (r *etagSyncResolver) GetByProviderUpstream(context.Context, account.Provider, string) (modeldomain.Route, error) {
	return modeldomain.Route{}, repository.ErrNotFound
}

func TestGatewayFailsOverBeforeReturningBody(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "gateway.db"))
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
	first, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "first", SourceKey: "first", EncryptedAccessToken: "one", ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 200, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "second", SourceKey: "second", EncryptedAccessToken: "two", ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 100, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderBuild, []string{"grok-test"}); err != nil {
		t.Fatal(err)
	}
	for _, accountID := range []uint64{first.ID, second.ID} {
		if err := modelRepo.ReplaceAccountCapabilities(ctx, accountID, []string{"grok-test"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{Name: "test-key", Prefix: "test-prefix", SecretHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", EncryptedSecret: "encrypted-key", Enabled: true, RPMLimit: 120, MaxConcurrent: 8})
	if err != nil {
		t.Fatal(err)
	}

	adapter := &failoverAdapter{
		firstID: first.ID, failureStatus: http.StatusPaymentRequired,
		failureBody:     `{"code":"personal-team-blocked:spending-limit","error":"You have run out of credits"}`,
		failureHeader:   http.Header{"X-Should-Retry": {"false"}},
		reasoningEffort: "high",
	}
	registry := provider.NewRegistry(adapter)
	cipher := testCipher(t)
	sticky := memory.NewStickyStore()
	concurrency := memory.NewConcurrencyLimiter()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, cipher, nil)
	clientService := clientkeyapp.NewService(nil, nil, nil, 60, 4, nil)
	selector := NewSelector(accountRepo, concurrency, sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientService, registry, selector, responseRepo, 3)
	result, err := service.CreateResponse(ctx, Input{RequestID: "req-1", ClientKey: clientKey, PublicModel: "grok-test", Body: []byte(`{"model":"grok-test","reasoning":{"effort":"high"}}`), PromptCacheSeed: "claude-session", GrokTurnIndex: "3"})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatal(err)
	}
	result.Finalize(Usage{Reported: true, InputTokens: 120, CachedInputTokens: 80, OutputTokens: 30, TotalTokens: 150, ResponseModel: "grok-test-build-free"}, "resp-test", "")
	_ = result.Body.Close()
	if string(body) != "ok" {
		t.Fatalf("body = %q", body)
	}
	if len(adapter.attempts) != 2 || adapter.attempts[0] != first.ID || adapter.attempts[1] != second.ID {
		t.Fatalf("attempts = %#v", adapter.attempts)
	}
	identity := resolveBuildSessionIdentity(clientKey.ID, account.ProviderBuild, "grok-test", "", "claude-session", nil)
	expectedCacheKey := identity.upstreamID
	if adapter.lastPromptCacheKey != expectedCacheKey {
		t.Fatalf("prompt cache key = %q, want %q", adapter.lastPromptCacheKey, expectedCacheKey)
	}
	if adapter.lastReasoningReplayKey != identity.replayKey {
		t.Fatalf("reasoning replay key = %q, want %q", adapter.lastReasoningReplayKey, identity.replayKey)
	}
	if adapter.lastGrokTurnIndex != "3" {
		t.Fatalf("Grok turn index = %q, want 3", adapter.lastGrokTurnIndex)
	}
	if boundID, ok, err := sticky.Get(ctx, stickySessionKey(identity.affinityKey), time.Now().UTC()); err != nil || !ok || boundID != second.ID {
		t.Fatalf("failover sticky binding = %d, %v, err = %v; want account %d", boundID, ok, err, second.ID)
	}
	observedAccount, err := accountRepo.Get(ctx, second.ID)
	if err != nil || observedAccount.ObservedModel != "grok-test-build-free" {
		t.Fatalf("observed account = %#v, err = %v", observedAccount, err)
	}
	logs, total, err := auditRepo.List(ctx, 0, 10)
	if err != nil || total != 1 || logs[0].AccountID == nil || *logs[0].AccountID != second.ID || logs[0].ClientKeyName != "test-key" || logs[0].ModelPublicID != "grok-test" || logs[0].ModelUpstreamModel != "Build/grok-test" || logs[0].ReasoningEffort != "high" || logs[0].AccountName != "second" || logs[0].CachedInputTokens != 80 || logs[0].UsageSource != audit.UsageSourceUpstream || logs[0].StatusCode != http.StatusOK || logs[0].AttemptCount != 1 {
		t.Fatalf("audit = %#v, %d, %v", logs, total, err)
	}
	detail, err := auditRepo.Get(ctx, logs[0].ID)
	if err != nil || len(detail.Attempts) != 1 || detail.Attempts[0].UpstreamStatusCode == nil || *detail.Attempts[0].UpstreamStatusCode != http.StatusPaymentRequired || !strings.Contains(string(detail.Attempts[0].ResponseBody), "personal-team-blocked:spending-limit") {
		t.Fatalf("audit detail = %#v, err = %v", detail, err)
	}
	ownership, err := responseRepo.Get(ctx, "resp-test", clientKey.ID, time.Now().UTC())
	if err != nil || ownership.AccountID != second.ID || ownership.PromptCacheKey != expectedCacheKey || ownership.ReasoningReplayKey != identity.replayKey {
		t.Fatalf("ownership = %#v, err = %v", ownership, err)
	}

	compacted, err := service.CreateResponse(ctx, Input{
		RequestID: "req-compact", ClientKey: clientKey, PublicModel: "grok-test", PromptCacheSeed: "claude-session",
		Body: []byte(`{"model":"grok-test","stream":true,"input":[{"role":"user","content":"continue"},{"type":"compaction_trigger"}]}`), Streaming: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(compacted.Body)
	if compacted.MarkFirstToken == nil {
		t.Fatal("first token marker is nil")
	}
	compacted.MarkFirstToken()
	compacted.Finalize(Usage{}, "resp-compact", "")
	_ = compacted.Body.Close()
	logs, total, err = auditRepo.List(ctx, 0, 10)
	if err != nil || total != 2 || logs[0].Operation != audit.OperationCompaction || !logs[0].Streaming || logs[0].FirstTokenMS == nil {
		t.Fatalf("compaction audit = %#v, total=%d, err=%v", logs, total, err)
	}
	if _, err := responseRepo.Get(ctx, "resp-compact", clientKey.ID, time.Now().UTC()); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("compaction response ownership err = %v", err)
	}

	// Grok TUI compaction is a normal Responses request on the wire. It skips
	// the quality hold and is labeled compaction only in the audit record, so
	// Provider routing and stored-response ownership must remain intact.
	service.UpdateQualityRetry(QualityRetryRuntime{
		Enabled: true, MaxAttempts: 2, MinOutputTokens: 32,
		OnExhausted: qualityRetryFailClosed, HoldTimeout: time.Second,
	})
	adapter.resetAttempts()
	tuiCompacted, err := service.CreateResponse(ctx, Input{
		RequestID: "req-tui-compact", ClientKey: clientKey, PublicModel: "grok-test", PromptCacheSeed: "tui-session",
		Body: []byte(`{"model":"grok-test","stream":true,"input":[{"role":"user","content":"` + tuiCompactionPrompt + `"}]}`), Streaming: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	tuiBody, err := io.ReadAll(tuiCompacted.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(tuiBody) != "ok" {
		t.Fatalf("TUI compaction body = %q", tuiBody)
	}
	tuiCompacted.MarkFirstToken()
	tuiCompacted.Finalize(Usage{}, "resp-tui-compact", "")
	_ = tuiCompacted.Body.Close()
	if len(adapter.attempts) != 1 || adapter.lastOperation != string(audit.OperationResponses) {
		t.Fatalf("TUI compaction attempts = %#v, Provider operation = %q", adapter.attempts, adapter.lastOperation)
	}
	logs, total, err = auditRepo.List(ctx, 0, 10)
	if err != nil || total != 3 || logs[0].Operation != audit.OperationCompaction {
		t.Fatalf("TUI compaction audit = %#v, total=%d, err=%v", logs, total, err)
	}
	if ownership, err := responseRepo.Get(ctx, "resp-tui-compact", clientKey.ID, time.Now().UTC()); err != nil || ownership.AccountID != second.ID {
		t.Fatalf("TUI compaction ownership = %#v, err=%v", ownership, err)
	}

	adapter.resetAttempts()
	continued, err := service.CreateResponse(ctx, Input{RequestID: "req-2", ClientKey: clientKey, PublicModel: "grok-test", PreviousResponseID: "resp-test", Body: []byte(`{"model":"grok-test","previous_response_id":"resp-test"}`)})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(continued.Body)
	continued.Finalize(Usage{}, "resp-next", "")
	_ = continued.Body.Close()
	if len(adapter.attempts) != 1 || adapter.attempts[0] != second.ID {
		t.Fatalf("continued attempts = %#v", adapter.attempts)
	}
	if adapter.lastPromptCacheKey != expectedCacheKey || adapter.lastReasoningReplayKey != identity.replayKey {
		t.Fatalf("continued session identity drifted: cache=%q replay=%q", adapter.lastPromptCacheKey, adapter.lastReasoningReplayKey)
	}
	nextOwnership, err := responseRepo.Get(ctx, "resp-next", clientKey.ID, time.Now().UTC())
	if err != nil || nextOwnership.PromptCacheKey != expectedCacheKey || nextOwnership.ReasoningReplayKey != identity.replayKey {
		t.Fatalf("continued ownership = %#v, err = %v", nextOwnership, err)
	}

	adapter.resetAttempts()
	blockedKey := clientKey
	blockedKey.ProviderScope = clientkey.ProviderScopeConsole
	if _, err := service.GetResponse(ctx, ResourceInput{ClientKey: blockedKey, ResponseID: "resp-test"}); err == nil {
		t.Fatal("owned response should be rejected after its provider leaves the key scope")
	} else {
		var unavailable *SelectionUnavailableError
		if !errors.As(err, &unavailable) || unavailable.Code() != "client_key_account_scope_unavailable" || len(adapter.attempts) != 0 {
			t.Fatalf("scoped owned response error = %#v, attempts = %#v, err = %v", unavailable, adapter.attempts, err)
		}
	}
	resource, err := service.GetResponse(ctx, ResourceInput{ClientKey: clientKey, ResponseID: "resp-test", RawQuery: "include=reasoning.encrypted_content"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resource.Body)
	resource.Finalize(Usage{}, "", "")
	_ = resource.Body.Close()
	if adapter.lastPath != "/responses/resp-test?include=reasoning.encrypted_content" || adapter.lastMethod != http.MethodGet || len(adapter.attempts) != 1 || adapter.attempts[0] != second.ID {
		t.Fatalf("resource request = %s %s, attempts = %#v", adapter.lastMethod, adapter.lastPath, adapter.attempts)
	}

	deleted, err := service.DeleteResponse(ctx, ResourceInput{ClientKey: clientKey, ResponseID: "resp-test"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(deleted.Body)
	deleted.Finalize(Usage{}, "", "")
	_ = deleted.Body.Close()
	if _, err := responseRepo.Get(ctx, "resp-test", clientKey.ID, time.Now().UTC()); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("deleted ownership err = %v", err)
	}

	adapter.setResourceStatus(http.StatusNotFound)
	missing, err := service.GetResponse(ctx, ResourceInput{ClientKey: clientKey, ResponseID: "resp-next"})
	if err != nil {
		t.Fatal(err)
	}
	_ = missing.Body.Close()
	missing.Finalize(Usage{}, "", "")
	if _, err := responseRepo.Get(ctx, "resp-next", clientKey.ID, time.Now().UTC()); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("stale ownership err = %v", err)
	}

	adapter.resetAttempts()
	streamFailed, err := service.CreateResponse(ctx, Input{RequestID: "req-stream-failed", ClientKey: clientKey, PublicModel: "grok-test", Body: []byte(`{"model":"grok-test"}`), PromptCacheSeed: "stream-failed-session"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(streamFailed.Body)
	if streamFailed.RecordStreamFailure == nil {
		t.Fatal("stream failure recorder is nil")
	}
	streamFailed.RecordStreamFailure(StreamFailureDiagnostic{Body: []byte(`{"type":"response.failed","error":{"message":"access_token=secret-token"}}`)})
	streamFailed.Finalize(Usage{}, "", "upstream_stream_error")
	_ = streamFailed.Body.Close()
	logs, _, err = auditRepo.List(ctx, 0, 10)
	if err != nil || len(logs) == 0 {
		t.Fatalf("stream failure audits = %#v, err = %v", logs, err)
	}
	streamDetail, err := auditRepo.Get(ctx, logs[0].ID)
	if err != nil || streamDetail.ErrorCode != "upstream_stream_error" || streamDetail.UsageSource != audit.UsageSourceNone || streamDetail.AttemptCount != 1 || len(streamDetail.Attempts) != 1 {
		t.Fatalf("stream failure detail = %#v, err = %v", streamDetail, err)
	}
	streamAttempt := streamDetail.Attempts[0]
	if streamAttempt.Stage != "response_stream" || streamAttempt.UpstreamStatusCode == nil || *streamAttempt.UpstreamStatusCode != http.StatusOK || string(streamAttempt.ResponseBody) != `{"type":"response.failed","error":{"message":"access_token=[REDACTED]"}}` {
		t.Fatalf("stream failure attempt = %#v", streamAttempt)
	}

	adapter.resetAttempts()
	expiredCooldown := time.Now().UTC().Add(-time.Minute)
	for _, accountID := range []uint64{first.ID, second.ID} {
		if err := accountRepo.UpdateHealth(ctx, accountID, account.ProviderBuild, 3, &expiredCooldown, "previous upstream failures", false); err != nil {
			t.Fatal(err)
		}
	}
	selector.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: account.ProviderBuild})
	interrupted, err := service.CreateResponse(ctx, Input{RequestID: "req-stream-cut", ClientKey: clientKey, PublicModel: "grok-test", Body: []byte(`{"model":"grok-test"}`), PromptCacheSeed: "other-session"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(interrupted.Body)
	healthBlocker := &blockingHealthAccountRepository{
		AccountRepository: accountRepo, started: make(chan struct{}), release: make(chan struct{}),
	}
	selector.accounts = healthBlocker
	finalized := make(chan struct{})
	go func() {
		interrupted.Finalize(Usage{}, "", "upstream_stream_idle_timeout")
		close(finalized)
	}()
	select {
	case <-healthBlocker.started:
	case <-time.After(time.Second):
		t.Fatal("stream failure health update did not start")
	}
	if len(adapter.attempts) != 1 {
		t.Fatalf("interrupted attempts = %#v", adapter.attempts)
	}
	selectedAccountID := adapter.attempts[0]
	if current, currentErr := concurrency.Current(ctx, accountConcurrencyKey(selectedAccountID)); currentErr != nil || current != 1 {
		t.Fatalf("account lease was released before stream failure cooldown: current=%d err=%v", current, currentErr)
	}
	close(healthBlocker.release)
	select {
	case <-finalized:
	case <-time.After(time.Second):
		t.Fatal("stream failure finalization did not finish")
	}
	_ = interrupted.Body.Close()
	interruptedAccount, err := accountRepo.Get(ctx, adapter.attempts[0])
	if err != nil || interruptedAccount.FailureCount != 1 || interruptedAccount.CooldownUntil == nil {
		t.Fatalf("interrupted account health = %#v, err=%v", interruptedAccount, err)
	}
	remaining := time.Until(*interruptedAccount.CooldownUntil)
	if remaining < 14*time.Minute || remaining > 15*time.Minute+time.Minute {
		t.Fatalf("idle stream cooldown = %s, want about 15m", remaining)
	}
}

func TestGatewayBuildResponseHeaderTimeoutDoesNotSwitchAccounts(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "gateway-build-header-timeout.db"))
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
	credentials := make([]account.Credential, 0, 2)
	for index, name := range []string{"timeout", "fallback"} {
		credential, _, createErr := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderBuild, Name: name, SourceKey: name, EncryptedAccessToken: "encrypted-" + name,
			ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive,
			Priority: 200 - index*100, MaxConcurrent: 1,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		credentials = append(credentials, credential)
	}
	const model = "grok-build-header-timeout"
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderBuild, []string{model}); err != nil {
		t.Fatal(err)
	}
	for _, credential := range credentials {
		if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{model}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	adapter := &failoverAdapter{transportErrorIDs: map[uint64]error{credentials[0].ID: responseHeaderTimeoutTestError{}}}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 3)

	if _, err := service.CreateResponse(ctx, Input{
		RequestID: "req-build-header-timeout", ClientKey: clientkey.Key{ID: 1, Name: "build-key"}, PublicModel: model,
		Body: []byte(`{"model":"grok-build-header-timeout","input":"hello"}`),
	}); err == nil {
		t.Fatal("expected response-header timeout")
	}
	if len(adapter.attempts) != 1 || adapter.attempts[0] != credentials[0].ID {
		t.Fatalf("attempts = %#v, want only account %d", adapter.attempts, credentials[0].ID)
	}
	latest, err := accountRepo.Get(ctx, credentials[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.FailureCount != 0 || latest.CooldownUntil != nil {
		t.Fatalf("ambiguous response-header timeout changed account health: %#v", latest)
	}
}

type blockingHealthAccountRepository struct {
	repository.AccountRepository
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *blockingHealthAccountRepository) UpdateHealth(ctx context.Context, id uint64, provider account.Provider, failureCount int, cooldownUntil *time.Time, lastError string, success bool) error {
	if !success {
		r.once.Do(func() { close(r.started) })
		select {
		case <-r.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return r.AccountRepository.UpdateHealth(ctx, id, provider, failureCount, cooldownUntil, lastError, success)
}

func TestRoutingAttemptPolicy(t *testing.T) {
	tests := []struct {
		name       string
		configured int
		allowed    []bool
		hasNext    []bool
	}{
		{name: "finite", configured: 2, allowed: []bool{true, true, false}, hasNext: []bool{true, false, false}},
		{name: "unlimited", configured: unlimitedRoutingAttempts, allowed: []bool{true, true, true}, hasNext: []bool{true, true, true}},
		{name: "invalid fallback", configured: 0, allowed: []bool{true, true, true, false}, hasNext: []bool{true, true, false, false}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := newRoutingAttemptPolicy(test.configured)
			for attempt, want := range test.allowed {
				if got := policy.allows(attempt); got != want {
					t.Fatalf("allows(%d) = %t, want %t", attempt, got, want)
				}
			}
			for attempt, want := range test.hasNext {
				if got := policy.hasNext(attempt); got != want {
					t.Fatalf("hasNext(%d) = %t, want %t", attempt, got, want)
				}
			}
		})
	}
}

func TestPinnedRequestAttemptPolicyAlwaysAllowsOneAttempt(t *testing.T) {
	for _, configured := range []int{1, 6, unlimitedRoutingAttempts} {
		policy := newRequestRoutingAttemptPolicy(configured, true)
		if !policy.allows(0) || policy.allows(1) || policy.hasNext(0) {
			t.Fatalf("configured=%d pinned policy = %#v", configured, policy)
		}
	}
}

func TestGatewayUnlimitedAttemptsExhaustsEligiblePool(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "gateway-unlimited-attempts.db"))
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
	credentials := make([]account.Credential, 0, 5)
	for index := range 5 {
		name := fmt.Sprintf("build-%d", index+1)
		credential, _, createErr := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderBuild, Name: name, SourceKey: name, EncryptedAccessToken: "encrypted-" + name,
			ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive,
			Priority: 500 - index*100, MaxConcurrent: 1,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		credentials = append(credentials, credential)
	}
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderBuild, []string{"grok-unlimited"}); err != nil {
		t.Fatal(err)
	}
	for _, credential := range credentials {
		if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-unlimited"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "unlimited-key", Prefix: "unlimited", SecretHash: strings.Repeat("a", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	failureIDs := make(map[uint64]bool, len(credentials)-1)
	for _, credential := range credentials[:len(credentials)-1] {
		failureIDs[credential.ID] = true
	}
	adapter := &failoverAdapter{
		failureIDs: failureIDs, failureStatus: http.StatusPaymentRequired,
		failureBody:   `{"code":"personal-team-blocked:spending-limit","error":"You have run out of credits"}`,
		failureHeader: http.Header{"X-Should-Retry": {"false"}},
	}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, unlimitedRoutingAttempts)

	result, err := service.CreateResponse(ctx, Input{
		RequestID: "req-unlimited", ClientKey: clientKey, PublicModel: "grok-unlimited",
		Body: []byte(`{"model":"grok-unlimited","input":"hello"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", result.StatusCode)
	}
	_, _ = io.ReadAll(result.Body)
	result.Finalize(Usage{}, "resp-unlimited", "")
	_ = result.Body.Close()
	if len(adapter.attempts) != len(credentials) {
		t.Fatalf("attempts = %#v, want all %d eligible accounts", adapter.attempts, len(credentials))
	}
	for index, credential := range credentials {
		if adapter.attempts[index] != credential.ID {
			t.Fatalf("attempt %d used account %d, want %d", index, adapter.attempts[index], credential.ID)
		}
	}

	adapter.resetAttempts()
	continued, err := service.CreateResponse(ctx, Input{
		RequestID: "req-unlimited-continued", ClientKey: clientKey, PublicModel: "grok-unlimited",
		PreviousResponseID: "resp-unlimited", Body: []byte(`{"model":"grok-unlimited","previous_response_id":"resp-unlimited"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(continued.Body)
	continued.Finalize(Usage{}, "resp-unlimited-next", "")
	_ = continued.Body.Close()
	if len(adapter.attempts) != 1 || adapter.attempts[0] != credentials[len(credentials)-1].ID {
		t.Fatalf("owned response attempts = %#v, want only account %d", adapter.attempts, credentials[len(credentials)-1].ID)
	}
}

func TestGatewayUnlimitedAttemptsRetainsEgressRetry(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "gateway-unlimited-egress-retry.db"))
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
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierSuper,
		Name: "web-egress-retry", SourceKey: "web-egress-retry", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	const model = "grok-web-egress-retry"
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderWeb, []string{model}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{model}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	adapter := &transientEgressForbiddenAdapter{}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, unlimitedRoutingAttempts)

	result, err := service.CreateResponse(ctx, Input{
		RequestID: "req-unlimited-egress-retry", ClientKey: clientkey.Key{ID: 1, Name: "web-key"}, PublicModel: model,
		Body: []byte(`{"model":"grok-web-egress-retry","input":"hello"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", result.StatusCode)
	}
	_, _ = io.ReadAll(result.Body)
	result.Finalize(Usage{}, "", "")
	_ = result.Body.Close()
	if calls := adapter.calls.Load(); calls != 2 {
		t.Fatalf("calls = %d, want one egress retry", calls)
	}
}

func TestGatewaySSOUnauthorizedMarksInvalidAndSwitchesAccount(t *testing.T) {
	for _, providerValue := range []account.Provider{account.ProviderWeb, account.ProviderConsole} {
		providerValue := providerValue
		t.Run(string(providerValue), func(t *testing.T) {
			testGatewaySSOFailureMarksInvalidAndSwitchesAccount(t, providerValue, http.StatusUnauthorized, `{"error":{"code":"unauthorized","message":"credential rejected"}}`)
		})
	}
}

func TestGatewaySSOBlockedForbiddenMarksInvalidAndSwitchesAccount(t *testing.T) {
	for _, providerValue := range []account.Provider{account.ProviderWeb, account.ProviderConsole} {
		providerValue := providerValue
		t.Run(string(providerValue), func(t *testing.T) {
			testGatewaySSOFailureMarksInvalidAndSwitchesAccount(t, providerValue, http.StatusForbidden, `{"error":{"code":7,"message":"User is blocked [WKE=unauthorized:blocked-user]"}}`)
		})
	}
}

func testGatewaySSOFailureMarksInvalidAndSwitchesAccount(t *testing.T, providerValue account.Provider, failureStatus int, failureBody string) {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "sso-failure.db"))
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
	credentials := make([]account.Credential, 0, 2)
	for index, name := range []string{"rejected", "healthy"} {
		credential, _, createErr := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: providerValue, AuthType: account.AuthTypeSSO, Name: name, SourceKey: string(providerValue) + "-" + name,
			EncryptedAccessToken: "encrypted-" + name, Enabled: true, AuthStatus: account.AuthStatusActive,
			Priority: 200 - index*100, MaxConcurrent: 1,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		credentials = append(credentials, credential)
	}
	modelName := "grok-sso-401"
	if err := modelRepo.UpsertDiscovered(ctx, providerValue, []string{modelName}); err != nil {
		t.Fatal(err)
	}
	for _, credential := range credentials {
		if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{modelName}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	key, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "sso-401-key", Prefix: "sso-401", SecretHash: strings.Repeat("e", 64), EncryptedSecret: "encrypted-key",
		Enabled: true, RPMLimit: 60, MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &ssoFailureAdapter{
		providerValue: providerValue, rejectedID: credentials[0].ID,
		failureStatus: failureStatus, failureBody: failureBody,
	}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 2)

	result, err := service.CreateResponse(ctx, Input{
		RequestID: "req-sso-401", ClientKey: key, PublicModel: modelName,
		Body: []byte(`{"model":"grok-sso-401","input":"hello"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatal(err)
	}
	result.Finalize(Usage{}, "", "")
	_ = result.Body.Close()
	if string(body) != "ok" {
		t.Fatalf("body = %q", body)
	}
	if attempts := adapter.Attempts(); len(attempts) != 2 || attempts[0] != credentials[0].ID || attempts[1] != credentials[1].ID {
		t.Fatalf("attempts = %#v", attempts)
	}
	rejected, err := accountRepo.Get(ctx, credentials[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if rejected.AuthStatus != account.AuthStatusReauthRequired || !rejected.Enabled {
		t.Fatalf("rejected account = %#v", rejected)
	}
	healthy, err := accountRepo.Get(ctx, credentials[1].ID)
	if err != nil || healthy.AuthStatus != account.AuthStatusActive {
		t.Fatalf("healthy account = %#v, err = %v", healthy, err)
	}
}

func TestGatewayTeamModelRateLimitOnlySkipsMatchingTeam(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "team-model-rate-limit.db"))
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
	credentials := make([]account.Credential, 0, 3)
	for index, seed := range []struct {
		name   string
		teamID string
	}{{"console-team-a-first", "team-a"}, {"console-team-a-second", "team-a"}, {"console-team-b", "team-b"}} {
		credential, _, createErr := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderConsole, AuthType: account.AuthTypeSSO, Name: seed.name, SourceKey: seed.name, TeamID: seed.teamID,
			EncryptedAccessToken: "encrypted-" + seed.name, Enabled: true, AuthStatus: account.AuthStatusActive,
			Priority: 200 - index, MaxConcurrent: 1,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		credentials = append(credentials, credential)
	}
	models := []string{"grok-console-team-rate-limit", "grok-console-team-rate-limit-other"}
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderConsole, models); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, credential := range credentials {
		if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, models, now); err != nil {
			t.Fatal(err)
		}
	}
	key, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "team-model-key", Prefix: "team-model", SecretHash: strings.Repeat("d", 64), EncryptedSecret: "encrypted-key",
		Enabled: true, RPMLimit: 60, MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatal(err)
	}

	adapter := &teamModelRateLimitConsoleAdapter{rateLimitedTeam: "team-a"}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 3)

	assertSuccess := func(requestID, publicModel string) {
		t.Helper()
		result, err := service.CreateResponse(ctx, Input{
			RequestID: requestID, ClientKey: key, PublicModel: publicModel,
			Body: []byte(`{"model":"` + publicModel + `","input":"hello"}`),
		})
		if err != nil || result == nil || result.StatusCode != http.StatusOK {
			t.Fatalf("result = %#v, err = %v", result, err)
		}
		_ = result.Body.Close()
		result.Finalize(Usage{}, "", "")
	}

	assertSuccess("req-team-model-first", models[0])
	if attempts := adapter.Attempts(); len(attempts) != 2 || attempts[0].AccountID != credentials[0].ID || attempts[1].AccountID != credentials[2].ID {
		t.Fatalf("first attempts = %#v, want first Team A account then Team B account", attempts)
	}
	assertSuccess("req-team-model-cached", models[0])
	if attempts := adapter.Attempts(); len(attempts) != 3 || attempts[2].AccountID != credentials[2].ID {
		t.Fatalf("cached Team A accounts should be skipped in favor of Team B, attempts = %#v", attempts)
	}
	assertSuccess("req-team-model-other", models[1])
	if attempts := adapter.Attempts(); len(attempts) != 5 || attempts[3].AccountID != credentials[0].ID || attempts[3].Model != models[1] || attempts[4].AccountID != credentials[2].ID {
		t.Fatalf("different model should have an independent Team limit, attempts = %#v", attempts)
	}
}

func TestSelectConversationRouteRespectsClientKeyAcrossSharedPublicModel(t *testing.T) {
	registry := provider.NewRegistry(&failoverAdapter{}, webStoredResponseAdapter{}, statelessConsoleAdapter{})
	service := &Service{
		clientKeys: clientkeyapp.NewService(nil, nil, nil, 60, 4, nil),
		providers:  registry,
	}
	routes := []modeldomain.Route{
		{ID: 10, PublicID: "Build/grok-shared", Provider: account.ProviderBuild, UpstreamModel: "grok-shared"},
		{ID: 15, PublicID: "Web/grok-shared", Provider: account.ProviderWeb, UpstreamModel: "grok-shared"},
		{ID: 20, PublicID: "Console/grok-shared", Provider: account.ProviderConsole, UpstreamModel: "grok-shared"},
	}
	selected, err := service.selectConversationRoute(routes, clientkey.Key{ProviderScope: clientkey.ProviderScopeWeb | clientkey.ProviderScopeConsole}, audit.OperationResponses, "/responses", false, nil)
	if err != nil || selected.ID != 15 {
		t.Fatalf("provider-scoped route = %#v, err = %v", selected, err)
	}
	selected, err = service.selectConversationRoute(routes, clientkey.Key{ProviderScope: clientkey.ProviderScopeConsole, AllowedModels: []uint64{20}}, audit.OperationResponses, "/responses", false, nil)
	if err != nil || selected.ID != 20 {
		t.Fatalf("selected route = %#v, err = %v", selected, err)
	}
	_, err = service.selectConversationRoute(routes, clientkey.Key{ProviderScope: clientkey.ProviderScopeBuild, AllowedModels: []uint64{20}}, audit.OperationResponses, "/responses", false, nil)
	if !errors.Is(err, clientkeyapp.ErrModelNotAllowed) {
		t.Fatalf("scope and model intersection should reject the request: %v", err)
	}
	_, err = service.selectConversationRoute(routes[1:], clientkey.Key{ProviderScope: clientkey.ProviderScopeBuild}, audit.OperationResponses, "/responses", false, nil)
	var unavailable *SelectionUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Code() != "client_key_account_scope_unavailable" {
		t.Fatalf("provider scope must not fall back: %#v, err = %v", unavailable, err)
	}
	ownership := &inferencedomain.ResponseOwnership{Provider: account.ProviderWeb}
	_, err = service.selectConversationRoute(routes, clientkey.Key{ProviderScope: clientkey.ProviderScopeBuild}, audit.OperationResponses, "/responses", true, ownership)
	if !errors.As(err, &unavailable) || unavailable.Code() != "client_key_account_scope_unavailable" {
		t.Fatalf("owned response must remain inside the updated provider scope: %#v, err = %v", unavailable, err)
	}
	duplicateBuild := modeldomain.Route{ID: 11, PublicID: "Build/grok-shared", Provider: account.ProviderBuild, UpstreamModel: "grok-alternate"}
	routes = append(routes, duplicateBuild)
	eligible, _, err := service.eligibleConversationRoutes(routes, clientkey.Key{ProviderScope: clientkey.ProviderScopeBuild, AllowedModels: []uint64{duplicateBuild.ID}}, audit.OperationResponses, "/responses", false, nil)
	if err != nil || len(eligible) != 1 || eligible[0].ID != duplicateBuild.ID {
		t.Fatalf("target-scoped permission candidates = %#v, err = %v", eligible, err)
	}
	selected, err = service.selectConversationRoute(routes, clientkey.Key{ProviderScope: clientkey.ProviderScopeBuild}, audit.OperationResponses, "/responses", true, &inferencedomain.ResponseOwnership{Provider: account.ProviderBuild, ModelRouteID: duplicateBuild.ID})
	if err != nil || selected.ID != duplicateBuild.ID {
		t.Fatalf("route-owned response selected %#v, err = %v", selected, err)
	}
}

func TestOrderConversationRouteTargetsIsSessionStableAndProviderScoped(t *testing.T) {
	routes := []modeldomain.Route{
		{ID: 10, Provider: account.ProviderBuild},
		{ID: 20, Provider: account.ProviderBuild},
		{ID: 30, Provider: account.ProviderWeb},
	}
	first := orderConversationRouteTargets(routes, "session-a")
	repeated := orderConversationRouteTargets(routes, "session-a")
	if first[0].ID != repeated[0].ID || first[1].ID != repeated[1].ID {
		t.Fatalf("session order changed: %#v vs %#v", first, repeated)
	}
	if first[2].Provider != account.ProviderWeb {
		t.Fatalf("provider priority changed: %#v", first)
	}
	seen := make(map[uint64]bool)
	for index := 0; index < 64; index++ {
		ordered := orderConversationRouteTargets(routes, fmt.Sprintf("session-%d", index))
		seen[ordered[0].ID] = true
	}
	if !seen[10] || !seen[20] || seen[30] {
		t.Fatalf("same-provider target distribution = %#v", seen)
	}
}

func TestRouteTargetSeedUsesSessionSignalsAndSoftMessageAnchor(t *testing.T) {
	base := Input{
		RequestID: "request-a", ClientKey: clientkey.Key{ID: 17},
		Body: []byte(`{"model":"pooled-model","instructions":"be concise","input":"hello"}`),
	}
	continued := base
	continued.RequestID = "request-b"
	continued.Body = []byte(`{"model":"pooled-model","instructions":"be concise","input":[{"type":"message","role":"user","content":"hello"},{"type":"message","role":"assistant","content":"hi"}]}`)
	if first, second := routeTargetSeed(base), routeTargetSeed(continued); first != second {
		t.Fatalf("soft route seed changed across session: %q != %q", first, second)
	}
	explicit := base
	explicit.PromptCacheKey = "body-fallback"
	explicit.PromptCacheSeed = "transport-session"
	explicit.Body = []byte(`{"input":"different"}`)
	if got, want := routeTargetSeed(explicit), "17:transport-session"; got != want {
		t.Fatalf("explicit route seed = %q, want %q", got, want)
	}
}

func TestCreateResponseFallsBackAcrossSameNameTargetsWithUnavailablePool(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "route-target-failover.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	models := relational.NewModelRepository(database)
	audits := relational.NewAuditRepository(database)
	responses := relational.NewResponseRepository(database)
	keys := relational.NewClientKeyRepository(database)
	coolingUntil := time.Now().UTC().Add(time.Hour)
	cooling, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "cooling-target", SourceKey: "cooling-target",
		EncryptedAccessToken: "cooling-token", Enabled: true, AuthStatus: account.AuthStatusActive,
		CooldownUntil: &coolingUntil, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	healthy, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "healthy-target", SourceKey: "healthy-target",
		EncryptedAccessToken: "healthy-token", Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	coolingRoute, err := models.Create(ctx, modeldomain.Route{
		PublicID: "pooled-model", Provider: account.ProviderBuild, UpstreamModel: "upstream-cooling",
		Capability: modeldomain.CapabilityResponses, Enabled: true,
	}, []uint64{cooling.ID})
	if err != nil {
		t.Fatal(err)
	}
	healthyRoute, err := models.Create(ctx, modeldomain.Route{
		PublicID: "pooled-model", Provider: account.ProviderBuild, UpstreamModel: "upstream-healthy",
		Capability: modeldomain.CapabilityResponses, Enabled: true,
	}, []uint64{healthy.ID})
	if err != nil {
		t.Fatal(err)
	}
	if coolingRoute.PublicID != healthyRoute.PublicID {
		t.Fatalf("target pool split: %#v %#v", coolingRoute, healthyRoute)
	}
	key, err := keys.Create(ctx, clientkey.Key{
		Name: "target-pool", Prefix: "target-pool", SecretHash: strings.Repeat("a", 64),
		EncryptedSecret: "encrypted", Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &failoverAdapter{}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accounts, audits, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(models, audits, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responses, 3)
	result, err := service.CreateResponse(ctx, Input{
		RequestID: "route-target-failover", ClientKey: key, PublicModel: "pooled-model",
		PromptCacheSeed: "stable-target-session", Body: []byte(`{"model":"pooled-model","input":"hello"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(result.Body)
	result.Finalize(Usage{}, "resp-target-pool", "")
	_ = result.Body.Close()
	if forwarded := adapter.ForwardedModels(); len(forwarded) != 1 || forwarded[0] != healthyRoute.UpstreamModel {
		t.Fatalf("forwarded models = %#v, want %q", forwarded, healthyRoute.UpstreamModel)
	}
	ownership, err := responses.Get(ctx, "resp-target-pool", key.ID, time.Now().UTC())
	if err != nil || ownership.ModelRouteID != healthyRoute.ID {
		t.Fatalf("target ownership = %#v, err = %v", ownership, err)
	}
}

func TestSelectMediaRouteSkipsSameNamedConversationRoute(t *testing.T) {
	registry := provider.NewRegistry(&failoverAdapter{}, &webImageStreamAdapter{})
	service := &Service{
		clientKeys: clientkeyapp.NewService(nil, nil, nil, 60, 4, nil),
		providers:  registry,
	}
	routes := []modeldomain.Route{
		{ID: 10, PublicID: "Build/grok-shared", Provider: account.ProviderBuild, UpstreamModel: "grok-shared", Capability: modeldomain.CapabilityResponses},
		{ID: 20, PublicID: "Web/grok-shared", Provider: account.ProviderWeb, UpstreamModel: "grok-shared", Capability: modeldomain.CapabilityImage},
	}
	selected, err := service.selectMediaRoute(routes, clientkey.Key{}, modeldomain.CapabilityImage, func(providerValue account.Provider) bool {
		_, ok := registry.ImageGeneration(providerValue)
		return ok
	})
	if err != nil || selected.ID != 20 {
		t.Fatalf("selected route = %#v, err = %v", selected, err)
	}
	_, err = service.selectMediaRoute(routes, clientkey.Key{ProviderScope: clientkey.ProviderScopeBuild}, modeldomain.CapabilityImage, func(providerValue account.Provider) bool {
		_, ok := registry.ImageGeneration(providerValue)
		return ok
	})
	var unavailable *SelectionUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Code() != "client_key_account_scope_unavailable" {
		t.Fatalf("media route must not leave provider scope: %#v, err = %v", unavailable, err)
	}
}

func TestSelectSchedulableMediaRouteSkipsUnavailableFirstTarget(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "media-route-failover.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	now := time.Now().UTC()
	coolingUntil := now.Add(time.Hour)
	buildAccount, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth,
		Name: "cooling-build", SourceKey: "cooling-build", EncryptedAccessToken: "build-token",
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1, CooldownUntil: &coolingUntil,
	})
	if err != nil {
		t.Fatal(err)
	}
	webAccount, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierSuper,
		Name: "healthy-web", SourceKey: "healthy-web", EncryptedAccessToken: "web-token",
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	const publicModel = "shared-media-target"
	routeInputs := []modeldomain.Route{
		{PublicID: publicModel, Provider: account.ProviderBuild, UpstreamModel: "build-image-target", Capability: modeldomain.CapabilityImage, Enabled: true},
		{PublicID: publicModel, Provider: account.ProviderWeb, UpstreamModel: "grok-imagine-image", Capability: modeldomain.CapabilityImage, Enabled: true},
	}
	if err := modelRepo.UpsertRoutes(ctx, routeInputs); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, buildAccount.ID, []string{"build-image-target"}, now); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, webAccount.ID, []string{"grok-imagine-image"}, now); err != nil {
		t.Fatal(err)
	}
	routes, err := modelRepo.GetByPublicIDCandidates(ctx, publicModel)
	if err != nil {
		t.Fatal(err)
	}
	registry := provider.NewRegistry(&credentialFailureImageAdapter{}, &webImageStreamAdapter{})
	sticky := memory.NewStickyStore()
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := &Service{
		clientKeys: clientkeyapp.NewService(nil, nil, nil, 60, 4, nil),
		providers:  registry,
		selector:   selector,
	}
	selected, selection, err := service.selectSchedulableMediaRoute(ctx, routes, clientkey.Key{}, modeldomain.CapabilityImage, true, func(providerValue account.Provider) bool {
		_, ok := registry.ImageGeneration(providerValue)
		return ok
	})
	if err != nil {
		t.Fatal(err)
	}
	if selected.Provider != account.ProviderWeb || selection == nil {
		t.Fatalf("selected route = %#v, selection = %#v", selected, selection)
	}
	lease, err := selection.Acquire(ctx, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Credential.ID != webAccount.ID {
		t.Fatalf("selected account = %d, want %d", lease.Credential.ID, webAccount.ID)
	}
	lease.Release()

	// Non-consuming metadata calls (for example voice listing) must not inherit
	// the Provider quota mode from route probing. Otherwise an account with an
	// exhausted inference window cannot serve a request that consumes no quota.
	if err := accountRepo.SaveQuotaWindows(ctx, webAccount.ID, account.WebTierSuper, now, []account.QuotaWindow{{
		AccountID: webAccount.ID, Mode: "fast", Remaining: 0, Total: 10, WindowSeconds: 3600,
		Source: account.QuotaSourceUpstream, SyncedAt: &now,
	}}); err != nil {
		t.Fatal(err)
	}
	quotaSelector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), registry, time.Hour, time.Second, time.Minute)
	service.selector = quotaSelector
	if _, _, err := service.selectSchedulableMediaRoute(ctx, routes, clientkey.Key{}, modeldomain.CapabilityImage, true, func(providerValue account.Provider) bool {
		_, ok := registry.ImageGeneration(providerValue)
		return ok
	}); err == nil {
		t.Fatal("quota-consuming selection unexpectedly accepted an exhausted account")
	}
	selected, selection, err = service.selectSchedulableMediaRoute(ctx, routes, clientkey.Key{}, modeldomain.CapabilityImage, false, func(providerValue account.Provider) bool {
		_, ok := registry.ImageGeneration(providerValue)
		return ok
	})
	if err != nil || selected.Provider != account.ProviderWeb || selection == nil {
		t.Fatalf("non-consuming route = %#v, selection = %#v, err = %v", selected, selection, err)
	}
}

func TestUnpricedVoiceRemainsAvailableToFiniteClientKey(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "voice-billing-policy.db"))
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
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderConsole, AuthType: account.AuthTypeSSO,
		Name: "console-voice", SourceKey: "console-voice", EncryptedAccessToken: "console-token",
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	const voiceModel = "voice-billing-policy"
	if err := modelRepo.UpsertRoutes(ctx, []modeldomain.Route{{
		PublicID: voiceModel, Provider: account.ProviderConsole, UpstreamModel: voiceModel,
		Capability: modeldomain.CapabilityTTS, Enabled: true,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{voiceModel}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	registry := provider.NewRegistry(statelessConsoleAdapter{})
	sticky := memory.NewStickyStore()
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, nil, 1)
	executed := false
	result, err := service.executeVoice(ctx, "req-voice-billing", clientkey.Key{ID: 1, BillingLimitUSDTicks: 1}, voiceModel, audit.OperationTTS, modeldomain.CapabilityTTS, true, audit.PricingResult{}, "", "", nil, func(account.Provider) bool {
		return true
	}, func(context.Context, account.Provider, account.Credential, string) (voiceExecutionResult, error) {
		executed = true
		return voiceExecutionResult{response: jsonVoiceResponse(http.StatusOK, map[string]any{"ok": true})}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !executed {
		t.Fatal("unpriced voice request did not reach the provider")
	}
	result.Finalize(Usage{}, "", "")
	_ = result.Body.Close()
}

func TestVoicePricingSettlesTTSAndRESTSTTUsage(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "voice-pricing.db"))
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
	keyRepo := relational.NewClientKeyRepository(database)
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderConsole, AuthType: account.AuthTypeSSO,
		Name: "console-priced-voice", SourceKey: "console-priced-voice", EncryptedAccessToken: "console-token",
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	const (
		ttsModel = "voice-priced-tts"
		sttModel = "voice-priced-stt"
	)
	if err := modelRepo.UpsertRoutes(ctx, []modeldomain.Route{
		{PublicID: ttsModel, Provider: account.ProviderConsole, UpstreamModel: ttsModel, Capability: modeldomain.CapabilityTTS, Enabled: true},
		{PublicID: sttModel, Provider: account.ProviderConsole, UpstreamModel: sttModel, Capability: modeldomain.CapabilitySTT, Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{ttsModel, sttModel}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	limitedKey, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "voice-priced-key", Prefix: "voice-priced", SecretHash: strings.Repeat("a", 64), EncryptedSecret: "encrypted",
		Enabled: true, BillingLimitUSDTicks: 10_000_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}

	registry := provider.NewRegistry(statelessConsoleAdapter{})
	sticky := memory.NewStickyStore()
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	clientKeyService := clientkeyapp.NewService(keyRepo, nil, nil, 60, 4, nil)
	service := NewService(modelRepo, auditRepo, accountService, clientKeyService, registry, selector, nil, 1)

	ttsPricing, ok := audit.EstimateOfficialTTSCost("Hello 世界")
	if !ok {
		t.Fatal("TTS pricing unavailable")
	}
	ttsResult, err := service.executeVoice(ctx, "req-priced-tts", limitedKey, ttsModel, audit.OperationTTS, modeldomain.CapabilityTTS, true, ttsPricing, "", "", nil, func(account.Provider) bool {
		return true
	}, func(context.Context, account.Provider, account.Credential, string) (voiceExecutionResult, error) {
		return voiceExecutionResult{response: jsonVoiceResponse(http.StatusOK, map[string]any{"ok": true}), pricing: ttsPricing}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ttsResult.Finalize(Usage{}, "", "")
	_ = ttsResult.Body.Close()

	settledKey, err := keyRepo.Get(ctx, limitedKey.ID)
	if err != nil {
		t.Fatal(err)
	}
	if settledKey.BilledUsageUSDTicks != ttsPricing.CostInUSDTicks || settledKey.ReservedUsageUSDTicks != 0 {
		t.Fatalf("TTS billing = billed %d reserved %d, want billed %d reserved 0", settledKey.BilledUsageUSDTicks, settledKey.ReservedUsageUSDTicks, ttsPricing.CostInUSDTicks)
	}

	sttPricing, ok := audit.EstimateOfficialSTTCost(3.45, false)
	if !ok {
		t.Fatal("STT pricing unavailable")
	}
	sttResult, err := service.executeVoice(ctx, "req-priced-stt", limitedKey, sttModel, audit.OperationSTT, modeldomain.CapabilitySTT, true, audit.PricingResult{}, "", "", nil, func(account.Provider) bool {
		return true
	}, func(context.Context, account.Provider, account.Credential, string) (voiceExecutionResult, error) {
		return voiceExecutionResult{response: jsonVoiceResponse(http.StatusOK, map[string]any{"text": "hello"}), pricing: sttPricing}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sttResult.Finalize(Usage{}, "", "")
	_ = sttResult.Body.Close()

	settledKey, err = keyRepo.Get(ctx, limitedKey.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantBilled := ttsPricing.CostInUSDTicks + sttPricing.CostInUSDTicks
	if settledKey.BilledUsageUSDTicks != wantBilled || settledKey.ReservedUsageUSDTicks != 0 {
		t.Fatalf("voice billing = billed %d reserved %d, want billed %d reserved 0", settledKey.BilledUsageUSDTicks, settledKey.ReservedUsageUSDTicks, wantBilled)
	}
	audits, total, err := auditRepo.List(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(audits) != 2 {
		t.Fatalf("voice audits = %d/%d, want 2/2", len(audits), total)
	}
	if audits[0].RequestID != "req-priced-stt" || audits[0].PricingModel != sttPricing.Model || audits[0].EstimatedCostInUSDTicks != sttPricing.CostInUSDTicks {
		t.Fatalf("STT audit = %#v", audits[0])
	}
	if audits[1].RequestID != "req-priced-tts" || audits[1].PricingModel != ttsPricing.Model || audits[1].EstimatedCostInUSDTicks != ttsPricing.CostInUSDTicks {
		t.Fatalf("TTS audit = %#v", audits[1])
	}

	cappedKey, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "voice-capped-key", Prefix: "voice-capped", SecretHash: strings.Repeat("b", 64), EncryptedSecret: "encrypted",
		Enabled: true, BillingLimitUSDTicks: ttsPricing.CostInUSDTicks - 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	executed := false
	_, err = service.executeVoice(ctx, "req-capped-tts", cappedKey, ttsModel, audit.OperationTTS, modeldomain.CapabilityTTS, true, ttsPricing, "", "", nil, func(account.Provider) bool {
		return true
	}, func(context.Context, account.Provider, account.Credential, string) (voiceExecutionResult, error) {
		executed = true
		return voiceExecutionResult{response: jsonVoiceResponse(http.StatusOK, map[string]any{"ok": true}), pricing: ttsPricing}, nil
	})
	if !errors.Is(err, clientkeyapp.ErrBillingLimit) {
		t.Fatalf("capped TTS error = %v, want billing limit", err)
	}
	if executed {
		t.Fatal("TTS reached upstream after exact reservation exceeded the key limit")
	}
}

func TestGenerateImageReturnsWhenEveryCredentialRefreshFails(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "image-credential-failure.db"))
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
	now := time.Now().UTC()
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth,
		Name: "expired-image", SourceKey: "expired-image", EncryptedAccessToken: "expired", EncryptedRefreshToken: "refresh",
		ExpiresAt: now.Add(-time.Minute), Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertRoutes(ctx, []modeldomain.Route{{
		PublicID: "image-credential-failure", Provider: account.ProviderBuild, UpstreamModel: "image-credential-failure",
		Capability: modeldomain.CapabilityImage, Enabled: true,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"image-credential-failure"}, now); err != nil {
		t.Fatal(err)
	}
	adapter := &credentialFailureImageAdapter{}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 1)

	_, err = service.GenerateImage(ctx, ImageGenerationInput{
		RequestID: "req-image-credential-failure", ClientKey: clientkey.Key{ID: 1, Name: "image-key"},
		PublicModel: "image-credential-failure", Prompt: "test", Count: 1, ResponseFormat: "url",
	})
	if !errors.Is(err, ErrNoAvailableAccount) {
		t.Fatalf("error = %v", err)
	}
	if adapter.generationCalls.Load() != 0 {
		t.Fatalf("generation calls = %d", adapter.generationCalls.Load())
	}
}

func TestGenerateImageUnlimitedAttemptsRetainsEgressRetry(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "image-unlimited-egress-retry.db"))
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
	now := time.Now().UTC()
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierSuper,
		Name: "image-egress-retry", SourceKey: "image-egress-retry", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	const model = "grok-image-egress-retry"
	if err := modelRepo.UpsertRoutes(ctx, []modeldomain.Route{{
		PublicID: model, Provider: account.ProviderWeb, UpstreamModel: model,
		Capability: modeldomain.CapabilityImage, Enabled: true,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{model}, now); err != nil {
		t.Fatal(err)
	}
	adapter := &webImageStreamAdapter{forbiddenRemaining: 1}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, unlimitedRoutingAttempts)

	result, err := service.GenerateImage(ctx, ImageGenerationInput{
		RequestID: "req-image-unlimited-egress-retry", ClientKey: clientkey.Key{ID: 1, Name: "image-key"},
		PublicModel: model, Prompt: "test", Count: 1, ResponseFormat: "url",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(result.Body)
	result.Finalize(Usage{}, "", "")
	_ = result.Body.Close()
	if attempts := adapter.Attempts(); len(attempts) != 2 || attempts[0] != credential.ID || attempts[1] != credential.ID {
		t.Fatalf("attempts = %#v, want one egress retry on account %d", attempts, credential.ID)
	}
}

func TestGatewayDoesNotPersistStatelessConsoleResponses(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "console-stateless.db"))
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
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderConsole, AuthType: account.AuthTypeSSO, Name: "console", SourceKey: "console",
		EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	const model = "grok-console-stateless"
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderConsole, []string{model}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{model}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	key, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "console-key", Prefix: "console", SecretHash: strings.Repeat("c", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 60, MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := statelessConsoleAdapter{}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 1)

	result, err := service.CreateResponse(ctx, Input{RequestID: "req-console", ClientKey: key, PublicModel: model, Body: []byte(`{"model":"grok-console-stateless","input":"hello"}`)})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(result.Body)
	result.Finalize(Usage{}, "resp-console", "")
	_ = result.Body.Close()
	if _, err := responseRepo.Get(ctx, "resp-console", key.ID, time.Now().UTC()); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("stateless response ownership err = %v", err)
	}
	continued, err := service.CreateResponse(ctx, Input{RequestID: "req-console-next", ClientKey: key, PublicModel: model, PreviousResponseID: "resp-console", Body: []byte(`{"model":"grok-console-stateless","previous_response_id":"resp-console","input":"hello again"}`)})
	if err != nil {
		t.Fatalf("Console stateless previous response fallback = %v", err)
	}
	_, _ = io.ReadAll(continued.Body)
	continued.Finalize(Usage{}, "resp-console-next", "")
	_ = continued.Body.Close()
	if _, err := service.CompactResponse(ctx, Input{RequestID: "req-console-compact", ClientKey: key, PublicModel: model, Body: []byte(`{"model":"grok-console-stateless","input":"hello"}`)}); !errors.Is(err, ErrConversationUnsupported) {
		t.Fatalf("compact response error = %v", err)
	}

	now := time.Now().UTC()
	if err := responseRepo.Save(ctx, inferencedomain.ResponseOwnership{
		ResponseID: "resp-console-stale", AccountID: credential.ID, ClientKeyID: key.ID, Provider: account.ProviderConsole,
		ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetResponse(ctx, ResourceInput{ClientKey: key, ResponseID: "resp-console-stale"}); !errors.Is(err, ErrResponseNotFound) {
		t.Fatalf("stale console resource error = %v", err)
	}
	if _, err := responseRepo.Get(ctx, "resp-console-stale", key.ID, time.Now().UTC()); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("stale console ownership was not removed: %v", err)
	}
}

func TestGatewayWebOwnershipDoesNotPersistRawPromptCacheKey(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "web-ownership.db"))
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
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: "web", SourceKey: "web",
		EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	const model = "grok-web-ownership"
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderWeb, []string{model}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{model}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	key, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "web-key", Prefix: "web-key", SecretHash: strings.Repeat("d", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 60, MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := webStoredResponseAdapter{}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 1)

	rawKey := strings.Repeat("raw-session-", 16)
	result, err := service.CreateResponse(ctx, Input{
		RequestID: "req-web-ownership", ClientKey: key, PublicModel: model, PromptCacheKey: rawKey,
		Body: []byte(`{"model":"grok-web-ownership","input":"hello"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(result.Body)
	result.Finalize(Usage{}, "resp-web-ownership", "")
	_ = result.Body.Close()
	ownership, err := responseRepo.Get(ctx, "resp-web-ownership", key.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if ownership.AccountID != credential.ID || ownership.Provider != account.ProviderWeb || ownership.PromptCacheKey != "" || ownership.ReasoningReplayKey != "" {
		t.Fatalf("web ownership = %#v", ownership)
	}
}

func TestFinalizationCommitsOwnershipAndLocalQuotaBeforeSlowAudit(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "finalization-order.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	responseRepo := relational.NewResponseRepository(database)
	keyRepo := relational.NewClientKeyRepository(database)
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierBasic,
		Name: "web", SourceKey: "finalization-order", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := accountRepo.SaveQuotaWindows(ctx, credential.ID, account.WebTierBasic, now, []account.QuotaWindow{{
		AccountID: credential.ID, Mode: "fast", Remaining: 5, Total: 10, WindowSeconds: 3600,
		Source: account.QuotaSourceUpstream, SyncedAt: &now,
	}}); err != nil {
		t.Fatal(err)
	}
	const model = "grok-finalization-order"
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderWeb, []string{model}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{model}, now); err != nil {
		t.Fatal(err)
	}
	key, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "key", Prefix: "finalization-order", SecretHash: strings.Repeat("e", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 60, MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := finalizationOrderAdapter{}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, nil, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	audits := &blockingFinalizeAudit{started: make(chan struct{}), release: make(chan struct{})}
	service := NewService(modelRepo, audits, accountService, clientkeyapp.NewService(keyRepo, nil, nil, 60, 4, nil), registry, selector, responseRepo, 1)

	result, err := service.CreateResponse(ctx, Input{RequestID: "req-finalization-order", ClientKey: key, PublicModel: model, Body: []byte(`{"model":"grok-finalization-order","input":"hello"}`)})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(result.Body)
	done := make(chan struct{})
	go func() {
		result.Finalize(Usage{}, "resp-finalization-order", "")
		close(done)
	}()
	select {
	case <-audits.started:
	case <-time.After(time.Second):
		t.Fatal("audit finalization did not start")
	}
	if _, err := responseRepo.Get(ctx, "resp-finalization-order", key.ID, time.Now().UTC()); err != nil {
		t.Fatalf("response ownership was blocked by audit: %v", err)
	}
	windows, err := accountRepo.GetQuotaWindows(ctx, []uint64{credential.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(windows[credential.ID]) != 1 || windows[credential.ID][0].Remaining != 4 {
		t.Fatalf("local quota was blocked by audit: %#v", windows[credential.ID])
	}
	close(audits.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("finalization did not finish")
	}
	_ = result.Body.Close()
}

type blockingFinalizeAudit struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (a *blockingFinalizeAudit) Create(ctx context.Context, _ audit.Record) error {
	a.once.Do(func() { close(a.started) })
	select {
	case <-a.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type finalizationOrderAdapter struct{}

func (finalizationOrderAdapter) Provider() account.Provider { return account.ProviderWeb }
func (finalizationOrderAdapter) Definition() provider.Definition {
	return provider.Definition{
		Provider:     account.ProviderWeb,
		Quota:        provider.QuotaRemoteWindow,
		Conversation: provider.ConversationSurface{Responses: true, StoredResponses: true},
		Inference:    provider.InferencePolicy{Usage: provider.UsageEstimated},
	}
}
func (finalizationOrderAdapter) QuotaMode(string) string { return "fast" }
func (finalizationOrderAdapter) TierOrder(string) []account.WebTier {
	return []account.WebTier{account.WebTierBasic, account.WebTierSuper, account.WebTierHeavy}
}
func (finalizationOrderAdapter) ForwardResponse(context.Context, provider.ResponseResourceRequest) (*provider.Response, error) {
	return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"resp-finalization-order"}`)), QuotaUnits: 1}, nil
}

func TestParseFreeQuotaExhaustion(t *testing.T) {
	body := []byte(`{"error":{"code":"subscription:free-usage-exhausted","message":"tokens (actual/limit): 1065387/1000000; Usage resets over a rolling 24-hour window"}}`)
	used, limit, exhausted := parseFreeQuotaExhaustion(body)
	if !exhausted || used != 1_065_387 || limit != 1_000_000 {
		t.Fatalf("parsed = %d/%d, exhausted = %v", used, limit, exhausted)
	}
	if _, _, exhausted := parseFreeQuotaExhaustion([]byte(`{"error":"rate limited"}`)); exhausted {
		t.Fatal("ordinary 429 body must not be treated as Free quota exhaustion")
	}
}

func TestParseFreeQuotaExhaustionCurrentBuildFreeLimit(t *testing.T) {
	body := []byte(`{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage for model grok-4.5-build-free for now. Usage resets over a rolling 24-hour window — tokens (actual/limit): 537365/500000."}`)
	used, limit, exhausted := parseFreeQuotaExhaustion(body)
	if !exhausted || used != 537_365 || limit != 500_000 {
		t.Fatalf("exhausted=%v used=%d limit=%d", exhausted, used, limit)
	}
}

func TestGatewayUnknownBuildForbiddenTraversesAllAccountsWithoutCooldown(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "systemic-forbidden.db"))
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
	credentials := make([]account.Credential, 0, 3)
	for index, name := range []string{"first", "second", "third"} {
		credential, _, createErr := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderBuild, Name: name, SourceKey: name, EncryptedAccessToken: name,
			ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive,
			Priority: 300 - index, MaxConcurrent: 1,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		credentials = append(credentials, credential)
	}
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderBuild, []string{"grok-systemic"}); err != nil {
		t.Fatal(err)
	}
	for _, credential := range credentials {
		if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-systemic"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "systemic-key", Prefix: "systemic", SecretHash: strings.Repeat("a", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}

	adapter := &systemicForbiddenAdapter{}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 3)

	_, err = service.CreateResponse(ctx, Input{
		RequestID: "req-systemic-403", ClientKey: clientKey, PublicModel: "grok-systemic",
		Body: []byte(`{"model":"grok-systemic","input":"hello"}`),
	})
	var upstreamFailure *UpstreamFailure
	if !errors.As(err, &upstreamFailure) || errors.Is(err, ErrNoAvailableAccount) {
		t.Fatalf("error = %T %v", err, err)
	}
	if upstreamFailure.HTTPStatus != http.StatusForbidden || upstreamFailure.Code != "upstream_forbidden" || upstreamFailure.AccountScoped {
		t.Fatalf("upstream failure = %#v", upstreamFailure)
	}
	attempts := adapter.Attempts()
	if len(attempts) != 3 || attempts[0] != credentials[0].ID || attempts[1] != credentials[1].ID || attempts[2] != credentials[2].ID {
		t.Fatalf("attempts = %#v", attempts)
	}
	for _, credential := range credentials {
		observed, getErr := accountRepo.Get(ctx, credential.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if observed.FailureCount != 0 || observed.CooldownUntil != nil || observed.AuthStatus != account.AuthStatusActive {
			t.Fatalf("account %d was penalized after unknown 403: %#v", credential.ID, observed)
		}
	}
	logs, total, err := auditRepo.List(ctx, 0, 10)
	if err != nil || total != 1 || logs[0].StatusCode != http.StatusForbidden || logs[0].ErrorCode != "upstream_forbidden" || logs[0].AccountID == nil || *logs[0].AccountID != credentials[2].ID {
		t.Fatalf("audit = %#v, total=%d, err=%v", logs, total, err)
	}
}

func TestGatewayRefreshesAndRetriesBuildUnauthorizedOnce(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "auth-rescue.db"))
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
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "rescue", SourceKey: "rescue",
		EncryptedAccessToken: "access-old", EncryptedRefreshToken: "refresh-old", ExpiresAt: time.Now().Add(time.Hour),
		Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 100, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderBuild, []string{"grok-rescue"}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-rescue"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "rescue-key", Prefix: "rescue", SecretHash: strings.Repeat("b", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &authRescueAdapter{}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 2)

	result, err := service.CreateResponse(ctx, Input{
		RequestID: "req-rescue", ClientKey: clientKey, PublicModel: "grok-rescue",
		Body: []byte(`{"model":"grok-rescue","input":"hello"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatal(err)
	}
	result.Finalize(Usage{}, "", "")
	_ = result.Body.Close()
	if string(body) != "ok" || adapter.attempts.Load() != 2 || adapter.refreshes.Load() != 1 {
		t.Fatalf("body=%q attempts=%d refreshes=%d", body, adapter.attempts.Load(), adapter.refreshes.Load())
	}
	updated, err := accountRepo.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.EncryptedAccessToken != "access-new" || updated.AuthStatus != account.AuthStatusActive || updated.RefreshFailureCount != 0 {
		t.Fatalf("updated credential = %#v", updated)
	}
	if err := accountRepo.UpdateCredentialRefreshFailure(ctx, credential.ID, repository.CredentialRefreshFailure{Count: 1, RetryAt: updated.ExpiresAt, Status: 400, Code: "invalid_grant", Message: "Refresh token has expired", Permanent: true}); err != nil {
		t.Fatal(err)
	}
	adapter.rejectAll.Store(true)
	if _, err := service.CreateResponse(ctx, Input{
		RequestID: "req-rejected", ClientKey: clientKey, PublicModel: "grok-rescue",
		Body: []byte(`{"model":"grok-rescue","input":"hello again"}`),
	}); err == nil {
		t.Fatal("rejected access token unexpectedly succeeded")
	}
	rejected, err := accountRepo.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rejected.AuthStatus != account.AuthStatusReauthRequired || adapter.refreshes.Load() != 1 {
		t.Fatalf("rejected credential = %#v, refreshes = %d", rejected, adapter.refreshes.Load())
	}
}

func TestBuildChatPermissionDenialDoesNotInvalidateVideoCredential(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "model-scoped-denial.db"))
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
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "chat-denied-video-valid", SourceKey: "chat-denied-video-valid",
		EncryptedAccessToken: "access-old", EncryptedRefreshToken: "refresh-old", ExpiresAt: time.Now().Add(time.Hour),
		Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 100, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := accountRepo.SaveBilling(ctx, account.Billing{AccountID: credential.ID, MonthlyLimit: 140, SyncedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderBuild, []string{"grok-chat-denied"}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-chat-denied"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "chat-denied-key", Prefix: "chat-denied", SecretHash: strings.Repeat("c", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &authRescueAdapter{}
	adapter.denyChat.Store(true)
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 2)

	if _, err := service.CreateResponse(ctx, Input{
		RequestID: "req-chat-denied", ClientKey: clientKey, PublicModel: "grok-chat-denied",
		Body: []byte(`{"model":"grok-chat-denied","input":"hello"}`),
	}); err == nil {
		t.Fatal("chat permission denial unexpectedly succeeded")
	}
	updated, err := accountRepo.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.AuthStatus != account.AuthStatusActive || updated.FailureCount != 0 || updated.CooldownUntil != nil {
		t.Fatalf("chat denial invalidated the whole credential: %#v", updated)
	}
	if adapter.refreshes.Load() != 0 || adapter.attempts.Load() != 1 {
		t.Fatalf("Build 403 must not refresh OAuth or replay the request: attempts=%d refreshes=%d", adapter.attempts.Load(), adapter.refreshes.Load())
	}
	candidates, err := accountRepo.ListRoutingCandidates(ctx, account.ProviderBuild, 0, "grok-chat-denied", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].ModelQuotaBlock == nil || candidates[0].ModelQuotaBlock.Reason != "model_access_denied" {
		t.Fatalf("model-scoped denial was not persisted: %#v", candidates)
	}

	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderBuild, []string{"grok-chat-denied-opt-in"}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-chat-denied", "grok-chat-denied-opt-in"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	service.UpdateBuildForbiddenReauthPolicy(true, []string{"permission-denied"})
	adapter.recoverDenied.Store(true)
	refreshesBefore := adapter.refreshes.Load()
	result, err := service.CreateResponse(ctx, Input{
		RequestID: "req-chat-denied-opt-in", ClientKey: clientKey, PublicModel: "grok-chat-denied-opt-in",
		Body: []byte(`{"model":"grok-chat-denied-opt-in","input":"hello"}`),
	})
	if err != nil {
		t.Fatalf("recovered permission denial should keep the current request successful: %v", err)
	}
	_ = result.Body.Close()
	result.Finalize(Usage{}, "", "")
	invalidated, err := accountRepo.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if invalidated.AuthStatus != account.AuthStatusReauthRequired {
		t.Fatalf("opt-in denial did not invalidate account: %#v", invalidated)
	}
	if adapter.refreshes.Load() != refreshesBefore {
		t.Fatalf("opt-in denial performed a redundant refresh: before=%d after=%d", refreshesBefore, adapter.refreshes.Load())
	}
}

func TestBuildChatPermissionDenialMarksReauthWhenEnabled(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "chat-denial-reauth.db"))
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
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "chat-denied-reauth", SourceKey: "chat-denied-reauth",
		EncryptedAccessToken: "access-old", EncryptedRefreshToken: "refresh-old", ExpiresAt: time.Now().Add(time.Hour),
		Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 100, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderBuild, []string{"grok-chat-denied"}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-chat-denied"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "chat-denied-reauth-key", Prefix: "chat-denied-reauth", SecretHash: strings.Repeat("d", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &authRescueAdapter{}
	adapter.denyChat.Store(true)
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 2)
	service.UpdateMarkBuildChatDeniedAsReauth(true)

	if _, err := service.CreateResponse(ctx, Input{
		RequestID: "req-chat-denied-reauth", ClientKey: clientKey, PublicModel: "grok-chat-denied",
		Body: []byte(`{"model":"grok-chat-denied","input":"hello"}`),
	}); err == nil {
		t.Fatal("chat permission denial unexpectedly succeeded")
	}
	updated, err := accountRepo.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.AuthStatus != account.AuthStatusReauthRequired {
		t.Fatalf("expected reauthRequired, got %#v", updated)
	}
	candidates, err := accountRepo.ListRoutingCandidates(ctx, account.ProviderBuild, 0, "grok-chat-denied", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("reauth account should leave routing pool: %#v", candidates)
	}
}

func TestSpendingLimitBlockedMarksQuotaRecovery(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "spending-limit-quota.db"))
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
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "spending-limit", SourceKey: "spending-limit",
		EncryptedAccessToken: "access-old", EncryptedRefreshToken: "refresh-old", ExpiresAt: time.Now().Add(time.Hour),
		Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 100, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderBuild, []string{"grok-paid"}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-paid"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "spending-limit-key", Prefix: "spending-limit", SecretHash: strings.Repeat("s", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &spendingLimitAdapter{}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 2)

	if _, err := service.CreateResponse(ctx, Input{
		RequestID: "req-spending-limit", ClientKey: clientKey, PublicModel: "grok-paid",
		Body: []byte(`{"model":"grok-paid","input":"hello"}`),
	}); err == nil {
		t.Fatal("spending limit block unexpectedly succeeded")
	}
	updated, err := accountRepo.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.AuthStatus != account.AuthStatusActive {
		t.Fatalf("spending limit should not invalidate credentials, got %#v", updated)
	}
	recovery, err := accountRepo.GetQuotaRecovery(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovery.Kind != account.QuotaRecoveryKindFree || recovery.Status != account.QuotaRecoveryStatusExhausted || recovery.NextProbeAt == nil {
		t.Fatalf("unexpected quota recovery: %#v", recovery)
	}

	_, err = selector.beginSelectionSession(ctx, account.ProviderBuild, 0, "grok-paid", "", "", map[uint64]bool{}, false)
	var unavailable *SelectionUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("expected selection unavailable, got %v", err)
	}
	if unavailable.Reason != SelectionQuotaExhausted {
		t.Fatalf("selection reason = %s, want %s", unavailable.Reason, SelectionQuotaExhausted)
	}
}

func TestWebRateLimitExhaustsOnlyRequestedQuotaMode(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "web-rate-limit.db"))
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
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierSuper,
		Name: "web", SourceKey: "web", EncryptedAccessToken: "encrypted", Enabled: true,
		AuthStatus: account.AuthStatusActive, MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := accountRepo.SaveQuotaWindows(ctx, credential.ID, account.WebTierSuper, now, []account.QuotaWindow{
		{AccountID: credential.ID, Mode: "fast", Remaining: 3, Total: 20, WindowSeconds: 3600, Source: account.QuotaSourceUpstream},
		{AccountID: credential.ID, Mode: "auto", Remaining: 4, Total: 10, WindowSeconds: 3600, Source: account.QuotaSourceUpstream},
	}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderWeb, []string{"grok-web-test"}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-web-test"}, now); err != nil {
		t.Fatal(err)
	}
	key, err := keyRepo.Create(ctx, clientkey.Key{Name: "key", Prefix: "web-key", SecretHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", EncryptedSecret: "encrypted-key", Enabled: true, RPMLimit: 60, MaxConcurrent: 4})
	if err != nil {
		t.Fatal(err)
	}
	adapter := webRateLimitAdapter{}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	accountService.SetQuotaRecoveryQueue(memory.NewQuotaRecoveryQueue())
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 1)
	if _, err := service.CreateResponse(ctx, Input{RequestID: "req-web-429", ClientKey: key, PublicModel: "grok-web-test", Body: []byte(`{"model":"grok-web-test"}`)}); err == nil {
		t.Fatal("expected rate-limited request to fail")
	}
	windows, err := accountRepo.GetQuotaWindows(ctx, []uint64{credential.ID})
	if err != nil {
		t.Fatal(err)
	}
	remaining := map[string]int{}
	for _, window := range windows[credential.ID] {
		remaining[window.Mode] = window.Remaining
	}
	if remaining["fast"] != 0 || remaining["auto"] != 4 {
		t.Fatalf("quota remaining = %#v", remaining)
	}
	if _, err := accountRepo.GetQuotaRecovery(ctx, credential.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("Web 429 must not create Build quota recovery state: %v", err)
	}
}

func TestImageStreamPropagatesWithoutTouchingChatQuota(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "image-stream.db"))
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
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierSuper,
		Name: "web-image", SourceKey: "web-image", EncryptedAccessToken: "encrypted", Enabled: true,
		AuthStatus: account.AuthStatusActive, MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := accountRepo.SaveQuotaWindows(ctx, credential.ID, account.WebTierSuper, now, []account.QuotaWindow{{
		AccountID: credential.ID, Mode: "fast", Remaining: 3, Total: 10,
		WindowSeconds: 3600, Source: account.QuotaSourceUpstream, SyncedAt: &now,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertRoutes(ctx, []modeldomain.Route{
		{PublicID: "grok-imagine-image", Provider: account.ProviderWeb, UpstreamModel: "grok-imagine-image-quality", Capability: modeldomain.CapabilityImage, Enabled: true},
		{PublicID: "grok-imagine-image-lite", Provider: account.ProviderWeb, UpstreamModel: "grok-imagine-image", Capability: modeldomain.CapabilityImage, Enabled: true},
		{PublicID: "grok-imagine-image-edit", Provider: account.ProviderWeb, UpstreamModel: "grok-imagine-image-edit", Capability: modeldomain.CapabilityImageEdit, Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-imagine-image-quality", "grok-imagine-image", "grok-imagine-image-edit"}, now); err != nil {
		t.Fatal(err)
	}
	key, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "image-key", Prefix: "image-key", SecretHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", EncryptedSecret: "encrypted-key",
		Enabled: true, RPMLimit: 60, MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &webImageStreamAdapter{synced: make(chan string, 1)}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	runQuotaRefreshWorkers(t, accountService)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(keyRepo, nil, nil, 60, 4, nil), registry, selector, responseRepo, 1)

	result, err := service.GenerateImage(ctx, ImageGenerationInput{
		RequestID: "req-image-stream", ClientKey: key, PublicModel: "grok-imagine-image",
		Prompt: "test", Count: 1, Resolution: "2k", Quality: "medium", ResponseFormat: "url", Streaming: true, PartialImages: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "event: image_generation.completed\ndata: {}\n\ndata: [DONE]\n\n" {
		t.Fatalf("stream body = %q", body)
	}
	if !adapter.Streaming() || adapter.PartialImages() != 1 {
		t.Fatalf("image stream options were not propagated: streaming=%t partial_images=%d", adapter.Streaming(), adapter.PartialImages())
	}
	if logs, total, err := auditRepo.List(ctx, 0, 10); err != nil || total != 0 || len(logs) != 0 {
		t.Fatalf("audit persisted before finalization: logs=%#v total=%d err=%v", logs, total, err)
	}
	result.Finalize(Usage{}, "", "")
	_ = result.Body.Close()

	logs, total, err := auditRepo.List(ctx, 0, 10)
	if err != nil || total != 1 || len(logs) != 1 {
		t.Fatalf("audit logs=%#v total=%d err=%v", logs, total, err)
	}
	if !logs[0].Streaming || logs[0].Operation != "image" || logs[0].Provider != string(account.ProviderWeb) || logs[0].ErrorCode != "" ||
		logs[0].MediaInputImages != 0 || logs[0].MediaOutputImages != 1 ||
		logs[0].PricingModel != "grok-imagine-image-quality-1k" || logs[0].EstimatedCostInUSDTicks != 500_000_000 {
		t.Fatalf("audit = %#v", logs[0])
	}
	windows, err := accountRepo.GetQuotaWindows(ctx, []uint64{credential.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(windows[credential.ID]) != 1 || windows[credential.ID][0].Remaining != 3 {
		t.Fatalf("quota windows = %#v", windows[credential.ID])
	}

	liteResult, err := service.GenerateImage(ctx, ImageGenerationInput{
		RequestID: "req-image-lite", ClientKey: key, PublicModel: "grok-imagine-image-lite",
		Prompt: "test", Count: 1, ResponseFormat: "url",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(liteResult.Body); err != nil {
		t.Fatal(err)
	}
	liteResult.Finalize(Usage{}, "", "")
	_ = liteResult.Body.Close()
	logs, total, err = auditRepo.List(ctx, 0, 10)
	if err != nil || total != 2 || logs[0].RequestID != "req-image-lite" || logs[0].PricingModel != "grok-imagine-image" || logs[0].EstimatedCostInUSDTicks != 200_000_000 {
		t.Fatalf("Lite image pricing audit = %#v, total=%d, err=%v", logs, total, err)
	}
	select {
	case mode := <-adapter.synced:
		if mode != "fast" {
			t.Fatalf("Lite image synced mode = %q", mode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Lite image quota refresh")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		windows, err = accountRepo.GetQuotaWindows(ctx, []uint64{credential.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(windows[credential.ID]) == 1 && windows[credential.ID][0].Remaining == 8 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Lite image quota was not refreshed: %#v", windows[credential.ID])
		}
		time.Sleep(10 * time.Millisecond)
	}

	chatResult, err := service.CreateChatCompletion(ctx, Input{
		RequestID: "req-image-lite-chat", ClientKey: key, PublicModel: "grok-imagine-image-lite",
		Body: []byte(`{"model":"grok-imagine-image-lite","messages":[{"role":"user","content":"draw"}],"image_config":{"n":3}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(chatResult.Body)
	chatResult.Finalize(Usage{}, "resp-image-lite", "")
	_ = chatResult.Body.Close()
	logs, total, err = auditRepo.List(ctx, 0, 10)
	if err != nil || total != 3 || logs[0].RequestID != "req-image-lite-chat" || logs[0].MediaOutputImages != 3 || logs[0].PricingModel != "grok-imagine-image" || logs[0].EstimatedCostInUSDTicks != 600_000_000 {
		t.Fatalf("Lite Chat pricing audit = %#v, total=%d, err=%v", logs, total, err)
	}

	editResult, err := service.EditImage(ctx, ImageEditInput{
		RequestID: "req-image-edit", ClientKey: key, PublicModel: "grok-imagine-image-edit",
		Prompt: "edit", ImageURLs: []string{"data:image/png;base64,a", "data:image/png;base64,b"},
		Count: 3, Size: "1024x1024", AspectRatio: "1:1", Resolution: "2k", ResponseFormat: "url",
		Streaming: true, PartialImages: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(editResult.Body)
	editResult.Finalize(Usage{}, "", "")
	_ = editResult.Body.Close()
	logs, total, err = auditRepo.List(ctx, 0, 10)
	if err != nil || total != 4 || logs[0].RequestID != "req-image-edit" || logs[0].MediaInputImages != 2 || logs[0].MediaOutputImages != 3 || logs[0].PricingModel != "grok-imagine-image-edit-2k" || logs[0].EstimatedCostInUSDTicks != 2_300_000_000 {
		t.Fatalf("image edit pricing audit = %#v, total=%d, err=%v", logs, total, err)
	}
	editRequest := adapter.EditRequest()
	if editRequest.Resolution != "2k" || editRequest.Size != "1024x1024" || editRequest.AspectRatio != "1:1" || !editRequest.Streaming || editRequest.PartialImages != 2 {
		t.Fatalf("image edit request = %#v", editRequest)
	}

	billingBeforeFailure, err := keyRepo.Get(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	backupCredential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierSuper,
		Name: "web-image-backup", SourceKey: "web-image-backup", EncryptedAccessToken: "encrypted-backup", Enabled: true,
		AuthStatus: account.AuthStatusActive, MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, backupCredential.ID, []string{"grok-imagine-image-quality"}, now); err != nil {
		t.Fatal(err)
	}
	selector.MarkQuotaStateChanged(account.ProviderWeb)
	service.UpdateMaxAttempts(3)
	attemptsBeforeFailure := len(adapter.Attempts())
	adapter.FailWithEgress(infraegress.NewManager(relational.NewEgressRepository(database), testCipher(t)))
	failureCtx := requestmeta.WithClientIP(ctx, "203.0.113.51")
	if _, err := service.GenerateImage(failureCtx, ImageGenerationInput{
		RequestID: "req-image-failed", ClientKey: key, PublicModel: "grok-imagine-image",
		Prompt: "test", Count: 1, Resolution: "1k", ResponseFormat: "url",
	}); err == nil {
		t.Fatal("expected image transport failure")
	}
	if attempts := adapter.Attempts(); len(attempts) != attemptsBeforeFailure+1 {
		t.Fatalf("image failure switched accounts after generation started: %#v", attempts)
	}
	logs, total, err = auditRepo.List(ctx, 0, 10)
	if err != nil || total != 5 || len(logs) != 5 {
		t.Fatalf("failure audit logs=%#v total=%d err=%v", logs, total, err)
	}
	failureAudit := logs[0]
	if failureAudit.ClientIP != "203.0.113.51" || failureAudit.RequestID != "req-image-failed" || failureAudit.StatusCode != http.StatusBadGateway || failureAudit.ErrorCode != "upstream_unavailable" || failureAudit.MediaOutputImages != 0 || failureAudit.EstimatedCostInUSDTicks != 0 || failureAudit.EgressMode != audit.EgressModeDirect || failureAudit.EgressScope != string(egressdomain.ScopeWeb) || failureAudit.EgressNodeName != "direct" {
		t.Fatalf("failure audit = %#v", failureAudit)
	}
	updatedKey, err := keyRepo.Get(ctx, key.ID)
	if err != nil || updatedKey.ReservedUsageUSDTicks != 0 || updatedKey.BilledUsageUSDTicks != billingBeforeFailure.BilledUsageUSDTicks {
		t.Fatalf("failed image billing key = %#v, err = %v", updatedKey, err)
	}
}

func TestWebImageUnauthorizedMarksInvalidAndSwitchesAccount(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "web-image-401.db"))
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
	credentials := make([]account.Credential, 0, 2)
	for index, name := range []string{"rejected-image", "healthy-image"} {
		credential, _, createErr := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierSuper,
			Name: name, SourceKey: name, EncryptedAccessToken: "encrypted-" + name,
			Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 200 - index*100, MaxConcurrent: 1,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		credentials = append(credentials, credential)
	}
	now := time.Now().UTC()
	if err := modelRepo.UpsertRoutes(ctx, []modeldomain.Route{{
		PublicID: "grok-image-401", Provider: account.ProviderWeb, UpstreamModel: "grok-image-401",
		Capability: modeldomain.CapabilityImage, Enabled: true,
	}}); err != nil {
		t.Fatal(err)
	}
	for _, credential := range credentials {
		if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-image-401"}, now); err != nil {
			t.Fatal(err)
		}
	}
	key, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "image-401-key", Prefix: "image-401", SecretHash: strings.Repeat("f", 64), EncryptedSecret: "encrypted-key",
		Enabled: true, RPMLimit: 60, MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &webImageStreamAdapter{synced: make(chan string, 1), unauthorizedID: credentials[0].ID}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(keyRepo, nil, nil, 60, 4, nil), registry, selector, responseRepo, 2)

	result, err := service.GenerateImage(ctx, ImageGenerationInput{
		RequestID: "req-image-401", ClientKey: key, PublicModel: "grok-image-401", Prompt: "test", Count: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(result.Body)
	result.Finalize(Usage{}, "", "")
	_ = result.Body.Close()
	if attempts := adapter.Attempts(); len(attempts) != 2 || attempts[0] != credentials[0].ID || attempts[1] != credentials[1].ID {
		t.Fatalf("attempts = %#v", attempts)
	}
	rejected, err := accountRepo.Get(ctx, credentials[0].ID)
	if err != nil || rejected.AuthStatus != account.AuthStatusReauthRequired || !rejected.Enabled {
		t.Fatalf("rejected account = %#v, err = %v", rejected, err)
	}
}

func TestSuccessfulWebChatRefreshesCurrentModeQuota(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "chat-quota-refresh.db"))
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
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierBasic,
		Name: "web-chat", SourceKey: "web-chat", EncryptedAccessToken: "encrypted", Enabled: true,
		AuthStatus: account.AuthStatusActive, MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := accountRepo.SaveQuotaWindows(ctx, credential.ID, account.WebTierBasic, now, []account.QuotaWindow{{
		AccountID: credential.ID, Mode: "fast", Remaining: 3, Total: 20,
		WindowSeconds: 3600, Source: account.QuotaSourceUpstream, SyncedAt: &now,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertRoutes(ctx, []modeldomain.Route{{
		PublicID: "grok-chat-fast", Provider: account.ProviderWeb, UpstreamModel: "grok-chat-fast",
		Capability: modeldomain.CapabilityChat, Enabled: true,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-chat-fast"}, now); err != nil {
		t.Fatal(err)
	}
	key, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "chat-key", Prefix: "chat-key", SecretHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", EncryptedSecret: "encrypted-key",
		Enabled: true, RPMLimit: 60, MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &webChatQuotaAdapter{synced: make(chan string, 1)}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	runQuotaRefreshWorkers(t, accountService)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(keyRepo, nil, nil, 60, 4, nil), registry, selector, responseRepo, 1)

	result, err := service.CreateChatCompletion(ctx, Input{
		RequestID: "req-chat-quota", ClientKey: key, PublicModel: "grok-chat-fast",
		Body: []byte(`{"model":"grok-chat-fast","messages":[{"role":"user","content":"hi"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(result.Body); err != nil {
		t.Fatal(err)
	}
	result.Finalize(Usage{}, "", "")
	_ = result.Body.Close()

	select {
	case mode := <-adapter.synced:
		if mode != "fast" {
			t.Fatalf("synced mode = %q", mode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for post-success quota refresh")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		windows, err := accountRepo.GetQuotaWindows(ctx, []uint64{credential.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(windows[credential.ID]) == 1 && windows[credential.ID][0].Remaining == 17 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("quota windows were not refreshed: %#v", windows[credential.ID])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func runQuotaRefreshWorkers(t *testing.T, service *accountapp.Service) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		service.RunQuotaRefresh(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("quota refresh workers did not stop")
		}
	})
}

type failoverAdapter struct {
	mu                     sync.Mutex
	firstID                uint64
	failureIDs             map[uint64]bool
	failureStatus          int
	failureBody            string
	failureHeader          http.Header
	attempts               []uint64
	lastMethod             string
	lastPath               string
	lastPromptCacheKey     string
	lastReasoningReplayKey string
	lastGrokTurnIndex      string
	lastOperation          string
	reasoningEffort        string
	forwardedModels        []string
	resourceStatus         int
	transportErrorIDs      map[uint64]error
}

type ssoFailureAdapter struct {
	mu            sync.Mutex
	providerValue account.Provider
	rejectedID    uint64
	failureStatus int
	failureBody   string
	attempts      []uint64
}

func (a *ssoFailureAdapter) Provider() account.Provider { return a.providerValue }
func (a *ssoFailureAdapter) Definition() provider.Definition {
	return testConversationDefinition(a.providerValue)
}
func (a *ssoFailureAdapter) ForwardResponse(_ context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	a.mu.Lock()
	a.attempts = append(a.attempts, request.Credential.ID)
	a.mu.Unlock()
	if request.Credential.ID == a.rejectedID {
		status := a.failureStatus
		if status == 0 {
			status = http.StatusUnauthorized
		}
		body := a.failureBody
		if body == "" {
			body = `{"error":{"code":"unauthorized","message":"credential rejected"}}`
		}
		return &provider.Response{
			StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)),
		}, nil
	}
	return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
}
func (a *ssoFailureAdapter) Attempts() []uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]uint64(nil), a.attempts...)
}

type statelessConsoleAdapter struct{}

type transientEgressForbiddenAdapter struct {
	calls atomic.Int64
}

func (a *transientEgressForbiddenAdapter) Provider() account.Provider { return account.ProviderWeb }
func (a *transientEgressForbiddenAdapter) Definition() provider.Definition {
	return testConversationDefinition(account.ProviderWeb)
}
func (a *transientEgressForbiddenAdapter) ForwardResponse(context.Context, provider.ResponseResourceRequest) (*provider.Response, error) {
	if a.calls.Add(1) == 1 {
		return &provider.Response{
			StatusCode: http.StatusForbidden, Status: "403 Forbidden", Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"error":"egress session rejected"}`)),
		}, nil
	}
	return &provider.Response{
		StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(`{"id":"resp-egress-retry","status":"completed"}`)),
	}, nil
}

type teamModelRateLimitConsoleAttempt struct {
	AccountID uint64
	Model     string
}

type teamModelRateLimitConsoleAdapter struct {
	mu              sync.Mutex
	attempts        []teamModelRateLimitConsoleAttempt
	rateLimitedTeam string
}

func (statelessConsoleAdapter) Provider() account.Provider { return account.ProviderConsole }
func (statelessConsoleAdapter) Definition() provider.Definition {
	return provider.Definition{
		Provider: account.ProviderConsole,
		Conversation: provider.ConversationSurface{
			Responses: true,
		},
	}
}
func (statelessConsoleAdapter) ForwardResponse(context.Context, provider.ResponseResourceRequest) (*provider.Response, error) {
	return &provider.Response{
		StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"id":"resp-console","object":"response","status":"completed"}`)),
	}, nil
}

func (a *teamModelRateLimitConsoleAdapter) Provider() account.Provider {
	return account.ProviderConsole
}
func (a *teamModelRateLimitConsoleAdapter) Definition() provider.Definition {
	return testConversationDefinition(account.ProviderConsole)
}
func (a *teamModelRateLimitConsoleAdapter) ForwardResponse(_ context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	a.mu.Lock()
	a.attempts = append(a.attempts, teamModelRateLimitConsoleAttempt{AccountID: request.Credential.ID, Model: request.Model})
	a.mu.Unlock()
	if request.Credential.TeamID != a.rateLimitedTeam {
		return &provider.Response{
			StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"id":"resp-team-success","object":"response","status":"completed"}`)),
		}, nil
	}
	return &provider.Response{
		StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests", Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"error":"team model rate limited"}`)),
		RateLimit: &provider.RateLimitMetadata{
			Scope: provider.RateLimitScopeRPM, TeamID: request.Credential.TeamID, Model: request.Model,
			Actual: 61, Limit: 60, RetryAfter: time.Hour,
		},
	}, nil
}
func (a *teamModelRateLimitConsoleAdapter) Attempts() []teamModelRateLimitConsoleAttempt {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]teamModelRateLimitConsoleAttempt(nil), a.attempts...)
}

func TestGatewaySafetyRejectionDoesNotTouchAccountState(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "safety-rejection.db"))
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

	credentials := make([]account.Credential, 0, 2)
	for index, name := range []string{"safety-a", "safety-b"} {
		credential, _, createErr := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderBuild, Name: name, SourceKey: name, EncryptedAccessToken: name,
			EncryptedRefreshToken: "refresh-" + name, ExpiresAt: time.Now().Add(time.Hour),
			Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 200 - index, MaxConcurrent: 1,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		credentials = append(credentials, credential)
	}
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderBuild, []string{"grok-safety"}); err != nil {
		t.Fatal(err)
	}
	for _, credential := range credentials {
		if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-safety"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "safety-key", Prefix: "safety", SecretHash: strings.Repeat("e", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}

	body := `{"code":"permission-denied","error":"Content violates usage guidelines. SAFETY_CHECK_TYPE_VIOLENCE"}`
	adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{
		credentials[0].ID: {{status: http.StatusForbidden, body: body}},
		credentials[1].ID: {{status: http.StatusOK, body: `{"id":"resp-should-not-run"}`}},
	}}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 3)
	service.UpdateBuildForbiddenReauthPolicy(true, []string{"permission-denied"})

	result, err := service.CreateResponse(ctx, Input{
		RequestID: "req-safety", ClientKey: clientKey, PublicModel: "grok-safety",
		Body: []byte(`{"model":"grok-safety","input":"hello"}`),
	})
	if err != nil {
		t.Fatalf("safety rejection should return the upstream 403 response, err=%v", err)
	}
	if result.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d", result.StatusCode)
	}
	responseBody, _ := io.ReadAll(result.Body)
	result.Finalize(Usage{}, "", "upstream_forbidden")
	_ = result.Body.Close()
	if !strings.Contains(string(responseBody), "Content violates usage guidelines") {
		t.Fatalf("body = %s", responseBody)
	}
	if attempts := adapter.Attempts(); len(attempts) != 1 || attempts[0] != credentials[0].ID {
		t.Fatalf("safety rejection must use exactly one physical request, attempts=%#v", attempts)
	}
	if adapter.refreshes.Load() != 0 {
		t.Fatalf("safety rejection refreshed OAuth: %d", adapter.refreshes.Load())
	}
	for _, credential := range credentials {
		observed, getErr := accountRepo.Get(ctx, credential.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if observed.AuthStatus != account.AuthStatusActive || observed.FailureCount != 0 || observed.CooldownUntil != nil {
			t.Fatalf("account %d changed after safety rejection: %#v", credential.ID, observed)
		}
		candidates, listErr := accountRepo.ListRoutingCandidates(ctx, account.ProviderBuild, 0, "grok-safety", "")
		if listErr != nil {
			t.Fatal(listErr)
		}
		for _, candidate := range candidates {
			if candidate.Credential.ID == credential.ID && candidate.ModelQuotaBlock != nil {
				t.Fatalf("safety rejection must not mark model access denied: %#v", candidate.ModelQuotaBlock)
			}
		}
	}
}

func TestGatewayConsoleDPoPRequirementStopsAfterOneAccount(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "console-dpop-required.db"))
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

	credentials := make([]account.Credential, 0, 2)
	for index, name := range []string{"console-dpop-a", "console-dpop-b"} {
		credential, _, createErr := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderConsole, AuthType: account.AuthTypeSSO,
			Name: name, SourceKey: name, EncryptedAccessToken: name,
			Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 200 - index, MaxConcurrent: 1,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		credentials = append(credentials, credential)
	}
	const modelName = "grok-console-dpop"
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderConsole, []string{modelName}); err != nil {
		t.Fatal(err)
	}
	for _, credential := range credentials {
		if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{modelName}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}

	adapter := &dpopRequiredConsoleAdapter{}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 3)

	result, err := service.CreateResponse(ctx, Input{
		RequestID: "req-console-dpop", ClientKey: clientkey.Key{ID: 1, Name: "console-dpop-key"}, PublicModel: modelName,
		Body: []byte(`{"model":"grok-console-dpop","input":"hello"}`),
	})
	if err != nil {
		t.Fatalf("DPoP requirement should return the upstream response, err=%v", err)
	}
	if result.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d", result.StatusCode)
	}
	responseBody, _ := io.ReadAll(result.Body)
	result.Finalize(Usage{}, "", "upstream_forbidden_unauthorized_dpop_required")
	_ = result.Body.Close()
	if !strings.Contains(string(responseBody), "dpop-required") {
		t.Fatalf("body = %s", responseBody)
	}
	if attempts := adapter.Attempts(); len(attempts) != 1 || attempts[0] != credentials[0].ID {
		t.Fatalf("DPoP requirement must use exactly one account, attempts=%#v", attempts)
	}
	for _, credential := range credentials {
		observed, getErr := accountRepo.Get(ctx, credential.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if observed.AuthStatus != account.AuthStatusActive || observed.FailureCount != 0 || observed.CooldownUntil != nil {
			t.Fatalf("account %d changed after DPoP requirement: %#v", credential.ID, observed)
		}
	}
}

func TestGatewayFreeUsageExhaustionFailsOverToAnotherAccount(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "free-usage-failover.db"))
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

	credentials := make([]account.Credential, 0, 2)
	for index, name := range []string{"free-a", "free-b"} {
		credential, _, createErr := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderBuild, Name: name, SourceKey: name, EncryptedAccessToken: name,
			ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive,
			Priority: 200 - index, MaxConcurrent: 1,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		credentials = append(credentials, credential)
	}
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderBuild, []string{"grok-free-usage"}); err != nil {
		t.Fatal(err)
	}
	for _, credential := range credentials {
		if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-free-usage"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "free-key", Prefix: "free", SecretHash: strings.Repeat("f", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	periodEnd := time.Now().UTC().Add(7 * 24 * time.Hour).Truncate(time.Second)
	if err := accountRepo.SaveBilling(ctx, account.Billing{
		AccountID: credentials[0].ID, PlanName: "free", BillingPeriodEnd: periodEnd.Format(time.RFC3339), SyncedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	exhausted := `{"code":"subscription:free-usage-exhausted","error":"tokens (actual/limit): 10/10; Usage resets over a rolling 24-hour window"}`
	adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{
		credentials[0].ID: {{status: http.StatusTooManyRequests, body: exhausted, header: http.Header{"X-Should-Retry": {"false"}}}},
		credentials[1].ID: {{status: http.StatusOK, body: `{"id":"resp-free-b"}`}},
	}}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 3)

	exhaustedAt := time.Now().UTC()
	result, err := service.CreateResponse(ctx, Input{
		RequestID: "req-free-usage", ClientKey: clientKey, PublicModel: "grok-free-usage",
		Body: []byte(`{"model":"grok-free-usage","input":"hello"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", result.StatusCode)
	}
	result.Finalize(Usage{}, "resp-free-b", "")
	_ = result.Body.Close()
	if attempts := adapter.Attempts(); len(attempts) != 2 || attempts[0] != credentials[0].ID || attempts[1] != credentials[1].ID {
		t.Fatalf("free-usage must fail over A->B, attempts=%#v", attempts)
	}
	candidates, err := accountRepo.ListRoutingCandidates(ctx, account.ProviderBuild, 0, "grok-free-usage", "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, candidate := range candidates {
		if candidate.Credential.ID == credentials[0].ID {
			found = true
			if candidate.ModelQuotaBlock != nil {
				t.Fatalf("subscription quota must not create a model block: %#v", candidate.ModelQuotaBlock)
			}
			if candidate.QuotaRecovery == nil || candidate.QuotaRecovery.Kind != account.QuotaRecoveryKindFree || candidate.QuotaRecovery.Status != account.QuotaRecoveryStatusExhausted || candidate.QuotaRecovery.ConfirmedUsed != 10 || candidate.QuotaRecovery.ConfirmedLimit != 10 {
				t.Fatalf("account A quota recovery = %#v", candidate.QuotaRecovery)
			}
			assertRecoveryDelay(t, *candidate.QuotaRecovery, exhaustedAt, defaultFreeQuotaRecoveryPause)
		}
	}
	if !found {
		t.Fatal("account A missing from candidates snapshot")
	}
}

func TestGatewayBuildTeamRPSRateLimitSwitchesTeam(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "build-team-rps.db"))
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

	teamX := "00000000-0000-0000-0000-0000000000a1"
	teamY := "00000000-0000-0000-0000-0000000000b2"
	staleTeam := "00000000-0000-0000-0000-0000000000c3"
	credentials := make([]account.Credential, 0, 3)
	// A has stale credential metadata but reports Team X in the 429 body. B also
	// uses Team X and C uses Team Y. Both A and B must be skipped after the limit.
	for index, seed := range []struct {
		name   string
		teamID string
	}{{"build-team-x-a", staleTeam}, {"build-team-x-b", teamX}, {"build-team-y-c", teamY}} {
		credential, _, createErr := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderBuild, Name: seed.name, SourceKey: seed.name, TeamID: seed.teamID,
			EncryptedAccessToken: "token-" + seed.name, ExpiresAt: time.Now().Add(time.Hour),
			Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 300 - index, MaxConcurrent: 1,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		credentials = append(credentials, credential)
	}
	model := "grok-build-team-rps"
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderBuild, []string{model}); err != nil {
		t.Fatal(err)
	}
	for _, credential := range credentials {
		if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{model}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "team-rps-key", Prefix: "teamrps", SecretHash: strings.Repeat("1", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Body TeamID is the source of truth for the rate-limit key (even if credential metadata drifts).
	limited := `{"code":"resource-exhausted","error":"Too many requests for team ` + teamX + ` and model ` + model + `. Requests per Second (actual/limit): 2/2."}`
	adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{
		credentials[0].ID: {{status: http.StatusTooManyRequests, body: limited}},
		credentials[1].ID: {{status: http.StatusOK, body: `{"id":"resp-should-skip-same-team"}`}},
		credentials[2].ID: {{status: http.StatusOK, body: `{"id":"resp-team-y"}`}},
	}}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 3)

	result, err := service.CreateResponse(ctx, Input{
		RequestID: "req-team-rps", ClientKey: clientKey, PublicModel: model,
		Body: []byte(`{"model":"` + model + `","input":"hello"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", result.StatusCode)
	}
	result.Finalize(Usage{}, "resp-team-y", "")
	_ = result.Body.Close()
	if attempts := adapter.Attempts(); len(attempts) != 2 || attempts[0] != credentials[0].ID || attempts[1] != credentials[2].ID {
		t.Fatalf("team RPS must skip same-team B and choose different-team C, attempts=%#v want A then C", attempts)
	}
	if adapter.lastRateLimit == nil || adapter.lastRateLimit.TeamID != teamX || adapter.lastRateLimit.Model != model || adapter.lastRateLimit.Scope != provider.RateLimitScopeRPS || adapter.lastRateLimit.Actual != 2 || adapter.lastRateLimit.Limit != 2 {
		t.Fatalf("rate limit metadata = %#v", adapter.lastRateLimit)
	}
	// Second request must keep skipping Team X (A and B) via cached Team+Model limit.
	before := len(adapter.Attempts())
	result, err = service.CreateResponse(ctx, Input{
		RequestID: "req-team-rps-cached", ClientKey: clientKey, PublicModel: model,
		Body: []byte(`{"model":"` + model + `","input":"again"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	result.Finalize(Usage{}, "", "")
	_ = result.Body.Close()
	attempts := adapter.Attempts()
	if len(attempts) != before+1 || attempts[len(attempts)-1] != credentials[2].ID {
		t.Fatalf("cached Team X limit should go straight to Team Y, attempts=%#v", attempts)
	}
}

func TestActiveTeamModelRateLimitFallsBackToCurrentCredentialTeam(t *testing.T) {
	now := time.Now().UTC()
	const model = "grok-team-fallback"
	const observedTeam = "00000000-0000-0000-0000-0000000000e5"
	const currentTeam = "00000000-0000-0000-0000-0000000000f6"
	credential := account.Credential{ID: 42, Provider: account.ProviderBuild, TeamID: currentTeam}
	currentFingerprint := rateLimitTeamFingerprint(currentTeam)
	service := &Service{
		rateLimits: map[string]teamModelRateLimit{
			teamModelRateLimitKey(account.ProviderBuild, currentFingerprint, model): {
				TeamFingerprint: shortTeamFingerprint(currentFingerprint), Until: now.Add(time.Minute),
			},
		},
		rateLimitTeams: map[uint64]teamRateLimitObservation{
			credential.ID: {Fingerprint: rateLimitTeamFingerprint(observedTeam), ExpiresAt: now.Add(time.Minute)},
		},
	}
	service.rateLimitActive.Store(true)

	limited, ok := service.activeTeamModelRateLimit(credential, model, now)
	if !ok || limited.TeamFingerprint != shortTeamFingerprint(currentFingerprint) {
		t.Fatalf("limit = %#v, ok=%v", limited, ok)
	}
}

func TestActiveTeamModelRateLimitDropsExpiredObservedTeam(t *testing.T) {
	now := time.Now().UTC()
	const model = "grok-team-observation-expiry"
	const observedTeam = "00000000-0000-0000-0000-0000000000A1"
	const currentTeam = "00000000-0000-0000-0000-0000000000b2"
	credential := account.Credential{ID: 43, Provider: account.ProviderBuild, TeamID: currentTeam}
	currentFingerprint := rateLimitTeamFingerprint(currentTeam)
	service := &Service{
		rateLimits: map[string]teamModelRateLimit{
			teamModelRateLimitKey(account.ProviderBuild, rateLimitTeamFingerprint(observedTeam), model): {
				TeamFingerprint: shortTeamFingerprint(rateLimitTeamFingerprint(observedTeam)), Until: now.Add(time.Minute),
			},
			teamModelRateLimitKey(account.ProviderBuild, currentFingerprint, model): {
				TeamFingerprint: shortTeamFingerprint(currentFingerprint), Until: now.Add(time.Minute),
			},
		},
		rateLimitTeams: map[uint64]teamRateLimitObservation{
			credential.ID: {Fingerprint: rateLimitTeamFingerprint(observedTeam), ExpiresAt: now.Add(-time.Second)},
		},
	}
	service.rateLimitActive.Store(true)

	limited, ok := service.activeTeamModelRateLimit(credential, model, now)
	if !ok || limited.TeamFingerprint != shortTeamFingerprint(currentFingerprint) {
		t.Fatalf("limit = %#v, ok=%v", limited, ok)
	}
	if _, exists := service.rateLimitTeams[credential.ID]; exists {
		t.Fatal("expired observed Team mapping was retained")
	}
}

func TestActiveTeamModelRateLimitPrunesExpiredUnrelatedLimit(t *testing.T) {
	now := time.Now().UTC()
	service := &Service{
		rateLimits: map[string]teamModelRateLimit{
			teamModelRateLimitKey(account.ProviderBuild, rateLimitTeamFingerprint("00000000-0000-0000-0000-0000000000a1"), "old-model"): {
				Until: now.Add(-time.Second),
			},
		},
		rateLimitTeams: map[uint64]teamRateLimitObservation{
			99: {Fingerprint: rateLimitTeamFingerprint("00000000-0000-0000-0000-0000000000a1"), ExpiresAt: now.Add(-time.Second)},
		},
	}
	service.rateLimitActive.Store(true)
	service.rateLimitNextExpiry.Store(now.Add(-time.Second).UnixNano())

	credential := account.Credential{ID: 100, Provider: account.ProviderBuild, TeamID: "00000000-0000-0000-0000-0000000000b2"}
	if limited, ok := service.activeTeamModelRateLimit(credential, "new-model", now); ok {
		t.Fatalf("expired unrelated limit remained active: %#v", limited)
	}
	if service.rateLimitActive.Load() || service.rateLimitNextExpiry.Load() != 0 || len(service.rateLimits) != 0 || len(service.rateLimitTeams) != 0 {
		t.Fatalf("expired state was not fully pruned: active=%v next=%d limits=%d teams=%d", service.rateLimitActive.Load(), service.rateLimitNextExpiry.Load(), len(service.rateLimits), len(service.rateLimitTeams))
	}
}

func TestGatewayGeneric429CoolsAccountAndRotates(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "generic-429.db"))
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

	credentials := make([]account.Credential, 0, 2)
	for index, name := range []string{"fast-a", "fast-b"} {
		credential, _, createErr := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderBuild, Name: name, SourceKey: name, EncryptedAccessToken: name,
			ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive,
			Priority: 200 - index, MaxConcurrent: 1,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		credentials = append(credentials, credential)
	}
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderBuild, []string{"grok-fast"}); err != nil {
		t.Fatal(err)
	}
	for _, credential := range credentials {
		if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-fast"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "fast-key", Prefix: "fast", SecretHash: strings.Repeat("2", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{
		credentials[0].ID: {{status: http.StatusTooManyRequests, body: `{"error":"You are sending requests too quickly. Please try again later."}`}},
		credentials[1].ID: {{status: http.StatusOK, body: `{"id":"resp-fast-b"}`}},
	}}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 3)

	result, err := service.CreateResponse(ctx, Input{
		RequestID: "req-generic-429", ClientKey: clientKey, PublicModel: "grok-fast",
		Body: []byte(`{"model":"grok-fast","input":"hello"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	result.Finalize(Usage{}, "resp-fast-b", "")
	_ = result.Body.Close()
	if attempts := adapter.Attempts(); len(attempts) != 2 || attempts[0] != credentials[0].ID || attempts[1] != credentials[1].ID {
		t.Fatalf("generic 429 must rotate, attempts=%#v", attempts)
	}
	cooled, err := accountRepo.Get(ctx, credentials[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if cooled.AuthStatus != account.AuthStatusActive || cooled.FailureCount != 1 || cooled.CooldownUntil == nil {
		t.Fatalf("generic 429 must briefly cool account A without permanent invalidation: %#v", cooled)
	}
}

func TestGatewayNonAccount5xxSoftCoolsAndRotates(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "soft-5xx.db"))
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

	credentials := make([]account.Credential, 0, 2)
	for index, name := range []string{"gateway-a", "gateway-b"} {
		credential, _, createErr := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderBuild, Name: name, SourceKey: name, EncryptedAccessToken: name,
			ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive,
			Priority: 200 - index, MaxConcurrent: 1,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		credentials = append(credentials, credential)
	}
	if err := accountRepo.UpdateHealth(ctx, credentials[0].ID, account.ProviderBuild, 3, nil, "prior account failure", false); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderBuild, []string{"grok-soft-5xx"}); err != nil {
		t.Fatal(err)
	}
	for _, credential := range credentials {
		if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-soft-5xx"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "soft-5xx-key", Prefix: "soft5xx", SecretHash: strings.Repeat("3", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{
		credentials[0].ID: {{status: http.StatusGatewayTimeout, body: `{"error":"temporary upstream timeout"}`}},
		credentials[1].ID: {{status: http.StatusOK, body: `{"id":"resp-soft-5xx-b"}`}},
	}}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, 30*time.Second, 30*time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 3)

	before := time.Now().UTC()
	result, err := service.CreateResponse(ctx, Input{
		RequestID: "req-soft-5xx", ClientKey: clientKey, PublicModel: "grok-soft-5xx",
		Body: []byte(`{"model":"grok-soft-5xx","input":"hello"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	result.Finalize(Usage{}, "resp-soft-5xx-b", "")
	_ = result.Body.Close()
	if attempts := adapter.Attempts(); len(attempts) != 2 || attempts[0] != credentials[0].ID || attempts[1] != credentials[1].ID {
		t.Fatalf("non-account 5xx must rotate, attempts=%#v", attempts)
	}
	cooled, err := accountRepo.Get(ctx, credentials[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if cooled.AuthStatus != account.AuthStatusActive || cooled.FailureCount != 3 || cooled.LastError != "upstream status 504" || cooled.CooldownUntil == nil {
		t.Fatalf("non-account 5xx soft cooldown state = %#v", cooled)
	}
	cooldown := cooled.CooldownUntil.Sub(before)
	if cooldown < 4*time.Second || cooldown > 6*time.Second {
		t.Fatalf("non-account 5xx cooldown = %s, want ~5s", cooldown)
	}
}

func TestGatewayExhausted429PreservesLastBodyInFailure(t *testing.T) {
	// When all attempts fail, CreateResponse returns UpstreamFailure (sanitized).
	// captureResponse must reattach the diagnostic body so subsequent classification
	// and attempt diagnostics still see the complete last JSON payload.
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "exhausted-429-body.db"))
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

	credentials := make([]account.Credential, 0, 2)
	for index, name := range []string{"body-a", "body-b"} {
		credential, _, createErr := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderBuild, Name: name, SourceKey: name, EncryptedAccessToken: name,
			ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive,
			Priority: 200 - index, MaxConcurrent: 1,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		credentials = append(credentials, credential)
	}
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderBuild, []string{"grok-body"}); err != nil {
		t.Fatal(err)
	}
	for _, credential := range credentials {
		if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-body"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "body-key", Prefix: "body", SecretHash: strings.Repeat("4", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstBody := `{"code":"resource-exhausted","error":"first too quickly"}`
	secondBody := `{"code":"resource-exhausted","error":"second too quickly complete json"}`
	adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{
		credentials[0].ID: {{status: http.StatusTooManyRequests, body: firstBody}},
		credentials[1].ID: {{status: http.StatusTooManyRequests, body: secondBody}},
	}}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	// Capture attempts via a wrapping audit recorder that keeps the in-memory Attempts slice.
	audits := &attemptCapturingAudit{inner: auditRepo}
	service := NewService(modelRepo, audits, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 2)

	requestCtx := requestmeta.WithClientIP(ctx, "2001:db8::42")
	_, err = service.CreateResponse(requestCtx, Input{
		RequestID: "req-body-429", ClientKey: clientKey, PublicModel: "grok-body",
		Body: []byte(`{"model":"grok-body","input":"hello"}`),
	})
	var upstreamFailure *UpstreamFailure
	if !errors.As(err, &upstreamFailure) {
		t.Fatalf("err = %T %v", err, err)
	}
	if upstreamFailure.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("status = %d", upstreamFailure.HTTPStatus)
	}
	if audits.last.ClientIP != "2001:db8::42" {
		t.Fatalf("client IP = %q", audits.last.ClientIP)
	}
	if len(audits.last.Attempts) < 2 {
		t.Fatalf("attempts = %#v", audits.last.Attempts)
	}
	last := audits.last.Attempts[len(audits.last.Attempts)-1]
	if !strings.Contains(string(last.ResponseBody), "second too quickly complete json") {
		t.Fatalf("last attempt body not preserved: %q", last.ResponseBody)
	}
	if attempts := adapter.Attempts(); len(attempts) != 2 {
		t.Fatalf("attempts = %#v", attempts)
	}
}

type attemptCapturingAudit struct {
	inner auditRecorder
	last  audit.Record
}

func (a *attemptCapturingAudit) Create(ctx context.Context, value audit.Record) error {
	a.last = value
	if a.inner != nil {
		return a.inner.Create(ctx, value)
	}
	return nil
}

func TestGatewayExplicitPolicyRejectionDoesNotPenalizeOrRotateAccount(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "bare-permission.db"))
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

	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "bare", SourceKey: "bare", EncryptedAccessToken: "access",
		EncryptedRefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour),
		Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 100, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderBuild, []string{"grok-bare"}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-bare"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "bare-key", Prefix: "bare", SecretHash: strings.Repeat("5", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"code":"permission-denied","error":"request rejected by policy"}`
	adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{
		credential.ID: {{status: http.StatusForbidden, body: body}},
	}}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 2)
	service.UpdateBuildForbiddenReauthPolicy(true, []string{"permission-denied"})

	result, err := service.CreateResponse(ctx, Input{
		RequestID: "req-bare", ClientKey: clientKey, PublicModel: "grok-bare",
		Body: []byte(`{"model":"grok-bare","input":"hello"}`),
	})
	if err != nil {
		t.Fatalf("explicit policy rejection must return the original response: %v", err)
	}
	if result.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d", result.StatusCode)
	}
	responseBody, _ := io.ReadAll(result.Body)
	result.Finalize(Usage{}, "", "upstream_forbidden")
	_ = result.Body.Close()
	if !strings.Contains(string(responseBody), "request rejected by policy") {
		t.Fatalf("body = %s", responseBody)
	}
	if attempts := adapter.Attempts(); len(attempts) != 1 {
		t.Fatalf("attempts = %#v, want one terminal request", attempts)
	}
	observed, getErr := accountRepo.Get(ctx, credential.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if observed.AuthStatus != account.AuthStatusActive || observed.FailureCount != 0 || observed.CooldownUntil != nil {
		t.Fatalf("explicit policy rejection penalized account: %#v", observed)
	}
	if adapter.refreshes.Load() != 0 {
		t.Fatalf("explicit policy rejection refreshed OAuth: %d", adapter.refreshes.Load())
	}
	candidates, listErr := accountRepo.ListRoutingCandidates(ctx, account.ProviderBuild, 0, "grok-bare", "")
	if listErr != nil {
		t.Fatal(listErr)
	}
	for _, candidate := range candidates {
		if candidate.Credential.ID == credential.ID && candidate.ModelQuotaBlock != nil {
			t.Fatalf("explicit policy rejection must not create model block: %#v", candidate.ModelQuotaBlock)
		}
	}
}

func TestGatewayUnknownBuildForbiddenRotatesWithoutPenalizingAccount(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "unknown-forbidden.db"))
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

	credentials := make([]account.Credential, 0, 2)
	for index, name := range []string{"unknown-a", "unknown-b"} {
		credential, _, createErr := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderBuild, Name: name, SourceKey: name, EncryptedAccessToken: name,
			EncryptedRefreshToken: "refresh-" + name, ExpiresAt: time.Now().Add(time.Hour),
			Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 200 - index, MaxConcurrent: 1,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		credentials = append(credentials, credential)
	}
	const model = "grok-unknown-forbidden"
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderBuild, []string{model}); err != nil {
		t.Fatal(err)
	}
	for _, credential := range credentials {
		if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{model}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "unknown-forbidden-key", Prefix: "unknown403", SecretHash: strings.Repeat("a", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}

	adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{
		credentials[0].ID: {{
			status: http.StatusForbidden,
			body:   `{"code":"permission_denied","error":"denied"}`,
			header: http.Header{"X-Should-Retry": {"false"}},
		}},
		credentials[1].ID: {{status: http.StatusOK, body: `{"id":"resp-after-403","status":"completed","output":[]}`}},
	}}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 2)
	// A matching code alone must not invalidate an account.
	service.UpdateBuildForbiddenReauthPolicy(true, []string{"permission-denied"})

	result, err := service.CreateResponse(ctx, Input{
		RequestID: "req-unknown-forbidden", ClientKey: clientKey, PublicModel: model,
		Body: []byte(`{"model":"grok-unknown-forbidden","input":"hello"}`),
	})
	if err != nil {
		t.Fatalf("unknown 403 did not fail over to the next credential: %v", err)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", result.StatusCode)
	}
	result.Finalize(Usage{}, "", "")
	_ = result.Body.Close()
	if attempts := adapter.Attempts(); len(attempts) != 2 || attempts[0] != credentials[0].ID || attempts[1] != credentials[1].ID {
		t.Fatalf("credential traversal = %#v", attempts)
	}
	if adapter.refreshes.Load() != 0 {
		t.Fatalf("unknown 403 refreshed OAuth: %d", adapter.refreshes.Load())
	}
	observed, err := accountRepo.Get(ctx, credentials[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if observed.AuthStatus != account.AuthStatusActive || observed.FailureCount != 0 || observed.CooldownUntil != nil {
		t.Fatalf("unknown 403 penalized first account: %#v", observed)
	}
	candidates, err := accountRepo.ListRoutingCandidates(ctx, account.ProviderBuild, 0, model, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range candidates {
		if candidate.Credential.ID == credentials[0].ID && candidate.ModelQuotaBlock != nil {
			t.Fatalf("unknown 403 created model block: %#v", candidate.ModelQuotaBlock)
		}
	}
}

func TestGatewayBarePermissionDeniedRetainsEgressRetryForWebAndConsole(t *testing.T) {
	for _, providerValue := range []account.Provider{account.ProviderWeb, account.ProviderConsole} {
		providerValue := providerValue
		t.Run(string(providerValue), func(t *testing.T) {
			ctx := context.Background()
			database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "bare-permission-egress.db"))
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

			model := "grok-egress-" + string(providerValue)
			credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
				Provider: providerValue, AuthType: account.AuthTypeSSO, Name: "egress", SourceKey: model,
				EncryptedAccessToken: "access", ExpiresAt: time.Now().Add(time.Hour), WebTier: account.WebTierSuper,
				Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 100, MaxConcurrent: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := modelRepo.UpsertDiscovered(ctx, providerValue, []string{model}); err != nil {
				t.Fatal(err)
			}
			if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{model}, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			clientKey, err := keyRepo.Create(ctx, clientkey.Key{
				Name: "egress-key", Prefix: "egress", SecretHash: strings.Repeat("7", 64), EncryptedSecret: "encrypted",
				Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
			})
			if err != nil {
				t.Fatal(err)
			}

			adapter := &barePermissionEgressAdapter{providerValue: providerValue}
			registry := provider.NewRegistry(adapter)
			sticky := memory.NewStickyStore()
			accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
			selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
			service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 2)

			result, err := service.CreateResponse(ctx, Input{
				RequestID: "req-egress", ClientKey: clientKey, PublicModel: model,
				Body: []byte(`{"model":"` + model + `","input":"hello"}`),
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", result.StatusCode)
			}
			result.Finalize(Usage{}, "", "")
			_ = result.Body.Close()
			if attempts := adapter.attempts.Load(); attempts != 2 {
				t.Fatalf("egress attempts = %d, want 2", attempts)
			}
			observed, err := accountRepo.Get(ctx, credential.ID)
			if err != nil {
				t.Fatal(err)
			}
			if observed.AuthStatus != account.AuthStatusActive || observed.FailureCount != 0 || observed.CooldownUntil != nil {
				t.Fatalf("egress retry changed account state: %#v", observed)
			}
		})
	}
}

func TestGatewayPreviousResponseIDDoesNotCrossAccounts(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "prev-response-pin.db"))
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

	credentials := make([]account.Credential, 0, 2)
	for index, name := range []string{"pin-a", "pin-b"} {
		credential, _, createErr := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderBuild, Name: name, SourceKey: name, EncryptedAccessToken: name,
			ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive,
			Priority: 200 - index, MaxConcurrent: 1,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		credentials = append(credentials, credential)
	}
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderBuild, []string{"grok-pin"}); err != nil {
		t.Fatal(err)
	}
	for _, credential := range credentials {
		if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-pin"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "pin-key", Prefix: "pin", SecretHash: strings.Repeat("3", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := responseRepo.Save(ctx, inferencedomain.ResponseOwnership{
		ResponseID: "resp-pin-root", AccountID: credentials[0].ID, ClientKeyID: clientKey.ID,
		Provider: account.ProviderBuild, PromptCacheKey: "session-pin", ExpiresAt: now.Add(time.Hour),
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	exhausted := `{"code":"subscription:free-usage-exhausted","error":"tokens (actual/limit): 10/10"}`
	adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{
		credentials[0].ID: {{status: http.StatusTooManyRequests, body: exhausted, header: http.Header{"X-Should-Retry": {"false"}}}},
		credentials[1].ID: {{status: http.StatusOK, body: `{"id":"resp-should-not-run"}`}},
	}}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 3)

	_, err = service.CreateResponse(ctx, Input{
		RequestID: "req-pin", ClientKey: clientKey, PublicModel: "grok-pin",
		PreviousResponseID: "resp-pin-root",
		Body:               []byte(`{"model":"grok-pin","previous_response_id":"resp-pin-root","input":"hello"}`),
	})
	if err == nil {
		t.Fatal("pinned free-usage exhaustion should fail without cross-account failover")
	}
	if attempts := adapter.Attempts(); len(attempts) != 1 || attempts[0] != credentials[0].ID {
		t.Fatalf("previous_response_id must stay pinned to account A, attempts=%#v", attempts)
	}
}

func TestGatewayPinnedResponseReturnsCachedTeamRateLimitWithoutSpinning(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "pinned-team-rate-limit.db"))
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

	const model = "grok-pinned-team-limit"
	const teamID = "00000000-0000-0000-0000-0000000000d4"
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "pinned-team", SourceKey: "pinned-team", TeamID: teamID,
		EncryptedAccessToken: "access", ExpiresAt: time.Now().Add(time.Hour),
		Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 100, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderBuild, []string{model}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{model}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	clientKey, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "pinned-team-key", Prefix: "pinnedteam", SecretHash: strings.Repeat("6", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := responseRepo.Save(ctx, inferencedomain.ResponseOwnership{
		ResponseID: "resp-pinned-team", AccountID: credential.ID, ClientKeyID: clientKey.ID,
		Provider: account.ProviderBuild, PromptCacheKey: "session-pinned-team", ExpiresAt: now.Add(time.Hour),
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responseRepo, 3)
	service.markTeamModelRateLimit(credential, model, provider.RateLimitMetadata{
		Scope: provider.RateLimitScopeRPS, TeamID: teamID, Model: model, Actual: 2, Limit: 2, RetryAfter: time.Minute,
	}, now)

	requestCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	_, err = service.CreateResponse(requestCtx, Input{
		RequestID: "req-pinned-team", ClientKey: clientKey, PublicModel: model,
		PreviousResponseID: "resp-pinned-team",
		Body:               []byte(`{"model":"grok-pinned-team-limit","previous_response_id":"resp-pinned-team","input":"hello"}`),
	})
	var failure *UpstreamFailure
	if !errors.As(err, &failure) || failure.HTTPStatus != http.StatusTooManyRequests || failure.RetryAfter <= 0 {
		t.Fatalf("failure = %#v, err = %v", failure, err)
	}
	if requestCtx.Err() != nil {
		t.Fatalf("pinned request waited for context expiry: %v", requestCtx.Err())
	}
	if attempts := adapter.Attempts(); len(attempts) != 0 {
		t.Fatalf("cached Team limit must not reach upstream, attempts=%#v", attempts)
	}
}

type scriptedBuildResponse struct {
	status int
	body   string
	header http.Header
}

type barePermissionEgressAdapter struct {
	providerValue account.Provider
	attempts      atomic.Int64
}

func (a *barePermissionEgressAdapter) Provider() account.Provider { return a.providerValue }
func (a *barePermissionEgressAdapter) Definition() provider.Definition {
	return testConversationDefinition(a.providerValue)
}
func (a *barePermissionEgressAdapter) ForwardResponse(context.Context, provider.ResponseResourceRequest) (*provider.Response, error) {
	if a.attempts.Add(1) == 1 {
		return &provider.Response{
			StatusCode: http.StatusForbidden, Status: "403 Forbidden", Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"code":"permission-denied","error":"denied"}`)),
		}, nil
	}
	return &provider.Response{
		StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(`{"id":"resp-egress-ok","status":"completed","output":[]}`)),
	}, nil
}

type scriptedBuildAdapter struct {
	mu            sync.Mutex
	attempts      []uint64
	responses     map[uint64][]scriptedBuildResponse
	refreshes     atomic.Int64
	lastRateLimit *provider.RateLimitMetadata
}

func (a *scriptedBuildAdapter) Provider() account.Provider { return account.ProviderBuild }
func (a *scriptedBuildAdapter) Definition() provider.Definition {
	definition := testConversationDefinition(account.ProviderBuild)
	definition.Conversation.StoredResponses = true
	definition.Conversation.Compact = true
	return definition
}
func (a *scriptedBuildAdapter) ForwardResponse(_ context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.attempts = append(a.attempts, request.Credential.ID)
	queue := a.responses[request.Credential.ID]
	if len(queue) == 0 {
		return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"resp-default"}`))}, nil
	}
	next := queue[0]
	a.responses[request.Credential.ID] = queue[1:]
	header := next.header.Clone()
	if header == nil {
		header = make(http.Header)
	}
	var rateLimit *provider.RateLimitMetadata
	if next.status == http.StatusTooManyRequests {
		rateLimit = provider.ParseRateLimitMetadata([]byte(next.body))
		a.lastRateLimit = rateLimit
	}
	return &provider.Response{
		StatusCode: next.status, Status: http.StatusText(next.status), Header: header,
		Body: io.NopCloser(strings.NewReader(next.body)), RateLimit: rateLimit,
	}, nil
}
func (a *scriptedBuildAdapter) RefreshCredential(context.Context, account.Credential) (provider.RefreshedCredential, error) {
	a.refreshes.Add(1)
	return provider.RefreshedCredential{EncryptedAccessToken: "access-new", EncryptedRefreshToken: "refresh-new", ExpiresAt: time.Now().Add(6 * time.Hour)}, nil
}
func (a *scriptedBuildAdapter) Attempts() []uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]uint64(nil), a.attempts...)
}

type systemicForbiddenAdapter struct {
	mu       sync.Mutex
	attempts []uint64
}

type dpopRequiredConsoleAdapter struct {
	mu       sync.Mutex
	attempts []uint64
}

type spendingLimitAdapter struct{}

func (spendingLimitAdapter) Provider() account.Provider { return account.ProviderBuild }
func (spendingLimitAdapter) Definition() provider.Definition {
	return testConversationDefinition(account.ProviderBuild)
}
func (spendingLimitAdapter) ForwardResponse(context.Context, provider.ResponseResourceRequest) (*provider.Response, error) {
	return &provider.Response{
		StatusCode: http.StatusPaymentRequired, Status: "402 Payment Required", Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(`{"code":"personal-team-blocked:spending-limit","error":"quota exhausted"}`)),
	}, nil
}

type authRescueAdapter struct {
	attempts      atomic.Int64
	refreshes     atomic.Int64
	rejectAll     atomic.Bool
	denyChat      atomic.Bool
	recoverDenied atomic.Bool
}

func (a *authRescueAdapter) Provider() account.Provider { return account.ProviderBuild }
func (a *authRescueAdapter) Definition() provider.Definition {
	return testConversationDefinition(account.ProviderBuild)
}
func (a *authRescueAdapter) ForwardResponse(_ context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	a.attempts.Add(1)
	if a.denyChat.Load() {
		if a.recoverDenied.Load() {
			return &provider.Response{
				StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(`{"id":"resp-fallback","status":"completed","output":[]}`)),
				RecoveredPrimaryFailure: &provider.DiagnosticResponse{
					StatusCode: http.StatusForbidden, Status: "403 Forbidden", Header: make(http.Header),
					Body: []byte(`{"code":"permission-denied","error":"Access to the chat endpoint is denied"}`),
				},
			}, nil
		}
		return &provider.Response{
			StatusCode: http.StatusForbidden, Status: "403 Forbidden", Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"error":{"code":"permission_denied","message":"Access to the chat endpoint is denied"}}`)),
		}, nil
	}
	if a.rejectAll.Load() {
		return &provider.Response{
			StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized", Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"error":{"code":"unauthorized","message":"access token rejected"}}`)),
		}, nil
	}
	if request.Credential.EncryptedAccessToken == "access-old" {
		return &provider.Response{
			StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized", Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"error":{"code":"permission_denied","message":"Access to the chat endpoint is denied"}}`)),
		}, nil
	}
	return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
}
func (a *authRescueAdapter) RefreshCredential(context.Context, account.Credential) (provider.RefreshedCredential, error) {
	a.refreshes.Add(1)
	return provider.RefreshedCredential{EncryptedAccessToken: "access-new", EncryptedRefreshToken: "refresh-new", ExpiresAt: time.Now().Add(6 * time.Hour)}, nil
}

func (a *systemicForbiddenAdapter) Provider() account.Provider { return account.ProviderBuild }
func (a *systemicForbiddenAdapter) Definition() provider.Definition {
	return testConversationDefinition(account.ProviderBuild)
}
func (a *systemicForbiddenAdapter) ForwardResponse(_ context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	a.mu.Lock()
	a.attempts = append(a.attempts, request.Credential.ID)
	a.mu.Unlock()
	return &provider.Response{
		StatusCode: http.StatusForbidden, Status: "403 Forbidden", Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(`{"error":"forbidden"}`)),
	}, nil
}
func (a *systemicForbiddenAdapter) Attempts() []uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]uint64(nil), a.attempts...)
}

func (a *dpopRequiredConsoleAdapter) Provider() account.Provider { return account.ProviderConsole }
func (a *dpopRequiredConsoleAdapter) Definition() provider.Definition {
	return testConversationDefinition(account.ProviderConsole)
}
func (a *dpopRequiredConsoleAdapter) ForwardResponse(_ context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	a.mu.Lock()
	a.attempts = append(a.attempts, request.Credential.ID)
	a.mu.Unlock()
	return &provider.Response{
		StatusCode: http.StatusForbidden, Status: "403 Forbidden", Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(`{"code":"unauthorized:dpop-required","error":"DPoP proof required but was not verified."}`)),
	}, nil
}
func (a *dpopRequiredConsoleAdapter) Attempts() []uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]uint64(nil), a.attempts...)
}

type webStoredResponseAdapter struct{}

func (webStoredResponseAdapter) Provider() account.Provider { return account.ProviderWeb }
func (webStoredResponseAdapter) Definition() provider.Definition {
	return provider.Definition{
		Provider: account.ProviderWeb,
		Conversation: provider.ConversationSurface{
			Responses: true, StoredResponses: true,
		},
		Inference: provider.InferencePolicy{Usage: provider.UsageEstimated},
	}
}
func (webStoredResponseAdapter) ForwardResponse(context.Context, provider.ResponseResourceRequest) (*provider.Response, error) {
	return &provider.Response{
		StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(`{"id":"resp-web-ownership"}`)),
	}, nil
}

type webRateLimitAdapter struct{}

type webImageStreamAdapter struct {
	mu                 sync.Mutex
	streaming          bool
	partialImages      int
	editRequest        provider.ImageEditRequest
	synced             chan string
	failureEgress      *infraegress.Manager
	attempts           []uint64
	unauthorizedID     uint64
	forbiddenRemaining int
}

type webChatQuotaAdapter struct {
	synced chan string
}

type credentialFailureImageAdapter struct {
	generationCalls atomic.Int64
}

func (a *credentialFailureImageAdapter) Provider() account.Provider { return account.ProviderBuild }

func (a *credentialFailureImageAdapter) Definition() provider.Definition {
	return provider.Definition{
		Provider: account.ProviderBuild, ModelNamespace: account.ProviderBuild.ModelNamespace(),
		Credential: provider.CredentialSurface{AuthType: account.AuthTypeOAuth, Refresh: true},
		Media:      provider.MediaSurface{ImageGeneration: true},
	}
}

func (a *credentialFailureImageAdapter) RefreshCredential(context.Context, account.Credential) (provider.RefreshedCredential, error) {
	return provider.RefreshedCredential{}, errors.New("simulated credential refresh failure")
}

func (a *credentialFailureImageAdapter) GenerateImage(context.Context, provider.ImageGenerationRequest) (*provider.Response, error) {
	a.generationCalls.Add(1)
	return nil, errors.New("unexpected image generation")
}

func (webRateLimitAdapter) Provider() account.Provider { return account.ProviderWeb }
func (webRateLimitAdapter) Definition() provider.Definition {
	return testConversationDefinition(account.ProviderWeb)
}
func (webRateLimitAdapter) QuotaMode(string) string { return "fast" }
func (webRateLimitAdapter) TierOrder(string) []account.WebTier {
	return []account.WebTier{account.WebTierBasic, account.WebTierSuper, account.WebTierHeavy}
}
func (webRateLimitAdapter) ForwardResponse(context.Context, provider.ResponseResourceRequest) (*provider.Response, error) {
	header := make(http.Header)
	header.Set("Retry-After", "3600")
	return &provider.Response{StatusCode: http.StatusTooManyRequests, Header: header, Body: io.NopCloser(strings.NewReader(`{"error":"limited"}`))}, nil
}

func (a *webImageStreamAdapter) Provider() account.Provider { return account.ProviderWeb }
func (a *webImageStreamAdapter) Definition() provider.Definition {
	return testConversationDefinition(account.ProviderWeb)
}
func (a *webImageStreamAdapter) QuotaMode(model string) string {
	if model == "grok-imagine-image" {
		return "fast"
	}
	return ""
}
func (a *webImageStreamAdapter) TierOrder(string) []account.WebTier {
	return []account.WebTier{account.WebTierSuper, account.WebTierHeavy}
}
func (a *webImageStreamAdapter) GenerateImage(ctx context.Context, request provider.ImageGenerationRequest) (*provider.Response, error) {
	a.mu.Lock()
	a.streaming = request.Streaming
	a.partialImages = request.PartialImages
	failureEgress := a.failureEgress
	a.attempts = append(a.attempts, request.Credential.ID)
	unauthorizedID := a.unauthorizedID
	forbidden := a.forbiddenRemaining > 0
	if forbidden {
		a.forbiddenRemaining--
	}
	a.mu.Unlock()
	if forbidden {
		return &provider.Response{
			StatusCode: http.StatusForbidden, Status: "403 Forbidden", Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"error":"egress session rejected"}`)),
		}, nil
	}
	if request.Credential.ID == unauthorizedID {
		return &provider.Response{
			StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized", Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"error":{"code":"unauthorized"}}`)),
		}, nil
	}
	if failureEgress != nil {
		lease, err := failureEgress.Acquire(ctx, egressdomain.ScopeWeb, "image-failure")
		if err != nil {
			return nil, err
		}
		lease.Release()
		return nil, errors.New("simulated image transport failure")
	}
	body := "event: image_generation.completed\ndata: {}\n\ndata: [DONE]\n\n"
	return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), QuotaUnits: 1}, nil
}
func (a *webImageStreamAdapter) ForwardResponse(context.Context, provider.ResponseResourceRequest) (*provider.Response, error) {
	return &provider.Response{
		StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"id":"resp-image-lite","object":"response"}`)), QuotaUnits: 3,
	}, nil
}
func (a *webImageStreamAdapter) EditImage(_ context.Context, request provider.ImageEditRequest) (*provider.Response, error) {
	a.mu.Lock()
	a.editRequest = request
	a.mu.Unlock()
	return &provider.Response{
		StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"created":1,"data":[{"url":"https://example.com/edit.png"}]}`)), QuotaUnits: request.Count,
	}, nil
}
func (a *webImageStreamAdapter) Streaming() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.streaming
}
func (a *webImageStreamAdapter) PartialImages() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.partialImages
}
func (a *webImageStreamAdapter) EditRequest() provider.ImageEditRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.editRequest
}
func (a *webImageStreamAdapter) FailWithEgress(manager *infraegress.Manager) {
	a.mu.Lock()
	a.failureEgress = manager
	a.mu.Unlock()
}
func (a *webImageStreamAdapter) Attempts() []uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]uint64(nil), a.attempts...)
}
func (a *webImageStreamAdapter) SyncQuota(context.Context, account.Credential) (provider.QuotaSnapshot, error) {
	return provider.QuotaSnapshot{}, errors.New("unexpected full quota sync")
}
func (a *webImageStreamAdapter) SyncQuotaMode(_ context.Context, credential account.Credential, mode string) (account.QuotaWindow, error) {
	now := time.Now().UTC()
	a.synced <- mode
	return account.QuotaWindow{
		AccountID: credential.ID, Mode: mode, Remaining: 8, Total: 10,
		WindowSeconds: 3600, SyncedAt: &now, Source: account.QuotaSourceUpstream,
	}, nil
}

func (a *webChatQuotaAdapter) Provider() account.Provider { return account.ProviderWeb }
func (a *webChatQuotaAdapter) Definition() provider.Definition {
	return testConversationDefinition(account.ProviderWeb)
}
func (a *webChatQuotaAdapter) QuotaMode(string) string { return "fast" }
func (a *webChatQuotaAdapter) TierOrder(string) []account.WebTier {
	return []account.WebTier{account.WebTierBasic, account.WebTierSuper, account.WebTierHeavy}
}
func (a *webChatQuotaAdapter) ForwardResponse(context.Context, provider.ResponseResourceRequest) (*provider.Response, error) {
	return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"chat-response"}`))}, nil
}
func (a *webChatQuotaAdapter) SyncQuota(context.Context, account.Credential) (provider.QuotaSnapshot, error) {
	return provider.QuotaSnapshot{}, errors.New("unexpected full quota sync")
}
func (a *webChatQuotaAdapter) SyncQuotaMode(_ context.Context, credential account.Credential, mode string) (account.QuotaWindow, error) {
	now := time.Now().UTC()
	a.synced <- mode
	return account.QuotaWindow{
		AccountID: credential.ID, Mode: mode, Remaining: 17, Total: 20,
		WindowSeconds: 3600, SyncedAt: &now, Source: account.QuotaSourceUpstream,
	}, nil
}

func (a *failoverAdapter) Provider() account.Provider { return account.ProviderBuild }
func (a *failoverAdapter) Definition() provider.Definition {
	return provider.Definition{
		Provider: account.ProviderBuild,
		Conversation: provider.ConversationSurface{
			Responses: true, Compact: true, StoredResponses: true,
		},
	}
}
func (a *failoverAdapter) ForwardResponse(_ context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	if request.NormalizedMetadata != nil {
		request.NormalizedMetadata.ReasoningEffort = a.reasoningEffort
	}
	a.mu.Lock()
	a.attempts = append(a.attempts, request.Credential.ID)
	a.forwardedModels = append(a.forwardedModels, request.Model)
	a.lastMethod = request.Method
	a.lastPath = request.Path
	a.lastPromptCacheKey = request.PromptCacheKey
	a.lastReasoningReplayKey = request.ReasoningReplayKey
	a.lastGrokTurnIndex = request.GrokTurnIndex
	a.lastOperation = request.Operation
	resourceStatus := a.resourceStatus
	transportErr := a.transportErrorIDs[request.Credential.ID]
	a.mu.Unlock()
	if transportErr != nil {
		return nil, transportErr
	}
	status, body := http.StatusOK, "ok"
	header := make(http.Header)
	if request.Method != http.MethodPost && resourceStatus != 0 {
		status, body = resourceStatus, "missing"
	} else if request.Credential.ID == a.firstID || a.failureIDs[request.Credential.ID] {
		status = a.failureStatus
		if status == 0 {
			status = http.StatusTooManyRequests
		}
		body = a.failureBody
		if body == "" {
			body = "limited"
		}
		header = a.failureHeader.Clone()
	}
	return &provider.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func (a *failoverAdapter) setResourceStatus(status int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resourceStatus = status
}

func (a *failoverAdapter) resetAttempts() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.attempts = nil
	a.forwardedModels = nil
	a.lastMethod = ""
	a.lastPath = ""
	a.lastPromptCacheKey = ""
	a.lastReasoningReplayKey = ""
	a.lastGrokTurnIndex = ""
	a.lastOperation = ""
}

func (a *failoverAdapter) ForwardedModels() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.forwardedModels...)
}
func (a *failoverAdapter) ListModels(context.Context, account.Credential) ([]string, error) {
	return nil, nil
}

func testConversationDefinition(providerValue account.Provider) provider.Definition {
	definition := provider.Definition{
		Provider: providerValue,
		Conversation: provider.ConversationSurface{
			Responses: true, ChatCompletions: true, Messages: true,
		},
		Inference: provider.InferencePolicy{Usage: provider.UsageUpstream},
	}
	if providerValue == account.ProviderBuild {
		definition.Credential.Refresh = true
	}
	if providerValue == account.ProviderWeb {
		definition.Quota = provider.QuotaRemoteWindow
		definition.Inference.Usage = provider.UsageEstimated
	}
	if providerValue == account.ProviderWeb || providerValue == account.ProviderConsole {
		definition.Inference.RetryForbiddenAsEgress = true
	}
	return definition
}
func (a *failoverAdapter) GetBilling(context.Context, account.Credential) (account.Billing, error) {
	return account.Billing{}, nil
}
func (a *failoverAdapter) RefreshCredential(context.Context, account.Credential) (provider.RefreshedCredential, error) {
	return provider.RefreshedCredential{}, nil
}
func (a *failoverAdapter) StartDeviceAuthorization(context.Context) (provider.DeviceAuthorization, error) {
	return provider.DeviceAuthorization{}, nil
}
func (a *failoverAdapter) PollDeviceAuthorization(context.Context, string) (provider.CredentialSeed, error) {
	return provider.CredentialSeed{}, nil
}
func (a *failoverAdapter) ParseImportedCredentials([]byte) ([]provider.CredentialSeed, error) {
	return nil, nil
}
func (a *failoverAdapter) MarshalCredentials([]provider.CredentialSeed) ([]byte, error) {
	return nil, nil
}

func testCipher(t *testing.T) *security.Cipher {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	return cipher
}

func TestAuditRequestSucceeded(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		errorCode  string
		want       bool
	}{
		{name: "2xx without error succeeds", statusCode: 200, errorCode: "", want: true},
		{name: "2xx stream interruption fails", statusCode: 200, errorCode: "upstream_stream_interrupted", want: false},
		{name: "2xx stream incomplete fails", statusCode: 200, errorCode: "upstream_stream_incomplete", want: false},
		{name: "2xx stream idle timeout fails", statusCode: 200, errorCode: "upstream_stream_idle_timeout", want: false},
		{name: "any 2xx error fails", statusCode: 201, errorCode: "stream_interrupted", want: false},
		{name: "4xx fails", statusCode: 404, errorCode: "upstream_error", want: false},
		{name: "5xx fails", statusCode: 502, errorCode: "upstream_server_error", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := auditRequestSucceeded(tc.statusCode, tc.errorCode); got != tc.want {
				t.Fatalf("auditRequestSucceeded(%d, %q) = %t, want %t", tc.statusCode, tc.errorCode, got, tc.want)
			}
		})
	}
}

func TestIsUpstreamStreamFailureIncludesIdleTimeout(t *testing.T) {
	if !isUpstreamStreamFailure("upstream_stream_idle_timeout") {
		t.Fatal("idle timeout must cool the hanging account")
	}
	if !isUpstreamStreamFailure("upstream_stream_interrupted") || !isUpstreamStreamFailure("upstream_stream_incomplete") {
		t.Fatal("existing stream-failure codes must stay classified")
	}
	if isUpstreamStreamFailure("") || isUpstreamStreamFailure("quality_degraded") {
		t.Fatal("non-stream codes must not look like mid-stream failures")
	}
}

func TestStreamFailureHealthPenaltyOnlyLongCoolsTrulyEmptyIdle(t *testing.T) {
	t.Parallel()
	status, cooldown := streamFailureHealthPenalty("upstream_stream_idle_timeout", Usage{}, 15*time.Minute)
	if status != http.StatusGatewayTimeout || cooldown != 15*time.Minute {
		t.Fatalf("empty idle penalty = (%d, %s)", status, cooldown)
	}
	status, cooldown = streamFailureHealthPenalty("upstream_stream_idle_timeout", Usage{}, 0)
	if status != http.StatusGatewayTimeout || cooldown != qualityIdleAccountCooldown {
		t.Fatalf("zero idle cooldown must fall back to default (%d, %s)", status, cooldown)
	}
	for _, usage := range []Usage{
		{OutputObserved: true},
		{OutputTokens: 1},
		{ReasoningTokens: 1},
	} {
		status, cooldown = streamFailureHealthPenalty("upstream_stream_idle_timeout", usage, 15*time.Minute)
		if status != 0 || cooldown != 0 {
			t.Fatalf("non-empty idle usage %#v received long penalty (%d, %s)", usage, status, cooldown)
		}
	}
}
