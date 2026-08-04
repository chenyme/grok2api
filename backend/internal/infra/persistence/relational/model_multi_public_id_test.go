package relational

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
)

func TestMultiplePublicIDsCanShareUpstream(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewModelRepository(database)
	accounts := NewAccountRepository(database)
	buildAccount, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "build", SourceKey: "build-multi-public",
		EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceAccountCapabilities(ctx, buildAccount.ID, []string{"grok-4.5"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	discovered, err := repo.Create(ctx, model.Route{
		PublicID: "grok-4.5", Provider: account.ProviderBuild, UpstreamModel: "grok-4.5",
		Capability: model.CapabilityResponses, Origin: model.OriginDiscovered, Enabled: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	aliasA, err := repo.Create(ctx, model.Route{
		PublicID: "gpt-5.4", Provider: account.ProviderBuild, UpstreamModel: "grok-4.5",
		Capability: model.CapabilityResponses, Origin: model.OriginManual, Enabled: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	aliasB, err := repo.Create(ctx, model.Route{
		PublicID: "gpt-5.5", Provider: account.ProviderBuild, UpstreamModel: "grok-4.5",
		Capability: model.CapabilityResponses, Origin: model.OriginManual, Enabled: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := repo.Create(ctx, model.Route{
		PublicID: "gpt-5.5", Provider: account.ProviderBuild, UpstreamModel: "grok-4.5",
		Capability: model.CapabilityResponses, Origin: model.OriginManual, Enabled: true,
	}, nil)
	if err != nil || duplicate.ID == aliasB.ID {
		t.Fatalf("duplicate manual target = %#v, err = %v", duplicate, err)
	}
	shared, err := repo.GetByPublicIDCandidates(ctx, "gpt-5.5")
	if err != nil || len(shared) != 2 {
		t.Fatalf("shared public candidates = %#v, err = %v", shared, err)
	}

	preferred, err := repo.GetByProviderUpstream(ctx, account.ProviderBuild, "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if preferred.ID != discovered.ID || preferred.PublicID != "Build/grok-4.5" {
		t.Fatalf("preferred route = %#v, want discovered %#v", preferred, discovered)
	}

	for _, publicID := range []string{"gpt-5.4", "gpt-5.5", "grok-4.5"} {
		route, lookupErr := repo.GetByPublicIDIncludingDisabled(ctx, publicID)
		if lookupErr != nil {
			t.Fatalf("lookup %s: %v", publicID, lookupErr)
		}
		if route.UpstreamModel != "grok-4.5" {
			t.Fatalf("route %s upstream = %q", publicID, route.UpstreamModel)
		}
	}
	if aliasA.PublicID != "Build/gpt-5.4" || aliasB.PublicID != "Build/gpt-5.5" {
		t.Fatalf("alias public ids = %#v %#v", aliasA, aliasB)
	}
}

func TestSharedUpstreamRoutesKeepAccountBindingsIsolated(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	models := NewModelRepository(database)
	accounts := NewAccountRepository(database)
	createAccount := func(name, sourceKey string) account.Credential {
		value, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderBuild, Name: name, SourceKey: sourceKey,
			EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive, Enabled: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := models.ReplaceAccountCapabilities(ctx, value.ID, []string{"shared-upstream"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		return value
	}
	accountA := createAccount("account-a", "shared-binding-a")
	accountB := createAccount("account-b", "shared-binding-b")
	createRoute := func(publicID string, accountIDs []uint64) model.Route {
		route, err := models.Create(ctx, model.Route{
			PublicID: publicID, Provider: account.ProviderBuild, UpstreamModel: "shared-upstream",
			Capability: model.CapabilityResponses, Origin: model.OriginManual, Enabled: true,
		}, accountIDs)
		if err != nil {
			t.Fatal(err)
		}
		return route
	}
	routeA := createRoute("alias-a", []uint64{accountA.ID})
	routeB := createRoute("alias-b", []uint64{accountB.ID})
	unboundRoute := createRoute("alias-unbound", nil)

	assertCandidates := func(route model.Route, expected ...uint64) {
		candidates, err := accounts.ListRoutingCandidates(ctx, account.ProviderBuild, route.ID, route.UpstreamModel, "")
		if err != nil {
			t.Fatal(err)
		}
		actual := make(map[uint64]bool, len(candidates))
		for _, candidate := range candidates {
			actual[candidate.Credential.ID] = true
		}
		if len(actual) != len(expected) {
			t.Fatalf("route %s candidates = %#v, want %v", route.PublicID, actual, expected)
		}
		for _, id := range expected {
			if !actual[id] {
				t.Fatalf("route %s candidates = %#v, missing %d", route.PublicID, actual, id)
			}
		}
	}
	assertCandidates(routeA, accountA.ID)
	assertCandidates(routeB, accountB.ID)
	assertCandidates(unboundRoute, accountA.ID, accountB.ID)
}

func TestUpsertDiscoveredCreatesCanonicalWhenManualAliasExists(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewModelRepository(database)

	manual, err := repo.Create(ctx, model.Route{
		PublicID: "gpt-5.5", Provider: account.ProviderBuild, UpstreamModel: "grok-4.5",
		Capability: model.CapabilityResponses, Origin: model.OriginManual, Enabled: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertDiscovered(ctx, account.ProviderBuild, []string{"grok-4.5"}); err != nil {
		t.Fatal(err)
	}
	var rows []modelRouteModel
	if err := database.db.WithContext(ctx).Where("provider = ? AND upstream_model = ?", account.ProviderBuild, "grok-4.5").Order("id ASC").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("routes = %#v", rows)
	}
	if rows[0].ID != manual.ID || rows[0].PublicID != "Build/gpt-5.5" || rows[0].Origin != string(model.OriginManual) {
		t.Fatalf("manual route rewritten: %#v", rows[0])
	}
	if rows[1].PublicID != "Build/grok-4.5" || rows[1].Origin != string(model.OriginDiscovered) {
		t.Fatalf("discovered route missing: %#v", rows[1])
	}
}

func TestManualRouteTargetsMaySharePublicID(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewModelRepository(database)

	for _, upstream := range []string{"grok-4.5", "grok-code-fast-1"} {
		if _, err := repo.Create(ctx, model.Route{
			PublicID: "shared-model", Provider: account.ProviderBuild, UpstreamModel: upstream,
			Capability: model.CapabilityResponses, Origin: model.OriginManual, Enabled: true,
		}, nil); err != nil {
			t.Fatalf("create manual target %q: %v", upstream, err)
		}
	}
	// The managed canonical route remains independently idempotent and may join
	// the same target pool without being shadowed by a manual row.
	if err := repo.UpsertDiscovered(ctx, account.ProviderBuild, []string{"shared-model"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertDiscovered(ctx, account.ProviderBuild, []string{"shared-model"}); err != nil {
		t.Fatal(err)
	}

	var rows []modelRouteModel
	if err := database.db.WithContext(ctx).Where("public_id = ?", "Build/shared-model").Order("id ASC").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("shared targets = %#v", rows)
	}
	managed := 0
	for _, row := range rows {
		if row.Origin != string(model.OriginManual) {
			managed++
		}
	}
	if managed != 1 {
		t.Fatalf("managed targets = %d, rows %#v", managed, rows)
	}
}

func TestReplaceProviderRoutesKeepsManualAliasesWithSharedUpstream(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewModelRepository(database)

	if err := repo.UpsertRoutes(ctx, []model.Route{
		{PublicID: "grok-chat-fast", Provider: account.ProviderWeb, UpstreamModel: "fast", Capability: model.CapabilityChat, Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	manual, err := repo.Create(ctx, model.Route{
		PublicID: "gpt-fast", Provider: account.ProviderWeb, UpstreamModel: "fast",
		Capability: model.CapabilityChat, Origin: model.OriginManual, Enabled: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var catalogBefore modelRouteModel
	if err := database.db.WithContext(ctx).Where("public_id = ?", "Web/grok-chat-fast").First(&catalogBefore).Error; err != nil {
		t.Fatal(err)
	}

	if err := repo.ReplaceProviderRoutes(ctx, account.ProviderWeb, []model.Route{
		{PublicID: "grok-chat-fast", Provider: account.ProviderWeb, UpstreamModel: "grok-chat-fast", Capability: model.CapabilityChat, Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}

	var catalogAfter modelRouteModel
	if err := database.db.WithContext(ctx).Where("id = ?", catalogBefore.ID).First(&catalogAfter).Error; err != nil {
		t.Fatal(err)
	}
	if catalogAfter.PublicID != "Web/grok-chat-fast" || catalogAfter.UpstreamModel != "grok-chat-fast" {
		t.Fatalf("catalog route = %#v", catalogAfter)
	}
	var manualAfter modelRouteModel
	if err := database.db.WithContext(ctx).Where("id = ?", manual.ID).First(&manualAfter).Error; err != nil {
		t.Fatal(err)
	}
	if manualAfter.PublicID != "Web/gpt-fast" || manualAfter.UpstreamModel != "fast" || manualAfter.Origin != string(model.OriginManual) {
		t.Fatalf("manual alias rewritten: %#v", manualAfter)
	}
}

func TestDropProviderUpstreamUniqueIndexOnUpgrade(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	if err := database.db.WithContext(ctx).Exec("CREATE UNIQUE INDEX IF NOT EXISTS uidx_provider_upstream ON model_routes(provider, upstream_model)").Error; err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	assertSQLiteMissingIndexes(t, database, "model_routes", "uidx_provider_upstream")
	assertSQLiteIndexes(t, database, "model_routes", "idx_model_routes_provider_upstream")

	repo := NewModelRepository(database)
	if _, err := repo.Create(ctx, model.Route{
		PublicID: "shared-a", Provider: account.ProviderBuild, UpstreamModel: "shared-upstream",
		Capability: model.CapabilityResponses, Origin: model.OriginManual, Enabled: true,
	}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Create(ctx, model.Route{
		PublicID: "shared-b", Provider: account.ProviderBuild, UpstreamModel: "shared-upstream",
		Capability: model.CapabilityResponses, Origin: model.OriginManual, Enabled: true,
	}, nil); err != nil {
		t.Fatal(err)
	}
}
