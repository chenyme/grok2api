package account

import (
	"context"
	"errors"
	"path/filepath"
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
	service.QueueWebQuotaRefresh(42, "fast")
	service.QueueWebQuotaRefresh(42, "expert")
	service.QueueWebQuotaRefresh(42, "fast")

	service.quotaRefreshMu.Lock()
	defer service.quotaRefreshMu.Unlock()
	if len(service.quotaRefreshes) != 2 {
		t.Fatalf("refresh states = %#v", service.quotaRefreshes)
	}
	if service.quotaRefreshes["42:fast"].generation != 2 || !service.quotaRefreshes["42:fast"].queued {
		t.Fatal("duplicate fast refresh was not coalesced into the queued generation")
	}
	if service.quotaRefreshes["42:expert"].generation != 1 || !service.quotaRefreshes["42:expert"].queued {
		t.Fatal("independent expert refresh state is invalid")
	}
	if len(service.quotaRefreshQueue) != 2 {
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
		service.runWebQuotaRefresh(ctx, request)
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
	service.quotaRefreshQueue = make(chan webQuotaRefreshRequest, 1)
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
	go func() { first.RunWebQuotaRefresh(runCtx); done <- struct{}{} }()
	go func() { second.RunWebQuotaRefresh(runCtx); done <- struct{}{} }()
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
	time.Sleep(2 * webQuotaRefreshRetryInterval)
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
	service.runWebQuotaRefresh(ctx, request)
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
	service.runWebQuotaRefresh(ctx, request)
	if adapter.modeCalls.Load() != 2 {
		t.Fatalf("worker without distributed lease made %d mode calls", adapter.modeCalls.Load())
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

type quotaCountingAdapter struct {
	modeCalls     atomic.Int64
	fullCalls     atomic.Int64
	identityCalls atomic.Int64
	fullErr       error
	modeStarted   chan struct{}
	modeRelease   chan struct{}
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
	return provider.AccountIdentity{Email: "identity@example.com"}, nil
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
