package relational

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestAddRouteAccountsIsConcurrentIdempotentSetUnion(t *testing.T) {
	database := openTestDatabase(t)
	ctx := context.Background()
	repo := NewModelRepository(database)
	accounts := createRouteAccountTestRows(t, database, account.ProviderWeb, 4)
	route, err := repo.Create(ctx, modeldomain.Route{
		PublicID: "grok-imagine-image-2.0", Provider: account.ProviderWeb,
		UpstreamModel: "grok-imagine-image-2.0", Capability: modeldomain.CapabilityImage, Enabled: true,
	}, []uint64{accounts[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	expected := repository.ModelRouteExpectation{
		PublicID: route.PublicID, Provider: route.Provider, UpstreamModel: route.UpstreamModel,
		Capability: route.Capability, Enabled: route.Enabled,
	}

	var eventsMu sync.Mutex
	var events []repository.InvalidationEvent
	repo.SetInvalidationObserver(func(_ context.Context, event repository.InvalidationEvent) {
		eventsMu.Lock()
		events = append(events, event)
		eventsMu.Unlock()
	})
	errCh := make(chan error, 2)
	for _, accountID := range []uint64{accounts[1].ID, accounts[2].ID} {
		accountID := accountID
		go func() {
			_, addErr := repo.AddRouteAccounts(ctx, route.ID, expected, []uint64{accountID})
			errCh <- addErr
		}()
	}
	for range 2 {
		if addErr := <-errCh; addErr != nil {
			t.Fatal(addErr)
		}
	}
	updated, err := repo.AddRouteAccounts(ctx, route.ID, expected, []uint64{accounts[1].ID, accounts[3].ID})
	if err != nil {
		t.Fatal(err)
	}
	want := []uint64{accounts[0].ID, accounts[1].ID, accounts[2].ID, accounts[3].ID}
	if !slices.Equal(updated.BoundAccountIDs, want) {
		t.Fatalf("bound account IDs = %v, want %v", updated.BoundAccountIDs, want)
	}

	eventsMu.Lock()
	defer eventsMu.Unlock()
	if len(events) != 3 {
		t.Fatalf("binding invalidations = %d, want 3", len(events))
	}
	for _, event := range events {
		if event.Kind != repository.InvalidationModelBindingChanged || event.Provider != account.ProviderWeb || event.UpstreamModel != route.UpstreamModel {
			t.Fatalf("invalidation = %#v", event)
		}
	}
}

func TestAddRouteAccountsFailsClosedBeforeMutation(t *testing.T) {
	database := openTestDatabase(t)
	ctx := context.Background()
	repo := NewModelRepository(database)
	webAccounts := createRouteAccountTestRows(t, database, account.ProviderWeb, 2)
	buildAccount := createRouteAccountTestRows(t, database, account.ProviderBuild, 1)[0]
	route, err := repo.Create(ctx, modeldomain.Route{
		PublicID: "grok-imagine-image-2.0", Provider: account.ProviderWeb,
		UpstreamModel: "grok-imagine-image-2.0", Capability: modeldomain.CapabilityImage, Enabled: true,
	}, []uint64{webAccounts[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	valid := repository.ModelRouteExpectation{
		PublicID: route.PublicID, Provider: route.Provider, UpstreamModel: route.UpstreamModel,
		Capability: route.Capability, Enabled: route.Enabled,
	}
	tests := []struct {
		name       string
		expected   repository.ModelRouteExpectation
		accountIDs []uint64
	}{
		{name: "public id drift", expected: withRoutePublicID(valid, "Web/other"), accountIDs: []uint64{webAccounts[1].ID}},
		{name: "provider drift", expected: withRouteProvider(valid, account.ProviderBuild), accountIDs: []uint64{webAccounts[1].ID}},
		{name: "upstream drift", expected: withRouteUpstream(valid, "other"), accountIDs: []uint64{webAccounts[1].ID}},
		{name: "capability drift", expected: withRouteCapability(valid, modeldomain.CapabilityImageEdit), accountIDs: []uint64{webAccounts[1].ID}},
		{name: "enabled drift", expected: withRouteEnabled(valid, false), accountIDs: []uint64{webAccounts[1].ID}},
		{name: "wrong provider account", expected: valid, accountIDs: []uint64{buildAccount.ID}},
		{name: "missing account", expected: valid, accountIDs: []uint64{buildAccount.ID + 100000}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, addErr := repo.AddRouteAccounts(ctx, route.ID, test.expected, test.accountIDs)
			if !errors.Is(addErr, repository.ErrConflict) {
				t.Fatalf("error = %v, want conflict", addErr)
			}
			stored, getErr := repo.Get(ctx, route.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if !slices.Equal(stored.BoundAccountIDs, []uint64{webAccounts[0].ID}) {
				t.Fatalf("bindings changed after conflict: %v", stored.BoundAccountIDs)
			}
		})
	}
}

func TestAddRouteAccountsRefusesImplicitBindingMode(t *testing.T) {
	database := openTestDatabase(t)
	ctx := context.Background()
	repo := NewModelRepository(database)
	webAccount := createRouteAccountTestRows(t, database, account.ProviderWeb, 1)[0]
	err := repo.UpsertRoutes(ctx, []modeldomain.Route{{
		PublicID: "grok-imagine-image-2.0", Provider: account.ProviderWeb,
		UpstreamModel: "grok-imagine-image-2.0", Capability: modeldomain.CapabilityImage, Enabled: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	route, err := repo.GetByPublicIDIncludingDisabled(ctx, "grok-imagine-image-2.0")
	if err != nil {
		t.Fatal(err)
	}
	expected := repository.ModelRouteExpectation{
		PublicID: route.PublicID, Provider: route.Provider, UpstreamModel: route.UpstreamModel,
		Capability: route.Capability, Enabled: route.Enabled,
	}
	_, err = repo.AddRouteAccounts(ctx, route.ID, expected, []uint64{webAccount.ID})
	if !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("error = %v, want conflict", err)
	}
	stored, getErr := repo.Get(ctx, route.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if len(stored.BoundAccountIDs) != 0 {
		t.Fatalf("implicit route was narrowed: %v", stored.BoundAccountIDs)
	}
	enabled, listErr := repo.ListEnabled(ctx)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(enabled) != 1 || enabled[0].ID != route.ID || enabled[0].SupportedAccounts != 1 {
		t.Fatalf("wildcard eligibility changed after refused add: %#v", enabled)
	}
}

func createRouteAccountTestRows(t *testing.T, database *Database, provider account.Provider, count int) []accountModel {
	t.Helper()
	values := make([]accountModel, count)
	for index := range values {
		source := fmt.Sprintf("route-member-%s-%d", provider, index)
		values[index] = accountModel{
			IdentityKey: testIdentityKey(source), Provider: string(provider), Name: source,
			SourceKey: source, Enabled: true, AuthStatus: string(account.AuthStatusActive), Priority: 1,
		}
		if err := database.db.Create(&values[index]).Error; err != nil {
			t.Fatal(err)
		}
	}
	return values
}

func withRoutePublicID(value repository.ModelRouteExpectation, publicID string) repository.ModelRouteExpectation {
	value.PublicID = publicID
	return value
}

func withRouteProvider(value repository.ModelRouteExpectation, provider account.Provider) repository.ModelRouteExpectation {
	value.Provider = provider
	return value
}

func withRouteUpstream(value repository.ModelRouteExpectation, upstream string) repository.ModelRouteExpectation {
	value.UpstreamModel = upstream
	return value
}

func withRouteCapability(value repository.ModelRouteExpectation, capability modeldomain.Capability) repository.ModelRouteExpectation {
	value.Capability = capability
	return value
}

func withRouteEnabled(value repository.ModelRouteExpectation, enabled bool) repository.ModelRouteExpectation {
	value.Enabled = enabled
	return value
}
