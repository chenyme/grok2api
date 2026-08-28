package gateway

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type layeredAccountRepository struct {
	repository.AccountRepository
	mu             sync.Mutex
	baseCalls      int
	overlayCalls   map[string]int
	bases          []account.RoutingAccountBase
	nextBases      []account.RoutingAccountBase
	overlays       map[string]account.RoutingOverlaySnapshot
	routeOverlays  map[uint64]account.RoutingOverlaySnapshot
	firstBaseStart chan struct{}
	firstBaseReady chan struct{}
	baseHook       func()
	baseErr        error
	overlayErr     error
	combined       []account.RoutingCandidate
	combinedCalls  int
	materialErrors map[uint64]error
	materials      map[uint64]account.CredentialMaterial
	materialCalls  []uint64
	healthUpdates  []repository.InvalidationEvent
	lastUsedAt     map[uint64]time.Time
}

type temporaryRoutingLoadError struct{ message string }

func (e temporaryRoutingLoadError) Error() string { return e.message }
func (temporaryRoutingLoadError) Temporary() bool { return true }

type sqliteRoutingLoadError struct{ code int }

func (e sqliteRoutingLoadError) Error() string { return "sqlite routing load failure" }
func (e sqliteRoutingLoadError) Code() int     { return e.code }

type postgresRoutingLoadError struct{ state string }

func (e postgresRoutingLoadError) Error() string    { return "postgres routing load failure" }
func (e postgresRoutingLoadError) SQLState() string { return e.state }

func (r *layeredAccountRepository) ListRoutingAccountBases(context.Context, account.Provider, string) ([]account.RoutingAccountBase, error) {
	r.mu.Lock()
	r.baseCalls++
	call := r.baseCalls
	values := r.bases
	if call > 1 && r.nextBases != nil {
		values = r.nextBases
	}
	start, ready := r.firstBaseStart, r.firstBaseReady
	hook := r.baseHook
	loadErr := r.baseErr
	r.mu.Unlock()
	if hook != nil {
		hook()
	}
	if call == 1 && start != nil {
		close(start)
		<-ready
	}
	return values, loadErr
}

func (r *layeredAccountRepository) ListRoutingCandidates(context.Context, account.Provider, uint64, string, string) ([]account.RoutingCandidate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.combinedCalls++
	return r.combined, nil
}

func (r *layeredAccountRepository) GetCredentialMaterial(ctx context.Context, accountID uint64, provider account.Provider) (account.CredentialMaterial, error) {
	if err := ctx.Err(); err != nil {
		return account.CredentialMaterial{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.materialCalls = append(r.materialCalls, accountID)
	if err := r.materialErrors[accountID]; err != nil {
		return account.CredentialMaterial{}, err
	}
	if material, ok := r.materials[accountID]; ok {
		return material, nil
	}
	return account.CredentialMaterial{AccountID: accountID, Provider: provider, AuthType: account.AuthTypeOAuth, EncryptedAccessToken: "encrypted"}, nil
}

func (r *layeredAccountRepository) UpdateHealth(_ context.Context, id uint64, _ account.Provider, failureCount int, cooldownUntil *time.Time, lastError string, _ bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	provider := account.ProviderBuild
	for _, base := range r.bases {
		if base.Credential.ID == id {
			provider = base.Credential.Provider
			break
		}
	}
	r.healthUpdates = append(r.healthUpdates, repository.InvalidationEvent{
		Kind: repository.InvalidationAccountHealthChanged, Provider: provider, AccountID: id,
		FailureCount: failureCount, CooldownUntil: cooldownUntil, HealthMarker: account.NormalizeHealthMarker(lastError),
	})
	return nil
}

func (r *layeredAccountRepository) TouchLastUsed(_ context.Context, id uint64, usedAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastUsedAt == nil {
		r.lastUsedAt = make(map[uint64]time.Time)
	}
	r.lastUsedAt[id] = usedAt
	return nil
}

func (r *layeredAccountRepository) ListRoutingAccountOverlays(_ context.Context, _ account.Provider, modelRouteID uint64, upstreamModel string) (account.RoutingOverlaySnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.overlayCalls == nil {
		r.overlayCalls = make(map[string]int)
	}
	r.overlayCalls[upstreamModel]++
	if r.overlayErr != nil {
		return account.RoutingOverlaySnapshot{}, r.overlayErr
	}
	if modelRouteID > 0 && r.routeOverlays != nil {
		return r.routeOverlays[modelRouteID], nil
	}
	return r.overlays[upstreamModel], nil
}

func (r *layeredAccountRepository) callCounts(model string) (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.baseCalls, r.overlayCalls[model]
}

func TestSelectorLayeredCacheReusesBaseAcrossModels(t *testing.T) {
	repo := newLayeredRepositoryFixture()
	selector := NewSelector(repo, nil, nil, nil, time.Hour, time.Second, time.Minute)
	now := time.Now().UTC()
	for _, model := range []string{"model-a", "model-b"} {
		values, err := selector.loadCandidates(context.Background(), account.ProviderBuild, 0, model, "", now)
		if err != nil || len(values) != 1 || values[0].Credential.ID != 1 {
			t.Fatalf("model %s candidates = %#v, err = %v", model, values, err)
		}
	}
	baseCalls, modelACalls := repo.callCounts("model-a")
	_, modelBCalls := repo.callCounts("model-b")
	if baseCalls != 1 || modelACalls != 1 || modelBCalls != 1 {
		t.Fatalf("base=%d model-a=%d model-b=%d", baseCalls, modelACalls, modelBCalls)
	}

	selector.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationAccountBillingChanged, Provider: account.ProviderBuild})
	if _, err := selector.loadCandidates(context.Background(), account.ProviderBuild, 0, "model-a", "", now); err != nil {
		t.Fatal(err)
	}
	baseCalls, modelACalls = repo.callCounts("model-a")
	if baseCalls != 2 || modelACalls != 1 {
		t.Fatalf("base invalidation reloaded base=%d overlay=%d", baseCalls, modelACalls)
	}

	selector.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationAccountCapabilityChanged, Provider: account.ProviderBuild})
	if _, err := selector.loadCandidates(context.Background(), account.ProviderBuild, 0, "model-a", "", now); err != nil {
		t.Fatal(err)
	}
	baseCalls, modelACalls = repo.callCounts("model-a")
	if baseCalls != 2 || modelACalls != 2 {
		t.Fatalf("overlay invalidation reloaded base=%d overlay=%d", baseCalls, modelACalls)
	}
}

