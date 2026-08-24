package account

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestWebQuotaRefreshDeduplicatesPerMode(t *testing.T) {
	service := NewService(nil, nil, nil, nil, nil, nil, nil)
	service.QueueQuotaRefresh(42, "fast")
	service.QueueQuotaRefresh(42, "expert")
	service.QueueQuotaRefresh(42, "fast")
	service.QueueQuotaRefresh(43, "console")
	service.QueueQuotaRefresh(43, "console_image")
	service.QueueQuotaRefresh(43, "console_video")
	service.QueueQuotaRefresh(44, accountdomain.QuotaModeWebImagePro)
	service.QueueQuotaRefresh(44, accountdomain.QuotaModeWebVideo720p)

	service.quotaRefreshMu.Lock()
	defer service.quotaRefreshMu.Unlock()
	if len(service.quotaRefreshes) != 6 {
		t.Fatalf("refresh states = %#v", service.quotaRefreshes)
	}
	if service.quotaRefreshes["42:fast"].generation != 2 || !service.quotaRefreshes["42:fast"].queued {
		t.Fatal("duplicate fast refresh was not coalesced into the queued generation")
	}
	if service.quotaRefreshes["42:expert"].generation != 1 || !service.quotaRefreshes["42:expert"].queued {
		t.Fatal("independent expert refresh state is invalid")
	}
	if service.quotaRefreshes["43:console"].generation != 1 || !service.quotaRefreshes["43:console"].queued {
		t.Fatal("Console refresh state is invalid")
	}
	for _, mode := range []string{"console_image", "console_video"} {
		if state := service.quotaRefreshes["43:"+mode]; state == nil || state.generation != 1 || !state.queued {
			t.Fatalf("Console %s refresh state is invalid: %#v", mode, state)
		}
	}
	if state := service.quotaRefreshes["44:"+accountdomain.QuotaGroupWebImagine]; state == nil || state.generation != 2 || !state.queued {
		t.Fatalf("Imagine group refresh state is invalid: %#v", state)
	}
	if len(service.quotaRefreshQueue) != 6 {
		t.Fatalf("queued refreshes = %d", len(service.quotaRefreshQueue))
	}
}

func TestWeeklyQuotaRefreshPreservesTrailingSnapshot(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "weekly-quota-refresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	credential, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO,
		Name: "weekly", SourceKey: "weekly", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive, WebTier: accountdomain.WebTierSuper,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &quotaCountingAdapter{
		modeStarted: make(chan struct{}, 2),
		modeRelease: make(chan struct{}, 2),
	}
	service := NewService(accounts, nil, nil, nil, provider.NewRegistry(adapter), nil, nil)

	service.QueueQuotaRefresh(credential.ID, "weekly")
	request := <-service.quotaRefreshQueue
	done := make(chan struct{})
	go func() {
		service.runQuotaRefresh(ctx, request)
		close(done)
	}()

	select {
	case <-adapter.modeStarted:
	case <-time.After(time.Second):
		t.Fatal("first weekly refresh did not start")
	}
	service.QueueQuotaRefresh(credential.ID, "weekly")
	adapter.modeRelease <- struct{}{}

	select {
	case <-adapter.modeStarted:
	case <-time.After(time.Second):
		t.Fatal("trailing weekly refresh did not start")
	}
	adapter.modeRelease <- struct{}{}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("weekly refresh did not finish")
	}
	if adapter.modeCalls.Load() != 2 {
		t.Fatalf("weekly refresh calls = %d, want 2", adapter.modeCalls.Load())
	}
	service.quotaRefreshMu.Lock()
	_, exists := service.quotaRefreshes[request.key]
	service.quotaRefreshMu.Unlock()
	if exists {
		t.Fatal("completed weekly refresh retained queue state")
	}
}

func TestQuotaRefreshQueueOverflowRetainsDirtyState(t *testing.T) {
	service := NewService(nil, nil, nil, nil, nil, nil, nil)
	service.quotaRefreshQueue = make(chan quotaRefreshRequest, 1)
	service.QueueQuotaRefresh(1, "fast")
	service.QueueQuotaRefresh(2, "fast")

	service.quotaRefreshMu.Lock()
	state := service.quotaRefreshes["2:fast"]
	if state == nil || state.generation != 1 || state.queued || state.running {
		service.quotaRefreshMu.Unlock()
		t.Fatalf("overflow state = %#v", state)
	}
	service.quotaRefreshMu.Unlock()

	first := <-service.quotaRefreshQueue
	if first.accountID != 1 {
		t.Fatalf("first queued account = %d", first.accountID)
	}
	service.requeueQuotaRefreshes()
	second := <-service.quotaRefreshQueue
	if second.accountID != 2 || second.mode != "fast" {
		t.Fatalf("recovered request = %#v", second)
	}
	service.quotaRefreshMu.Lock()
	recovered := service.quotaRefreshes["2:fast"]
	service.quotaRefreshMu.Unlock()
	if recovered == nil || !recovered.queued || recovered.running {
		t.Fatalf("recovered state = %#v", recovered)
	}
}

func TestQuotaRefreshFailureUsesBoundedExponentialBackoff(t *testing.T) {
	service := NewService(nil, nil, nil, nil, nil, nil, nil)
	now := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	service.quotaRefreshes["1:console"] = &quotaRefreshState{running: true}
	for failure := 1; failure <= 10; failure++ {
		service.deferQuotaRefresh("1:console")
		state := service.quotaRefreshes["1:console"]
		maximum := quotaRefreshBackoffBase * time.Duration(1<<min(failure-1, 6))
		if maximum > quotaRefreshBackoffMax {
			maximum = quotaRefreshBackoffMax
		}
		delay := state.nextAttemptAt.Sub(now)
		if state.failures != failure || !state.pending || delay < maximum/2 || delay > maximum {
			t.Fatalf("failure=%d state=%#v delay=%s range=[%s,%s]", failure, state, delay, maximum/2, maximum)
		}
	}
}

