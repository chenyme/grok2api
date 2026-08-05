package relational

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
)

func TestInitializeSchemaUpgradesProviderChecksForConsole(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	legacy := []any{
		&legacyProviderAccountModel{}, &legacyModelRouteModel{}, &legacyRequestAuditModel{},
		&legacyResponseOwnershipModel{}, &legacyEgressSubscriptionSourceModel{}, &legacyEgressNodeModel{},
	}
	if err := database.db.WithContext(ctx).AutoMigrate(legacy...); err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).AutoMigrate(schemaModels...); err != nil {
		t.Fatal(err)
	}
	accountRepository := NewAccountRepository(database)
	created, _, err := accountRepository.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: "existing-build", SourceKey: "existing-build",
		EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := accountRepository.SaveQuotaWindows(ctx, created.ID, account.WebTierAuto, now, []account.QuotaWindow{{
		AccountID: created.ID, Mode: "test", Remaining: 7, Total: 20, WindowSeconds: 3600,
		Source: account.QuotaSourceUpstream, SyncedAt: &now,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if preserved, err := accountRepository.Get(ctx, created.ID); err != nil || preserved.Name != "existing-build" || preserved.EncryptedAccessToken != "encrypted" || preserved.AuthType != account.AuthTypeOAuth {
		t.Fatalf("existing account was not preserved: %#v, err=%v", preserved, err)
	}
	windows, err := accountRepository.GetQuotaWindows(ctx, []uint64{created.ID})
	if err != nil || len(windows[created.ID]) != 1 || windows[created.ID][0].Remaining != 7 {
		t.Fatalf("existing quota windows were not preserved: %#v, err=%v", windows, err)
	}
	for _, table := range []string{"provider_accounts", "model_routes", "request_audits", "response_ownership", "egress_nodes", "egress_subscription_sources"} {
		var sql string
		if err := database.db.WithContext(ctx).Raw("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&sql).Error; err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(sql, "grok_console") {
			t.Fatalf("table %s was not upgraded: %s", table, sql)
		}
		if (table == "request_audits" || table == "egress_nodes" || table == "egress_subscription_sources") && !strings.Contains(sql, "grok_console_asset") {
			t.Fatalf("table %s was not upgraded for Console assets: %s", table, sql)
		}
		if table == "request_audits" && !strings.Contains(sql, "compaction") {
			t.Fatalf("table %s operation constraint was not upgraded: %s", table, sql)
		}
	}
	assertSQLiteUniqueIndexes(t, database, "provider_accounts", "idx_provider_accounts_identity_key")
	assertSQLiteUniqueIndexes(t, database, "model_routes", "uidx_model_routes_managed_public_capability")
	assertSQLiteIndexes(t, database, "model_routes", "idx_model_routes_public_id_lookup", "idx_model_routes_provider_upstream", "idx_model_routes_grouping")
	assertSQLiteMissingIndexes(t, database, "model_routes", "idx_model_routes_public_id")
	assertSQLiteMissingIndexes(t, database, "model_routes", "uidx_provider_upstream")
	assertTableColumns(t, database, "request_audits", []string{"first_token_ms"}, nil)
	assertTableColumns(t, database, "response_ownership", []string{"model_route_id", "prompt_cache_key", "reasoning_replay_key"}, nil)
}

func TestInitializeSchemaDropsRedundantResponseExpiryIndexes(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "response-indexes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"CREATE INDEX IF NOT EXISTS idx_response_ownership_expires ON response_ownership(expires_at)",
		"CREATE INDEX IF NOT EXISTS idx_web_response_states_expires ON web_response_states(expires_at)",
	} {
		if err := database.db.WithContext(ctx).Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"response_ownership", "web_response_states"} {
		var indexes []struct{ Name string }
		if err := database.db.Raw("PRAGMA index_list('" + table + "')").Scan(&indexes).Error; err != nil {
			t.Fatal(err)
		}
		for _, index := range indexes {
			if index.Name == "idx_response_ownership_expires" || index.Name == "idx_web_response_states_expires" {
				t.Fatalf("redundant expiry index %s remains on %s", index.Name, table)
			}
		}
	}
}

func TestInitializeSchemaUpgradesPublicIDUniqueIndexForManualTargetPools(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "model-target-pool-upgrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	for _, index := range []string{"uidx_model_routes_managed_public_capability", "idx_model_routes_public_id_lookup"} {
		if err := database.db.Exec("DROP INDEX IF EXISTS " + index).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := database.db.Exec("CREATE UNIQUE INDEX idx_model_routes_public_id ON model_routes(public_id)").Error; err != nil {
		t.Fatal(err)
	}
	repo := NewModelRepository(database)
	if _, err := repo.Create(ctx, modeldomain.Route{
		PublicID: "shared-upgrade", Provider: account.ProviderBuild, UpstreamModel: "upstream-a",
		Capability: modeldomain.CapabilityResponses, Enabled: true,
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Create(ctx, modeldomain.Route{
		PublicID: "shared-upgrade", Provider: account.ProviderBuild, UpstreamModel: "upstream-b",
		Capability: modeldomain.CapabilityResponses, Enabled: true,
	}, nil); err != nil {
		t.Fatalf("create duplicate target after upgrade: %v", err)
	}
	assertSQLiteMissingIndexes(t, database, "model_routes", "idx_model_routes_public_id")
	assertSQLiteIndexes(t, database, "model_routes", "idx_model_routes_public_id_lookup")
	assertSQLiteUniqueIndexes(t, database, "model_routes", "uidx_model_routes_managed_public_capability")
	assertSQLiteMissingIndexes(t, database, "model_routes", "uidx_model_routes_managed_public_id")
}

func TestManagedRoutesAllowOnePublicIDPerCapability(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "model-capability-pool.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewModelRepository(database)
	routes := []modeldomain.Route{
		{PublicID: "grok-imagine-image-quality", Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-image-quality", Capability: modeldomain.CapabilityImage, Enabled: true},
		{PublicID: "grok-imagine-image-quality", Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-image-quality", Capability: modeldomain.CapabilityImageEdit, Enabled: true},
	}
	if err := repo.ReplaceProviderRoutes(ctx, account.ProviderConsole, routes); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertDiscovered(ctx, account.ProviderConsole, []string{"grok-imagine-image-quality"}); err != nil {
		t.Fatal(err)
	}
	var rows []modelRouteModel
	if err := database.db.Where("public_id = ?", "Console/grok-imagine-image-quality").Order("capability ASC").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Capability == rows[1].Capability {
		t.Fatalf("managed capability routes = %#v", rows)
	}
	firstIDs := []uint64{rows[0].ID, rows[1].ID}
	if err := repo.ReplaceProviderRoutes(ctx, account.ProviderConsole, routes); err != nil {
		t.Fatal(err)
	}
	rows = nil
	if err := database.db.Where("public_id = ?", "Console/grok-imagine-image-quality").Order("capability ASC").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].ID != firstIDs[0] || rows[1].ID != firstIDs[1] {
		t.Fatalf("managed capability route IDs changed: before=%v after=%#v", firstIDs, rows)
	}
}

func assertSQLiteUniqueIndexes(t *testing.T, database *Database, table string, expected ...string) {
	t.Helper()
	var indexes []struct {
		Name   string
		Unique int
	}
	if err := database.db.Raw("PRAGMA index_list('" + table + "')").Scan(&indexes).Error; err != nil {
		t.Fatal(err)
	}
	found := make(map[string]bool, len(indexes))
	for _, index := range indexes {
		if index.Unique == 1 {
			found[index.Name] = true
		}
	}
	for _, name := range expected {
		if !found[name] {
			t.Fatalf("table %s missing unique index %s: %#v", table, name, indexes)
		}
	}
}

func assertSQLiteIndexes(t *testing.T, database *Database, table string, expected ...string) {
	t.Helper()
	var indexes []struct {
		Name   string
		Unique int
	}
	if err := database.db.Raw("PRAGMA index_list('" + table + "')").Scan(&indexes).Error; err != nil {
		t.Fatal(err)
	}
	found := make(map[string]bool, len(indexes))
	for _, index := range indexes {
		found[index.Name] = true
	}
	for _, name := range expected {
		if !found[name] {
			t.Fatalf("table %s missing index %s: %#v", table, name, indexes)
		}
	}
}

func assertSQLiteMissingIndexes(t *testing.T, database *Database, table string, unexpected ...string) {
	t.Helper()
	var indexes []struct {
		Name   string
		Unique int
	}
	if err := database.db.Raw("PRAGMA index_list('" + table + "')").Scan(&indexes).Error; err != nil {
		t.Fatal(err)
	}
	found := make(map[string]bool, len(indexes))
	for _, index := range indexes {
		found[index.Name] = true
	}
	for _, name := range unexpected {
		if found[name] {
			t.Fatalf("table %s still has index %s: %#v", table, name, indexes)
		}
	}
}

func TestInitializeSchemaBackfillsStreamFailureStatusCodes(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "legacy-status.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.db.WithContext(ctx).AutoMigrate(schemaModels...); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rows := []requestAuditModel{
		{RequestID: "stream-interrupted", ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build", Operation: "responses", UsageSource: "upstream", StatusCode: 200, Streaming: true, ErrorCode: "upstream_stream_interrupted", CreatedAt: now},
		{RequestID: "stream-error", ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build", Operation: "responses", UsageSource: "upstream", StatusCode: 200, Streaming: true, ErrorCode: "upstream_stream_error", CreatedAt: now},
		{RequestID: "healthy", ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build", Operation: "responses", UsageSource: "upstream", StatusCode: 200, CreatedAt: now},
		{RequestID: "http-500", ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build", Operation: "responses", UsageSource: "upstream", StatusCode: 500, ErrorCode: "upstream_server_error", CreatedAt: now},
	}
	// 先插入不受旧约束影响的记录，再模拟旧约束：回填前 2xx+error 记录必须先被改写成 0，
	// 否则重建约束（BETWEEN 0 AND 599）不会失败；此处通过迁移后数据断言回填生效。
	if err := database.db.WithContext(ctx).Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	// 执行迁移：应把 2xx+error_code 回填为 0，并放宽约束。
	if err := database.ensureAuditStatusCodeConstraints(ctx); err != nil {
		t.Fatal(err)
	}
	var byRequest map[string]requestAuditModel
	byRequest = make(map[string]requestAuditModel, len(rows))
	var stored []requestAuditModel
	if err := database.db.WithContext(ctx).Find(&stored).Error; err != nil {
		t.Fatal(err)
	}
	for _, row := range stored {
		byRequest[row.RequestID] = row
	}
	if got := byRequest["stream-interrupted"].StatusCode; got != 0 {
		t.Fatalf("stream-interrupted status = %d, want 0", got)
	}
	if got := byRequest["stream-error"].StatusCode; got != 0 {
		t.Fatalf("stream-error status = %d, want 0", got)
	}
	if got := byRequest["healthy"].StatusCode; got != 200 {
		t.Fatalf("healthy status = %d, want 200", got)
	}
	if got := byRequest["http-500"].StatusCode; got != 500 {
		t.Fatalf("http-500 status = %d, want 500", got)
	}
	// 约束现在必须允许写入状态码 0。
	candidate := requestAuditModel{RequestID: "zero-allowed", ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build", Operation: "responses", UsageSource: "upstream", StatusCode: 0, ErrorCode: "upstream_stream_interrupted", CreatedAt: now}
	if err := database.db.WithContext(ctx).Create(&candidate).Error; err != nil {
		t.Fatalf("status code 0 rejected after migration: %v", err)
	}
	// 幂等：再次执行迁移不应报错。
	if err := database.ensureAuditStatusCodeConstraints(ctx); err != nil {
		t.Fatalf("migration is not idempotent: %v", err)
	}
}

func TestAuditStatusCodeConstraintAllowsZero(t *testing.T) {
	// SQLite 与 PostgreSQL 对同一 CHECK 生成不同的约束定义，归一化判断必须同时命中。
	allowed := []string{
		"CONSTRAINT `chk_request_audits_status_code` CHECK (status_code BETWEEN 0 AND 599)",
		"CONSTRAINT chk_request_audits_status_code CHECK ((status_code >= 0) AND (status_code <= 599))",
	}
	for _, definition := range allowed {
		if !auditStatusCodeConstraintAllowsZero(definition) {
			t.Fatalf("constraint should allow zero: %s", definition)
		}
	}
	rejected := []string{
		"CONSTRAINT `chk_request_audits_status_code` CHECK (status_code BETWEEN 100 AND 599)",
		"CONSTRAINT chk_request_audits_status_code CHECK ((status_code >= 100) AND (status_code <= 599))",
		"",
	}
	for _, definition := range rejected {
		if auditStatusCodeConstraintAllowsZero(definition) {
			t.Fatalf("constraint should not allow zero: %s", definition)
		}
	}
}

type legacyProviderAccountModel struct {
	ID       uint64 `gorm:"primaryKey"`
	Provider string `gorm:"size:32;not null;check:chk_accounts_provider,provider IN ('grok_build','grok_web')"`
}

func (legacyProviderAccountModel) TableName() string { return "provider_accounts" }

type legacyModelRouteModel struct {
	ID       uint64 `gorm:"primaryKey"`
	Provider string `gorm:"size:32;not null;check:chk_model_routes_provider,provider IN ('grok_build','grok_web')"`
}

func (legacyModelRouteModel) TableName() string { return "model_routes" }

type legacyRequestAuditModel struct {
	ID          uint64 `gorm:"primaryKey"`
	Provider    string `gorm:"size:32;not null;check:chk_request_audits_provider,provider IN ('grok_build','grok_web')"`
	Operation   string `gorm:"size:32;not null;default:'responses';check:chk_request_audits_operation,operation IN ('responses','chat','messages','image','image_edit','video')"`
	EgressScope string `gorm:"size:32;not null;default:'';check:chk_request_audits_egress_scope,egress_scope IN ('','grok_build','grok_web','grok_web_asset')"`
}

func (legacyRequestAuditModel) TableName() string { return "request_audits" }

type legacyResponseOwnershipModel struct {
	ID       uint64 `gorm:"primaryKey"`
	Provider string `gorm:"size:32;not null;check:chk_response_ownership_provider,provider IN ('grok_build','grok_web')"`
}

func (legacyResponseOwnershipModel) TableName() string { return "response_ownership" }

type legacyEgressSubscriptionSourceModel struct {
	ID    uint64 `gorm:"primaryKey"`
	Scope string `gorm:"size:32;not null;check:chk_egress_subscription_sources_scope,scope IN ('grok_build','grok_web','grok_web_asset')"`
}

func (legacyEgressSubscriptionSourceModel) TableName() string {
	return "egress_subscription_sources"
}

type legacyEgressNodeModel struct {
	ID    uint64 `gorm:"primaryKey"`
	Scope string `gorm:"size:32;not null;check:chk_egress_nodes_specific_scope,scope IN ('all','grok_build','grok_web','grok_web_asset')"`
}

func (legacyEgressNodeModel) TableName() string { return "egress_nodes" }