func TestSelectorHealthInvalidationDoesNotRebuildProviderSnapshots(t *testing.T) {
	repo := newLayeredRepositoryFixture()
	selector := NewSelector(repo, nil, nil, nil, time.Hour, time.Second, time.Minute)
	now := time.Now().UTC()
	if _, err := selector.beginSelectionSession(context.Background(), account.ProviderBuild, 0, "model-a", "", "", nil, false); err != nil {
		t.Fatal(err)
	}
	baseCalls, overlayCalls := repo.callCounts("model-a")

	cooldownUntil := now.Add(time.Minute)
	selector.ApplyInvalidation(repository.InvalidationEvent{
		Kind: repository.InvalidationAccountHealthChanged, Provider: account.ProviderBuild, AccountID: 1,
		FailureCount: 1, CooldownUntil: &cooldownUntil, HealthMarker: account.LastErrorMissingThinking, PublishedAt: now,
	})
	marked := selector.applyRoutingHealth(account.Credential{ID: 1, Provider: account.ProviderBuild}, now)
	if marked.LastError != account.LastErrorMissingThinking {
		t.Fatalf("durable health marker was lost in the runtime overlay: %#v", marked)
	}
	_, err := selector.beginSelectionSession(context.Background(), account.ProviderBuild, 0, "model-a", "", "", nil, false)
	var unavailable *SelectionUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Reason != SelectionCooling {
		t.Fatalf("selection error = %v, want cooling", err)
	}
	if currentBase, currentOverlay := repo.callCounts("model-a"); currentBase != baseCalls || currentOverlay != overlayCalls {
		t.Fatalf("health update rebuilt snapshots: base %d->%d overlay %d->%d", baseCalls, currentBase, overlayCalls, currentOverlay)
	}

	selector.ApplyInvalidation(repository.InvalidationEvent{
		Kind: repository.InvalidationAccountHealthChanged, Provider: account.ProviderBuild, AccountID: 1, PublishedAt: time.Now().UTC(),
	})
	if _, err := selector.beginSelectionSession(context.Background(), account.ProviderBuild, 0, "model-a", "", "", nil, false); err != nil {
		t.Fatalf("cleared health override did not restore account: %v", err)
	}
	if currentBase, currentOverlay := repo.callCounts("model-a"); currentBase != baseCalls || currentOverlay != overlayCalls {
		t.Fatalf("health recovery rebuilt snapshots: base %d->%d overlay %d->%d", baseCalls, currentBase, overlayCalls, currentOverlay)
	}

	selector.MarkFailure(context.Background(), repo.bases[0].Credential, 500, 0)
	_, err = selector.beginSelectionSession(context.Background(), account.ProviderBuild, 0, "model-a", "", "", nil, false)
	if !errors.As(err, &unavailable) || unavailable.Reason != SelectionCooling {
		t.Fatalf("mark failure selection error = %v, want cooling", err)
	}
	if currentBase, currentOverlay := repo.callCounts("model-a"); currentBase != baseCalls || currentOverlay != overlayCalls {
		t.Fatalf("mark failure rebuilt snapshots: base %d->%d overlay %d->%d", baseCalls, currentBase, overlayCalls, currentOverlay)
	}
}

func TestSelectorHealthInvalidationRejectsOlderAccountRevision(t *testing.T) {
	selector := NewSelector(nil, nil, nil, nil, time.Hour, time.Second, time.Minute)
	now := time.Now().UTC()
	cooldownUntil := now.Add(time.Minute)
	selector.ApplyInvalidation(repository.InvalidationEvent{
		Kind: repository.InvalidationAccountHealthChanged, Provider: account.ProviderBuild, AccountID: 7,
		FailureCount: 2, CooldownUntil: &cooldownUntil, Revision: 20, PublishedAt: now,
	})
	selector.ApplyInvalidation(repository.InvalidationEvent{
		Kind: repository.InvalidationAccountHealthChanged, Provider: account.ProviderBuild, AccountID: 7,
		Revision: 19, PublishedAt: now.Add(time.Second),
	})
	value := selector.applyRoutingHealth(account.Credential{ID: 7, Provider: account.ProviderBuild}, now)
	if value.FailureCount != 2 || value.CooldownUntil == nil || !value.CooldownUntil.Equal(cooldownUntil) {
		t.Fatalf("older recovery replaced newer cooldown: %#v", value)
	}

	selector.ApplyInvalidation(repository.InvalidationEvent{
		Kind: repository.InvalidationAccountHealthChanged, Provider: account.ProviderBuild, AccountID: 7,
		Revision: 21, PublishedAt: now.Add(2 * time.Second),
	})
	value = selector.applyRoutingHealth(value, now)
	if value.FailureCount != 0 || value.CooldownUntil != nil || value.LastError != "" {
		t.Fatalf("newer recovery was not applied: %#v", value)
	}
}

