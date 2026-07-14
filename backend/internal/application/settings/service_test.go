package settings

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type runtimeSettingsRepositoryStub struct {
	value     settingsdomain.Config
	updatedAt time.Time
	revision  uint64
	found     bool
	getCount  int
}

func (r *runtimeSettingsRepositoryStub) Get(context.Context) (settingsdomain.Config, time.Time, uint64, bool, error) {
	r.getCount++
	return r.value, r.updatedAt, r.revision, r.found, nil
}

func (r *runtimeSettingsRepositoryStub) Save(_ context.Context, value settingsdomain.Config, expectedRevision uint64) (time.Time, uint64, error) {
	if expectedRevision != r.revision {
		return time.Time{}, 0, repository.ErrConflict
	}
	r.value = value
	r.updatedAt = time.Now().UTC()
	r.revision++
	r.found = true
	return r.updatedAt, r.revision, nil
}

func TestUpdatePersistsAppliesAndReportsRestart(t *testing.T) {
	cfg := testConfig(t)
	repository := &runtimeSettingsRepositoryStub{}
	var applied config.Config
	service := NewService(cfg, time.Time{}, 0, repository, nil, func(next config.Config) { applied = next })
	input := service.Get().Config
	input.Routing.MaxAttempts = 5
	input.Audit.BufferSize = cfg.Audit.BufferSize + 1
	input.Media.MaxTotalBytes = 2 << 30
	input.Media.CleanupThresholdPercent = 75
	input.Media.CleanupInterval = "5m"
	input.ProviderConsole.BaseURL = "https://console.example.com"
	input.ProviderConsole.UserAgent = "console-test-agent"
	input.ProviderConsole.ChatTimeout = "6m"
	input.Batch = BatchConfig{ImportConcurrency: 26, ConversionConcurrency: 27, SyncConcurrency: 28, RefreshConcurrency: 29, RandomDelay: "750ms"}

	snapshot, err := service.Update(context.Background(), service.Get().Revision, input)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Routing.MaxAttempts != 5 {
		t.Fatalf("runtime configuration was not applied: %#v", applied.Routing)
	}
	if applied.Media.MaxTotalBytes != 2<<30 || applied.Media.CleanupThresholdPercent != 75 || applied.Media.CleanupInterval.Value() != 5*time.Minute {
		t.Fatalf("media configuration was not applied: %#v", applied.Media)
	}
	if applied.Batch.ImportConcurrency != 26 || applied.Batch.ConversionConcurrency != 27 || applied.Batch.SyncConcurrency != 28 || applied.Batch.RefreshConcurrency != 29 || applied.Batch.RandomDelay.Value() != 750*time.Millisecond {
		t.Fatalf("batch configuration was not applied: %#v", applied.Batch)
	}
	if applied.Provider.Console.BaseURL != "https://console.example.com" || applied.Provider.Console.UserAgent != "console-test-agent" || applied.Provider.Console.ChatTimeout.Value() != 6*time.Minute {
		t.Fatalf("console configuration was not applied: %#v", applied.Provider.Console)
	}
	if len(snapshot.RestartRequired) != 1 || snapshot.RestartRequired[0] != "audit.bufferSize" {
		t.Fatalf("restartRequired = %#v", snapshot.RestartRequired)
	}
	reloaded, _, _, err := LoadPersisted(context.Background(), cfg, repository)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Routing.MaxAttempts != 5 || reloaded.Audit.BufferSize != input.Audit.BufferSize || reloaded.Media.MaxTotalBytes != 2<<30 || reloaded.Media.CleanupThresholdPercent != 75 || reloaded.Batch.SyncConcurrency != 28 || reloaded.Batch.RandomDelay.Value() != 750*time.Millisecond || reloaded.Provider.Console.BaseURL != "https://console.example.com" {
		t.Fatalf("configuration was not persisted")
	}
}

func TestLoadPersistedKeepsConsoleDefaultsWhenFieldIsMissing(t *testing.T) {
	cfg := testConfig(t)
	value := toDomainConfig(cfg)
	value.ProviderConsole = settingsdomain.ProviderConsoleConfig{}
	repository := &runtimeSettingsRepositoryStub{value: value, found: true}
	loaded, _, _, err := LoadPersisted(context.Background(), cfg, repository)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Provider.Console != cfg.Provider.Console {
		t.Fatalf("console config = %#v, want %#v", loaded.Provider.Console, cfg.Provider.Console)
	}
}

func TestSnapshotIncludesRecommendedBuildBaseline(t *testing.T) {
	service := NewService(testConfig(t), time.Time{}, 0, &runtimeSettingsRepositoryStub{}, nil, nil)
	recommended := service.Get().RecommendedProviderBuild
	if recommended.ClientVersion != config.RecommendedBuildClientVersion || recommended.UserAgent != config.RecommendedBuildUserAgent {
		t.Fatalf("recommended build = %#v", recommended)
	}
}

func TestUpdateRejectsBatchConcurrencyOutsideSafeRange(t *testing.T) {
	cfg := testConfig(t)
	repository := &runtimeSettingsRepositoryStub{}
	service := NewService(cfg, time.Time{}, 0, repository, nil, nil)
	input := service.Get().Config
	input.Batch.ConversionConcurrency = 51
	if _, err := service.Update(context.Background(), 0, input); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("error = %v", err)
	}
	if repository.found {
		t.Fatal("invalid batch settings were persisted")
	}
}

