package relational

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
)

func TestSchemaBackfillsBuildResponseHeaderTimeout(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "runtime-settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	assertSchemaBackfillsBuildResponseHeaderTimeout(t, ctx, database)
}

func TestPostgresSchemaBackfillsBuildResponseHeaderTimeout(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not configured")
	}
	ctx := context.Background()
	database, err := OpenPostgres(ctx, dsn, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Where("key = ?", runtimeSettingsKey).Delete(&runtimeSettingsModel{}).Error; err != nil {
		t.Fatal(err)
	}
	defer database.db.WithContext(ctx).Where("key = ?", runtimeSettingsKey).Delete(&runtimeSettingsModel{})
	assertSchemaBackfillsBuildResponseHeaderTimeout(t, ctx, database)
}

func assertSchemaBackfillsBuildResponseHeaderTimeout(t *testing.T, ctx context.Context, database *Database) {
	t.Helper()
	legacy, err := json.Marshal(runtimeSettingsPayload{Config: settingsdomain.Config{
		ProviderBuild: settingsdomain.ProviderBuildConfig{BaseURL: "https://cli-chat-proxy.grok.com/v1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	updatedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	row := runtimeSettingsModel{Key: runtimeSettingsKey, ValueJSON: string(legacy), Revision: 7, UpdatedAt: updatedAt}
	if err := database.db.WithContext(ctx).Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	var migrated runtimeSettingsModel
	if err := database.db.WithContext(ctx).Where("key = ?", runtimeSettingsKey).First(&migrated).Error; err != nil {
		t.Fatal(err)
	}
	var payload runtimeSettingsPayload
	if err := json.Unmarshal([]byte(migrated.ValueJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Config.ProviderBuild.ResponseHeaderTimeout != settingsdomain.DefaultBuildResponseHeaderTimeout {
		t.Fatalf("response header timeout = %s", payload.Config.ProviderBuild.ResponseHeaderTimeout)
	}
	if migrated.Revision != 7 || !migrated.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("migration changed revision metadata: revision=%d updatedAt=%s", migrated.Revision, migrated.UpdatedAt)
	}
	firstValue := migrated.ValueJSON
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Where("key = ?", runtimeSettingsKey).First(&migrated).Error; err != nil {
		t.Fatal(err)
	}
	if migrated.ValueJSON != firstValue {
		t.Fatal("repeated schema initialization rewrote the migrated settings")
	}
}