func TestRecentConsoleUsageSnapshotSuppressesDuplicateUpstreamRefresh(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "console-refresh-throttle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	credential, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderConsole, AuthType: accountdomain.AuthTypeSSO,
		Name: "console-throttle", SourceKey: "console-throttle", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	windows := []accountdomain.QuotaWindow{
		{AccountID: credential.ID, Mode: "console", Remaining: 9, Total: 10, SyncedAt: &now, Source: accountdomain.QuotaSourceUpstream, UpdatedAt: now},
		{AccountID: credential.ID, Mode: "console_image", Remaining: 5, Total: 5, SyncedAt: &now, Source: accountdomain.QuotaSourceUpstream, UpdatedAt: now},
		{AccountID: credential.ID, Mode: "console_video", Remaining: 2, Total: 2, SyncedAt: &now, Source: accountdomain.QuotaSourceUpstream, UpdatedAt: now},
	}
	if err := accounts.ReplaceQuotaWindows(ctx, credential.ID, "", now, windows); err != nil {
		t.Fatal(err)
	}
	adapter := &consoleQuotaSnapshotAdapter{}
	service := NewService(accounts, nil, nil, nil, provider.NewRegistry(adapter), nil, nil)
	service.now = func() time.Time { return now.Add(10 * time.Second) }
	service.QueueQuotaRefresh(credential.ID, "console")
	request := <-service.quotaRefreshQueue
	service.quotaRefreshMu.Lock()
	state := service.quotaRefreshes[request.key]
	state.queued = false
	state.running = true
	service.quotaRefreshMu.Unlock()
	service.runQuotaRefresh(ctx, request)
	if adapter.fullCalls.Load() != 0 {
		t.Fatalf("recent Console snapshot triggered %d upstream refreshes", adapter.fullCalls.Load())
	}
	service.quotaRefreshMu.Lock()
	state = service.quotaRefreshes[request.key]
	service.quotaRefreshMu.Unlock()
	if state == nil || state.running || state.pending || !state.nextAttemptAt.Equal(now.Add(10*time.Second+consoleQuotaRefreshMinInterval)) {
		t.Fatalf("Console cooldown state = %#v", state)
	}
}

func TestQuotaRefreshCrossInstanceGenerationTriggersSingleTrailingRefresh(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "quota-cross-instance.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	credential, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO,
		Name: "cross-instance", SourceKey: "cross-instance", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive, WebTier: accountdomain.WebTierSuper,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &quotaCountingAdapter{modeStarted: make(chan struct{}, 4), modeRelease: make(chan struct{}, 4)}
	registry := provider.NewRegistry(adapter)
	coordinator := memory.NewQuotaRefreshCoordinator()
	lock := memory.NewLockStore()
	first := NewService(accounts, nil, nil, nil, registry, nil, lock)
	second := NewService(accounts, nil, nil, nil, registry, nil, lock)
	first.SetQuotaRefreshCoordinator(coordinator)
	second.SetQuotaRefreshCoordinator(coordinator)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{}, 2)
	go func() { first.RunQuotaRefresh(runCtx); done <- struct{}{} }()
	go func() { second.RunQuotaRefresh(runCtx); done <- struct{}{} }()
	t.Cleanup(func() {
		for range 4 {
			adapter.modeRelease <- struct{}{}
		}
		cancel()
		<-done
		<-done
	})

	first.QueueQuotaRefresh(credential.ID, "weekly")
	select {
	case <-adapter.modeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first refresh did not start")
	}
	second.QueueQuotaRefresh(credential.ID, "weekly")
	deadline := time.Now().Add(2 * time.Second)
	for {
		generation, dirty, generationErr := coordinator.QuotaRefreshGeneration(ctx, credential.ID, "weekly")
		if generationErr != nil {
			t.Fatal(generationErr)
		}
		if generation >= 2 && dirty {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("shared generation = %d, dirty = %v", generation, dirty)
		}
		time.Sleep(10 * time.Millisecond)
	}
	adapter.modeRelease <- struct{}{}
	select {
	case <-adapter.modeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("trailing refresh did not start")
	}
	adapter.modeRelease <- struct{}{}

	deadline = time.Now().Add(2 * time.Second)
	for adapter.modeCalls.Load() != 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if calls := adapter.modeCalls.Load(); calls != 2 {
		t.Fatalf("refresh calls = %d, want 2", calls)
	}
	time.Sleep(2 * quotaRefreshPollInterval)
	if calls := adapter.modeCalls.Load(); calls != 2 {
		t.Fatalf("losing instance performed duplicate refresh: %d", calls)
	}
}

func TestRefreshQuotaModeDoesNotTriggerFullProviderSyncForAutoTier(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "quota-mode.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	credential, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO,
		Name: "web-auto", SourceKey: "web-auto", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive, WebTier: accountdomain.WebTierAuto,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &quotaCountingAdapter{}
	service := NewService(accounts, nil, nil, nil, provider.NewRegistry(adapter), nil, nil)
	window, err := service.RefreshQuotaMode(ctx, credential.ID, "fast")
	if err != nil {
		t.Fatal(err)
	}
	if window.Mode != "fast" || adapter.modeCalls.Load() != 1 || adapter.fullCalls.Load() != 0 {
		t.Fatalf("window = %#v, mode calls = %d, full calls = %d", window, adapter.modeCalls.Load(), adapter.fullCalls.Load())
	}
	stored, err := accounts.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.WebTier != accountdomain.WebTierAuto {
		t.Fatalf("single-mode sync changed tier to %q", stored.WebTier)
	}

	service.QueueQuotaRefresh(credential.ID, "fast")
	service.QueueQuotaRefresh(credential.ID, "fast")
	request := <-service.quotaRefreshQueue
	service.runQuotaRefresh(ctx, request)
	if adapter.modeCalls.Load() != 2 || adapter.fullCalls.Load() != 0 {
		t.Fatalf("coalesced mode calls = %d, full calls = %d", adapter.modeCalls.Load(), adapter.fullCalls.Load())
	}
	service.quotaRefreshMu.Lock()
	_, queued := service.quotaRefreshes[request.key]
	service.quotaRefreshMu.Unlock()
	if queued {
		t.Fatal("completed coalesced refresh retained queue state")
	}

	service.refreshLock = deniedQuotaRefreshLock{}
	service.QueueQuotaRefresh(credential.ID, "fast")
	request = <-service.quotaRefreshQueue
	service.runQuotaRefresh(ctx, request)
	if adapter.modeCalls.Load() != 2 {
		t.Fatalf("worker without distributed lease made %d mode calls", adapter.modeCalls.Load())
	}
}

