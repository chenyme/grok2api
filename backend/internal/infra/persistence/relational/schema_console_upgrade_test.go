package relational

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
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
		&legacyResponseOwnershipModel{}, &legacyEgressNodeModel{},
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
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if preserved, err := accountRepository.Get(ctx, created.ID); err != nil || preserved.Name != "existing-build" {
		t.Fatalf("existing account was not preserved: %#v, err=%v", preserved, err)
	}
	for _, table := range []string{"provider_accounts", "model_routes", "request_audits", "response_ownership", "egress_nodes"} {
		var sql string
		if err := database.db.WithContext(ctx).Raw("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&sql).Error; err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(sql, "grok_console") {
			t.Fatalf("table %s was not upgraded: %s", table, sql)
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
	ID       uint64 `gorm:"primaryKey"`
	Provider string `gorm:"size:32;not null;check:chk_request_audits_provider,provider IN ('grok_build','grok_web')"`
}

func (legacyRequestAuditModel) TableName() string { return "request_audits" }

type legacyResponseOwnershipModel struct {
	ID       uint64 `gorm:"primaryKey"`
	Provider string `gorm:"size:32;not null;check:chk_response_ownership_provider,provider IN ('grok_build','grok_web')"`
}

func (legacyResponseOwnershipModel) TableName() string { return "response_ownership" }

type legacyEgressNodeModel struct {
	ID    uint64 `gorm:"primaryKey"`
	Scope string `gorm:"size:32;not null;check:chk_egress_nodes_specific_scope,scope IN ('all','grok_build','grok_web','grok_web_asset')"`
}

func (legacyEgressNodeModel) TableName() string { return "egress_nodes" }
