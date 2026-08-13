package relational

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestRoutingProjectionLeavesSecretsAndLargeJSONOutOfCandidateLoad(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "routing-projection.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := NewAccountRepository(database)
	now := time.Now().UTC().Truncate(time.Second)
	created, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: "routing projection", Email: "route@example.test",
		SourceKey: "routing:projection", OIDCClientID: "client-id", EncryptedAccessToken: "access-secret",
		EncryptedCloudflareCookie: "cookie-secret", ExpiresAt: now.Add(time.Hour), Enabled: true,
		AuthStatus: account.AuthStatusActive, Priority: 9, MaxConcurrent: 3, MinimumRemaining: 1.5,
		WebTier: account.WebTierSuper,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.SaveBilling(ctx, account.Billing{
		AccountID: created.ID, PlanCode: "super", MonthlyLimit: 100, Used: 12, SyncedAt: now,
		History: []account.BillingHistoryEntry{{Year: 2026, Month: 7, IncludedUsed: 12}},
	}); err != nil {
		t.Fatal(err)
	}
	resetAt := now.Add(time.Hour)
	if err := accounts.SaveQuotaWindows(ctx, created.ID, account.WebTierSuper, now, []account.QuotaWindow{{
		AccountID: created.ID, Mode: "weekly", Remaining: 10, Total: 20, UsagePercent: 50,
		Breakdown:     []account.QuotaBreakdown{{ProductCode: account.QuotaProductChat, UsagePercent: 50}},
		WindowSeconds: 3600, ResetAt: &resetAt, SyncedAt: &now, Source: account.QuotaSourceUpstream,
	}, {
		AccountID: created.ID, Mode: account.QuotaModeWebImagePro, Remaining: 3,
		WindowSeconds: 86400, SyncedAt: &now, Source: account.QuotaSourceUpstream,
	}}); err != nil {
		t.Fatal(err)
	}

	bases, err := accounts.ListRoutingAccountBases(ctx, account.ProviderWeb, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(bases) != 1 {
		t.Fatalf("routing bases = %d, want 1", len(bases))
	}
	assertRoutingProjection(t, bases[0].Credential, created)
	if bases[0].Billing == nil || bases[0].Billing.PlanCode != "super" || len(bases[0].Billing.History) != 0 {
		t.Fatalf("routing billing = %#v, want scalar billing without history", bases[0].Billing)
	}
	if bases[0].QuotaWindow == nil || bases[0].QuotaWindow.Remaining != 10 || len(bases[0].QuotaWindow.Breakdown) != 0 {
		t.Fatalf("routing quota = %#v, want scalar quota without breakdown", bases[0].QuotaWindow)
	}
	imagineBases, err := accounts.ListRoutingAccountBases(ctx, account.ProviderWeb, account.QuotaModeWebImagePro)
	if err != nil {
		t.Fatal(err)
	}
	if len(imagineBases) != 1 || imagineBases[0].QuotaWindow == nil || imagineBases[0].QuotaWindow.Mode != account.QuotaModeWebImagePro || imagineBases[0].QuotaWindow.Remaining != 3 {
		t.Fatalf("Imagine routing quota = %#v", imagineBases)
	}

	candidates, err := accounts.ListRoutingCandidates(ctx, account.ProviderWeb, 0, "grok-test", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("routing candidates = %d, want 1", len(candidates))
	}
	assertRoutingProjection(t, candidates[0].Credential, created)
	if candidates[0].Billing == nil || len(candidates[0].Billing.History) != 0 || candidates[0].QuotaWindow == nil || len(candidates[0].QuotaWindow.Breakdown) != 0 {
		t.Fatalf("routing candidate loaded large payloads: %#v", candidates[0])
	}

	// Management reads retain their complete representations.
	managed, err := accounts.ListEnabled(ctx, account.ProviderWeb)
	if err != nil {
		t.Fatal(err)
	}
	if len(managed) != 1 || managed[0].EncryptedAccessToken != "access-secret" || managed[0].EncryptedCloudflareCookie != "cookie-secret" {
		t.Fatalf("management credentials = %#v", managed)
	}
	billing, err := accounts.GetBilling(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(billing.History) != 1 {
		t.Fatalf("management billing history = %#v, want one entry", billing.History)
	}
	windows, err := accounts.GetQuotaWindows(ctx, []uint64{created.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(windows[created.ID]) != 2 {
		t.Fatalf("management quota windows = %#v, want two modes", windows)
	}
}

func TestGetCredentialMaterialHydratesOneAccountAndMapsNotFound(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "credential-material.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := NewAccountRepository(database)
	now := time.Now().UTC().Truncate(time.Second)
	created, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: "hydrate", SourceKey: "credential:material",
		OIDCClientID: "client-id", EncryptedAccessToken: "access-secret", EncryptedRefreshToken: "refresh-secret",
		EncryptedCloudflareCookie: "cookie-secret", ExpiresAt: now.Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}

	material, err := accounts.GetCredentialMaterial(ctx, created.ID, account.ProviderBuild)
	if err != nil {
		t.Fatal(err)
	}
	if material.AccountID != created.ID || material.Provider != account.ProviderBuild || material.AuthType != account.AuthTypeOAuth || material.OIDCClientID != "client-id" ||
		material.EncryptedAccessToken != "access-secret" || material.EncryptedRefreshToken != "refresh-secret" || material.EncryptedCloudflareCookie != "cookie-secret" ||
		!material.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("credential material = %#v", material)
	}
	hydrated, ok := material.ApplyTo(account.Credential{ID: created.ID, Provider: account.ProviderBuild})
	if !ok || hydrated.EncryptedAccessToken != "access-secret" || hydrated.EncryptedRefreshToken != "refresh-secret" {
		t.Fatalf("hydrated credential = %#v, ok=%v", hydrated, ok)
	}
	if _, ok := material.ApplyTo(account.Credential{ID: created.ID + 1}); ok {
		t.Fatal("credential material applied to a different account")
	}
	if _, ok := material.ApplyTo(account.Credential{ID: created.ID, Provider: account.ProviderWeb}); ok {
		t.Fatal("credential material applied to a different provider")
	}
	if _, err := accounts.GetCredentialMaterial(ctx, created.ID+1, account.ProviderBuild); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("missing credential material error = %v, want ErrNotFound", err)
	}
	if _, err := accounts.GetCredentialMaterial(ctx, created.ID, account.ProviderWeb); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("cross-provider credential material error = %v, want ErrNotFound", err)
	}
	disabled := false
	if _, err := accounts.UpdateMany(ctx, account.ProviderBuild, []uint64{created.ID}, repository.AccountUpdates{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.GetCredentialMaterial(ctx, created.ID, account.ProviderBuild); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("disabled credential material error = %v, want ErrNotFound", err)
	}
}

func assertRoutingProjection(t *testing.T, value, created account.Credential) {
	t.Helper()
	if value.ID != created.ID || value.Provider != created.Provider || value.Name != created.Name || value.SourceKey != created.SourceKey ||
		value.Priority != created.Priority || value.MaxConcurrent != created.MaxConcurrent || value.MinimumRemaining != created.MinimumRemaining || value.WebTier != account.WebTierSuper ||
		value.AuthType != created.AuthType || value.OIDCClientID != created.OIDCClientID || !value.ExpiresAt.Equal(created.ExpiresAt) {
		t.Fatalf("routing metadata = %#v", value)
	}
	if value.EncryptedAccessToken != "" || value.EncryptedRefreshToken != "" || value.EncryptedCloudflareCookie != "" {
		t.Fatalf("routing projection includes credential material: %#v", value)
	}
}
