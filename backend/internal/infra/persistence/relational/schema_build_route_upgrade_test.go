package relational

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestInitializeSchemaUpgradesBuildRouteConstraintWithReferencedAccounts(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "legacy-build-route.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}

	accounts := NewAccountRepository(database)
	created, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, AuthType: accountdomain.AuthTypeOAuth,
		Name: "legacy-build", SourceKey: "legacy-build-route", EncryptedAccessToken: testEncryptedToken,
		AuthStatus: accountdomain.AuthStatusActive, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.withSQLiteForeignKeysDisabled(ctx, func() error {
		return database.db.WithContext(ctx).Migrator().DropConstraint(&accountModel{}, "chk_accounts_build_route_mode")
	}); err != nil {
		t.Fatal(err)
	}
	definition, err := database.constraintDefinition(ctx, consoleConstraint{
		model: &accountModel{}, table: "provider_accounts", name: "chk_accounts_build_route_mode",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(definition, "build_route_mode") {
		t.Fatalf("legacy constraint was not removed: %s", definition)
	}

	// account_credentials 已引用 provider_accounts；升级仍应安全重建父表并保留账号。
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	definition, err = database.constraintDefinition(ctx, consoleConstraint{
		model: &accountModel{}, table: "provider_accounts", name: "chk_accounts_build_route_mode",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(definition, "auto") || !strings.Contains(definition, "build") || !strings.Contains(definition, "xai") {
		t.Fatalf("upgraded constraint = %s", definition)
	}
	stored, err := accounts.Get(ctx, created.ID)
	if err != nil || stored.ID != created.ID {
		t.Fatalf("stored account = %#v, err = %v", stored, err)
	}
}
