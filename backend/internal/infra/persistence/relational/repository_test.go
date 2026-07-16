package relational

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/admin"
	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

const (
	testEncryptedToken = "encrypted-token"
	testSecretHash     = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func TestSchemaAndRepositoryConstraints(t *testing.T) {
	database := openTestDatabase(t)
	adminRepo := NewAdminRepository(database)
	if _, err := adminRepo.Create(context.Background(), admin.Admin{Username: "admin", PasswordHash: "hash"}); err != nil {
		t.Fatal(err)
	}
	if _, err := adminRepo.Create(context.Background(), admin.Admin{Username: "admin", PasswordHash: "hash"}); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("重复管理员错误 = %v", err)
	}

	accountRepo := NewAccountRepository(database)
	value := account.Credential{Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: "first", UserID: "user-1", SourceKey: "source", EncryptedAccessToken: "encrypted-a", EncryptedCloudflareCookie: "encrypted-cf", ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 100, MaxConcurrent: 4}
	created, wasCreated, err := accountRepo.UpsertByIdentity(context.Background(), value)
	if err != nil || !wasCreated {
		t.Fatalf("首次 upsert = %#v, %v, %v", created, wasCreated, err)
	}
	observedAt := time.Now().UTC()
	if err := accountRepo.UpdateObservedModel(context.Background(), created.ID, "grok-observed", observedAt); err != nil {
		t.Fatal(err)
	}
	if err := accountRepo.UpdateHealth(context.Background(), created.ID, 0, nil, "", true); err != nil {
		t.Fatal(err)
	}
	value.Name = "updated"
	value.EncryptedAccessToken = "encrypted-new"
	value.EncryptedCloudflareCookie = ""
	updated, wasCreated, err := accountRepo.UpsertByIdentity(context.Background(), value)
	if err != nil || wasCreated || updated.ID != created.ID || updated.Name != "updated" || updated.LastUsedAt == nil || updated.ObservedModel != "grok-observed" || updated.ObservedModelAt == nil || updated.EncryptedCloudflareCookie != "encrypted-cf" {
		t.Fatalf("幂等 upsert = %#v, %v, %v", updated, wasCreated, err)
	}
}

func TestAccountRepositoryAppliesRoutingDefaults(t *testing.T) {
	database := openTestDatabase(t)
	value, created, err := NewAccountRepository(database).UpsertByIdentity(context.Background(), account.Credential{
		Provider: account.ProviderBuild, Name: "defaults", SourceKey: "defaults", EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive,
	})
	if err != nil || !created {
		t.Fatalf("created = %v, err = %v", created, err)
	}
	if value.Priority != account.DefaultPriority || value.MaxConcurrent != account.DefaultMaxConcurrent || value.MinimumRemaining != account.DefaultMinimumRemaining {
		t.Fatalf("routing defaults = priority %d, concurrency %d, minimum %v", value.Priority, value.MaxConcurrent, value.MinimumRemaining)
	}
}

