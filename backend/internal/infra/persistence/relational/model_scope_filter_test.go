package relational

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestModelListFiltersByClientKeyProviderAndTierScope(t *testing.T) {
	database := openTestDatabase(t)
	assertModelListFiltersByClientKeyProviderAndTierScope(t, database)
}

func TestPostgresModelListFiltersByClientKeyProviderAndTierScope(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	database, err := OpenPostgres(ctx, dsn, 5, 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("close PostgreSQL model scope database: %v", err)
		}
	})
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	assertModelListFiltersByClientKeyProviderAndTierScope(t, database)
}

func assertModelListFiltersByClientKeyProviderAndTierScope(t *testing.T, database *Database) {
	t.Helper()
	ctx := context.Background()
	repo := NewModelRepository(database)

	now := time.Now().UTC()
	accounts := []accountModel{
		{IdentityKey: testIdentityKey("scope-build-free"), Provider: string(account.ProviderBuild), Name: "build-free", SourceKey: "build-free", ObservedModel: "grok-build-free", Enabled: true, AuthStatus: string(account.AuthStatusActive), Priority: 1},
		{IdentityKey: testIdentityKey("scope-build-super"), Provider: string(account.ProviderBuild), Name: "build-super", SourceKey: "build-super", Enabled: true, AuthStatus: string(account.AuthStatusActive), Priority: 1},
		{IdentityKey: testIdentityKey("scope-web-free"), Provider: string(account.ProviderWeb), Name: "web-free", SourceKey: "web-free", Enabled: true, AuthStatus: string(account.AuthStatusActive), Priority: 1},
		{IdentityKey: testIdentityKey("scope-web-super"), Provider: string(account.ProviderWeb), Name: "web-super", SourceKey: "web-super", Enabled: true, AuthStatus: string(account.AuthStatusActive), Priority: 1},
		{IdentityKey: testIdentityKey("scope-console"), Provider: string(account.ProviderConsole), Name: "console", SourceKey: "console", Enabled: true, AuthStatus: string(account.AuthStatusActive), Priority: 1},
	}
	for index := range accounts {
		if err := database.db.WithContext(ctx).Create(&accounts[index]).Error; err != nil {
			t.Fatal(err)
		}
	}
	accountIDs := make([]uint64, 0, len(accounts))
	for _, value := range accounts {
		accountIDs = append(accountIDs, value.ID)
	}
	if err := database.db.WithContext(ctx).Create(&billingModel{AccountID: accounts[1].ID, PlanName: "SuperGrokPro", SyncedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Create(&webAccountProfileModel{AccountID: accounts[2].ID, Tier: "basic", SyncedAt: &now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Create(&webAccountProfileModel{AccountID: accounts[3].ID, Tier: "super", SyncedAt: &now}).Error; err != nil {
		t.Fatal(err)
	}

	routes := []modeldomain.Route{
		{PublicID: "Build/scope-free", Provider: account.ProviderBuild, UpstreamModel: "scope-free", Capability: modeldomain.CapabilityResponses, Enabled: true},
		{PublicID: "Build/scope-super", Provider: account.ProviderBuild, UpstreamModel: "scope-super", Capability: modeldomain.CapabilityResponses, Enabled: true},
		{PublicID: "scope-web-free", Provider: account.ProviderWeb, UpstreamModel: "scope-web-free", Capability: modeldomain.CapabilityChat, Enabled: true},
		{PublicID: "scope-web-super", Provider: account.ProviderWeb, UpstreamModel: "scope-web-super", Capability: modeldomain.CapabilityChat, Enabled: true},
		{PublicID: "scope-console", Provider: account.ProviderConsole, UpstreamModel: "scope-console", Capability: modeldomain.CapabilityResponses, Enabled: true},
	}
	routeIDs := make([]uint64, 0, len(routes))
	for index := range routes {
		created, err := repo.Create(ctx, routes[index], nil)
		if err != nil {
			t.Fatal(err)
		}
		routeIDs = append(routeIDs, created.ID)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := repo.DeleteMany(cleanupCtx, routeIDs); err != nil {
			t.Errorf("delete scoped model routes: %v", err)
		}
		if err := database.db.WithContext(cleanupCtx).Where("id IN ?", accountIDs).Delete(&accountModel{}).Error; err != nil {
			t.Errorf("delete scoped model accounts: %v", err)
		}
	})
	capabilities := []accountModelCapabilityModel{
		{AccountID: accounts[0].ID, UpstreamModel: "scope-free"},
		{AccountID: accounts[1].ID, UpstreamModel: "scope-super"},
		{AccountID: accounts[2].ID, UpstreamModel: "scope-web-free"},
		{AccountID: accounts[3].ID, UpstreamModel: "scope-web-super"},
		{AccountID: accounts[4].ID, UpstreamModel: "scope-console"},
	}
	if err := database.db.WithContext(ctx).Create(&capabilities).Error; err != nil {
		t.Fatal(err)
	}

	list := func(tiers []string, providers []string) map[string]bool {
		values, _, err := repo.List(ctx, repository.ModelListQuery{Page: repository.PageQuery{Limit: 20}, Filter: repository.ModelListFilter{Providers: providers, Tiers: tiers}})
		if err != nil {
			t.Fatal(err)
		}
		result := make(map[string]bool, len(values))
		for _, value := range values {
			result[value.PublicID] = true
		}
		return result
	}

	free := list([]string{"free"}, nil)
	if len(free) != 3 || !free["Build/scope-free"] || !free["Web/scope-web-free"] || !free["Console/scope-console"] || free["Build/scope-super"] || free["Web/scope-web-super"] {
		t.Fatalf("free scope models = %#v", free)
	}
	super := list([]string{"super"}, nil)
	if len(super) != 3 || !super["Build/scope-super"] || !super["Web/scope-web-super"] || !super["Console/scope-console"] || super["Build/scope-free"] || super["Web/scope-web-free"] {
		t.Fatalf("super scope models = %#v", super)
	}
	buildFree := list([]string{"free"}, []string{"grok_build"})
	if len(buildFree) != 1 || !buildFree["Build/scope-free"] {
		t.Fatalf("build/free scope models = %#v", buildFree)
	}

	enabledForScope := func(tiers []string, providers []string) map[string]bool {
		values, err := repo.ListEnabledForScope(ctx, repository.ModelListFilter{Providers: providers, Tiers: tiers})
		if err != nil {
			t.Fatal(err)
		}
		result := make(map[string]bool, len(values))
		for _, value := range values {
			result[value.PublicID] = true
		}
		return result
	}
	enabledBuildFree := enabledForScope([]string{"free"}, []string{"grok_build"})
	if len(enabledBuildFree) != 1 || !enabledBuildFree["Build/scope-free"] {
		t.Fatalf("enabled build/free scope models = %#v", enabledBuildFree)
	}
	if err := database.db.WithContext(ctx).Model(&accountModel{}).Where("id IN ?", []uint64{accounts[1].ID, accounts[3].ID}).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	enabledSuper := enabledForScope([]string{"super"}, nil)
	if len(enabledSuper) != 1 || !enabledSuper["Console/scope-console"] {
		t.Fatalf("inactive Super accounts must not advertise routes: %#v", enabledSuper)
	}
}
