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
	if routes[1].ID != fastBefore.ID || routes[1].PublicID != "grok-chat-fast" || routes[1].Enabled {
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
	if len(after) != 2 || after[0].UpstreamModel != "grok-imagine-image" || after[0].PublicID != "grok-imagine-image" || after[1].UpstreamModel != "grok-imagine-image-quality" || after[1].PublicID != "grok-imagine-image-quality" {
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