func TestSelectorLayeredCacheUsesLastGoodSnapshotOnTransientLoadFailure(t *testing.T) {
	repo := newLayeredRepositoryFixture()
	selector := NewSelector(repo, nil, nil, nil, time.Hour, time.Second, time.Minute)
	now := time.Now().UTC()
	initial, err := selector.loadCandidates(context.Background(), account.ProviderBuild, 0, "model-a", "", now)
	if err != nil || len(initial) != 1 {
		t.Fatalf("initial candidates = %#v, err = %v", initial, err)
	}

	selector.candidateMu.Lock()
	for key, snapshot := range selector.candidates {
		snapshot.expiresAt = now.Add(-time.Second)
		snapshot.staleUntil = now.Add(time.Minute)
		selector.candidates[key] = snapshot
	}
	for key, snapshot := range selector.routingBases {
		snapshot.expiresAt = now.Add(-time.Second)
		snapshot.staleUntil = now.Add(time.Minute)
		selector.routingBases[key] = snapshot
	}
	for key, snapshot := range selector.routingOverlays {
		snapshot.expiresAt = now.Add(-time.Second)
		snapshot.staleUntil = now.Add(time.Minute)
		selector.routingOverlays[key] = snapshot
	}
	selector.candidateMu.Unlock()

	repo.mu.Lock()
	repo.baseErr = temporaryRoutingLoadError{message: "temporary base failure"}
	repo.overlayErr = temporaryRoutingLoadError{message: "temporary overlay failure"}
	repo.mu.Unlock()
	loaded, err := selector.loadCandidates(context.Background(), account.ProviderBuild, 0, "model-a", "", time.Now().UTC())
	if err != nil || len(loaded) != 1 || loaded[0].Credential.ID != initial[0].Credential.ID {
		t.Fatalf("stale candidates = %#v, err = %v", loaded, err)
	}
	baseCalls, overlayCalls := repo.callCounts("model-a")
	if baseCalls != 2 || overlayCalls != 2 {
		t.Fatalf("reload calls base=%d overlay=%d, want 2/2", baseCalls, overlayCalls)
	}
}

func TestSelectorStaleSnapshotDoesNotMaskCancellationOrPermanentFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "canceled", err: context.Canceled},
		{name: "deadline", err: context.DeadlineExceeded},
		{name: "permanent", err: errors.New("invalid routing query")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := newLayeredRepositoryFixture()
			selector := NewSelector(repo, nil, nil, nil, time.Hour, time.Second, time.Minute)
			now := time.Now().UTC()
			if _, err := selector.loadCandidates(context.Background(), account.ProviderBuild, 0, "model-a", "", now); err != nil {
				t.Fatal(err)
			}
			selector.candidateMu.Lock()
			for key, snapshot := range selector.candidates {
				snapshot.expiresAt = now.Add(-time.Second)
				snapshot.staleUntil = now.Add(time.Minute)
				selector.candidates[key] = snapshot
			}
			for key, snapshot := range selector.routingBases {
				snapshot.expiresAt = now.Add(-time.Second)
				snapshot.staleUntil = now.Add(time.Minute)
				selector.routingBases[key] = snapshot
			}
			selector.candidateMu.Unlock()

			repo.mu.Lock()
			repo.baseErr = test.err
			repo.mu.Unlock()
			_, err := selector.loadCandidates(context.Background(), account.ProviderBuild, 0, "model-a", "", time.Now().UTC())
			if !errors.Is(err, test.err) {
				t.Fatalf("load error = %v, want %v", err, test.err)
			}
		})
	}
}