func TestReconcileConsoleRateLimitVerifiesUsageBeforeExhausting(t *testing.T) {
	tests := []struct {
		name      string
		remaining int
		wantState RateLimitReconcileState
	}{
		{name: "transient rate limit keeps available quota", remaining: 9, wantState: RateLimitReconcileAvailable},
		{name: "confirmed zero quota remains exhausted", remaining: 0, wantState: RateLimitReconcileExhausted},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "console-rate-limit.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = database.Close() })
			if err := database.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			accounts := relational.NewAccountRepository(database)
			credential, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
				Provider: accountdomain.ProviderConsole, AuthType: accountdomain.AuthTypeSSO,
				Name: "console-rate-limit", SourceKey: "console-rate-limit", EncryptedAccessToken: "encrypted",
				Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
			})
			if err != nil {
				t.Fatal(err)
			}
			oldSyncedAt := time.Now().UTC().Add(-24 * time.Hour)
			if err := accounts.ReplaceQuotaWindows(ctx, credential.ID, "", oldSyncedAt, []accountdomain.QuotaWindow{
				{Mode: "console", Remaining: 10, Total: 10, SyncedAt: &oldSyncedAt, Source: accountdomain.QuotaSourceUpstream},
				{Mode: "console_image", Remaining: 5, Total: 5, SyncedAt: &oldSyncedAt, Source: accountdomain.QuotaSourceUpstream},
				{Mode: "console_video", Remaining: 2, Total: 2, SyncedAt: &oldSyncedAt, Source: accountdomain.QuotaSourceUpstream},
			}); err != nil {
				t.Fatal(err)
			}
			adapter := &rateLimitConsoleQuotaAdapter{remaining: test.remaining}
			service := NewService(accounts, nil, nil, nil, provider.NewRegistry(adapter), nil, nil)
			service.SetQuotaRecoveryQueue(memory.NewQuotaRecoveryQueue())

			state, err := service.ReconcileRateLimit(ctx, credential.ID, "console", 0)
			if err != nil {
				t.Fatal(err)
			}
			if state != test.wantState || adapter.calls.Load() != 1 {
				t.Fatalf("state=%s calls=%d, want state=%s", state, adapter.calls.Load(), test.wantState)
			}
			stored, err := accounts.GetQuotaWindows(ctx, []uint64{credential.ID})
			if err != nil {
				t.Fatal(err)
			}
			window, ok := quotaWindowByMode(stored[credential.ID], "console")
			if !ok || window.Remaining != test.remaining {
				t.Fatalf("stored Console window = %#v, want remaining=%d", window, test.remaining)
			}
		})
	}
}

func TestReconcileConsoleRateLimitQueuesRetryWhenUsageProbeFails(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "console-rate-limit-retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	credential, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderConsole, AuthType: accountdomain.AuthTypeSSO,
		Name: "console-rate-limit-retry", SourceKey: "console-rate-limit-retry", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := accounts.ReplaceQuotaWindows(ctx, credential.ID, "", now, []accountdomain.QuotaWindow{
		{Mode: "console", Remaining: 9, Total: 10, SyncedAt: &now, Source: accountdomain.QuotaSourceUpstream},
		{Mode: "console_image", Remaining: 5, Total: 5, SyncedAt: &now, Source: accountdomain.QuotaSourceUpstream},
		{Mode: "console_video", Remaining: 2, Total: 2, SyncedAt: &now, Source: accountdomain.QuotaSourceUpstream},
	}); err != nil {
		t.Fatal(err)
	}
	adapter := &rateLimitConsoleQuotaAdapter{err: errors.New("usage temporarily rate limited")}
	service := NewService(accounts, nil, nil, nil, provider.NewRegistry(adapter), nil, nil)
	if state, err := service.ReconcileRateLimit(ctx, credential.ID, "console", 0); err == nil || state != RateLimitReconcileInconclusive {
		t.Fatalf("state=%s err=%v", state, err)
	}
	stored, err := accounts.GetQuotaWindows(ctx, []uint64{credential.ID})
	if err != nil {
		t.Fatal(err)
	}
	window, ok := quotaWindowByMode(stored[credential.ID], "console")
	if !ok || window.Remaining != 9 {
		t.Fatalf("failed probe overwrote last snapshot: %#v", window)
	}
	service.quotaRefreshMu.Lock()
	state := service.quotaRefreshes[strconv.FormatUint(credential.ID, 10)+":console"]
	service.quotaRefreshMu.Unlock()
	if state == nil || !state.pending {
		t.Fatalf("retry state = %#v", state)
	}
}

func TestReconcileConsoleRateLimitDoesNotQueueWhenAnotherReplicaIsRefreshing(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "console-rate-limit-busy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	credential, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderConsole, AuthType: accountdomain.AuthTypeSSO,
		Name: "console-rate-limit-busy", SourceKey: "console-rate-limit-busy", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &rateLimitConsoleQuotaAdapter{remaining: 9}
	service := NewService(accounts, nil, nil, nil, provider.NewRegistry(adapter), nil, nil)
	service.refreshLock = deniedQuotaRefreshLock{}

	state, err := service.ReconcileRateLimit(ctx, credential.ID, "console", 0)
	if err != nil || state != RateLimitReconcileRefreshing {
		t.Fatalf("state=%s err=%v, want refreshing without error", state, err)
	}
	if adapter.calls.Load() != 0 {
		t.Fatalf("busy refresh made %d upstream calls", adapter.calls.Load())
	}
	service.quotaRefreshMu.Lock()
	queued := service.quotaRefreshes[strconv.FormatUint(credential.ID, 10)+":console"]
	service.quotaRefreshMu.Unlock()
	if queued != nil {
		t.Fatalf("busy refresh queued duplicate work: %#v", queued)
	}
}

func TestConsoleImmediateAndQueuedRefreshUseSameDistributedLock(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "console-rate-limit-lock-key.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	credential, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderConsole, AuthType: accountdomain.AuthTypeSSO,
		Name: "console-rate-limit-lock-key", SourceKey: "console-rate-limit-lock-key", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	oldSyncedAt := time.Now().UTC().Add(-time.Hour)
	if err := accounts.ReplaceQuotaWindows(ctx, credential.ID, "", oldSyncedAt, []accountdomain.QuotaWindow{
		{Mode: "console", Remaining: 9, Total: 10, SyncedAt: &oldSyncedAt, Source: accountdomain.QuotaSourceUpstream},
		{Mode: "console_image", Remaining: 5, Total: 5, SyncedAt: &oldSyncedAt, Source: accountdomain.QuotaSourceUpstream},
		{Mode: "console_video", Remaining: 2, Total: 2, SyncedAt: &oldSyncedAt, Source: accountdomain.QuotaSourceUpstream},
	}); err != nil {
		t.Fatal(err)
	}
	adapter := &rateLimitConsoleQuotaAdapter{err: errors.New("usage temporarily unavailable")}
	refreshLock := &recordingQuotaRefreshLock{}
	service := NewService(accounts, nil, nil, nil, provider.NewRegistry(adapter), nil, nil)
	service.refreshLock = refreshLock

	if state, err := service.ReconcileRateLimit(ctx, credential.ID, "console", 0); err == nil || state != RateLimitReconcileInconclusive {
		t.Fatalf("state=%s err=%v", state, err)
	}
	request := <-service.quotaRefreshQueue
	service.runQuotaRefresh(ctx, request)

	keys := refreshLock.snapshot()
	want := consoleQuotaRefreshLockKey(credential.ID)
	if len(keys) < 2 {
		t.Fatalf("lock keys = %v, want immediate and queued acquisitions", keys)
	}
	for _, key := range keys {
		if key != want {
			t.Fatalf("lock key = %q, want %q (all keys: %v)", key, want, keys)
		}
	}
}

