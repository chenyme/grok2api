package relational

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestListFilters(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "list-filters.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)

	free := accountModel{IdentityKey: testIdentityKey("free"), Provider: "grok_build", Name: "free", SourceKey: "free", ObservedModel: "grok-build-free", Enabled: true, AuthStatus: "active", Priority: 1}
	paid := accountModel{IdentityKey: testIdentityKey("paid"), Provider: "grok_build", Name: "paid", SourceKey: "paid", Enabled: true, AuthStatus: "active", Priority: 1}
	disabled := accountModel{IdentityKey: testIdentityKey("disabled-filter"), Provider: "grok_build", Name: "disabled", SourceKey: "disabled-filter", Enabled: false, AuthStatus: "active", Priority: 1}
	for _, value := range []*accountModel{&free, &paid, &disabled} {
		if err := database.db.WithContext(ctx).Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	credentials := []accountCredentialModel{
		{AccountID: free.ID, AuthType: "oauth", EncryptedRefresh: "refresh", UpdatedAt: now},
		{AccountID: paid.ID, AuthType: "oauth", EncryptedPrimary: testEncryptedToken, UpdatedAt: now},
		{AccountID: disabled.ID, AuthType: "oauth", EncryptedPrimary: testEncryptedToken, UpdatedAt: now},
	}
	for index := range credentials {
		if err := database.db.WithContext(ctx).Create(&credentials[index]).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := database.db.WithContext(ctx).Create(&billingModel{AccountID: paid.ID, PlanName: "SuperGrokPro", IsUnifiedBillingUser: true, UsagePeriodType: "USAGE_PERIOD_TYPE_WEEKLY", SyncedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Create(&billingModel{AccountID: disabled.ID, CreditUsagePercent: 42.5, IsUnifiedBillingUser: true, UsagePeriodType: "USAGE_PERIOD_TYPE_WEEKLY", SyncedAt: now}).Error; err != nil {
		t.Fatal(err)
	}

	accounts := NewAccountRepository(database)
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{QuotaType: "free", Now: now}, 1)
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{QuotaType: "paid", Now: now}, 1)
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{QuotaType: "unknown", Now: now}, 1)
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{Status: "active", Now: now}, 2)
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{Status: "disabled", Now: now}, 1)
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{AccountIDs: []uint64{free.ID}, RestrictIDs: true, Now: now}, 1)
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{RestrictIDs: true, Now: now}, 0)
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{ExcludeIDs: []uint64{free.ID}, Now: now}, 2)
	refreshable := true
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{Refreshable: &refreshable, Now: now}, 1)
	boundNode := egressNodeModel{Name: "bound-filter", Scope: "grok_build", Enabled: true}
	if err := database.db.WithContext(ctx).Create(&boundNode).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Model(&accountModel{}).Where("id = ?", free.ID).Update("egress_node_id", boundNode.ID).Error; err != nil {
		t.Fatal(err)
	}
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{Egress: "bound", Now: now}, 1)
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{Egress: "unbound", Now: now}, 2)

	// 零 Billing + build_super_entitled：paid 可查到，free 查不到。
	entitled := accountModel{IdentityKey: testIdentityKey("entitled-zero"), Provider: "grok_build", Name: "entitled-zero", SourceKey: "entitled-zero", ObservedModel: "grok-build-free", Enabled: true, AuthStatus: "active", Priority: 1, BuildSuperEntitled: true}
	if err := database.db.WithContext(ctx).Create(&entitled).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Create(&accountCredentialModel{AccountID: entitled.ID, AuthType: "oauth", EncryptedPrimary: testEncryptedToken, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Create(&billingModel{AccountID: entitled.ID, IsUnifiedBillingUser: true, SyncedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{QuotaType: "paid", Now: now}, 2)
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{QuotaType: "free", Now: now}, 1)
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{Status: "active", Now: now}, 3)

	var webByTier = map[string]accountModel{}
	for _, tier := range []string{"auto", "basic", "super", "heavy"} {
		value := accountModel{IdentityKey: testIdentityKey("web-" + tier), Provider: "grok_web", Name: "web-" + tier, SourceKey: "web-" + tier, Enabled: true, AuthStatus: "active", Priority: 1}
		if err := database.db.WithContext(ctx).Create(&value).Error; err != nil {
			t.Fatal(err)
		}
		if err := database.db.WithContext(ctx).Create(&webAccountProfileModel{AccountID: value.ID, Tier: tier, SyncedAt: &now}).Error; err != nil {
			t.Fatal(err)
		}
		webByTier[tier] = value
		assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{Provider: "grok_web", QuotaType: tier, Now: now}, 1)
	}
	// Web agreement and association filters cover NSFW, terms, Build, and Console.
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{Provider: "grok_web", Agreement: "nsfwDisabled", Now: now}, 4)
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{Provider: "grok_web", Agreement: "termsNotAccepted", Now: now}, 4)
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{Provider: "grok_web", Agreement: "allNotAccepted", Now: now}, 4)
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{Provider: "grok_web", Association: "allUnlinked", Now: now}, 4)

	nsfwWeb := webByTier["auto"]
	termsWeb := webByTier["basic"]
	bothAgreedWeb := webByTier["super"]
	if err := database.db.WithContext(ctx).Model(&webAccountProfileModel{}).Where("account_id = ?", nsfwWeb.ID).Updates(map[string]any{"nsfw_enabled_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Model(&webAccountProfileModel{}).Where("account_id = ?", termsWeb.ID).Updates(map[string]any{
		"terms_accepted_at": now, "terms_accepted_version": account.CurrentWebTermsVersion,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Model(&webAccountProfileModel{}).Where("account_id = ?", bothAgreedWeb.ID).Updates(map[string]any{
		"nsfw_enabled_at": now, "terms_accepted_at": now, "terms_accepted_version": account.CurrentWebTermsVersion,
	}).Error; err != nil {
		t.Fatal(err)
	}
	// An outdated terms version must not count as accepted.
	staleTermsWeb := webByTier["heavy"]
	if err := database.db.WithContext(ctx).Model(&webAccountProfileModel{}).Where("account_id = ?", staleTermsWeb.ID).Updates(map[string]any{
		"terms_accepted_at": now, "terms_accepted_version": account.CurrentWebTermsVersion - 1,
	}).Error; err != nil {
		t.Fatal(err)
	}
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{Provider: "grok_web", Agreement: "nsfwEnabled", Now: now}, 2)
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{Provider: "grok_web", Agreement: "nsfwDisabled", Now: now}, 2)
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{Provider: "grok_web", Agreement: "termsAccepted", Now: now}, 2)
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{Provider: "grok_web", Agreement: "termsNotAccepted", Now: now}, 2)
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{Provider: "grok_web", Agreement: "allAccepted", Now: now}, 1)
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{Provider: "grok_web", Agreement: "allNotAccepted", Now: now}, 1)

	buildAccount := accountModel{IdentityKey: testIdentityKey("link-build"), Provider: "grok_build", Name: "link-build", SourceKey: "link-build", Enabled: true, AuthStatus: "active", Priority: 1}
	consoleAccount := accountModel{IdentityKey: testIdentityKey("link-console"), Provider: "grok_console", Name: "link-console", SourceKey: "link-console", Enabled: true, AuthStatus: "active", Priority: 1}
	for _, value := range []*accountModel{&buildAccount, &consoleAccount} {
		if err := database.db.WithContext(ctx).Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := database.db.WithContext(ctx).Create(&accountProviderLinkModel{WebAccountID: nsfwWeb.ID, BuildAccountID: buildAccount.ID, CreatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Create(&webConsoleAccountLinkModel{WebAccountID: termsWeb.ID, ConsoleAccountID: consoleAccount.ID, CreatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	bothLinkedBuild := accountModel{IdentityKey: testIdentityKey("both-build"), Provider: "grok_build", Name: "both-build", SourceKey: "both-build", Enabled: true, AuthStatus: "active", Priority: 1}
	bothLinkedConsole := accountModel{IdentityKey: testIdentityKey("both-console"), Provider: "grok_console", Name: "both-console", SourceKey: "both-console", Enabled: true, AuthStatus: "active", Priority: 1}
	for _, value := range []*accountModel{&bothLinkedBuild, &bothLinkedConsole} {
		if err := database.db.WithContext(ctx).Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := database.db.WithContext(ctx).Create(&accountProviderLinkModel{WebAccountID: bothAgreedWeb.ID, BuildAccountID: bothLinkedBuild.ID, CreatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Create(&webConsoleAccountLinkModel{WebAccountID: bothAgreedWeb.ID, ConsoleAccountID: bothLinkedConsole.ID, CreatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{Provider: "grok_web", Association: "buildLinked", Now: now}, 2)
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{Provider: "grok_web", Association: "buildUnlinked", Now: now}, 2)
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{Provider: "grok_web", Association: "consoleLinked", Now: now}, 2)
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{Provider: "grok_web", Association: "consoleUnlinked", Now: now}, 2)
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{Provider: "grok_web", Association: "allLinked", Now: now}, 1)
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{Provider: "grok_web", Association: "allUnlinked", Now: now}, 1)
	accountValues, _, err := accounts.List(ctx, repository.AccountListQuery{Page: repository.PageQuery{Limit: 20, Sort: repository.SortQuery{Field: "name", Direction: repository.SortAscending}}, Filter: repository.AccountListFilter{Provider: "grok_build", Now: now}})
	if err != nil || len(accountValues) < 4 || accountValues[0].Name != "both-build" || accountValues[len(accountValues)-1].Name != "paid" {
		t.Fatalf("account name sort = %#v, err = %v", accountValues, err)
	}

	models := NewModelRepository(database)
	for _, value := range []*modelRouteModel{
		{PublicID: "public-enabled", Provider: "grok_build", UpstreamModel: "upstream-alpha", Capability: "responses", Enabled: true},
		{PublicID: "public-disabled", Provider: "grok_web", UpstreamModel: "upstream-beta", Capability: "chat", Enabled: false},
	} {
		if err := database.db.WithContext(ctx).Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	enabled := true
	_, total, err := models.List(ctx, repository.ModelListQuery{Page: repository.PageQuery{Limit: 20}, Filter: repository.ModelListFilter{Provider: "grok_build", Enabled: &enabled}})
	if err != nil || total != 1 {
		t.Fatalf("model filter total = %d, err = %v", total, err)
	}
	assertModelSearchCount(t, ctx, models, "public-enabled", 1)
	assertModelSearchCount(t, ctx, models, "upstream-beta", 1)
	modelValues, _, err := models.List(ctx, repository.ModelListQuery{Page: repository.PageQuery{Limit: 20, Sort: repository.SortQuery{Field: "publicId", Direction: repository.SortAscending}}})
	if err != nil || len(modelValues) != 2 || modelValues[0].PublicID != "public-disabled" || modelValues[1].PublicID != "public-enabled" {
		t.Fatalf("model name sort = %#v, err = %v", modelValues, err)
	}

	keys := NewClientKeyRepository(database)
	activeKey, err := keys.Create(ctx, clientkeydomain.Key{Name: "production", Prefix: "abc123", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 120, MaxConcurrent: 8})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Create(ctx, clientkeydomain.Key{Name: "disabled", Prefix: "def456", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: false, RPMLimit: 120, MaxConcurrent: 8}); err != nil {
		t.Fatal(err)
	}
	expiredAt := now.Add(-time.Hour)
	if _, err := keys.Create(ctx, clientkeydomain.Key{Name: "expired", Prefix: "expired", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, ExpiresAt: &expiredAt, RPMLimit: 120, MaxConcurrent: 8}); err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Create(&clientKeyModelPermission{ClientKeyID: activeKey.ID, ModelRouteID: 1}).Error; err != nil {
		t.Fatal(err)
	}
	_, total, err = keys.List(ctx, repository.ClientKeyListQuery{Page: repository.PageQuery{Limit: 20}, Filter: repository.ClientKeyListFilter{Status: "expired", Now: now}})
	if err != nil || total != 1 {
		t.Fatalf("expired key filter total = %d, err = %v", total, err)
	}
	_, total, err = keys.List(ctx, repository.ClientKeyListQuery{Page: repository.PageQuery{Limit: 20}, Filter: repository.ClientKeyListFilter{ModelScope: "restricted", Now: now}})
	if err != nil || total != 1 {
		t.Fatalf("key scope filter total = %d, err = %v", total, err)
	}
	assertClientKeySearchCount(t, ctx, keys, "production", 1)
	assertClientKeySearchCount(t, ctx, keys, "abc123", 1)
	keyValues, _, err := keys.List(ctx, repository.ClientKeyListQuery{Page: repository.PageQuery{Limit: 20, Sort: repository.SortQuery{Field: "name", Direction: repository.SortDescending}}, Filter: repository.ClientKeyListFilter{Now: now}})
	if err != nil || len(keyValues) != 3 || keyValues[0].Name != "production" || keyValues[2].Name != "disabled" {
		t.Fatalf("client key name sort = %#v, err = %v", keyValues, err)
	}

	audits := NewAuditRepository(database)
	for _, value := range []requestAuditModel{
		{RequestID: "success", ClientKeyID: 1, ClientKeyName: "production-key", ModelRouteID: 1, ModelPublicID: "grok-public", Provider: "grok_build", Operation: "responses", UsageSource: "upstream", AccountID: uint64PointerForFilter(11), AccountName: "build-primary", StatusCode: 200, Streaming: true, CreatedAt: now},
		{RequestID: "client", ClientKeyID: 2, ClientKeyName: "staging-key", ModelRouteID: 1, ModelUpstreamModel: "grok-upstream", Provider: "grok_build", Operation: "responses", UsageSource: "upstream", AccountID: uint64PointerForFilter(12), AccountName: "build-secondary", StatusCode: 429, Streaming: false, CreatedAt: now},
		{RequestID: "server", ClientKeyID: 3, ClientKeyName: "fallback-key", ModelRouteID: 1, Provider: "grok_build", Operation: "responses", UsageSource: "upstream", AccountID: uint64PointerForFilter(13), AccountName: "web-heavy", StatusCode: 500, Streaming: true, CreatedAt: now},
	} {
		if err := database.db.WithContext(ctx).Create(&value).Error; err != nil {
			t.Fatal(err)
		}
	}
	values, _, err := audits.ListCursor(ctx, repository.AuditCursorQuery{Limit: 20, Filter: repository.AuditListFilter{Status: "success", Mode: "stream"}})
	if err != nil || len(values) != 1 || values[0].RequestID != "success" {
		t.Fatalf("audit filters = %#v, err = %v", values, err)
	}
	assertAuditSearchResult(t, ctx, audits, "success", "success")
	assertAuditSearchResult(t, ctx, audits, "grok-public", "success")
	assertAuditSearchResult(t, ctx, audits, "grok-upstream", "client")
	assertAuditFilterResult(t, ctx, audits, repository.AuditListFilter{Model: "grok-public"}, "success")
	assertAuditFilterResult(t, ctx, audits, repository.AuditListFilter{Status: "4xx"}, "client")
	assertAuditFilterResult(t, ctx, audits, repository.AuditListFilter{Mode: "nonStream"}, "client")
	assertAuditFilterResult(t, ctx, audits, repository.AuditListFilter{Key: "staging"}, "client")
	assertAuditFilterResult(t, ctx, audits, repository.AuditListFilter{Key: "2"}, "client")
	assertAuditFilterResult(t, ctx, audits, repository.AuditListFilter{Account: "secondary"}, "client")
	assertAuditFilterResult(t, ctx, audits, repository.AuditListFilter{Account: "12"}, "client")
	auditSort := repository.SortQuery{Field: "status", Direction: repository.SortDescending}
	firstAuditPage, hasMore, err := audits.ListCursor(ctx, repository.AuditCursorQuery{Limit: 2, Sort: auditSort})
	if err != nil || !hasMore || len(firstAuditPage) != 2 || firstAuditPage[0].RequestID != "server" || firstAuditPage[1].RequestID != "client" {
		t.Fatalf("audit status first page = %#v, hasMore = %v, err = %v", firstAuditPage, hasMore, err)
	}
	secondAuditPage, hasMore, err := audits.ListCursor(ctx, repository.AuditCursorQuery{Limit: 2, Sort: auditSort, Cursor: &repository.SortCursor{ID: firstAuditPage[1].ID, Value: int64(firstAuditPage[1].StatusCode)}})
	if err != nil || hasMore || len(secondAuditPage) != 1 || secondAuditPage[0].RequestID != "success" {
		t.Fatalf("audit status second page = %#v, hasMore = %v, err = %v", secondAuditPage, hasMore, err)
	}
}

func TestListFiltersRecognizesInferredFreeBilling(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "inferred-free.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	value := accountModel{IdentityKey: testIdentityKey("inferred-free"), Provider: "grok_build", Name: "inferred-free", SourceKey: "inferred-free", Enabled: true, AuthStatus: "active", Priority: 1}
	if err := database.db.WithContext(ctx).Create(&value).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Create(&billingModel{
		AccountID: value.ID, IsUnifiedBillingUser: true, UsagePeriodType: "USAGE_PERIOD_TYPE_WEEKLY", SyncedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	accounts := NewAccountRepository(database)
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{QuotaType: "free", Now: now}, 1)
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{QuotaType: "paid", Now: now}, 0)
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{QuotaType: "unknown", Now: now}, 0)
}

func assertAccountFilterCount(t *testing.T, ctx context.Context, accounts *AccountRepository, filter repository.AccountListFilter, expected int64) {
	t.Helper()
	_, total, err := accounts.List(ctx, repository.AccountListQuery{Page: repository.PageQuery{Limit: 20}, Filter: filter})
	if err != nil || total != expected {
		t.Fatalf("account filter %#v total = %d, err = %v", filter, total, err)
	}
}

func assertModelSearchCount(t *testing.T, ctx context.Context, models *ModelRepository, search string, expected int64) {
	t.Helper()
	_, total, err := models.List(ctx, repository.ModelListQuery{Page: repository.PageQuery{Limit: 20, Search: search}})
	if err != nil || total != expected {
		t.Fatalf("model search %q total = %d, err = %v", search, total, err)
	}
}

func assertClientKeySearchCount(t *testing.T, ctx context.Context, keys *ClientKeyRepository, search string, expected int64) {
	t.Helper()
	_, total, err := keys.List(ctx, repository.ClientKeyListQuery{Page: repository.PageQuery{Limit: 20, Search: search}, Filter: repository.ClientKeyListFilter{Now: time.Now().UTC()}})
	if err != nil || total != expected {
		t.Fatalf("client key search %q total = %d, err = %v", search, total, err)
	}
}

func assertAuditSearchResult(t *testing.T, ctx context.Context, audits *AuditRepository, search, expectedRequestID string) {
	t.Helper()
	values, _, err := audits.ListCursor(ctx, repository.AuditCursorQuery{Limit: 20, Search: search})
	if err != nil || len(values) != 1 || values[0].RequestID != expectedRequestID {
		t.Fatalf("audit search %q values = %#v, err = %v", search, values, err)
	}
}

func assertAuditFilterResult(t *testing.T, ctx context.Context, audits *AuditRepository, filter repository.AuditListFilter, expectedRequestID string) {
	t.Helper()
	values, _, err := audits.ListCursor(ctx, repository.AuditCursorQuery{Limit: 20, Filter: filter})
	if err != nil || len(values) != 1 || values[0].RequestID != expectedRequestID {
		t.Fatalf("audit filter %#v values = %#v, err = %v", filter, values, err)
	}
}

func uint64PointerForFilter(value uint64) *uint64 { return &value }