func TestCanUseStaleRoutingSnapshotClassifiesFailuresConservatively(t *testing.T) {
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{name: "temporary marker", ctx: context.Background(), err: temporaryRoutingLoadError{message: "retry"}, want: true},
		{name: "sqlite busy", ctx: context.Background(), err: sqliteRoutingLoadError{code: 5}, want: true},
		{name: "sqlite extended locked", ctx: context.Background(), err: sqliteRoutingLoadError{code: 6 | 1<<8}, want: true},
		{name: "sqlite constraint", ctx: context.Background(), err: sqliteRoutingLoadError{code: 19}, want: false},
		{name: "postgres connection", ctx: context.Background(), err: postgresRoutingLoadError{state: "08006"}, want: true},
		{name: "postgres serialization", ctx: context.Background(), err: postgresRoutingLoadError{state: "40001"}, want: true},
		{name: "postgres query canceled", ctx: context.Background(), err: postgresRoutingLoadError{state: "57014"}, want: false},
		{name: "request canceled", ctx: context.Background(), err: context.Canceled, want: false},
		{name: "request deadline", ctx: context.Background(), err: context.DeadlineExceeded, want: false},
		{name: "canceled context wins", ctx: canceledCtx, err: temporaryRoutingLoadError{message: "retry"}, want: false},
		{name: "repository validation", ctx: context.Background(), err: repository.ErrInvalidRecord, want: false},
		{name: "unclassified failure", ctx: context.Background(), err: errors.New("broken query"), want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := canUseStaleRoutingSnapshot(test.ctx, test.err); got != test.want {
				t.Fatalf("canUseStaleRoutingSnapshot(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}

func TestSelectorInvalidationNeverFallsBackToStaleSnapshot(t *testing.T) {
	repo := newLayeredRepositoryFixture()
	selector := NewSelector(repo, nil, nil, nil, time.Hour, time.Second, time.Minute)
	if _, err := selector.loadCandidates(context.Background(), account.ProviderBuild, 0, "model-a", "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	selector.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: account.ProviderBuild})
	repo.mu.Lock()
	repo.baseErr = errors.New("base unavailable after invalidation")
	repo.mu.Unlock()
	if _, err := selector.loadCandidates(context.Background(), account.ProviderBuild, 0, "model-a", "", time.Now().UTC()); err == nil {
		t.Fatal("invalidated cache unexpectedly served its stale snapshot")
	}
}

func TestSelectorCacheSnapshotCountsAreBoundedAndLRU(t *testing.T) {
	selector := NewSelector(nil, nil, nil, nil, time.Hour, time.Second, time.Minute)
	now := time.Now().UTC()
	selector.candidateMu.Lock()
	for index := 0; index < maxCandidateCacheSnapshots+5; index++ {
		key := candidateCacheKey{provider: account.ProviderBuild, modelRouteID: uint64(index + 1)}
		snapshot := candidateSnapshot{values: []account.RoutingCandidate{{Credential: account.Credential{ID: uint64(index + 1)}}}, expiresAt: now.Add(time.Hour), staleUntil: now.Add(2 * time.Hour), lastAccess: now.Add(time.Duration(index) * time.Second)}
		selector.storeCandidateSnapshotLocked(key, snapshot, snapshot.lastAccess)
	}
	if len(selector.candidates) != maxCandidateCacheSnapshots {
		selector.candidateMu.Unlock()
		t.Fatalf("candidate snapshots = %d, want %d", len(selector.candidates), maxCandidateCacheSnapshots)
	}
	if _, exists := selector.candidates[candidateCacheKey{provider: account.ProviderBuild, modelRouteID: 1}]; exists {
		selector.candidateMu.Unlock()
		t.Fatal("least-recently-used candidate snapshot was retained")
	}
	latestKey := candidateCacheKey{provider: account.ProviderBuild, modelRouteID: uint64(maxCandidateCacheSnapshots + 5)}
	if _, exists := selector.candidates[latestKey]; !exists {
		selector.candidateMu.Unlock()
		t.Fatal("newest candidate snapshot was evicted")
	}
	selector.candidateMu.Unlock()
}

func TestSelectorLargePoolCacheUsesCandidateValueBudget(t *testing.T) {
	const accountCount = 41571
	repo := newLayeredRepositoryFixture()
	repo.bases = make([]account.RoutingAccountBase, accountCount)
	for index := range repo.bases {
		repo.bases[index].Credential = account.Credential{
			ID: uint64(index + 1), Provider: account.ProviderBuild, Enabled: true, AuthStatus: account.AuthStatusActive,
		}
	}
	selector := NewSelector(repo, nil, nil, nil, time.Hour, time.Second, time.Minute)
	now := time.Now().UTC()
	for _, model := range []string{"model-a", "model-b", "model-c"} {
		values, err := selector.loadCandidates(context.Background(), account.ProviderBuild, 0, model, "", now)
		if err != nil || len(values) != accountCount {
			t.Fatalf("model %s candidates=%d, err=%v", model, len(values), err)
		}
	}
	selector.candidateMu.Lock()
	cachedValues := 0
	for _, snapshot := range selector.candidates {
		cachedValues += len(snapshot.values)
	}
	_, oldestRetained := selector.candidates[candidateCacheKey{provider: account.ProviderBuild, upstreamModel: "model-a"}]
	selector.candidateMu.Unlock()
	if cachedValues > maxCandidateCacheValues {
		t.Fatalf("cached candidate values = %d, budget = %d", cachedValues, maxCandidateCacheValues)
	}
	if oldestRetained {
		t.Fatal("oldest large-pool model snapshot was not evicted")
	}

	// Reopening an evicted model reuses the provider base and model overlay; it
	// only reconstructs the bounded derived view and does not query the pool again.
	if _, err := selector.loadCandidates(context.Background(), account.ProviderBuild, 0, "model-a", "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	baseCalls, overlayCalls := repo.callCounts("model-a")
	if baseCalls != 1 || overlayCalls != 1 {
		t.Fatalf("reopened model queried repository: base=%d overlay=%d", baseCalls, overlayCalls)
	}
}

func TestSelectorQuotaConsumptionUsesDeltaWithoutMutatingLargeSnapshots(t *testing.T) {
	repo := newLayeredRepositoryFixture()
	repo.bases[0].QuotaWindow = &account.QuotaWindow{AccountID: 1, Mode: "fast", Remaining: 1}
	selector := NewSelector(repo, nil, nil, nil, time.Hour, time.Second, time.Minute)
	if _, err := selector.beginSelectionSession(context.Background(), account.ProviderBuild, 0, "model-a", "fast", "", nil, false); err != nil {
		t.Fatal(err)
	}
	selector.candidateMu.Lock()
	originalCandidate := selector.candidates[candidateCacheKey{provider: account.ProviderBuild, upstreamModel: "model-a", quotaMode: "fast"}].values[0].QuotaWindow.Remaining
	originalBase := selector.routingBases[routingBaseCacheKey{provider: account.ProviderBuild, quotaMode: "fast"}].values[0].QuotaWindow.Remaining
	selector.candidateMu.Unlock()

	selector.ConsumeQuota(account.ProviderBuild, 1, "fast", 1)
	_, err := selector.beginSelectionSession(context.Background(), account.ProviderBuild, 0, "model-a", "fast", "", nil, false)
	var unavailable *SelectionUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Reason != SelectionQuotaExhausted {
		t.Fatalf("selection error = %v, want quota exhausted", err)
	}
	selector.candidateMu.Lock()
	currentCandidate := selector.candidates[candidateCacheKey{provider: account.ProviderBuild, upstreamModel: "model-a", quotaMode: "fast"}].values[0].QuotaWindow.Remaining
	currentBase := selector.routingBases[routingBaseCacheKey{provider: account.ProviderBuild, quotaMode: "fast"}].values[0].QuotaWindow.Remaining
	selector.candidateMu.Unlock()
	if currentCandidate != originalCandidate || currentBase != originalBase {
		t.Fatalf("immutable snapshots changed: candidate %d->%d base %d->%d", originalCandidate, currentCandidate, originalBase, currentBase)
	}

	selector.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationAccountQuotaChanged, Provider: account.ProviderBuild})
	if _, err := selector.beginSelectionSession(context.Background(), account.ProviderBuild, 0, "model-a", "fast", "", nil, false); err != nil {
		t.Fatalf("authoritative quota invalidation did not clear local delta: %v", err)
	}
}

func TestSelectorLayeredCacheSeparatesRoutesSharingUpstream(t *testing.T) {
	repo := newLayeredRepositoryFixture()
	repo.bases = []account.RoutingAccountBase{
		{Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, Enabled: true, AuthStatus: account.AuthStatusActive}},
		{Credential: account.Credential{ID: 2, Provider: account.ProviderBuild, Enabled: true, AuthStatus: account.AuthStatusActive}},
	}
	repo.routeOverlays = map[uint64]account.RoutingOverlaySnapshot{
		101: {HasBindings: true, Values: []account.RoutingAccountOverlay{{AccountID: 1, Bound: true, ModelCapabilityKnown: true, SupportsModel: true}}},
		202: {HasBindings: true, Values: []account.RoutingAccountOverlay{{AccountID: 2, Bound: true, ModelCapabilityKnown: true, SupportsModel: true}}},
	}
	selector := NewSelector(repo, nil, nil, nil, time.Hour, time.Second, time.Minute)
	now := time.Now().UTC()
	first, err := selector.loadCandidates(context.Background(), account.ProviderBuild, 101, "shared-model", "", now)
	if err != nil || len(first) != 1 || first[0].Credential.ID != 1 {
		t.Fatalf("route 101 candidates = %#v, err = %v", first, err)
	}
	second, err := selector.loadCandidates(context.Background(), account.ProviderBuild, 202, "shared-model", "", now)
	if err != nil || len(second) != 1 || second[0].Credential.ID != 2 {
		t.Fatalf("route 202 candidates = %#v, err = %v", second, err)
	}
}

func TestSelectorLayeredLoadRetriesInsteadOfMixingVersions(t *testing.T) {
	repo := newLayeredRepositoryFixture()
	repo.nextBases = []account.RoutingAccountBase{{Credential: account.Credential{ID: 2, Provider: account.ProviderBuild, Enabled: true, AuthStatus: account.AuthStatusActive}}}
	repo.overlays["model-a"] = account.RoutingOverlaySnapshot{Values: []account.RoutingAccountOverlay{
		{AccountID: 1, ModelCapabilityKnown: true, SupportsModel: true},
		{AccountID: 2, ModelCapabilityKnown: true, SupportsModel: true},
	}}
	repo.firstBaseStart = make(chan struct{})
	repo.firstBaseReady = make(chan struct{})
	selector := NewSelector(repo, nil, nil, nil, time.Hour, time.Second, time.Minute)
	type result struct {
		values []account.RoutingCandidate
		err    error
	}
	resultCh := make(chan result, 1)
	go func() {
		values, err := selector.loadCandidates(context.Background(), account.ProviderBuild, 0, "model-a", "", time.Now().UTC())
		resultCh <- result{values: values, err: err}
	}()
	<-repo.firstBaseStart
	selector.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: account.ProviderBuild})
	close(repo.firstBaseReady)
	value := <-resultCh
	if value.err != nil || len(value.values) != 1 || value.values[0].Credential.ID != 2 {
		t.Fatalf("candidates = %#v, err = %v", value.values, value.err)
	}
	baseCalls, _ := repo.callCounts("model-a")
	if baseCalls != 2 {
		t.Fatalf("base calls = %d, want retry", baseCalls)
	}
}

func TestSelectorAppliesOutOfOrderInvalidationsSafely(t *testing.T) {
	selector := NewSelector(nil, nil, nil, nil, time.Hour, time.Second, time.Minute)
	event := repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: account.ProviderBuild, Revision: 2}
	selector.ApplyInvalidation(event)
	first := selector.routingBaseVersion(account.ProviderBuild)
	event.Revision = 1
	selector.ApplyInvalidation(event)
	second := selector.routingBaseVersion(account.ProviderBuild)
	if first.provider != 1 || second.provider != 2 {
		t.Fatalf("versions first=%#v second=%#v", first, second)
	}
}

