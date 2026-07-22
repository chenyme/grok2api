package gateway

import (
	"context"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
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
	firstBaseStart chan struct{}
	firstBaseReady chan struct{}
	baseHook       func()
	combined       []account.RoutingCandidate
	combinedCalls  int
}

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
	r.mu.Unlock()
	if hook != nil {
		hook()
	}
	if call == 1 && start != nil {
		close(start)
		<-ready
	}
	return values, nil
}

func (r *layeredAccountRepository) ListRoutingCandidates(context.Context, account.Provider, string, string) ([]account.RoutingCandidate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.combinedCalls++
	return r.combined, nil
}

func (r *layeredAccountRepository) ListRoutingAccountOverlays(_ context.Context, _ account.Provider, upstreamModel string) (account.RoutingOverlaySnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.overlayCalls == nil {
		r.overlayCalls = make(map[string]int)
	}
	r.overlayCalls[upstreamModel]++
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
		values, err := selector.loadCandidates(context.Background(), account.ProviderBuild, model, "", now)
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
	if _, err := selector.loadCandidates(context.Background(), account.ProviderBuild, "model-a", "", now); err != nil {
		t.Fatal(err)
	}
	baseCalls, modelACalls = repo.callCounts("model-a")
	if baseCalls != 2 || modelACalls != 1 {
		t.Fatalf("base invalidation reloaded base=%d overlay=%d", baseCalls, modelACalls)
	}

	selector.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationAccountCapabilityChanged, Provider: account.ProviderBuild})
	if _, err := selector.loadCandidates(context.Background(), account.ProviderBuild, "model-a", "", now); err != nil {
		t.Fatal(err)
	}
	baseCalls, modelACalls = repo.callCounts("model-a")
	if baseCalls != 2 || modelACalls != 2 {
		t.Fatalf("overlay invalidation reloaded base=%d overlay=%d", baseCalls, modelACalls)
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
		values, err := selector.loadCandidates(context.Background(), account.ProviderBuild, "model-a", "", time.Now().UTC())
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

func TestSelectorFallsBackWhenLayerVersionsKeepChanging(t *testing.T) {
	repo := newLayeredRepositoryFixture()
	repo.combined = []account.RoutingCandidate{{Credential: account.Credential{ID: 9, Provider: account.ProviderBuild}}}
	selector := NewSelector(repo, nil, nil, nil, time.Hour, time.Second, time.Minute)
	repo.baseHook = func() {
		selector.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: account.ProviderBuild})
	}
	values, err := selector.loadCandidates(context.Background(), account.ProviderBuild, "model-a", "", time.Now().UTC())
	if err != nil || len(values) != 1 || values[0].Credential.ID != 9 {
		t.Fatalf("fallback candidates = %#v, err = %v", values, err)
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.baseCalls != 4 || repo.combinedCalls != 1 {
		t.Fatalf("base calls=%d combined calls=%d", repo.baseCalls, repo.combinedCalls)
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
	combined, err := accounts.ListRoutingCandidates(ctx, account.ProviderBuild, "model-a", "")
	if err != nil {
		t.Fatal(err)
	}
	bases, err := accounts.ListRoutingAccountBases(ctx, account.ProviderBuild, "")
	if err != nil {
		t.Fatal(err)
	}
	overlay, err := accounts.ListRoutingAccountOverlays(ctx, account.ProviderBuild, "model-a")
	if err != nil {
		t.Fatal(err)
	}
	layered := assembleRoutingCandidates(account.ProviderBuild, bases, overlay)
	if !reflect.DeepEqual(layered, combined) {
		t.Fatalf("layered = %#v\ncombined = %#v", layered, combined)
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
