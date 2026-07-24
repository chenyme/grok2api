package relational

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
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
	if _, err := repo.Create(ctx, model.Route{
		PublicID: "gpt-5.5", Provider: account.ProviderBuild, UpstreamModel: "grok-4.5",
		Capability: model.CapabilityResponses, Origin: model.OriginManual, Enabled: true,
	}, nil); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("duplicate public id error = %v, want conflict", err)
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