func TestSelectorIgnoresClientKeyInvalidation(t *testing.T) {
	selector := NewSelector(nil, nil, nil, nil, time.Hour, time.Second, time.Minute)
	expiresAt := time.Now().Add(time.Hour)
	selector.routingBases[routingBaseCacheKey{provider: account.ProviderBuild}] = routingBaseSnapshot{expiresAt: expiresAt}
	selector.routingOverlays[routingOverlayCacheKey{provider: account.ProviderBuild}] = routingOverlaySnapshot{expiresAt: expiresAt}
	selector.candidates[candidateCacheKey{provider: account.ProviderBuild}] = candidateSnapshot{expiresAt: expiresAt}

	selector.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationClientKeyChanged, ClientKeyID: 42})

	if len(selector.routingBases) != 1 || len(selector.routingOverlays) != 1 || len(selector.candidates) != 1 {
		t.Fatalf("client-key event invalidated selector caches: bases=%d overlays=%d candidates=%d", len(selector.routingBases), len(selector.routingOverlays), len(selector.candidates))
	}
}

func TestSelectorScopesAccountInvalidationToCachedProvider(t *testing.T) {
	selector := NewSelector(nil, nil, nil, nil, time.Hour, time.Second, time.Minute)
	expiresAt := time.Now().Add(time.Hour)
	selector.routingBases[routingBaseCacheKey{provider: account.ProviderBuild}] = routingBaseSnapshot{expiresAt: expiresAt}
	selector.routingBases[routingBaseCacheKey{provider: account.ProviderWeb}] = routingBaseSnapshot{expiresAt: expiresAt}
	selector.routingAccountProvider[42] = account.ProviderBuild

	selector.ApplyInvalidation(repository.InvalidationEvent{
		Kind: repository.InvalidationAccountBillingChanged, AccountID: 42,
	})

	if _, ok := selector.routingBases[routingBaseCacheKey{provider: account.ProviderBuild}]; ok {
		t.Fatal("Build routing base survived its account invalidation")
	}
	if _, ok := selector.routingBases[routingBaseCacheKey{provider: account.ProviderWeb}]; !ok {
		t.Fatal("unrelated Web routing base was invalidated")
	}
	if selector.baseGlobalVersion != 0 || selector.baseProviderVersion[account.ProviderBuild] != 1 || selector.baseProviderVersion[account.ProviderWeb] != 0 {
		t.Fatalf("base versions = global:%d providers:%v", selector.baseGlobalVersion, selector.baseProviderVersion)
	}
}