func TestBatchRandomDelayCanBeDisabledAndPersisted(t *testing.T) {
	cfg := testConfig(t)
	repository := &runtimeSettingsRepositoryStub{}
	service := NewService(cfg, time.Time{}, 0, repository, nil, nil)
	input := service.Get().Config
	input.Batch.RandomDelay = "0s"
	if _, err := service.Update(context.Background(), 0, input); err != nil {
		t.Fatal(err)
	}
	if repository.value.Batch.RandomDelay == nil || *repository.value.Batch.RandomDelay != 0 {
		t.Fatalf("persisted random delay = %#v", repository.value.Batch.RandomDelay)
	}
	loaded, _, _, err := LoadPersisted(context.Background(), cfg, repository)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Batch.RandomDelay.Value() != 0 {
		t.Fatalf("loaded random delay = %s", loaded.Batch.RandomDelay.Value())
	}
}

func TestUpdateRejectsInvalidDurationWithoutChangingConfig(t *testing.T) {
	cfg := testConfig(t)
	repository := &runtimeSettingsRepositoryStub{}
	service := NewService(cfg, time.Time{}, 0, repository, nil, nil)
	input := service.Get().Config
	input.Routing.StickyTTL = "tomorrow"
	if _, err := service.Update(context.Background(), service.Get().Revision, input); err == nil {
		t.Fatal("expected invalid duration error")
	}
	if service.Get().Config.Routing.StickyTTL != cfg.Routing.StickyTTL.String() || repository.found {
		t.Fatal("invalid update changed or persisted runtime configuration")
	}
}

func TestStatsigManualValueIsWriteOnlyAndClearedByURLMode(t *testing.T) {
	cfg := testConfig(t)
	repository := &runtimeSettingsRepositoryStub{}
	service := NewService(cfg, time.Time{}, 0, repository, nil, nil)
	manual := base64.RawStdEncoding.EncodeToString(make([]byte, 70))
	input := service.Get().Config
	input.ProviderWeb.StatsigMode = config.StatsigModeManual
	input.ProviderWeb.StatsigManualValue = manual

	snapshot, err := service.Update(context.Background(), service.Get().Revision, input)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Config.ProviderWeb.StatsigManualValue != "" || !snapshot.Config.ProviderWeb.StatsigManualConfigured {
		t.Fatalf("manual value leaked in snapshot: %#v", snapshot.Config.ProviderWeb)
	}
	if repository.value.ProviderWeb.StatsigManualValue != manual {
		t.Fatal("manual value was not persisted")
	}

	keep := service.Get().Config
	if _, err := service.Update(context.Background(), service.Get().Revision, keep); err != nil {
		t.Fatalf("blank write-only value did not preserve existing value: %v", err)
	}
	if repository.value.ProviderWeb.StatsigManualValue != manual {
		t.Fatal("blank write-only value cleared the existing manual value")
	}

	urlMode := service.Get().Config
	urlMode.ProviderWeb.StatsigMode = config.StatsigModeURL
	if _, err := service.Update(context.Background(), service.Get().Revision, urlMode); err != nil {
		t.Fatal(err)
	}
	if repository.value.ProviderWeb.StatsigManualValue != "" {
		t.Fatal("URL mode retained the manual x-statsig-id")
	}
}

func TestLoadPersistedRejectsIncompleteStatsigPayload(t *testing.T) {
	cfg := testConfig(t)
	value := toDomainConfig(cfg)
	value.ProviderWeb.StatsigMode = ""
	value.ProviderWeb.StatsigSignerURL = ""
	repository := &runtimeSettingsRepositoryStub{value: value, found: true}
	if _, _, _, err := LoadPersisted(context.Background(), cfg, repository); err == nil {
		t.Fatal("incomplete Statsig settings were accepted")
	}
}

func TestLoadPersistedRejectsIncompleteBatchPayload(t *testing.T) {
	cfg := testConfig(t)
	value := toDomainConfig(cfg)
	value.Batch = settingsdomain.BatchConfig{}
	repository := &runtimeSettingsRepositoryStub{value: value, found: true}
	if _, _, _, err := LoadPersisted(context.Background(), cfg, repository); err == nil {
		t.Fatal("incomplete batch settings were accepted")
	}
}

func TestReloadPersistedAppliesOnlyNewerVersion(t *testing.T) {
	cfg := testConfig(t)
	updatedAt := time.Now().UTC()
	repository := &runtimeSettingsRepositoryStub{value: toDomainConfig(cfg), updatedAt: updatedAt, revision: 1, found: true}
	applyCount := 0
	service := NewService(cfg, updatedAt, 1, repository, nil, func(config.Config) { applyCount++ })

	if err := service.ReloadPersisted(context.Background()); err != nil {
		t.Fatal(err)
	}
	if applyCount != 0 {
		t.Fatalf("unchanged settings applied %d times", applyCount)
	}
	repository.value.Routing.MaxAttempts = cfg.Routing.MaxAttempts + 1
	repository.updatedAt = updatedAt.Add(time.Second)
	repository.revision = 2
	if err := service.ReloadPersisted(context.Background()); err != nil {
		t.Fatal(err)
	}
	if applyCount != 1 || service.Get().Config.Routing.MaxAttempts != cfg.Routing.MaxAttempts+1 {
		t.Fatalf("newer settings were not applied")
	}
}

func TestUpdateRejectsStaleRevision(t *testing.T) {
	cfg := testConfig(t)
	repository := &runtimeSettingsRepositoryStub{}
	service := NewService(cfg, time.Time{}, 0, repository, nil, nil)
	input := service.Get().Config
	input.Routing.MaxAttempts++
	if _, err := service.Update(context.Background(), 1, input); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale revision error = %v", err)
	}
}

func testConfig(t *testing.T) config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`secrets:
  jwtSecret: "12345678901234567890123456789012"
  credentialEncryptionKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
