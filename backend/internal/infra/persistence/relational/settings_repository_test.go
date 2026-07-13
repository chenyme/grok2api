package relational

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	repositorypkg "github.com/chenyme/grok2api/backend/internal/repository"
)

func TestRuntimeSettingsRepositoryRoundTrip(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repository := NewRuntimeSettingsRepository(database, cipher)
	if _, _, _, found, err := repository.Get(ctx); err != nil || found {
		t.Fatalf("initial get found = %v, err = %v", found, err)
	}
	settings := settingsdomain.Config{
		ProviderWeb: settingsdomain.ProviderWebConfig{StatsigManualValue: "sensitive-statsig-value"},
		Media: settingsdomain.MediaConfig{
			MaxImageBytes: 16 << 20, MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 80,
			CleanupInterval: 10 * time.Minute,
		},
		Routing: settingsdomain.RoutingConfig{StickyTTL: time.Hour, MaxAttempts: 3},
	}
	updatedAt, revision, err := repository.Save(ctx, settings, 0)
	if err != nil {
		t.Fatal(err)
	}
	value, storedUpdatedAt, storedRevision, found, err := repository.Get(ctx)
	if err != nil || !found {
		t.Fatalf("saved get found = %v, err = %v", found, err)
	}
	if value.Routing != settings.Routing || value.Media != settings.Media || !storedUpdatedAt.Equal(updatedAt) || revision != 1 || storedRevision != revision {
		t.Fatalf("saved value = %#v", value)
	}
	if value.ProviderWeb.StatsigManualValue != settings.ProviderWeb.StatsigManualValue {
		t.Fatalf("Statsig manual value = %q", value.ProviderWeb.StatsigManualValue)
	}
	var row runtimeSettingsModel
	if err := database.db.WithContext(ctx).Where("key = ?", runtimeSettingsKey).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row.ValueJSON, settings.ProviderWeb.StatsigManualValue) {
		t.Fatalf("Statsig manual value was stored in plaintext: %s", row.ValueJSON)
	}
	if strings.Contains(row.ValueJSON, `"version"`) {
		t.Fatalf("runtime settings contain version metadata: %s", row.ValueJSON)
	}
	if _, _, err := repository.Save(ctx, settings, 0); !errors.Is(err, repositorypkg.ErrConflict) {
		t.Fatalf("stale repository revision error = %v", err)
	}
}