func TestSelectorFallsBackToGlobalInvalidationForUnknownAccount(t *testing.T) {
	selector := NewSelector(nil, nil, nil, nil, time.Hour, time.Second, time.Minute)
	expiresAt := time.Now().Add(time.Hour)
	selector.routingBases[routingBaseCacheKey{provider: account.ProviderBuild}] = routingBaseSnapshot{expiresAt: expiresAt}
	selector.routingBases[routingBaseCacheKey{provider: account.ProviderWeb}] = routingBaseSnapshot{expiresAt: expiresAt}

	selector.ApplyInvalidation(repository.InvalidationEvent{
		Kind: repository.InvalidationAccountBillingChanged, AccountID: 99,
	})

	if len(selector.routingBases) != 0 || selector.baseGlobalVersion != 1 {
		t.Fatalf("unknown account did not trigger safe global invalidation: bases=%d global=%d", len(selector.routingBases), selector.baseGlobalVersion)
	}
}

func TestSelectorFallsBackWhenLayerVersionsKeepChanging(t *testing.T) {
	repo := newLayeredRepositoryFixture()
	repo.combined = []account.RoutingCandidate{{Credential: account.Credential{ID: 9, Provider: account.ProviderBuild}}}
	selector := NewSelector(repo, nil, nil, nil, time.Hour, time.Second, time.Minute)
	repo.baseHook = func() {
		selector.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: account.ProviderBuild})
	}
	values, err := selector.loadCandidates(context.Background(), account.ProviderBuild, 0, "model-a", "", time.Now().UTC())
	if err != nil || len(values) != 1 || values[0].Credential.ID != 9 {
		t.Fatalf("fallback candidates = %#v, err = %v", values, err)
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.baseCalls != 4 || repo.combinedCalls != 1 {
		t.Fatalf("base calls=%d combined calls=%d", repo.baseCalls, repo.combinedCalls)
	}
}

func TestSelectorHydratesOnlyClaimedCredentialAndSkipsStaleCandidate(t *testing.T) {
	repo := newLayeredRepositoryFixture()
	repo.bases = []account.RoutingAccountBase{
		{Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 20}},
		{Credential: account.Credential{ID: 2, Provider: account.ProviderBuild, Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 10}},
	}
	repo.materialErrors = map[uint64]error{1: repository.ErrNotFound}
	repo.materials = map[uint64]account.CredentialMaterial{
		2: {AccountID: 2, Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, EncryptedAccessToken: "selected-secret"},
	}
	selector := NewSelector(repo, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)

	lease, err := selector.Acquire(context.Background(), account.ProviderBuild, 0, "model-a", "", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Credential.ID != 2 || lease.Credential.EncryptedAccessToken != "selected-secret" {
		t.Fatalf("lease credential = %#v", lease.Credential)
	}
	if !reflect.DeepEqual(repo.materialCalls, []uint64{1, 2}) {
		t.Fatalf("material calls = %v, want [1 2]", repo.materialCalls)
	}
}

func TestSelectorSkipsCredentialMaterialLoadErrorAndUsesNextAccount(t *testing.T) {
	repo := newLayeredRepositoryFixture()
	repo.bases = []account.RoutingAccountBase{
		{Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 20}},
		{Credential: account.Credential{ID: 2, Provider: account.ProviderBuild, Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 10}},
	}
	repo.overlays["model-a"] = account.RoutingOverlaySnapshot{Values: []account.RoutingAccountOverlay{
		{AccountID: 1, ModelCapabilityKnown: true, SupportsModel: true},
		{AccountID: 2, ModelCapabilityKnown: true, SupportsModel: true},
	}}
	repo.materialErrors = map[uint64]error{1: errors.New("temporary credential store timeout")}
	repo.materials = map[uint64]account.CredentialMaterial{
		2: {AccountID: 2, Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, EncryptedAccessToken: "selected-secret"},
	}
	selector := NewSelector(repo, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)

	lease, err := selector.Acquire(context.Background(), account.ProviderBuild, 0, "model-a", "", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Credential.ID != 2 || lease.Credential.EncryptedAccessToken != "selected-secret" {
		t.Fatalf("lease credential = %#v", lease.Credential)
	}
	if !reflect.DeepEqual(repo.materialCalls, []uint64{1, 2}) {
		t.Fatalf("material calls = %v, want [1 2]", repo.materialCalls)
	}
}

