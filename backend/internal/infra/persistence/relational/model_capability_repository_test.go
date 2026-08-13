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

func TestConsoleBuiltInModelIgnoresStaleAccountCapabilitySnapshot(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accounts := NewAccountRepository(database)
	models := NewModelRepository(database)

	credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderConsole, Name: "console", SourceKey: "console",
		EncryptedAccessToken: testEncryptedToken, Enabled: true, AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := models.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-4.3"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	candidates, err := accounts.ListRoutingCandidates(ctx, account.ProviderConsole, 0, "grok-imagine-image-quality", "console_image")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || !candidates[0].ModelCapabilityKnown || !candidates[0].SupportsModel {
		t.Fatalf("built-in Console model must survive a stale account snapshot: %#v", candidates)
	}

	candidates, err = accounts.ListRoutingCandidates(ctx, account.ProviderConsole, 0, "unknown-console-model", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || !candidates[0].ModelCapabilityKnown || candidates[0].SupportsModel {
		t.Fatalf("unknown Console model must retain capability gating: %#v", candidates)
	}
}

func TestConsoleCatalogRoutesUseAutomaticAccountPoolWithoutCapabilitySnapshot(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accounts := NewAccountRepository(database)
	models := NewModelRepository(database)

	createAccount := func(name string) account.Credential {
		t.Helper()
		value, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderConsole, Name: name, SourceKey: name,
			EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive,
		})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	first := createAccount("console-first")
	second := createAccount("console-second")
	disabled := createAccount("console-disabled")
	disabled.Enabled = false
	if _, err := accounts.Update(ctx, disabled); err != nil {
		t.Fatal(err)
	}
	reauth := createAccount("console-reauth")
	reauth.AuthStatus = account.AuthStatusReauthRequired
	if _, err := accounts.Update(ctx, reauth); err != nil {
		t.Fatal(err)
	}

	const publicID = "grok-imagine-image-quality"
	if err := models.UpsertRoutes(ctx, []model.Route{
		{PublicID: publicID, Provider: account.ProviderConsole, UpstreamModel: publicID, Capability: model.CapabilityImage, Origin: model.OriginCatalog, Enabled: true},
		{PublicID: publicID, Provider: account.ProviderConsole, UpstreamModel: publicID, Capability: model.CapabilityImageEdit, Origin: model.OriginCatalog, Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}

	enabled, err := models.ListEnabled(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(enabled) != 2 {
		t.Fatalf("enabled Console routes = %#v", enabled)
	}
	for _, route := range enabled {
		if route.SupportedAccounts != 2 || route.TotalAccounts != 2 || route.SyncedAccounts != 0 || len(route.BoundAccountIDs) != 0 {
			t.Fatalf("automatic Console account pool = %#v", route)
		}
	}
	unknown, err := models.Create(ctx, model.Route{
		PublicID: "legacy-unknown-console", Provider: account.ProviderConsole, UpstreamModel: "unknown-console-model",
		Capability: model.CapabilityImage, Enabled: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if unknown.SupportedAccounts != 0 || unknown.TotalAccounts != 2 {
		t.Fatalf("unknown Console route must not inherit static catalog support: %#v", unknown)
	}
	if enabledAfterUnknown, err := models.ListEnabled(ctx); err != nil || len(enabledAfterUnknown) != 2 {
		t.Fatalf("unknown Console route leaked into enabled routes: %#v, err=%v", enabledAfterUnknown, err)
	}
	alias, err := models.Create(ctx, model.Route{
		PublicID: "console-image-alias", Provider: account.ProviderConsole, UpstreamModel: publicID,
		Capability: model.CapabilityImage, Enabled: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if alias.SupportedAccounts != 2 || alias.TotalAccounts != 2 {
		t.Fatalf("manual alias for Console catalog route = %#v", alias)
	}

	boundIDs := []uint64{first.ID}
	boundRoute := enabled[0]
	boundRoute, err = models.Update(ctx, boundRoute, &boundIDs)
	if err != nil {
		t.Fatal(err)
	}
	if boundRoute.SupportedAccounts != 1 || boundRoute.TotalAccounts != 1 || len(boundRoute.BoundAccountIDs) != 1 || boundRoute.BoundAccountIDs[0] != first.ID {
		t.Fatalf("manually bound Console route = %#v; second=%d", boundRoute, second.ID)
	}
}

func TestModelRouteGroupsKeepCapabilitiesCompleteAcrossStatusFilters(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	models := NewModelRepository(database)

	const publicID = "grouped-console-image"
	if err := models.UpsertRoutes(ctx, []model.Route{
		{PublicID: publicID, Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-image", Capability: model.CapabilityImage, Origin: model.OriginCatalog, Enabled: true},
		{PublicID: publicID, Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-image", Capability: model.CapabilityImageEdit, Origin: model.OriginCatalog, Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	values, total, err := models.ListGroups(ctx, repository.ModelListQuery{Page: repository.PageQuery{Limit: 20}})
	if err != nil || total != 1 || len(values) != 1 || len(values[0].Routes) != 2 {
		t.Fatalf("initial groups = %#v, total=%d, err=%v", values, total, err)
	}
	disabled := false
	editRoute := values[0].Routes[0]
	if editRoute.Capability != model.CapabilityImageEdit {
		editRoute = values[0].Routes[1]
	}
	editRoute.Enabled = false
	if _, err := models.Update(ctx, editRoute, nil); err != nil {
		t.Fatal(err)
	}

	enabled := true
	values, total, err = models.ListGroups(ctx, repository.ModelListQuery{
		Page: repository.PageQuery{Limit: 1}, Filter: repository.ModelListFilter{Enabled: &enabled},
	})
	if err != nil || total != 1 || len(values) != 1 || len(values[0].Routes) != 2 {
		t.Fatalf("enabled-filtered groups lost a capability: %#v, total=%d, err=%v", values, total, err)
	}
	if values[0].Routes[0].Enabled == values[0].Routes[1].Enabled {
		t.Fatalf("mixed group status was not preserved: %#v", values[0])
	}
	values, total, err = models.ListGroups(ctx, repository.ModelListQuery{
		Page: repository.PageQuery{Limit: 20}, Filter: repository.ModelListFilter{Enabled: &disabled},
	})
	if err != nil || total != 0 || len(values) != 0 {
		t.Fatalf("partially enabled group leaked into disabled filter: %#v, total=%d, err=%v", values, total, err)
	}

	for _, capability := range []model.Capability{model.CapabilityImage, model.CapabilityImageEdit} {
		if _, err := models.Create(ctx, model.Route{
			PublicID: "manual-group-target", Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-image",
			Capability: capability, Enabled: true,
		}, nil); err != nil {
			t.Fatal(err)
		}
	}
	values, total, err = models.ListGroups(ctx, repository.ModelListQuery{Page: repository.PageQuery{Limit: 20}})
	if err != nil || total != 3 || len(values) != 3 {
		t.Fatalf("manual targets must remain independent: %#v, total=%d, err=%v", values, total, err)
	}
	for _, field := range []string{"publicId", "upstreamModel", "status", "provider", "accountSupport", "lastSyncedAt"} {
		if _, _, err := models.ListGroups(ctx, repository.ModelListQuery{Page: repository.PageQuery{
			Limit: 20, Sort: repository.SortQuery{Field: field, Direction: repository.SortDescending},
		}}); err != nil {
			t.Fatalf("sort grouped models by %s: %v", field, err)
		}
	}
}

func TestBuildPaidCapabilitiesAreSharedAcrossActiveSuperAccounts(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accounts := NewAccountRepository(database)
	models := NewModelRepository(database)

	createAccount := func(name string) account.Credential {
		t.Helper()
		value, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderBuild, Name: name, SourceKey: name,
			EncryptedAccessToken: testEncryptedToken, Enabled: true, AuthStatus: account.AuthStatusActive,
		})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	observer := createAccount("super-observer")
	peer := createAccount("super-peer")
	freeObserver := createAccount("free-observer")
	freePeer := createAccount("free-peer")
	now := time.Now().UTC()
	if err := accounts.SaveBilling(ctx, account.Billing{AccountID: observer.ID, MonthlyLimit: 100, SyncedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.SaveBilling(ctx, account.Billing{AccountID: peer.ID, OnDemandCap: 50, SyncedAt: now}); err != nil {
		t.Fatal(err)
	}

	const sharedModel = "grok-super-shared"
	if err := models.UpsertDiscovered(ctx, account.ProviderBuild, []string{sharedModel, "grok-4.5"}); err != nil {
		t.Fatal(err)
	}
	for accountID, capabilities := range map[uint64][]string{
		observer.ID:     {sharedModel, "grok-4.5"},
		peer.ID:         {"grok-4.5"},
		freeObserver.ID: {sharedModel},
		freePeer.ID:     {"grok-4.5"},
	} {
		if err := models.ReplaceAccountCapabilities(ctx, accountID, capabilities, now); err != nil {
			t.Fatal(err)
		}
	}

	route, err := models.GetByProviderUpstream(ctx, account.ProviderBuild, sharedModel)
	if err != nil {
		t.Fatal(err)
	}
	route, err = models.Get(ctx, route.ID)
	if err != nil {
		t.Fatal(err)
	}
	if route.SupportedAccounts != 3 || route.TotalAccounts != 4 {
		t.Fatalf("shared route availability = %#v", route)
	}
	if _, _, err := models.List(ctx, repository.ModelListQuery{
		Page: repository.PageQuery{Limit: 20, Sort: repository.SortQuery{Field: "accountSupport", Direction: repository.SortDescending}},
	}); err != nil {
		t.Fatalf("sort by shared account support: %v", err)
	}
	candidates, err := accounts.ListRoutingCandidates(ctx, account.ProviderBuild, 0, sharedModel, "")
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[uint64]account.RoutingCandidate, len(candidates))
	for _, candidate := range candidates {
		byID[candidate.Credential.ID] = candidate
	}
	for _, accountID := range []uint64{observer.ID, peer.ID, freeObserver.ID} {
		if candidate := byID[accountID]; !candidate.ModelCapabilityKnown || !candidate.SupportsModel {
			t.Fatalf("account %d should support shared model: %#v", accountID, candidate)
		}
	}
	if candidate := byID[freePeer.ID]; !candidate.ModelCapabilityKnown || candidate.SupportsModel {
		t.Fatalf("free peer must keep its own capability snapshot: %#v", candidate)
	}

	observer.Enabled = false
	if _, err := accounts.Update(ctx, observer); err != nil {
		t.Fatal(err)
	}
	route, err = models.Get(ctx, route.ID)
	if err != nil {
		t.Fatal(err)
	}
	if route.SupportedAccounts != 1 || route.TotalAccounts != 3 {
		t.Fatalf("disabled observer must not grant shared entitlement: %#v", route)
	}
	candidates, err = accounts.ListRoutingCandidates(ctx, account.ProviderBuild, 0, sharedModel, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range candidates {
		if candidate.Credential.ID == peer.ID && candidate.SupportsModel {
			t.Fatalf("paid peer should lose shared capability without an active paid observer: %#v", candidate)
		}
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

func TestModelRouteLookupPrioritizesDirectPublicIDOverAlias(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	models := NewModelRepository(database)

	aliasRoute := modelRouteModel{
		PublicID: "Build/grok-legacy-priority", Provider: string(account.ProviderBuild), UpstreamModel: "grok-legacy-priority",
		Capability: string(model.CapabilityResponses), Origin: string(model.OriginManual), Enabled: true,
	}
	directRoute := modelRouteModel{
		PublicID: "Web/grok-priority", Provider: string(account.ProviderWeb), UpstreamModel: "grok-priority",
		Capability: string(model.CapabilityChat), Origin: string(model.OriginManual), Enabled: true,
	}
	if err := database.db.WithContext(ctx).Create(&aliasRoute).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Create(&directRoute).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Create(&modelRouteAliasModel{Alias: "grok-priority", ModelRouteID: aliasRoute.ID, CreatedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}

	routes, err := findModelRoutesByPublicID(database.db.WithContext(ctx), "grok-priority")
	if err != nil || len(routes) != 2 {
		t.Fatalf("routes = %#v, err = %v", routes, err)
	}
	if routes[0].ID != directRoute.ID || routes[1].ID != aliasRoute.ID {
		t.Fatalf("direct/alias priority = %#v", routes)
	}
	value, err := models.GetByPublicIDIncludingDisabled(ctx, "grok-priority")
	if err != nil || value.ID != directRoute.ID {
		t.Fatalf("selected direct route = %#v, err = %v", value, err)
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

func TestReplaceProviderRoutesRestoresWebImageLitePublicIDs(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewModelRepository(database)
	if err := repo.UpsertRoutes(ctx, []model.Route{
		{PublicID: "grok-imagine-image-2.0", Provider: account.ProviderWeb, UpstreamModel: "grok-imagine-image", Capability: model.CapabilityImage, Enabled: true},
		{PublicID: "grok-imagine-image-2.0", Provider: account.ProviderWeb, UpstreamModel: "grok-imagine-image", Capability: model.CapabilityImageEdit, Enabled: true},
		{PublicID: "grok-imagine-image-quality-2.0", Provider: account.ProviderWeb, UpstreamModel: "grok-imagine-image-quality", Capability: model.CapabilityImage, Enabled: true},
		{PublicID: "grok-imagine-image-quality-2.0", Provider: account.ProviderWeb, UpstreamModel: "grok-imagine-image-quality", Capability: model.CapabilityImageEdit, Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	var before []modelRouteModel
	if err := database.db.WithContext(ctx).Where("provider = ?", account.ProviderWeb).Order("upstream_model ASC").Find(&before).Error; err != nil {
		t.Fatal(err)
	}
	for _, route := range before {
		if route.Capability != string(model.CapabilityImage) {
			continue
		}
		alias := "Web/grok-imagine-image-lite"
		if route.UpstreamModel == "grok-imagine-image-quality" {
			alias = "Web/grok-imagine-image-quality-lite"
		}
		if err := database.db.WithContext(ctx).Create(&modelRouteAliasModel{Alias: alias, ModelRouteID: route.ID}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.ReplaceProviderRoutes(ctx, account.ProviderWeb, []model.Route{
		{PublicID: "grok-imagine-image-lite", Provider: account.ProviderWeb, UpstreamModel: "grok-imagine-image", Capability: model.CapabilityImage, Enabled: true},
		{PublicID: "grok-imagine-image-quality-lite", Provider: account.ProviderWeb, UpstreamModel: "grok-imagine-image-quality", Capability: model.CapabilityImage, Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	var after []modelRouteModel
	if err := database.db.WithContext(ctx).Where("provider = ?", account.ProviderWeb).Order("upstream_model ASC").Find(&after).Error; err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 {
		t.Fatalf("renamed routes = %#v", after)
	}
	beforeIDs := make(map[string]uint64, len(before))
	for _, route := range before {
		beforeIDs[route.UpstreamModel+"|"+route.Capability] = route.ID
	}
	for _, route := range after {
		wantPublicID := "Web/grok-imagine-image-lite"
		if route.UpstreamModel == "grok-imagine-image-quality" {
			wantPublicID = "Web/grok-imagine-image-quality-lite"
		}
		if route.Capability != string(model.CapabilityImage) || route.PublicID != wantPublicID || beforeIDs[route.UpstreamModel+"|"+route.Capability] != route.ID {
			t.Fatalf("route ID changed for %s: before=%#v after=%#v", route.UpstreamModel, before, after)
		}
	}
	compatibilityAliases := map[string]string{
		"grok-imagine-image-2.0":         "Web/grok-imagine-image-lite",
		"grok-imagine-image-quality-2.0": "Web/grok-imagine-image-quality-lite",
	}
	for oldPublicID, wantPublicID := range compatibilityAliases {
		route, err := repo.GetByPublicIDIncludingDisabled(ctx, oldPublicID)
		if err != nil || route.PublicID != wantPublicID || route.Capability != model.CapabilityImage {
			t.Fatalf("Web compatibility alias %s resolved as %#v, err=%v", oldPublicID, route, err)
		}
	}
	var promotedAliasCount int64
	if err := database.db.WithContext(ctx).Model(&modelRouteAliasModel{}).
		Where("alias IN ?", []string{"Web/grok-imagine-image-lite", "Web/grok-imagine-image-quality-lite"}).
		Count(&promotedAliasCount).Error; err != nil {
		t.Fatal(err)
	}
	if promotedAliasCount != 0 {
		t.Fatalf("promoted aliases remain: %d", promotedAliasCount)
	}
}

func TestReplaceProviderRoutesRestoresThreeConsoleImageModels(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewModelRepository(database)
	legacyCatalog := []model.Route{
		{PublicID: "grok-imagine-image-quality-2.0", Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-image-quality", Capability: model.CapabilityImage, Enabled: true},
		{PublicID: "grok-imagine-image-quality-2.0", Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-image-quality", Capability: model.CapabilityImageEdit, Enabled: true},
		{PublicID: "grok-imagine-image-2.0", Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-image", Capability: model.CapabilityImage, Enabled: true},
		{PublicID: "grok-imagine-image-2.0", Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-image", Capability: model.CapabilityImageEdit, Enabled: true},
	}
	if err := repo.UpsertRoutes(ctx, legacyCatalog); err != nil {
		t.Fatal(err)
	}
	var before []modelRouteModel
	if err := database.db.WithContext(ctx).Where("provider = ?", account.ProviderConsole).Find(&before).Error; err != nil {
		t.Fatal(err)
	}
	beforeIDs := make(map[string]uint64, len(before))
	for _, route := range before {
		beforeIDs[route.UpstreamModel+"|"+route.Capability] = route.ID
	}
	desiredCatalog := []model.Route{
		// Legacy and quality must be reconciled before the new 2.0 route so the
		// rows created by the temporary two-model catalog retain their IDs.
		{PublicID: "grok-imagine-image", Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-image", Capability: model.CapabilityImage, Enabled: true},
		{PublicID: "grok-imagine-image", Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-image", Capability: model.CapabilityImageEdit, Enabled: true},
		{PublicID: "grok-imagine-image-quality", Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-image-quality", Capability: model.CapabilityImage, Enabled: true},
		{PublicID: "grok-imagine-image-quality", Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-image-quality", Capability: model.CapabilityImageEdit, Enabled: true},
		{PublicID: "grok-imagine-image-2.0", Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-image-2.0", Capability: model.CapabilityImage, Enabled: true},
		{PublicID: "grok-imagine-image-2.0", Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-image-2.0", Capability: model.CapabilityImageEdit, Enabled: true},
	}
	for range 2 {
		if err := repo.ReplaceProviderRoutes(ctx, account.ProviderConsole, desiredCatalog); err != nil {
			t.Fatal(err)
		}
	}
	var after []modelRouteModel
	if err := database.db.WithContext(ctx).Where("provider = ? AND upstream_model LIKE ?", account.ProviderConsole, "grok-imagine-image%").Order("id ASC").Find(&after).Error; err != nil {
		t.Fatal(err)
	}
	if len(after) != 6 {
		t.Fatalf("Console image routes = %#v", after)
	}
	seen := make(map[string]map[string]bool)
	for _, route := range after {
		if seen[route.PublicID] == nil {
			seen[route.PublicID] = make(map[string]bool)
		}
		seen[route.PublicID][route.Capability] = true
		if route.UpstreamModel != "grok-imagine-image-2.0" && beforeIDs[route.UpstreamModel+"|"+route.Capability] != route.ID {
			t.Fatalf("existing Console route ID changed: before=%#v after=%#v", before, after)
		}
	}
	for _, publicID := range []string{"Console/grok-imagine-image", "Console/grok-imagine-image-quality", "Console/grok-imagine-image-2.0"} {
		if !seen[publicID][string(model.CapabilityImage)] || !seen[publicID][string(model.CapabilityImageEdit)] {
			t.Fatalf("Console image capabilities for %s = %#v", publicID, seen[publicID])
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
	candidates, err := accounts.ListRoutingCandidates(ctx, account.ProviderBuild, created.ID, created.UpstreamModel, "")
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

func TestWebImageRediscoveryUsesLitePublicNames(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewModelRepository(database)
	tests := map[string]string{
		"grok-imagine-image":         "Web/grok-imagine-image-lite",
		"grok-imagine-image-quality": "Web/grok-imagine-image-quality-lite",
	}
	for upstreamModel, publicID := range tests {
		if err := repo.UpsertDiscovered(ctx, account.ProviderWeb, []string{upstreamModel}); err != nil {
			t.Fatal(err)
		}
		route, err := repo.GetByPublicIDIncludingDisabled(ctx, publicID)
		if err != nil || route.UpstreamModel != upstreamModel || route.Capability != model.CapabilityImage {
			t.Fatalf("rediscovered %s as %#v, err=%v", upstreamModel, route, err)
		}
	}
}

func TestBuildVideo15DiscoveredAsVideoCapability(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewModelRepository(database)
	accounts := NewAccountRepository(database)
	buildAccount, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "build-video", SourceKey: "build-video",
		EncryptedAccessToken: testEncryptedToken, Enabled: true, AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	webAccount, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: "web-video", SourceKey: "web-video",
		EncryptedAccessToken: testEncryptedToken, Enabled: true, AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertDiscovered(ctx, account.ProviderBuild, []string{"grok-4.5", "grok-imagine-video-1.5"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := repo.ReplaceAccountCapabilities(ctx, buildAccount.ID, []string{"grok-4.5", "grok-imagine-video-1.5"}, now); err != nil {
		t.Fatal(err)
	}
	video, err := repo.GetByPublicID(ctx, "Build/grok-imagine-video-1.5")
	if err != nil {
		t.Fatal(err)
	}
	if video.Capability != model.CapabilityVideo || video.UpstreamModel != "grok-imagine-video-1.5" || video.Origin != model.OriginDiscovered {
		t.Fatalf("build video 1.5 route = %#v", video)
	}
	chat, err := repo.GetByPublicID(ctx, "Build/grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if chat.Capability != model.CapabilityResponses {
		t.Fatalf("build chat route capability = %s", chat.Capability)
	}
	// Web 既有 video 分类不受影响。
	if err := repo.UpsertDiscovered(ctx, account.ProviderWeb, []string{"grok-imagine-video"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceAccountCapabilities(ctx, webAccount.ID, []string{"grok-imagine-video"}, now); err != nil {
		t.Fatal(err)
	}
	webVideo, err := repo.GetByPublicID(ctx, "grok-imagine-video")
	if err != nil || webVideo.Capability != model.CapabilityVideo {
		t.Fatalf("web video route = %#v, err = %v", webVideo, err)
	}
}