func TestAccountRepositoryUpsertsImportChunkInOneBatch(t *testing.T) {
	ctx := context.Background()
	repo := NewAccountRepository(openTestDatabase(t))
	values := []account.Credential{
		{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: "batch-1", SourceKey: "batch-1", EncryptedAccessToken: testEncryptedToken, EncryptedRefreshToken: "encrypted-refresh", AuthStatus: account.AuthStatusActive},
		{Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: "batch-2", SourceKey: "batch-2", EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive},
	}
	created, err := repo.UpsertManyByIdentity(ctx, values)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 2 || !created[0].Created || !created[1].Created || created[0].ID == 0 || created[1].ID == 0 {
		t.Fatalf("created = %#v", created)
	}
	buildIDs, err := repo.ListEnabledAccountIDs(ctx, account.ProviderBuild, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(buildIDs) != 1 || buildIDs[0] != created[0].ID {
		t.Fatalf("refreshable build ids = %#v", buildIDs)
	}
	stored, err := repo.Get(ctx, created[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.Priority = 99
	if _, err := repo.Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	values[0].Name = "batch-1-updated"
	updated, err := repo.UpsertManyByIdentity(ctx, values[:1])
	if err != nil {
		t.Fatal(err)
	}
	if len(updated) != 1 || updated[0].Created || updated[0].ID != created[0].ID {
		t.Fatalf("updated = %#v", updated)
	}
	stored, err = repo.Get(ctx, created[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Name != "batch-1-updated" || stored.Priority != 99 {
		t.Fatalf("stored = %#v", stored)
	}
}

func TestAccountRepositoryUpsertsDuplicateIdentityWithinChunk(t *testing.T) {
	ctx := context.Background()
	repo := NewAccountRepository(openTestDatabase(t))
	values := []account.Credential{
		{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: "duplicate-first", SourceKey: "duplicate", EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive},
		{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: "duplicate-final", SourceKey: "duplicate", EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive},
	}

	results, err := repo.UpsertManyByIdentity(ctx, values)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || !results[0].Created || results[1].Created || results[0].ID != results[1].ID {
		t.Fatalf("results = %#v", results)
	}
	stored, err := repo.Get(ctx, results[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Name != "duplicate-final" {
		t.Fatalf("stored name = %q", stored.Name)
	}
}

func TestAccountRepositoryLinksWebAndBuildAccountsOnce(t *testing.T) {
	ctx := context.Background()
	repo := NewAccountRepository(openTestDatabase(t))
	web, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: "web", SourceKey: "web", EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	build, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: "build", SourceKey: "build", EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.LinkWebToBuild(ctx, web.ID, build.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.LinkWebToBuild(ctx, web.ID, build.ID); err != nil {
		t.Fatalf("idempotent link = %v", err)
	}
	linkedWeb, err := repo.Get(ctx, web.ID)
	if err != nil {
		t.Fatal(err)
	}
	linkedBuild, err := repo.Get(ctx, build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if linkedWeb.LinkedAccountID != build.ID || linkedWeb.LinkedProvider != account.ProviderBuild || linkedWeb.LinkedAccountName != build.Name {
		t.Fatalf("web link = %#v", linkedWeb)
	}
	if linkedBuild.LinkedAccountID != web.ID || linkedBuild.LinkedProvider != account.ProviderWeb || linkedBuild.LinkedAccountName != web.Name {
		t.Fatalf("build link = %#v", linkedBuild)
	}
	unlinkedWeb, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: "web-2", SourceKey: "web-2", EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	unlinkedIDs, total, err := repo.ListUnlinkedWebAccountIDs(ctx, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(unlinkedIDs) != 1 || unlinkedIDs[0] != unlinkedWeb.ID {
		t.Fatalf("unlinked web ids = %#v, total = %d", unlinkedIDs, total)
	}
	nextUnlinkedIDs, nextTotal, err := repo.ListUnlinkedWebAccountIDs(ctx, unlinkedWeb.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if nextTotal != 0 || len(nextUnlinkedIDs) != 0 {
		t.Fatalf("next unlinked web ids = %#v, total = %d", nextUnlinkedIDs, nextTotal)
	}
	buildCandidates, err := repo.FilterMissingBuildConversionIDs(ctx, []uint64{web.ID, build.ID, unlinkedWeb.ID, 999_999})
	if err != nil {
		t.Fatal(err)
	}
	if len(buildCandidates) != 3 || buildCandidates[0] != build.ID || buildCandidates[1] != unlinkedWeb.ID || buildCandidates[2] != 999_999 {
		t.Fatalf("build conversion candidates = %#v", buildCandidates)
	}
	if _, _, err := repo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderConsole, AuthType: account.AuthTypeSSO, Name: "console", SourceKey: "console-" + web.SourceKey,
		EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	consoleCandidates, err := repo.ListMissingConsoleSyncAccounts(ctx, []uint64{web.ID, unlinkedWeb.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(consoleCandidates) != 1 || consoleCandidates[0].ID != unlinkedWeb.ID {
		t.Fatalf("console sync candidates = %#v", consoleCandidates)
	}
	if _, err := repo.ListMissingConsoleSyncAccounts(ctx, []uint64{build.ID}); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("non-web console candidate error = %v", err)
	}
	if _, err := repo.ListMissingConsoleSyncAccounts(ctx, []uint64{web.ID, 999_999}); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("missing console candidate error = %v", err)
	}
	consoleBatch, consoleTotal, consoleSkipped, err := repo.ListMissingConsoleSyncBatch(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if consoleTotal != 1 || consoleSkipped != 1 || len(consoleBatch) != 1 || consoleBatch[0].ID != unlinkedWeb.ID {
		t.Fatalf("console sync batch = %#v, total = %d, skipped = %d", consoleBatch, consoleTotal, consoleSkipped)
	}
	otherBuild, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "build-2", SourceKey: "build-2", EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.LinkWebToBuild(ctx, web.ID, otherBuild.ID); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("duplicate web link = %v", err)
	}
}

func TestAccountRepositoryDecrementsQuotaByAmountAtomically(t *testing.T) {
	ctx := context.Background()
	repo := NewAccountRepository(openTestDatabase(t))
	value, _, err := repo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: "quota", SourceKey: "quota-batch",
		EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := repo.SaveQuotaWindows(ctx, value.ID, account.WebTierSuper, now, []account.QuotaWindow{{AccountID: value.ID, Mode: "fast", Remaining: 10, Total: 10, UpdatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	if updated, err := repo.DecrementQuotaWindowBy(ctx, value.ID, "fast", 4, now); err != nil || !updated {
		t.Fatalf("first decrement updated=%v err=%v", updated, err)
	}
	if updated, err := repo.DecrementQuotaWindowBy(ctx, value.ID, "fast", 9, now); err != nil || !updated {
		t.Fatalf("second decrement updated=%v err=%v", updated, err)
	}
	windows, err := repo.GetQuotaWindows(ctx, []uint64{value.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(windows[value.ID]) != 1 || windows[value.ID][0].Remaining != 0 {
		t.Fatalf("quota windows = %#v", windows[value.ID])
	}
}

func TestAccountRepositorySummarizesOperationalStates(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	repo := NewAccountRepository(openTestDatabase(t))
	create := func(provider account.Provider, name string) account.Credential {
		value, _, err := repo.UpsertByIdentity(ctx, account.Credential{
			Provider: provider, Name: name, SourceKey: name, EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive,
		})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}

	create(account.ProviderBuild, "build-active")
	cooldown := create(account.ProviderBuild, "build-cooldown")
	cooldownUntil := now.Add(time.Hour)
	if err := repo.UpdateHealth(ctx, cooldown.ID, 1, &cooldownUntil, "cooldown", false); err != nil {
		t.Fatal(err)
	}
	disabled := create(account.ProviderBuild, "build-disabled")
	disabled.Enabled = false
	if _, err := repo.Update(ctx, disabled); err != nil {
		t.Fatal(err)
	}

	exhausted := create(account.ProviderWeb, "web-exhausted")
	if err := repo.SaveQuotaWindows(ctx, exhausted.ID, account.WebTierSuper, now, []account.QuotaWindow{{AccountID: exhausted.ID, Mode: "fast", Remaining: 0, Total: 30, UpdatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	reauth := create(account.ProviderWeb, "web-reauth")
	reauth.AuthStatus = account.AuthStatusReauthRequired
	if _, err := repo.Update(ctx, reauth); err != nil {
		t.Fatal(err)
	}

	rows, err := repo.Summarize(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	byProvider := make(map[string]repository.AccountSummary, len(rows))
	for _, row := range rows {
		byProvider[row.Provider] = row
	}
	build := byProvider[string(account.ProviderBuild)]
	if build.Total != 3 || build.Available != 1 || build.Cooldown != 1 || build.Disabled != 1 {
		t.Fatalf("build summary = %#v", build)
	}
	web := byProvider[string(account.ProviderWeb)]
	if web.Total != 2 || web.Available != 0 || web.WaitingReset != 1 || web.ReauthRequired != 1 {
		t.Fatalf("web summary = %#v", web)
	}
}

func TestAccountIdentityPrefersStableOAuthIdentity(t *testing.T) {
	first := accountIdentity(account.Credential{Provider: account.ProviderBuild, UserID: "user-1", TeamID: "team-1", SourceKey: "device:old-token"})
	second := accountIdentity(account.Credential{Provider: account.ProviderBuild, UserID: "user-1", TeamID: "team-1", SourceKey: "device:new-token"})
	if first != second {
		t.Fatal("stable OAuth identity changed after token rotation")
	}
	fallbackFirst := accountIdentity(account.Credential{Provider: account.ProviderBuild, SourceKey: "source-1"})
	fallbackSecond := accountIdentity(account.Credential{Provider: account.ProviderBuild, SourceKey: "source-2"})
	if fallbackFirst == fallbackSecond {
		t.Fatal("source fallback did not distinguish accounts without stable claims")
	}
}

func TestAccountRepositoryPersistsObservedBuildBillingFields(t *testing.T) {
	database := openTestDatabase(t)
	repo := NewAccountRepository(database)
	credential, _, err := repo.UpsertByIdentity(context.Background(), account.Credential{Provider: account.ProviderBuild, Name: "billing", SourceKey: "billing", EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	onDemandEnabled := false
	if err := repo.UpdateObservedModel(context.Background(), credential.ID, "grok-4.5-build-free", now); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveBilling(context.Background(), account.Billing{AccountID: credential.ID, IsUnifiedBillingUser: true, OnDemandEnabled: &onDemandEnabled, TopUpMethod: "TOP_UP_METHOD_SAVED_PAYMENT_METHOD", UsagePeriodType: "USAGE_PERIOD_TYPE_WEEKLY", UsagePeriodStart: "2026-07-12T00:00:00Z", UsagePeriodEnd: "2026-07-19T00:00:00Z", History: []account.BillingHistoryEntry{{Year: 2026, Month: 6}}, SyncedAt: now}); err != nil {
		t.Fatal(err)
	}
	storedCredential, err := repo.Get(context.Background(), credential.ID)
	if err != nil || storedCredential.ObservedModel != "grok-4.5-build-free" || storedCredential.ObservedModelAt == nil {
		t.Fatalf("credential = %#v, err = %v", storedCredential, err)
	}
	billing, err := repo.GetBilling(context.Background(), credential.ID)
	if err != nil || !billing.IsUnifiedBillingUser || billing.OnDemandEnabled == nil || *billing.OnDemandEnabled || billing.TopUpMethod != "TOP_UP_METHOD_SAVED_PAYMENT_METHOD" || billing.UsagePeriodType != "USAGE_PERIOD_TYPE_WEEKLY" || billing.UsagePeriodEnd != "2026-07-19T00:00:00Z" || len(billing.History) != 1 {
		t.Fatalf("billing = %#v, err = %v", billing, err)
	}
}

func TestForeignKeysCascadeRuntimeStateButPreserveAuditHistory(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accounts := NewAccountRepository(database)
	models := NewModelRepository(database)
	keys := NewClientKeyRepository(database)
	responses := NewResponseRepository(database)
	audits := NewAuditRepository(database)

	accountValue, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "cascade", SourceKey: "cascade", EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	if err := models.UpsertDiscovered(ctx, account.ProviderBuild, []string{"grok-cascade"}); err != nil {
		t.Fatal(err)
	}
	if err := models.ReplaceAccountCapabilities(ctx, accountValue.ID, []string{"grok-cascade"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	route, err := models.GetByPublicID(ctx, "grok-cascade")
	if err != nil {
		t.Fatal(err)
	}
	key, err := keys.Create(ctx, clientkeydomain.Key{Name: "cascade", Prefix: "cascade-prefix", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 120, MaxConcurrent: 8, AllowedModels: []uint64{route.ID}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := accounts.SaveBilling(ctx, account.Billing{AccountID: accountValue.ID, SyncedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.SaveQuotaRecovery(ctx, account.QuotaRecovery{AccountID: accountValue.ID, Kind: account.QuotaRecoveryKindFree, Status: account.QuotaRecoveryStatusExhausted, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := responses.Save(ctx, inferencedomain.ResponseOwnership{ResponseID: "resp-account", AccountID: accountValue.ID, ClientKeyID: key.ID, Provider: account.ProviderBuild, ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := audits.Create(ctx, auditdomain.Record{RequestID: "audit-history", ClientKeyID: key.ID, ClientKeyName: key.Name, ModelRouteID: route.ID, ModelPublicID: route.PublicID, AccountID: &accountValue.ID, AccountName: accountValue.Name, StatusCode: 200, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	if err := accounts.Delete(ctx, accountValue.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"account_billing_snapshots", "account_quota_recovery", "account_model_capabilities", "account_model_sync_states", "response_ownership"} {
		if count := tableRowCount(t, database, table); count != 0 {
			t.Fatalf("%s rows after account delete = %d", table, count)
		}
	}
	if count := tableRowCount(t, database, "request_audits"); count != 1 {
		t.Fatalf("audit rows after account delete = %d", count)
	}

	secondAccount, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "second", SourceKey: "second-cascade", EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	if err := responses.Save(ctx, inferencedomain.ResponseOwnership{ResponseID: "resp-key", AccountID: secondAccount.ID, ClientKeyID: key.ID, Provider: account.ProviderBuild, ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := keys.Delete(ctx, key.ID); err != nil {
		t.Fatal(err)
	}
	if count := tableRowCount(t, database, "client_key_models"); count != 0 {
		t.Fatalf("permission rows after key delete = %d", count)
	}
	if count := tableRowCount(t, database, "response_ownership"); count != 0 {
		t.Fatalf("response ownership rows after key delete = %d", count)
	}
}

func TestFreshSchemaContract(t *testing.T) {
	database := openTestDatabase(t)
	if err := database.InitializeSchema(context.Background()); err != nil {
		t.Fatalf("repeated schema initialization: %v", err)
	}
	for _, model := range schemaModels {
		if !database.db.Migrator().HasTable(model) {
			t.Fatalf("missing table for %T", model)
		}
	}
	assertTableColumns(t, database, "provider_accounts", []string{"provider", "source_key", "auth_status"}, []string{"oidc_client_id", "expires_at", "encrypted_access_token", "encrypted_refresh_token"})
	assertTableColumns(t, database, "account_credentials", []string{"account_id", "auth_type", "client_id", "encrypted_primary", "encrypted_refresh", "expires_at", "refresh_due_at", "last_refresh_at", "refresh_failures", "last_refresh_error", "refresh_permanent"}, nil)
	assertTableColumns(t, database, "admin_sessions", nil, []string{"revoked_at"})
	assertTableColumns(t, database, "account_model_capabilities", []string{"account_id", "upstream_model"}, []string{"provider", "synced_at"})
	assertTableColumns(t, database, "request_audits", []string{"media_input_images", "media_output_images", "media_output_seconds"}, nil)
	assertTableColumns(t, database, "response_ownership", []string{"response_id", "account_id", "client_key_id", "provider", "expires_at"}, []string{"parent_response_id", "model_route_id"})

	var expiresNotNull int
	if err := database.db.Raw("SELECT `notnull` FROM pragma_table_info('account_credentials') WHERE name = 'expires_at'").Scan(&expiresNotNull).Error; err != nil {
		t.Fatal(err)
	}
	if expiresNotNull != 0 {
		t.Fatal("account credential expires_at must be nullable when the upstream expiry is unknown")
	}
}

func TestSchemaRejectsInvalidPersistentValues(t *testing.T) {
	database := openTestDatabase(t)
	ctx := context.Background()
	if err := database.db.WithContext(ctx).Create(&clientKeyModel{Name: "invalid", Prefix: "invalid", SecretHash: "short", EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 120, MaxConcurrent: 8}).Error; err == nil {
		t.Fatal("invalid client key hash was accepted")
	}
	if err := database.db.WithContext(ctx).Create(&requestAuditModel{RequestID: "negative", ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build", Operation: "responses", UsageSource: "upstream", StatusCode: 200, TotalTokens: -1, CreatedAt: time.Now().UTC()}).Error; err == nil {
		t.Fatal("negative audit token count was accepted")
	}
	accountRow := accountModel{IdentityKey: testIdentityKey("no-token"), Provider: "grok_build", Name: "no-token", SourceKey: "no-token", AuthStatus: "active", MaxConcurrent: 8}
	if err := database.db.WithContext(ctx).Create(&accountRow).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Create(&accountCredentialModel{AccountID: accountRow.ID, AuthType: "oauth", UpdatedAt: time.Now().UTC()}).Error; err == nil {
		t.Fatal("account credential without a primary or refresh secret was accepted")
	}
}

func TestSchemaDoesNotStoreForbiddenBrowserIdentityFields(t *testing.T) {
	database := openTestDatabase(t)
	var rows []struct {
		TableName  string
		ColumnName string
	}
	query := `SELECT m.name AS table_name, p.name AS column_name FROM sqlite_master m JOIN pragma_table_info(m.name) p WHERE m.type = 'table'`
	if err := database.db.Raw(query).Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	forbidden := []string{"grok_device_id", "x-anonuserid", "x-userid", "x-challenge", "x-signature"}
	for _, row := range rows {
		for _, value := range forbidden {
			if strings.EqualFold(row.ColumnName, value) {
				t.Fatalf("forbidden field %s exists in table %s", value, row.TableName)
			}
		}
	}
}

func assertTableColumns(t *testing.T, database *Database, table string, required, forbidden []string) {
	t.Helper()
	var rows []struct{ Name string }
	if err := database.db.Raw("SELECT name FROM pragma_table_info(?)", table).Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	columns := make(map[string]bool, len(rows))
	for _, row := range rows {
		columns[row.Name] = true
	}
	for _, name := range required {
		if !columns[name] {
			t.Fatalf("%s missing column %s", table, name)
		}
	}
	for _, name := range forbidden {
		if columns[name] {
			t.Fatalf("%s contains redundant column %s", table, name)
		}
	}
}

func tableRowCount(t *testing.T, database *Database, table string) int64 {
	t.Helper()
	var count int64
	if err := database.db.Table(table).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	return count
}

func testIdentityKey(source string) string {
	return accountIdentity(account.Credential{Provider: account.ProviderBuild, SourceKey: source})
}

func openTestDatabase(t *testing.T) *Database {
	t.Helper()
	database, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}
