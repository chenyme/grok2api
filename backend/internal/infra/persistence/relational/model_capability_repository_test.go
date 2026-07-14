package relational

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestModelCapabilitiesAggregateAndGateEnabledRoutes(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "capabilities.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}

	accounts := NewAccountRepository(database)
	models := NewModelRepository(database)
	first, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "basic", SourceKey: "basic", EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "premium", SourceKey: "premium", EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	if err := models.UpsertDiscovered(ctx, account.ProviderBuild, []string{"grok-basic", "grok-premium"}); err != nil {
		t.Fatal(err)
	}

	beforeSync, err := models.ListEnabled(ctx)
	if err != nil || len(beforeSync) != 0 {
		t.Fatalf("before sync = %#v, err = %v", beforeSync, err)
	}
	now := time.Now().UTC()
	if err := models.ReplaceAccountCapabilities(ctx, first.ID, []string{"grok-basic"}, now); err != nil {
		t.Fatal(err)
	}
	if synced, err := models.HasSuccessfulAccountSync(ctx, first.ID); err != nil || !synced {
		t.Fatalf("first account sync state = %v, err = %v", synced, err)
	}
	if err := models.ReplaceAccountCapabilities(ctx, second.ID, []string{"grok-basic", "grok-premium"}, now); err != nil {
		t.Fatal(err)
	}

	values, total, err := models.List(ctx, repository.ModelListQuery{Page: repository.PageQuery{Limit: 20}})
	if err != nil || total != 2 {
		t.Fatalf("list total = %d, err = %v", total, err)
	}
	byModel := make(map[string]struct{ supported, synced, total int })
	for _, value := range values {
		byModel[value.UpstreamModel] = struct{ supported, synced, total int }{value.SupportedAccounts, value.SyncedAccounts, value.TotalAccounts}
	}
	if got := byModel["grok-basic"]; got.supported != 2 || got.synced != 2 || got.total != 2 {
		t.Fatalf("basic availability = %#v", got)
	}
	if got := byModel["grok-premium"]; got.supported != 1 || got.synced != 2 || got.total != 2 {
		t.Fatalf("premium availability = %#v", got)
	}
	if err := models.MarkAccountCapabilitySyncFailed(ctx, second.ID, now.Add(30*time.Second), "temporary failure"); err != nil {
		t.Fatal(err)
	}
	if _, err := models.GetByPublicID(ctx, "grok-premium"); err != nil {
		t.Fatalf("last successful capability must survive a failed refresh: %v", err)
	}

	if err := models.ReplaceAccountCapabilities(ctx, second.ID, []string{"grok-basic"}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	enabled, err := models.ListEnabled(ctx)
	if err != nil || len(enabled) != 1 || enabled[0].UpstreamModel != "grok-basic" {
		t.Fatalf("enabled = %#v, err = %v", enabled, err)
	}
	if _, err := models.GetByPublicID(ctx, "grok-premium"); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("premium route err = %v", err)
	}
}