func TestSelectorCredentialMaterialContextCancelDoesNotDrainPool(t *testing.T) {
	repo := newLayeredRepositoryFixture()
	repo.bases = []account.RoutingAccountBase{
		{Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 20}},
		{Credential: account.Credential{ID: 2, Provider: account.ProviderBuild, Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 10}},
	}
	repo.overlays["model-a"] = account.RoutingOverlaySnapshot{Values: []account.RoutingAccountOverlay{
		{AccountID: 1, ModelCapabilityKnown: true, SupportsModel: true},
		{AccountID: 2, ModelCapabilityKnown: true, SupportsModel: true},
	}}
	selector := NewSelector(repo, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := selector.Acquire(ctx, account.ProviderBuild, 0, "model-a", "", "", nil, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	if len(repo.materialCalls) > 1 {
		t.Fatalf("material calls = %v, want canceled request not to drain the pool", repo.materialCalls)
	}
}

func TestSkipUnusableCredentialMaterialHonorsCancel(t *testing.T) {
	selector := NewSelector(nil, nil, nil, nil, time.Hour, time.Second, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := selector.skipUnusableCredentialMaterial(ctx, account.Credential{ID: 1, Provider: account.ProviderBuild}, errors.New("temporary credential store timeout"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
}

func TestSelectorReportsNoAccountsWhenEveryCredentialIsStale(t *testing.T) {
	repo := newLayeredRepositoryFixture()
	repo.materialErrors = map[uint64]error{1: repository.ErrNotFound}
	selector := NewSelector(repo, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)

	_, err := selector.Acquire(context.Background(), account.ProviderBuild, 0, "model-a", "", "", nil, false)
	var unavailable *SelectionUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Reason != SelectionNoAccounts {
		t.Fatalf("error = %v, want no accounts", err)
	}
}

func TestSelectorPinnedCredentialDoesNotSwitchWhenMaterialIsStale(t *testing.T) {
	repo := newLayeredRepositoryFixture()
	repo.bases = append(repo.bases,
		account.RoutingAccountBase{Credential: account.Credential{ID: 2, Provider: account.ProviderBuild, Enabled: true, AuthStatus: account.AuthStatusActive}},
	)
	repo.materialErrors = map[uint64]error{1: repository.ErrNotFound}
	selector := NewSelector(repo, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)

	_, err := selector.AcquirePinned(context.Background(), account.ProviderBuild, 1, 0, "model-a", "", true)
	var unavailable *SelectionUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Reason != SelectionNoAccounts {
		t.Fatalf("error = %v, want no accounts", err)
	}
	if !reflect.DeepEqual(repo.materialCalls, []uint64{1}) {
		t.Fatalf("material calls = %v, want pinned account only", repo.materialCalls)
	}
}

func TestSelectorRebindsStickySessionAfterCredentialMaterialLoadError(t *testing.T) {
	repo := newLayeredRepositoryFixture()
	repo.bases = []account.RoutingAccountBase{
		{Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 20}},
		{Credential: account.Credential{ID: 2, Provider: account.ProviderBuild, Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 10}},
	}
	repo.overlays["model-a"] = account.RoutingOverlaySnapshot{Values: []account.RoutingAccountOverlay{
		{AccountID: 1, ModelCapabilityKnown: true, SupportsModel: true},
		{AccountID: 2, ModelCapabilityKnown: true, SupportsModel: true},
	}}
	repo.materialErrors = map[uint64]error{1: errors.New("temporary credential store timeout")}
	repo.materials = map[uint64]account.CredentialMaterial{
		2: {AccountID: 2, Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, EncryptedAccessToken: "selected-secret"},
	}
	sticky := memory.NewStickyStore()
	affinity := "load-error-session"
	if err := sticky.Set(context.Background(), stickySessionKey(affinity), 1, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(repo, memory.NewConcurrencyLimiter(), sticky, nil, time.Hour, time.Second, time.Minute)

	lease, err := selector.Acquire(context.Background(), account.ProviderBuild, 0, "model-a", "", affinity, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Credential.ID != 2 {
		t.Fatalf("selected account = %d, want fallback account 2", lease.Credential.ID)
	}
}

func TestSelectorRebindsStickySessionAfterCredentialBecomesStale(t *testing.T) {
	repo := newLayeredRepositoryFixture()
	repo.bases = []account.RoutingAccountBase{
		{Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 20}},
		{Credential: account.Credential{ID: 2, Provider: account.ProviderBuild, Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 10}},
	}
	repo.materialErrors = map[uint64]error{1: repository.ErrNotFound}
	sticky := memory.NewStickyStore()
	affinity := "stale-session"
	if err := sticky.Set(context.Background(), stickySessionKey(affinity), 1, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(repo, memory.NewConcurrencyLimiter(), sticky, nil, time.Hour, time.Second, time.Minute)

	lease, err := selector.Acquire(context.Background(), account.ProviderBuild, 0, "model-a", "", affinity, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Credential.ID != 2 {
		t.Fatalf("selected account = %d, want fallback account 2", lease.Credential.ID)
	}
	boundID, ok, err := sticky.Get(context.Background(), stickySessionKey(affinity), time.Now())
	if err != nil || !ok || boundID != 2 {
		t.Fatalf("sticky binding = %d, ok=%v, err=%v", boundID, ok, err)
	}
}

func TestSelectorReleasesCapacityWhenCredentialHydrationFails(t *testing.T) {
	repo := newLayeredRepositoryFixture()
	loadErr := errors.New("credential storage unavailable")
	repo.materialErrors = map[uint64]error{1: loadErr}
	limiter := memory.NewConcurrencyLimiter()
	selector := NewSelector(repo, limiter, memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)

	_, err := selector.Acquire(context.Background(), account.ProviderBuild, 0, "model-a", "", "", nil, false)
	var unavailable *SelectionUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Reason != SelectionNoAccounts {
		t.Fatalf("error = %v, want no accounts after skipping the only candidate", err)
	}
	current, currentErr := limiter.Current(context.Background(), repository.AccountConcurrencyKey(1))
	if currentErr != nil {
		t.Fatal(currentErr)
	}
	if current != 0 {
		t.Fatalf("current concurrency = %d, want released slot", current)
	}
}

func TestSelectorRejectsCrossProviderCredentialMaterial(t *testing.T) {
	repo := newLayeredRepositoryFixture()
	repo.materials = map[uint64]account.CredentialMaterial{
		1: {AccountID: 1, Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, EncryptedAccessToken: "wrong-provider-secret"},
	}
	limiter := memory.NewConcurrencyLimiter()
	selector := NewSelector(repo, limiter, memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)

	_, err := selector.Acquire(context.Background(), account.ProviderBuild, 0, "model-a", "", "", nil, false)
	var unavailable *SelectionUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Reason != SelectionNoAccounts {
		t.Fatalf("error = %v, want no accounts", err)
	}
	current, currentErr := limiter.Current(context.Background(), repository.AccountConcurrencyKey(1))
	if currentErr != nil {
		t.Fatal(currentErr)
	}
	if current != 0 {
		t.Fatalf("current concurrency = %d, want released slot", current)
	}
}

func TestLayeredRoutingMatchesCombinedRepositoryResult(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "layered-equivalence.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	models := relational.NewModelRepository(database)
	first, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "first", SourceKey: "first", EncryptedAccessToken: "encrypted", AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "second", SourceKey: "second", EncryptedAccessToken: "encrypted", AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := accounts.SaveBilling(ctx, account.Billing{AccountID: second.ID, MonthlyLimit: 100, Used: 10, SyncedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := models.ReplaceAccountCapabilities(ctx, first.ID, []string{"other-model"}, now); err != nil {
		t.Fatal(err)
	}
	if err := models.ReplaceAccountCapabilities(ctx, second.ID, []string{"model-a"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := models.Create(ctx, model.Route{
		PublicID: "model-a", Provider: account.ProviderBuild, UpstreamModel: "model-a", Capability: model.CapabilityResponses, Enabled: true,
	}, []uint64{second.ID}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.UpsertModelQuotaBlock(ctx, account.ModelQuotaBlock{
		AccountID: second.ID, UpstreamModel: "model-a", Reason: "test", CooldownUntil: now.Add(time.Hour), UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	combined, err := accounts.ListRoutingCandidates(ctx, account.ProviderBuild, 0, "model-a", "")
	if err != nil {
		t.Fatal(err)
	}
	bases, err := accounts.ListRoutingAccountBases(ctx, account.ProviderBuild, "")
	if err != nil {
		t.Fatal(err)
	}
	overlay, err := accounts.ListRoutingAccountOverlays(ctx, account.ProviderBuild, 0, "model-a")
	if err != nil {
		t.Fatal(err)
	}
	layered := assembleRoutingCandidates(account.ProviderBuild, "", bases, overlay)
	if !reflect.DeepEqual(layered, combined) {
		t.Fatalf("layered = %#v\ncombined = %#v", layered, combined)
	}
}

func TestAssembleRoutingCandidatesAllowsRecognizedStaticConsoleModelWithStaleSnapshot(t *testing.T) {
	bases := []account.RoutingAccountBase{{Credential: account.Credential{
		ID: 1, Provider: account.ProviderConsole, Enabled: true, AuthStatus: account.AuthStatusActive,
	}}}
	overlay := account.RoutingOverlaySnapshot{Values: []account.RoutingAccountOverlay{{
		AccountID: 1, ModelCapabilityKnown: true, SupportsModel: false,
	}}}

	recognized := assembleRoutingCandidates(account.ProviderConsole, "console_image", bases, overlay)
	if len(recognized) != 1 || !recognized[0].ModelCapabilityKnown || !recognized[0].SupportsModel {
		t.Fatalf("recognized static Console model = %#v", recognized)
	}
	unknown := assembleRoutingCandidates(account.ProviderConsole, "", bases, overlay)
	if len(unknown) != 1 || !unknown[0].ModelCapabilityKnown || unknown[0].SupportsModel {
		t.Fatalf("unknown Console model = %#v", unknown)
	}
}

func TestAssembleRoutingCandidatesAllowsRecognizedWebImagineModelWithStaleSnapshot(t *testing.T) {
	bases := []account.RoutingAccountBase{{Credential: account.Credential{
		ID: 1, Provider: account.ProviderWeb, Enabled: true, AuthStatus: account.AuthStatusActive,
	}}}
	overlay := account.RoutingOverlaySnapshot{Values: []account.RoutingAccountOverlay{{
		AccountID: 1, ModelCapabilityKnown: true, SupportsModel: false,
	}}}

	recognized := assembleRoutingCandidates(account.ProviderWeb, account.QuotaModeWebImagePro, bases, overlay)
	if len(recognized) != 1 || !recognized[0].ModelCapabilityKnown || !recognized[0].SupportsModel {
		t.Fatalf("recognized Web Imagine model = %#v", recognized)
	}
	unknown := assembleRoutingCandidates(account.ProviderWeb, "", bases, overlay)
	if len(unknown) != 1 || !unknown[0].ModelCapabilityKnown || unknown[0].SupportsModel {
		t.Fatalf("unknown Web model = %#v", unknown)
	}
}

func newLayeredRepositoryFixture() *layeredAccountRepository {
	return &layeredAccountRepository{
		bases: []account.RoutingAccountBase{{Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, Enabled: true, AuthStatus: account.AuthStatusActive}}},
		overlays: map[string]account.RoutingOverlaySnapshot{
			"model-a": {Values: []account.RoutingAccountOverlay{{AccountID: 1, ModelCapabilityKnown: true, SupportsModel: true}}},
			"model-b": {Values: []account.RoutingAccountOverlay{{AccountID: 1, ModelCapabilityKnown: true, SupportsModel: true}}},
		},
		overlayCalls: make(map[string]int),
	}
}
