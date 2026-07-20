package relational

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInitializeSchemaUpgradesClientKeyLimitsToAllowZero(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "legacy-client-key-limits.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	db := database.db.WithContext(ctx)
	now := time.Now().UTC()
	legacy := clientKeyModel{
		Name: "legacy", Prefix: "legacy-prefix", SecretHash: testSecretHash,
		EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	if err := recreatePositiveOnlyClientKeyLimits(ctx, database); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("UPDATE client_keys SET rpm_limit = 0 WHERE prefix = ?", "legacy-prefix").Error; err == nil {
		t.Fatal("legacy RPM constraint unexpectedly accepted zero")
	}

	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("UPDATE client_keys SET rpm_limit = 0, max_concurrent = 0 WHERE prefix = ?", "legacy-prefix").Error; err != nil {
		t.Fatalf("upgraded constraints rejected zero: %v", err)
	}
	var stored clientKeyModel
	if err := db.Where("prefix = ?", "legacy-prefix").First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Name != "legacy" || stored.RPMLimit != 0 || stored.MaxConcurrent != 0 {
		t.Fatalf("stored key after upgrade = %#v", stored)
	}
	if err := db.Exec("UPDATE client_keys SET rpm_limit = -1 WHERE prefix = ?", "legacy-prefix").Error; err == nil {
		t.Fatal("upgraded RPM constraint accepted a negative value")
	}
	if err := db.Exec("UPDATE client_keys SET max_concurrent = 1025 WHERE prefix = ?", "legacy-prefix").Error; err == nil {
		t.Fatal("upgraded concurrency constraint accepted an oversized value")
	}

	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatalf("repeated schema initialization: %v", err)
	}
	if err := db.Where("prefix = ?", "legacy-prefix").First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.RPMLimit != 0 || stored.MaxConcurrent != 0 {
		t.Fatalf("limits after repeated initialization = rpm %d, concurrency %d", stored.RPMLimit, stored.MaxConcurrent)
	}
}

func recreatePositiveOnlyClientKeyLimits(ctx context.Context, database *Database) error {
	return database.withSQLiteForeignKeysDisabled(ctx, func() error {
		db := database.db.WithContext(ctx)
		var tableSQL string
		if err := db.Raw("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'client_keys'").Scan(&tableSQL).Error; err != nil {
			return err
		}
		legacySQL := strings.Replace(tableSQL, "rpm_limit BETWEEN 0 AND 100000", "rpm_limit BETWEEN 1 AND 100000", 1)
		legacySQL = strings.Replace(legacySQL, "max_concurrent BETWEEN 0 AND 1024", "max_concurrent BETWEEN 1 AND 1024", 1)
		if legacySQL == tableSQL {
			return nil
		}
		legacySQL = strings.Replace(legacySQL, "client_keys", "client_keys_legacy", 1)
		if err := db.Exec(legacySQL).Error; err != nil {
			return err
		}
		if err := db.Exec("INSERT INTO client_keys_legacy SELECT * FROM client_keys").Error; err != nil {
			return err
		}
		if err := db.Exec("DROP TABLE client_keys").Error; err != nil {
			return err
		}
		return db.Exec("ALTER TABLE client_keys_legacy RENAME TO client_keys").Error
	})
}

func TestClientKeyLimitConstraintAllowsZero(t *testing.T) {
	for _, definition := range []string{
		"CHECK (rpm_limit BETWEEN 0 AND 100000)",
		`CHECK ((("max_concurrent" >= 0) AND ("max_concurrent" <= 1024)))`,
	} {
		if !clientKeyLimitConstraintAllowsZero(definition) {
			t.Fatalf("constraint should allow zero: %s", definition)
		}
	}
	if clientKeyLimitConstraintAllowsZero("CHECK (rpm_limit BETWEEN 1 AND 100000)") {
		t.Fatal("legacy positive-only constraint was treated as upgraded")
	}
}
