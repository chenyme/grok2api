package relational

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
)

func TestInitializeSchemaMigratesLegacyClientKeyAccountPoolToScopes(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "legacy-client-key-account-pool.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := NewClientKeyRepository(database)
	create := func(prefix string) clientkeydomain.Key {
		value, createErr := repository.Create(ctx, clientkeydomain.Key{Name: prefix, Prefix: prefix, SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true})
		if createErr != nil {
			t.Fatal(createErr)
		}
		return value
	}
	freeKey := create("legacy-free")
	superKey := create("legacy-super")
	allKey := create("legacy-all")
	if err := recreateClientKeysWithLegacyAccountPool(ctx, database); err != nil {
		t.Fatal(err)
	}
	if err := database.withSQLiteForeignKeysDisabled(ctx, func() error {
		db := database.db.WithContext(ctx)
		if err := db.Table("client_keys").Where("id = ?", freeKey.ID).Update("account_pool", "free").Error; err != nil {
			return err
		}
		return db.Table("client_keys").Where("id = ?", superKey.ID).Update("account_pool", "super").Error
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	assertScope := func(id uint64, wantTier clientkeydomain.TierScope) {
		stored, getErr := repository.Get(ctx, id)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if stored.ProviderScope != clientkeydomain.ProviderScopeAll || stored.TierScope != wantTier {
			t.Fatalf("migrated scope for %d = %+v", id, stored.AccountScope())
		}
	}
	assertScope(freeKey.ID, clientkeydomain.TierScopeFree)
	assertScope(superKey.ID, clientkeydomain.TierScopeSuper)
	assertScope(allKey.ID, clientkeydomain.TierScopeAll)
	if err := database.db.WithContext(ctx).Model(&clientKeyModel{}).Where("id = ?", allKey.ID).Update("tier_scope_mask", 4).Error; err == nil {
		t.Fatal("tier scope constraint accepted an unsupported unknown-only value")
	}
}

func recreateClientKeysWithLegacyAccountPool(ctx context.Context, database *Database) error {
	return database.withSQLiteForeignKeysDisabled(ctx, func() error {
		db := database.db.WithContext(ctx)
		var tableSQL string
		if err := db.Raw("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'client_keys'").Scan(&tableSQL).Error; err != nil {
			return err
		}
		legacySQL := strings.Replace(tableSQL, ",`provider_scope_mask` integer NOT NULL DEFAULT 7", "", 1)
		legacySQL = strings.Replace(legacySQL, ",`tier_scope_mask` integer NOT NULL DEFAULT 7", "", 1)
		legacySQL = strings.Replace(legacySQL, ",CONSTRAINT `chk_client_keys_provider_scope` CHECK (provider_scope_mask BETWEEN 1 AND 7)", "", 1)
		legacySQL = strings.Replace(legacySQL, ",CONSTRAINT `chk_client_keys_tier_scope` CHECK (tier_scope_mask IN (1,2,3,7))", "", 1)
		legacySQL = strings.Replace(legacySQL, ",CONSTRAINT", ",`account_pool` text NOT NULL DEFAULT 'all' CHECK (account_pool IN ('all','free','super')),CONSTRAINT", 1)
		legacySQL = strings.Replace(legacySQL, "client_keys", "client_keys_legacy_scope", 1)
		if err := db.Exec(legacySQL).Error; err != nil {
			return fmt.Errorf("create legacy table with %s: %w", legacySQL, err)
		}
		const columns = "id,name,prefix,secret_hash,encrypted_secret,enabled,expires_at,rpm_limit,max_concurrent,billing_limit_usd_ticks,billed_usage_usd_ticks,reserved_usage_usd_ticks,allow_model_aliases,last_used_at,created_at,updated_at"
		if err := db.Exec("INSERT INTO client_keys_legacy_scope (" + columns + ") SELECT " + columns + " FROM client_keys").Error; err != nil {
			return err
		}
		if err := db.Exec("DROP TABLE client_keys").Error; err != nil {
			return err
		}
		return db.Exec("ALTER TABLE client_keys_legacy_scope RENAME TO client_keys").Error
	})
}