func TestRefreshWebImagineQuotaModeAtomicallyReplacesGroup(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "imagine-quota-group.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	credential, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO,
		Name: "web-imagine", SourceKey: "web-imagine", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := accounts.SaveQuotaWindows(ctx, credential.ID, accountdomain.WebTierSuper, now, []accountdomain.QuotaWindow{
		{AccountID: credential.ID, Mode: "weekly", Remaining: 90, Total: 100, UpdatedAt: now},
		{AccountID: credential.ID, Mode: accountdomain.QuotaModeWebImagePro, Remaining: 4, UpdatedAt: now},
		{AccountID: credential.ID, Mode: accountdomain.QuotaModeWebVideo720p, Remaining: 1, UpdatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}
	adapter := &imagineQuotaGroupAdapter{}
	service := NewService(accounts, nil, nil, nil, provider.NewRegistry(adapter), nil, nil)
	window, err := service.RefreshQuotaMode(ctx, credential.ID, accountdomain.QuotaModeWebImagePro)
	if err != nil {
		t.Fatal(err)
	}
	if window.Remaining != 3 || adapter.calls.Load() != 1 {
		t.Fatalf("window = %#v, calls = %d", window, adapter.calls.Load())
	}
	stored, err := accounts.GetQuotaWindows(ctx, []uint64{credential.ID})
	if err != nil {
		t.Fatal(err)
	}
	byMode := make(map[string]accountdomain.QuotaWindow)
	for _, current := range stored[credential.ID] {
		byMode[current.Mode] = current
	}
	if byMode["weekly"].Remaining != 90 || byMode[accountdomain.QuotaModeWebImagePro].Remaining != 3 {
		t.Fatalf("stored = %#v", stored[credential.ID])
	}
	if _, exists := byMode[accountdomain.QuotaModeWebVideo720p]; exists {
		t.Fatalf("stale video_720p window survived group replacement: %#v", stored[credential.ID])
	}
}

func TestRefreshPaidWebImagineFallsBackToSharedWeeklyQuota(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "imagine-shared-weekly.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	credential, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, WebTier: accountdomain.WebTierSuper,
		Name: "web-imagine-weekly", SourceKey: "web-imagine-weekly", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := accounts.SaveQuotaWindows(ctx, credential.ID, accountdomain.WebTierSuper, now, []accountdomain.QuotaWindow{
		{AccountID: credential.ID, Mode: "weekly", Remaining: 50, Total: 100, UpdatedAt: now},
		{AccountID: credential.ID, Mode: accountdomain.QuotaModeWebImagePro, Remaining: 4, UpdatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}
	adapter := &sharedWeeklyImagineAdapter{}
	service := NewService(accounts, nil, nil, nil, provider.NewRegistry(adapter), nil, nil)
	window, err := service.RefreshQuotaMode(ctx, credential.ID, accountdomain.QuotaModeWebImagePro)
	if err != nil {
		t.Fatal(err)
	}
	if window.Mode != "weekly" || window.Remaining != 80 || adapter.groupCalls.Load() != 1 || adapter.modeCalls.Load() != 1 {
		t.Fatalf("window = %#v, group calls = %d, mode calls = %d", window, adapter.groupCalls.Load(), adapter.modeCalls.Load())
	}
	stored, err := accounts.GetQuotaWindows(ctx, []uint64{credential.ID})
	if err != nil {
		t.Fatal(err)
	}
	byMode := make(map[string]accountdomain.QuotaWindow)
	for _, current := range stored[credential.ID] {
		byMode[current.Mode] = current
	}
	if byMode["weekly"].Remaining != 80 {
		t.Fatalf("weekly quota was not refreshed: %#v", stored[credential.ID])
	}
	if _, exists := byMode[accountdomain.QuotaModeWebImagePro]; exists {
		t.Fatalf("stale product quota survived availability-only refresh: %#v", stored[credential.ID])
	}
}

func TestRefreshConsoleQuotaModePersistsCompleteUsageSnapshot(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "console-quota-mode.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	credential, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderConsole, AuthType: accountdomain.AuthTypeSSO,
		Name: "console", SourceKey: "console", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := accounts.SaveQuotaWindows(ctx, credential.ID, "", now, []accountdomain.QuotaWindow{{
		AccountID: credential.ID, Mode: "console", Remaining: 20, Total: 20,
		WindowSeconds: 3600, Source: accountdomain.QuotaSourceDefault, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	adapter := &consoleQuotaSnapshotAdapter{}
	service := NewService(accounts, nil, nil, nil, provider.NewRegistry(adapter), nil, nil)
	queue := memory.NewQuotaRecoveryQueue()
	service.SetQuotaRecoveryQueue(queue)
	if err := queue.ScheduleQuotaRecovery(ctx, accountdomain.QuotaRecoveryEvent{AccountID: credential.ID, Mode: "console", DueAt: now.Add(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	window, err := service.RefreshQuotaMode(ctx, credential.ID, "console")
	if err != nil {
		t.Fatal(err)
	}
	if window.Remaining != 9 || adapter.fullCalls.Load() != 1 || adapter.modeCalls.Load() != 0 {
		t.Fatalf("window=%#v full=%d mode=%d", window, adapter.fullCalls.Load(), adapter.modeCalls.Load())
	}
	stored, err := accounts.GetQuotaWindows(ctx, []uint64{credential.ID})
	if err != nil {
		t.Fatal(err)
	}
	if values := stored[credential.ID]; len(values) != 3 || !completeConsoleUsageSnapshot(values) {
		t.Fatalf("stored Console usage snapshot = %#v", values)
	}
	if values, claimErr := queue.ClaimDueQuotaRecoveries(ctx, now.Add(25*time.Hour), 1, time.Minute); claimErr != nil || len(values) != 0 {
		t.Fatalf("healthy Console refresh retained stale recovery event: %#v, err = %v", values, claimErr)
	}
}

func TestConsoleFullAndModeQuotaRefreshShareOneSnapshot(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "console-quota-singleflight.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	credential, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderConsole, AuthType: accountdomain.AuthTypeSSO,
		Name: "console-singleflight", SourceKey: "console-singleflight", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &consoleQuotaSnapshotAdapter{fullStarted: make(chan struct{}, 1), fullRelease: make(chan struct{})}
	service := NewService(accounts, nil, nil, nil, provider.NewRegistry(adapter), nil, nil)

	fullDone := make(chan error, 1)
	go func() {
		windows, refreshErr := service.RefreshQuota(ctx, credential.ID)
		if refreshErr == nil && len(windows) != 3 {
			refreshErr = errors.New("full refresh did not return complete Console snapshot")
		}
		fullDone <- refreshErr
	}()
	select {
	case <-adapter.fullStarted:
	case <-time.After(time.Second):
		t.Fatal("full Console quota refresh did not start")
	}
	probeDone := make(chan error, 1)
	go func() {
		window, probeErr := service.ProbeQuotaMode(ctx, credential.ID, "console")
		if probeErr == nil && window.Remaining != 9 {
			probeErr = errors.New("mode probe did not receive shared Console snapshot")
		}
		probeDone <- probeErr
	}()
	// Give the second caller time to join the in-flight singleflight call before
	// releasing the adapter. A duplicate call remains observable via fullCalls.
	time.Sleep(20 * time.Millisecond)
	close(adapter.fullRelease)
	if err := <-fullDone; err != nil {
		t.Fatal(err)
	}
	if err := <-probeDone; err != nil {
		t.Fatal(err)
	}
	if calls := adapter.fullCalls.Load(); calls != 1 {
		t.Fatalf("Console full and mode refresh made %d upstream calls", calls)
	}
}

func TestSyncIncompleteConsoleQuotasMigratesOnlyLegacySnapshot(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "console-quota-migration.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	now := time.Now().UTC()
	create := func(name string, windows []accountdomain.QuotaWindow) uint64 {
		credential, _, createErr := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
			Provider: accountdomain.ProviderConsole, AuthType: accountdomain.AuthTypeSSO,
			Name: name, SourceKey: name, EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		for index := range windows {
			windows[index].AccountID = credential.ID
		}
		if err := accounts.ReplaceQuotaWindows(ctx, credential.ID, "", now, windows); err != nil {
			t.Fatal(err)
		}
		return credential.ID
	}
	legacyID := create("legacy-console", []accountdomain.QuotaWindow{{Mode: "console", Remaining: 20, Total: 20, SyncedAt: &now, Source: accountdomain.QuotaSourceDefault, UpdatedAt: now}})
	completeWindows := []accountdomain.QuotaWindow{
		{Mode: "console", Remaining: 9, Total: 10, SyncedAt: &now, Source: accountdomain.QuotaSourceUpstream, UpdatedAt: now},
		{Mode: "console_image", Remaining: 5, Total: 5, SyncedAt: &now, Source: accountdomain.QuotaSourceUpstream, UpdatedAt: now},
		{Mode: "console_video", Remaining: 2, Total: 2, SyncedAt: &now, Source: accountdomain.QuotaSourceUpstream, UpdatedAt: now},
	}
	completeID := create("complete-console", completeWindows)
	adapter := &consoleQuotaSnapshotAdapter{}
	service := NewService(accounts, nil, nil, nil, provider.NewRegistry(adapter), nil, nil)
	succeeded, failed, err := service.SyncIncompleteConsoleQuotas(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if succeeded != 1 || failed != 0 || adapter.fullCalls.Load() != 1 {
		t.Fatalf("migration succeeded=%d failed=%d calls=%d", succeeded, failed, adapter.fullCalls.Load())
	}
	stored, err := accounts.GetQuotaWindows(ctx, []uint64{legacyID, completeID})
	if err != nil {
		t.Fatal(err)
	}
	if !completeConsoleUsageSnapshot(stored[legacyID]) || !completeConsoleUsageSnapshot(stored[completeID]) {
		t.Fatalf("migration snapshots = %#v", stored)
	}
	blockedID := create("blocked-console", []accountdomain.QuotaWindow{{Mode: "console", Remaining: 20, Total: 20, SyncedAt: &now, Source: accountdomain.QuotaSourceDefault, UpdatedAt: now}})
	service.refreshLock = deniedQuotaRefreshLock{}
	succeeded, failed, err = service.SyncIncompleteConsoleQuotas(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if succeeded != 1 || failed != 0 {
		t.Fatalf("locked migration succeeded=%d failed=%d for account %d", succeeded, failed, blockedID)
	}
}

func TestSyncStaleConsoleQuotasRefreshesOnlyOldCompleteSnapshots(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "console-quota-stale.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	now := time.Now().UTC()
	staleAt := now.Add(-7 * time.Hour)
	createComplete := func(name string, syncedAt time.Time) uint64 {
		credential, _, createErr := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
			Provider: accountdomain.ProviderConsole, AuthType: accountdomain.AuthTypeSSO,
			Name: name, SourceKey: name, EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		if replaceErr := accounts.ReplaceQuotaWindows(ctx, credential.ID, "", syncedAt, []accountdomain.QuotaWindow{
			{Mode: "console", Remaining: 9, Total: 10, SyncedAt: &syncedAt, Source: accountdomain.QuotaSourceUpstream},
			{Mode: "console_image", Remaining: 5, Total: 5, SyncedAt: &syncedAt, Source: accountdomain.QuotaSourceUpstream},
			{Mode: "console_video", Remaining: 2, Total: 2, SyncedAt: &syncedAt, Source: accountdomain.QuotaSourceUpstream},
		}); replaceErr != nil {
			t.Fatal(replaceErr)
		}
		return credential.ID
	}
	staleID := createComplete("stale-console", staleAt)
	secondStaleID := createComplete("second-stale-console", staleAt)
	freshID := createComplete("fresh-console", now)
	legacy, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderConsole, AuthType: accountdomain.AuthTypeSSO,
		Name: "legacy-console", SourceKey: "legacy-console", EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.ReplaceQuotaWindows(ctx, legacy.ID, "", staleAt, []accountdomain.QuotaWindow{{
		Mode: "console", Remaining: 20, Total: 20, SyncedAt: &staleAt, Source: accountdomain.QuotaSourceDefault,
	}}); err != nil {
		t.Fatal(err)
	}

	adapter := &consoleQuotaSnapshotAdapter{}
	service := NewService(accounts, nil, nil, nil, provider.NewRegistry(adapter), nil, nil)
	succeeded, failed, nextAfterID, err := service.SyncStaleConsoleQuotas(ctx, now.Add(-6*time.Hour), 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if succeeded != 1 || failed != 0 || adapter.fullCalls.Load() != 1 {
		t.Fatalf("catch-up succeeded=%d failed=%d calls=%d", succeeded, failed, adapter.fullCalls.Load())
	}
	if nextAfterID != staleID {
		t.Fatalf("first scan cursor = %d, want %d", nextAfterID, staleID)
	}
	succeeded, failed, nextAfterID, err = service.SyncStaleConsoleQuotas(ctx, now.Add(-6*time.Hour), nextAfterID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if succeeded != 1 || failed != 0 || adapter.fullCalls.Load() != 2 {
		t.Fatalf("second catch-up succeeded=%d failed=%d calls=%d", succeeded, failed, adapter.fullCalls.Load())
	}
	if nextAfterID != secondStaleID {
		t.Fatalf("second scan cursor = %d, want %d", nextAfterID, secondStaleID)
	}
	stored, err := accounts.GetQuotaWindows(ctx, []uint64{staleID, secondStaleID, freshID, legacy.ID})
	if err != nil {
		t.Fatal(err)
	}
	staleWindow, _ := quotaWindowByMode(stored[staleID], "console")
	secondStaleWindow, _ := quotaWindowByMode(stored[secondStaleID], "console")
	freshWindow, _ := quotaWindowByMode(stored[freshID], "console")
	if staleWindow.SyncedAt == nil || !staleWindow.SyncedAt.After(now.Add(-6*time.Hour)) {
		t.Fatalf("stale snapshot was not refreshed: %#v", staleWindow)
	}
	if secondStaleWindow.SyncedAt == nil || !secondStaleWindow.SyncedAt.After(now.Add(-6*time.Hour)) {
		t.Fatalf("second stale snapshot was not refreshed: %#v", secondStaleWindow)
	}
	if freshWindow.SyncedAt == nil || !freshWindow.SyncedAt.Equal(now) {
		t.Fatalf("fresh snapshot was unexpectedly refreshed: %#v", freshWindow)
	}
	if completeConsoleUsageSnapshot(stored[legacy.ID]) {
		t.Fatalf("legacy snapshot must remain owned by migration: %#v", stored[legacy.ID])
	}
}

func TestObserveResponseModelCoalescesUnchangedValues(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "observed-model.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	base := relational.NewAccountRepository(database)
	credential, _, err := base.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, AuthType: accountdomain.AuthTypeOAuth,
		Name: "observed", SourceKey: "observed", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	accounts := &observedModelCountingRepository{AccountRepository: base}
	service := NewService(accounts, nil, nil, nil, nil, nil, nil)
	now := time.Now().UTC()
	service.now = func() time.Time { return now }

	if err := service.ObserveResponseModel(ctx, credential.ID, "grok-4.5"); err != nil {
		t.Fatal(err)
	}
	if err := service.ObserveResponseModel(ctx, credential.ID, "grok-4.5"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(observedModelPersistInterval - time.Second)
	if err := service.ObserveResponseModel(ctx, credential.ID, "grok-4.5"); err != nil {
		t.Fatal(err)
	}
	if calls := accounts.calls.Load(); calls != 1 {
		t.Fatalf("unchanged model writes = %d, want 1", calls)
	}

	now = now.Add(2 * time.Second)
	if err := service.ObserveResponseModel(ctx, credential.ID, "grok-4.5"); err != nil {
		t.Fatal(err)
	}
	if err := service.ObserveResponseModel(ctx, credential.ID, "grok-4.5-mini"); err != nil {
		t.Fatal(err)
	}
	if calls := accounts.calls.Load(); calls != 3 {
		t.Fatalf("interval and model transition writes = %d, want 3", calls)
	}
}

func TestObserveResponseModelRefreshesAfterCrossInstanceStateChange(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "observed-model-shared.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	base := relational.NewAccountRepository(database)
	credential, _, err := base.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, AuthType: accountdomain.AuthTypeOAuth,
		Name: "observed-shared", SourceKey: "observed-shared", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	accounts := &observedModelCountingRepository{AccountRepository: base}
	shared := &observedModelTestStore{values: make(map[uint64]repository.ObservedModelState)}
	service := NewService(accounts, nil, nil, nil, nil, nil, nil)
	service.SetObservedModelStore(shared)
	now := time.Now().UTC()
	service.now = func() time.Time { return now }
	if err := service.ObserveResponseModel(ctx, credential.ID, "grok-4.5"); err != nil {
		t.Fatal(err)
	}
	shared.mu.Lock()
	shared.values[credential.ID] = repository.ObservedModelState{Model: "grok-4.5-build-free", ObservedAt: now}
	shared.mu.Unlock()
	now = now.Add(observedModelLocalCacheTTL + time.Second)
	if err := service.ObserveResponseModel(ctx, credential.ID, "grok-4.5"); err != nil {
		t.Fatal(err)
	}
	if calls := accounts.calls.Load(); calls != 2 {
		t.Fatalf("cross-instance model change was suppressed, writes = %d", calls)
	}
}

func TestObserveResponseModelCoalescesConcurrentFirstWrite(t *testing.T) {
	accounts := &observedModelBlockingRepository{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	service := NewService(accounts, nil, nil, nil, nil, nil, nil)
	const workers = 32
	start := make(chan struct{})
	var launched sync.WaitGroup
	launched.Add(workers)
	errorsCh := make(chan error, workers)
	var wait sync.WaitGroup
	wait.Add(workers)
	for range workers {
		go func() {
			defer wait.Done()
			launched.Done()
			<-start
			errorsCh <- service.ObserveResponseModel(context.Background(), 42, "grok-4.5")
		}()
	}
	launched.Wait()
	close(start)
	<-accounts.started
	time.Sleep(25 * time.Millisecond)
	close(accounts.release)
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls := accounts.calls.Load(); calls != 1 {
		t.Fatalf("concurrent unchanged model writes = %d, want 1", calls)
	}
}

func TestObserveResponseModelKeepsNewerLocalStateAfterOutOfOrderCompletion(t *testing.T) {
	accounts := &observedModelOrderingRepository{
		olderStarted: make(chan struct{}),
		olderRelease: make(chan struct{}),
	}
	service := NewService(accounts, nil, nil, nil, nil, nil, nil)
	current := time.Now().UTC()
	var nowMu sync.RWMutex
	service.now = func() time.Time {
		nowMu.RLock()
		defer nowMu.RUnlock()
		return current
	}
	olderDone := make(chan error, 1)
	go func() {
		olderDone <- service.ObserveResponseModel(context.Background(), 42, "grok-older")
	}()
	<-accounts.olderStarted
	nowMu.Lock()
	current = current.Add(time.Minute)
	nowMu.Unlock()
	if err := service.ObserveResponseModel(context.Background(), 42, "grok-newer"); err != nil {
		t.Fatal(err)
	}
	close(accounts.olderRelease)
	if err := <-olderDone; err != nil {
		t.Fatal(err)
	}
	if err := service.ObserveResponseModel(context.Background(), 42, "grok-newer"); err != nil {
		t.Fatal(err)
	}
	if calls := accounts.calls.Load(); calls != 2 {
		t.Fatalf("out-of-order writes = %d, want 2", calls)
	}
}

type observedModelCountingRepository struct {
	repository.AccountRepository
	calls atomic.Int64
}

type observedModelBlockingRepository struct {
	repository.AccountRepository
	calls   atomic.Int64
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

type observedModelOrderingRepository struct {
	repository.AccountRepository
	calls        atomic.Int64
	olderStarted chan struct{}
	olderRelease chan struct{}
}

type observedModelTestStore struct {
	mu     sync.Mutex
	values map[uint64]repository.ObservedModelState
}

func (s *observedModelTestStore) GetObservedModelState(_ context.Context, accountID uint64) (repository.ObservedModelState, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[accountID]
	return value, ok, nil
}

func (s *observedModelTestStore) SetObservedModelState(_ context.Context, accountID uint64, value repository.ObservedModelState, _ time.Duration) error {
	s.mu.Lock()
	s.values[accountID] = value
	s.mu.Unlock()
	return nil
}

func (r *observedModelCountingRepository) UpdateObservedModel(ctx context.Context, id uint64, model string, observedAt time.Time) error {
	r.calls.Add(1)
	return r.AccountRepository.UpdateObservedModel(ctx, id, model, observedAt)
}

func (r *observedModelBlockingRepository) UpdateObservedModel(context.Context, uint64, string, time.Time) error {
	r.calls.Add(1)
	r.once.Do(func() { close(r.started) })
	<-r.release
	return nil
}

func (r *observedModelOrderingRepository) UpdateObservedModel(_ context.Context, _ uint64, model string, _ time.Time) error {
	r.calls.Add(1)
	if model == "grok-older" {
		close(r.olderStarted)
		<-r.olderRelease
	}
	return nil
}

func TestRefreshQuotaFetchesWebIdentityOnlyUntilDataExists(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "quota-identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	credential, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO,
		Name: "web-identity", SourceKey: "web-identity", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive, WebTier: accountdomain.WebTierAuto,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &quotaCountingAdapter{}
	service := NewService(accounts, nil, nil, nil, provider.NewRegistry(adapter), nil, nil)
	for range 2 {
		if _, err := service.RefreshQuota(ctx, credential.ID); err != nil {
			t.Fatal(err)
		}
	}
	if adapter.fullCalls.Load() != 2 || adapter.identityCalls.Load() != 1 {
		t.Fatalf("quota calls=%d identity calls=%d", adapter.fullCalls.Load(), adapter.identityCalls.Load())
	}
	stored, err := accounts.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Email != "identity@example.com" {
		t.Fatalf("email = %q", stored.Email)
	}
	if stored.UserID != "55555555-5555-4555-8555-555555555555" {
		t.Fatalf("user_id = %q", stored.UserID)
	}
}

func TestRefreshQuotaUnauthorizedMarksWebAccountInvalid(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "quota-unauthorized.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	credential, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO,
		Name: "web-unauthorized", SourceKey: "web-unauthorized", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &quotaCountingAdapter{fullErr: provider.ErrUnauthorized}
	service := NewService(accounts, nil, nil, nil, provider.NewRegistry(adapter), nil, nil)
	if _, err := service.RefreshQuota(ctx, credential.ID); !errors.Is(err, provider.ErrUnauthorized) {
		t.Fatalf("err = %v", err)
	}
	stored, err := accounts.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.AuthStatus != accountdomain.AuthStatusReauthRequired || !stored.Enabled {
		t.Fatalf("account state = %#v", stored)
	}
}

type deniedQuotaRefreshLock struct{}

func (deniedQuotaRefreshLock) Acquire(context.Context, string, time.Duration) (func(), bool, error) {
	return nil, false, nil
}

type recordingQuotaRefreshLock struct {
	mu   sync.Mutex
	keys []string
}

func (l *recordingQuotaRefreshLock) Acquire(_ context.Context, key string, _ time.Duration) (func(), bool, error) {
	l.mu.Lock()
	l.keys = append(l.keys, key)
	l.mu.Unlock()
	return func() {}, true, nil
}

func (l *recordingQuotaRefreshLock) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.keys...)
}

type quotaCountingAdapter struct {
	modeCalls     atomic.Int64
	fullCalls     atomic.Int64
	identityCalls atomic.Int64
	fullErr       error
	modeStarted   chan struct{}
	modeRelease   chan struct{}
}

type imagineQuotaGroupAdapter struct {
	calls atomic.Int64
}

type sharedWeeklyImagineAdapter struct {
	groupCalls atomic.Int64
	modeCalls  atomic.Int64
}

func (a *sharedWeeklyImagineAdapter) Provider() accountdomain.Provider {
	return accountdomain.ProviderWeb
}

func (a *sharedWeeklyImagineAdapter) Definition() provider.Definition {
	return provider.Definition{
		Provider: accountdomain.ProviderWeb, ModelNamespace: accountdomain.ProviderWeb.ModelNamespace(),
		Quota: provider.QuotaRemoteWindow, Credential: provider.CredentialSurface{AuthType: accountdomain.AuthTypeSSO},
	}
}

func (a *sharedWeeklyImagineAdapter) SyncQuota(context.Context, accountdomain.Credential) (provider.QuotaSnapshot, error) {
	return provider.QuotaSnapshot{}, errors.New("unexpected full quota sync")
}

func (a *sharedWeeklyImagineAdapter) SyncQuotaMode(_ context.Context, credential accountdomain.Credential, mode string) (accountdomain.QuotaWindow, error) {
	a.modeCalls.Add(1)
	if mode != "weekly" {
		return accountdomain.QuotaWindow{}, fmt.Errorf("unexpected quota mode %q", mode)
	}
	now := time.Now().UTC()
	return accountdomain.QuotaWindow{
		AccountID: credential.ID, Mode: mode, Remaining: 80, Total: 100,
		SyncedAt: &now, Source: accountdomain.QuotaSourceUpstream, UpdatedAt: now,
	}, nil
}

func (a *sharedWeeklyImagineAdapter) SyncQuotaGroup(_ context.Context, _ accountdomain.Credential, group string) (provider.QuotaGroupSnapshot, error) {
	a.groupCalls.Add(1)
	if group != accountdomain.QuotaGroupWebImagine {
		return provider.QuotaGroupSnapshot{}, fmt.Errorf("unexpected quota group %q", group)
	}
	return provider.QuotaGroupSnapshot{
		Group: group, Modes: accountdomain.WebImagineQuotaModes(), SyncedAt: time.Now().UTC(),
	}, nil
}

func (a *imagineQuotaGroupAdapter) Provider() accountdomain.Provider {
	return accountdomain.ProviderWeb
}

func (a *imagineQuotaGroupAdapter) Definition() provider.Definition {
	return provider.Definition{
		Provider: accountdomain.ProviderWeb, ModelNamespace: accountdomain.ProviderWeb.ModelNamespace(),
		Quota: provider.QuotaRemoteWindow, Credential: provider.CredentialSurface{AuthType: accountdomain.AuthTypeSSO},
	}
}

func (a *imagineQuotaGroupAdapter) SyncQuotaGroup(_ context.Context, credential accountdomain.Credential, group string) (provider.QuotaGroupSnapshot, error) {
	a.calls.Add(1)
	if group != accountdomain.QuotaGroupWebImagine {
		return provider.QuotaGroupSnapshot{}, errors.New("unexpected group")
	}
	now := time.Now().UTC()
	return provider.QuotaGroupSnapshot{
		Group: group, Modes: accountdomain.WebImagineQuotaModes(), SyncedAt: now,
		Windows: []accountdomain.QuotaWindow{{
			AccountID: credential.ID, Mode: accountdomain.QuotaModeWebImagePro, Remaining: 3,
			WindowSeconds: 86400, SyncedAt: &now, Source: accountdomain.QuotaSourceUpstream, UpdatedAt: now,
		}},
	}, nil
}

type consoleQuotaSnapshotAdapter struct {
	fullCalls   atomic.Int64
	modeCalls   atomic.Int64
	fullStarted chan struct{}
	fullRelease chan struct{}
}

type rateLimitConsoleQuotaAdapter struct {
	calls     atomic.Int64
	remaining int
	err       error
}

func (a *rateLimitConsoleQuotaAdapter) Provider() accountdomain.Provider {
	return accountdomain.ProviderConsole
}

func (a *rateLimitConsoleQuotaAdapter) Definition() provider.Definition {
	return provider.Definition{
		Provider: accountdomain.ProviderConsole, ModelNamespace: accountdomain.ProviderConsole.ModelNamespace(),
		Quota: provider.QuotaRemoteWindow, Credential: provider.CredentialSurface{AuthType: accountdomain.AuthTypeSSO},
	}
}

func (a *rateLimitConsoleQuotaAdapter) SyncQuota(_ context.Context, credential accountdomain.Credential) (provider.QuotaSnapshot, error) {
	a.calls.Add(1)
	if a.err != nil {
		return provider.QuotaSnapshot{}, a.err
	}
	now := time.Now().UTC()
	return provider.QuotaSnapshot{SyncedAt: now, Windows: []accountdomain.QuotaWindow{
		{AccountID: credential.ID, Mode: "console", Remaining: a.remaining, Total: 10, SyncedAt: &now, Source: accountdomain.QuotaSourceUpstream, UpdatedAt: now},
		{AccountID: credential.ID, Mode: "console_image", Remaining: 5, Total: 5, SyncedAt: &now, Source: accountdomain.QuotaSourceUpstream, UpdatedAt: now},
		{AccountID: credential.ID, Mode: "console_video", Remaining: 2, Total: 2, SyncedAt: &now, Source: accountdomain.QuotaSourceUpstream, UpdatedAt: now},
	}}, nil
}

func (a *rateLimitConsoleQuotaAdapter) SyncQuotaMode(context.Context, accountdomain.Credential, string) (accountdomain.QuotaWindow, error) {
	return accountdomain.QuotaWindow{}, errors.New("unexpected single-mode Console sync")
}

func (a *consoleQuotaSnapshotAdapter) Provider() accountdomain.Provider {
	return accountdomain.ProviderConsole
}

func (a *consoleQuotaSnapshotAdapter) Definition() provider.Definition {
	return provider.Definition{
		Provider: accountdomain.ProviderConsole, ModelNamespace: accountdomain.ProviderConsole.ModelNamespace(),
		Quota: provider.QuotaRemoteWindow, Credential: provider.CredentialSurface{AuthType: accountdomain.AuthTypeSSO},
	}
}

func (a *consoleQuotaSnapshotAdapter) SyncQuota(_ context.Context, credential accountdomain.Credential) (provider.QuotaSnapshot, error) {
	a.fullCalls.Add(1)
	if a.fullStarted != nil {
		a.fullStarted <- struct{}{}
	}
	if a.fullRelease != nil {
		<-a.fullRelease
	}
	now := time.Now().UTC()
	return provider.QuotaSnapshot{SyncedAt: now, Windows: []accountdomain.QuotaWindow{
		{AccountID: credential.ID, Mode: "console", Remaining: 9, Total: 10, SyncedAt: &now, Source: accountdomain.QuotaSourceUpstream, UpdatedAt: now},
		{AccountID: credential.ID, Mode: "console_image", Remaining: 4, Total: 5, SyncedAt: &now, Source: accountdomain.QuotaSourceUpstream, UpdatedAt: now},
		{AccountID: credential.ID, Mode: "console_video", Remaining: 2, Total: 2, SyncedAt: &now, Source: accountdomain.QuotaSourceUpstream, UpdatedAt: now},
	}}, nil
}

func (a *consoleQuotaSnapshotAdapter) SyncQuotaMode(_ context.Context, _ accountdomain.Credential, _ string) (accountdomain.QuotaWindow, error) {
	a.modeCalls.Add(1)
	return accountdomain.QuotaWindow{}, errors.New("unexpected single-mode Console sync")
}

func (a *quotaCountingAdapter) Provider() accountdomain.Provider { return accountdomain.ProviderWeb }

func (a *quotaCountingAdapter) Definition() provider.Definition {
	return provider.Definition{
		Provider: accountdomain.ProviderWeb, ModelNamespace: accountdomain.ProviderWeb.ModelNamespace(),
		Quota: provider.QuotaRemoteWindow, Credential: provider.CredentialSurface{AuthType: accountdomain.AuthTypeSSO},
	}
}

func (a *quotaCountingAdapter) SyncQuota(context.Context, accountdomain.Credential) (provider.QuotaSnapshot, error) {
	a.fullCalls.Add(1)
	return provider.QuotaSnapshot{}, a.fullErr
}

func (a *quotaCountingAdapter) SyncAccountIdentity(context.Context, accountdomain.Credential) (provider.AccountIdentity, error) {
	a.identityCalls.Add(1)
	return provider.AccountIdentity{Email: "identity@example.com", UserID: "55555555-5555-4555-8555-555555555555"}, nil
}

func (a *quotaCountingAdapter) SyncQuotaMode(_ context.Context, credential accountdomain.Credential, mode string) (accountdomain.QuotaWindow, error) {
	a.modeCalls.Add(1)
	if a.modeStarted != nil {
		a.modeStarted <- struct{}{}
	}
	if a.modeRelease != nil {
		<-a.modeRelease
	}
	now := time.Now().UTC()
	resetAt := now.Add(time.Hour)
	return accountdomain.QuotaWindow{
		AccountID: credential.ID, Mode: mode, Remaining: 0, Total: 30,
		WindowSeconds: 3600, ResetAt: &resetAt, SyncedAt: &now, Source: accountdomain.QuotaSourceUpstream, UpdatedAt: now,
	}, nil
}