func TestPublicModelNameResolvesAcrossAvailableProviders(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	models := NewModelRepository(database)
	accounts := NewAccountRepository(database)

	build, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "build", SourceKey: "shared-build",
		EncryptedAccessToken: testEncryptedToken, Enabled: true, AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	console, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderConsole, Name: "console", SourceKey: "shared-console",
		EncryptedAccessToken: testEncryptedToken, Enabled: true, AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, providerValue := range []account.Provider{account.ProviderBuild, account.ProviderConsole} {
		if err := models.UpsertDiscovered(ctx, providerValue, []string{"grok-shared"}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	if err := models.ReplaceAccountCapabilities(ctx, build.ID, []string{"grok-shared"}, now); err != nil {
		t.Fatal(err)
	}
	if err := models.ReplaceAccountCapabilities(ctx, console.ID, []string{"grok-shared"}, now); err != nil {
		t.Fatal(err)
	}

	routes, err := models.GetByPublicIDCandidates(ctx, "grok-shared")
	if err != nil || len(routes) != 2 || routes[0].Provider != account.ProviderBuild || routes[1].Provider != account.ProviderConsole {
		t.Fatalf("shared routes = %#v, err = %v", routes, err)
	}
	explicit, err := models.GetByPublicIDCandidates(ctx, "Console/grok-shared")
	if err != nil || len(explicit) != 1 || explicit[0].Provider != account.ProviderConsole {
		t.Fatalf("explicit Console route = %#v, err = %v", explicit, err)
	}
	build.Enabled = false
	if _, err := accounts.Update(ctx, build); err != nil {
		t.Fatal(err)
	}
	route, err := models.GetByPublicID(ctx, "grok-shared")
	if err != nil || route.Provider != account.ProviderConsole {
		t.Fatalf("fallback route = %#v, err = %v", route, err)
	}
}

func TestReplaceProviderRoutesReconcilesStaticCatalog(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewModelRepository(database)
	accounts := NewAccountRepository(database)
	webAccount, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, Name: "web", SourceKey: "web",
		EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceAccountCapabilities(ctx, webAccount.ID, []string{"fast"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	if err := repo.UpsertRoutes(ctx, []model.Route{
		{PublicID: "grok-chat-fast", Provider: account.ProviderWeb, UpstreamModel: "fast", Capability: model.CapabilityChat, Enabled: false},
		{PublicID: "old-obsolete", Provider: account.ProviderWeb, UpstreamModel: "obsolete", Capability: model.CapabilityChat, Enabled: true},
		{PublicID: "build-model", Provider: account.ProviderBuild, UpstreamModel: "build-model", Capability: model.CapabilityResponses, Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	var fastBefore, buildBefore modelRouteModel
	if err := database.db.WithContext(ctx).Where("provider = ? AND upstream_model = ?", account.ProviderWeb, "fast").First(&fastBefore).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Where("provider = ? AND upstream_model = ?", account.ProviderBuild, "build-model").First(&buildBefore).Error; err != nil {
		t.Fatal(err)
	}

	if err := repo.ReplaceProviderRoutes(ctx, account.ProviderWeb, []model.Route{
		{PublicID: "grok-chat-fast", Provider: account.ProviderWeb, UpstreamModel: "grok-chat-fast", Capability: model.CapabilityChat, Enabled: true},
		{PublicID: "grok-chat-auto", Provider: account.ProviderWeb, UpstreamModel: "grok-chat-auto", Capability: model.CapabilityChat, Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}

	var routes []modelRouteModel
	if err := database.db.WithContext(ctx).Where("provider = ?", account.ProviderWeb).Order("upstream_model ASC").Find(&routes).Error; err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 || routes[0].UpstreamModel != "grok-chat-auto" || routes[1].UpstreamModel != "grok-chat-fast" {
		t.Fatalf("web routes = %#v", routes)
	}
	if routes[1].ID != fastBefore.ID || routes[1].PublicID != "Web/grok-chat-fast" || routes[1].Enabled {
		t.Fatalf("reconciled fast route = %#v", routes[1])
	}
	var capability accountModelCapabilityModel
	if err := database.db.WithContext(ctx).Where("account_id = ?", webAccount.ID).First(&capability).Error; err != nil {
		t.Fatal(err)
	}
	if capability.UpstreamModel != "grok-chat-fast" {
		t.Fatalf("account capability = %#v", capability)
	}
	var buildAfter modelRouteModel
	if err := database.db.WithContext(ctx).Where("provider = ? AND upstream_model = ?", account.ProviderBuild, "build-model").First(&buildAfter).Error; err != nil {
		t.Fatal(err)
	}
	if buildAfter.ID != buildBefore.ID || buildAfter.PublicID != buildBefore.PublicID {
		t.Fatalf("build route changed: before=%#v after=%#v", buildBefore, buildAfter)
	}
}

func TestReplaceProviderRoutesCanRenameUpstreamModels(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewModelRepository(database)
	if err := repo.UpsertRoutes(ctx, []model.Route{
		{PublicID: "grok-imagine-image", Provider: account.ProviderWeb, UpstreamModel: "imagine-lite", Capability: model.CapabilityImage, Enabled: true},
		{PublicID: "grok-imagine-image-quality", Provider: account.ProviderWeb, UpstreamModel: "imagine", Capability: model.CapabilityImage, Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	var before []modelRouteModel
	if err := database.db.WithContext(ctx).Where("provider = ?", account.ProviderWeb).Order("upstream_model ASC").Find(&before).Error; err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceProviderRoutes(ctx, account.ProviderWeb, []model.Route{
		{PublicID: "grok-imagine-image", Provider: account.ProviderWeb, UpstreamModel: "grok-imagine-image", Capability: model.CapabilityImage, Enabled: true},
		{PublicID: "grok-imagine-image-quality", Provider: account.ProviderWeb, UpstreamModel: "grok-imagine-image-quality", Capability: model.CapabilityImage, Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	var after []modelRouteModel
	if err := database.db.WithContext(ctx).Where("provider = ?", account.ProviderWeb).Order("upstream_model ASC").Find(&after).Error; err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 || after[0].UpstreamModel != "grok-imagine-image" || after[0].PublicID != "Web/grok-imagine-image" || after[1].UpstreamModel != "grok-imagine-image-quality" || after[1].PublicID != "Web/grok-imagine-image-quality" {
		t.Fatalf("swapped routes = %#v", after)
	}
	beforeIDs := make(map[string]uint64, len(before))
	for _, route := range before {
		beforeIDs[route.PublicID] = route.ID
	}
	for _, route := range after {
		if beforeIDs[route.PublicID] != route.ID {
			t.Fatalf("route ID changed for %s: before=%#v after=%#v", route.PublicID, before, after)
		}
	}
}

func TestManualModelRouteBindingsAndRediscovery(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	models := NewModelRepository(database)
	accounts := NewAccountRepository(database)
	first, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "first", SourceKey: "first",
		EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "second", SourceKey: "second",
		EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}

	created, err := models.Create(ctx, model.Route{
		PublicID: "custom-build", Provider: account.ProviderBuild, UpstreamModel: "custom-upstream",
		Capability: model.CapabilityResponses, Enabled: true,
	}, []uint64{first.ID})
	if err != nil {
		t.Fatal(err)
	}
	if created.Origin != model.OriginManual || len(created.BoundAccountIDs) != 1 || created.BoundAccountIDs[0] != first.ID || created.SupportedAccounts != 1 || created.TotalAccounts != 1 {
		t.Fatalf("created route = %#v", created)
	}
	if _, err := models.GetByPublicID(ctx, created.PublicID); err != nil {
		t.Fatalf("bound route must be available without a discovery snapshot: %v", err)
	}
	candidates, err := accounts.ListRoutingCandidates(ctx, account.ProviderBuild, created.UpstreamModel, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].Credential.ID != first.ID || !candidates[0].ModelCapabilityKnown || !candidates[0].SupportsModel {
		t.Fatalf("bound candidates = %#v; second=%d", candidates, second.ID)
	}

	if err := models.Delete(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if err := models.ReplaceAccountCapabilities(ctx, first.ID, []string{created.UpstreamModel}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := models.UpsertDiscovered(ctx, account.ProviderBuild, []string{created.UpstreamModel}); err != nil {
		t.Fatal(err)
	}
	recreated, err := models.GetByPublicID(ctx, created.UpstreamModel)
	if err != nil {
		t.Fatalf("deleted route was not rediscovered: %v", err)
	}
	if recreated.ID == created.ID || recreated.PublicID != "Build/"+created.UpstreamModel || recreated.Origin != model.OriginDiscovered || len(recreated.BoundAccountIDs) != 0 {
		t.Fatalf("recreated route = %#v", recreated)
	}
}

func TestManualWebRouteSurvivesCatalogReconciliation(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewModelRepository(database)
	manual, err := repo.Create(ctx, model.Route{
		PublicID: "manual-web", Provider: account.ProviderWeb, UpstreamModel: "manual-web-upstream",
		Capability: model.CapabilityChat, Enabled: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceProviderRoutes(ctx, account.ProviderWeb, []model.Route{{
		PublicID: "grok-chat-fast", Provider: account.ProviderWeb, UpstreamModel: "grok-chat-fast",
		Capability: model.CapabilityChat, Enabled: true,
	}}); err != nil {
		t.Fatal(err)
	}
	value, err := repo.Get(ctx, manual.ID)
	if err != nil || value.Origin != model.OriginManual {
		t.Fatalf("manual route after catalog reconciliation = %#v, err = %v", value, err)
	}
}

func TestBatchDeleteModelRoutesAllowsRediscovery(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewModelRepository(database)
	first, err := repo.Create(ctx, model.Route{
		PublicID: "batch-first", Provider: account.ProviderBuild, UpstreamModel: "batch-upstream-first",
		Capability: model.CapabilityResponses, Enabled: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := repo.Create(ctx, model.Route{
		PublicID: "batch-second", Provider: account.ProviderBuild, UpstreamModel: "batch-upstream-second",
		Capability: model.CapabilityResponses, Enabled: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := repo.DeleteMany(ctx, []uint64{first.ID, second.ID})
	if err != nil || deleted != 2 {
		t.Fatalf("deleted = %d, err = %v", deleted, err)
	}
	if err := repo.UpsertDiscovered(ctx, account.ProviderBuild, []string{first.UpstreamModel, second.UpstreamModel}); err != nil {
		t.Fatal(err)
	}
	for _, value := range []model.Route{first, second} {
		if _, err := repo.Get(ctx, value.ID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("deleted route %d still exists: %v", value.ID, err)
		}
		items, total, err := repo.List(ctx, repository.ModelListQuery{Page: repository.PageQuery{Limit: 10, Search: value.UpstreamModel}})
		if err != nil || total != 1 || len(items) != 1 || items[0].ID == value.ID || items[0].UpstreamModel != value.UpstreamModel {
			t.Fatalf("rediscovered route for %s = %#v, total=%d, err=%v", value.UpstreamModel, items, total, err)
		}
	}
}

func TestWebRediscoveryRestoresCatalogRouteDefaults(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewModelRepository(database)
	value, err := repo.Create(ctx, model.Route{
		PublicID: "grok-imagine-image-edit", Provider: account.ProviderWeb, UpstreamModel: "imagine-image-edit",
		Capability: model.CapabilityImageEdit, Enabled: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Delete(ctx, value.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertDiscovered(ctx, account.ProviderWeb, []string{value.UpstreamModel}); err != nil {
		t.Fatal(err)
	}
	items, total, err := repo.List(ctx, repository.ModelListQuery{Page: repository.PageQuery{Limit: 10, Search: value.UpstreamModel}})
	if err != nil || total != 1 || len(items) != 1 {
		t.Fatalf("rediscovered web route = %#v, total=%d, err=%v", items, total, err)
	}
	if items[0].PublicID != value.PublicID || items[0].Capability != model.CapabilityImageEdit || items[0].Origin != model.OriginDiscovered {
		t.Fatalf("rediscovered web route defaults = %#v", items[0])
	}
}
