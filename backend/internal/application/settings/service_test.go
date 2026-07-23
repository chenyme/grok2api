package settings

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
	input.Server.MaxConcurrentRequests = 2048
	input.ProviderBuild.ResponseHeaderTimeout = "7m"
	input.Routing.MaxAttempts = 5
	input.Routing.PreferFreeBuild = true
	input.Routing.SegmentedSelector = SegmentedSelectorConfig{Enabled: true, MinCandidates: 5000, WindowSize: 96}
	input.Audit.BufferSize = cfg.Audit.BufferSize + 1
	input.Media.MaxTotalBytes = 2 << 30
	input.Media.CleanupThresholdPercent = 75
	input.Media.CleanupInterval = "5m"
	input.Frontend.PublicAPIBaseURL = "https://public.example.com"
	input.ProviderConsole.BaseURL = "https://console.example.com"
	input.ProviderConsole.ChatTimeout = "6m"
	input.Batch = BatchConfig{ImportConcurrency: 26, ConversionConcurrency: 27, SyncConcurrency: 28, RefreshConcurrency: 29, RandomDelay: "750ms"}

	snapshot, err := service.Update(context.Background(), service.Get().Revision, input)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Routing.MaxAttempts != 5 || !applied.Routing.PreferFreeBuild || !applied.Routing.SegmentedSelectorEnabled || applied.Routing.SegmentedMinCandidates != 5000 || applied.Routing.SegmentedWindowSize != 96 {
		t.Fatalf("runtime configuration was not applied: %#v", applied.Routing)
	}
	if applied.Server.MaxConcurrentRequests != 2048 {
		t.Fatalf("server configuration was not applied: %#v", applied.Server)
	}
	if applied.Provider.Build.ResponseHeaderTimeout.Value() != 7*time.Minute {
		t.Fatalf("Build response header timeout was not applied: %s", applied.Provider.Build.ResponseHeaderTimeout.Value())
	}
	if applied.Media.MaxTotalBytes != 2<<30 || applied.Media.CleanupThresholdPercent != 75 || applied.Media.CleanupInterval.Value() != 5*time.Minute {
		t.Fatalf("media configuration was not applied: %#v", applied.Media)
	}
	if applied.Frontend.PublicAPIBaseURLOverride != "https://public.example.com" || applied.Frontend.EffectivePublicAPIBaseURL() != "https://public.example.com" {
		t.Fatalf("frontend configuration was not applied: %#v", applied.Frontend)
	}
	if applied.Batch.ImportConcurrency != 26 || applied.Batch.ConversionConcurrency != 27 || applied.Batch.SyncConcurrency != 28 || applied.Batch.RefreshConcurrency != 29 || applied.Batch.RandomDelay.Value() != 750*time.Millisecond {
		t.Fatalf("batch configuration was not applied: %#v", applied.Batch)
	}
	if applied.Provider.Console.BaseURL != "https://console.example.com" || applied.Provider.Console.ChatTimeout.Value() != 6*time.Minute {
		t.Fatalf("console configuration was not applied: %#v", applied.Provider.Console)
	}
	if len(snapshot.RestartRequired) != 1 || snapshot.RestartRequired[0] != "audit.bufferSize" {
		t.Fatalf("restartRequired = %#v", snapshot.RestartRequired)
	}
	reloaded, _, _, err := LoadPersisted(context.Background(), cfg, repository)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Server.MaxConcurrentRequests != 2048 || reloaded.Provider.Build.ResponseHeaderTimeout.Value() != 7*time.Minute || reloaded.Routing.MaxAttempts != 5 || !reloaded.Routing.PreferFreeBuild || !reloaded.Routing.SegmentedSelectorEnabled || reloaded.Routing.SegmentedMinCandidates != 5000 || reloaded.Routing.SegmentedWindowSize != 96 || reloaded.Audit.BufferSize != input.Audit.BufferSize || reloaded.Media.MaxTotalBytes != 2<<30 || reloaded.Media.CleanupThresholdPercent != 75 || reloaded.Batch.SyncConcurrency != 28 || reloaded.Batch.RandomDelay.Value() != 750*time.Millisecond || reloaded.Provider.Console.BaseURL != "https://console.example.com" {
		t.Fatalf("configuration was not persisted")
	}
}

func TestUpdateRejectsBuildResponseHeaderTimeoutOutsideSafeRange(t *testing.T) {
	for _, value := range []string{"29s", "31m"} {
		t.Run(value, func(t *testing.T) {
			cfg := testConfig(t)
			repository := &runtimeSettingsRepositoryStub{}
			service := NewService(cfg, time.Time{}, 0, repository, nil, nil)
			input := service.Get().Config
			input.ProviderBuild.ResponseHeaderTimeout = value
			if _, err := service.Update(context.Background(), 0, input); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("error = %v", err)
			}
			if repository.found {
				t.Fatal("invalid Build response header timeout was persisted")
			}
		})
	}
}

func TestLoadPersistedKeepsSegmentedSelectorDefaultsForOlderPayload(t *testing.T) {
	cfg := testConfig(t)
	cfg.Routing.SegmentedSelectorEnabled = true
	cfg.Routing.SegmentedMinCandidates = 4321
	cfg.Routing.SegmentedWindowSize = 72
	value := toDomainConfig(cfg)
	value.Routing.SegmentedSelector = nil
	repository := &runtimeSettingsRepositoryStub{value: value, found: true}
	loaded, _, _, err := LoadPersisted(context.Background(), cfg, repository)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Routing.SegmentedSelectorEnabled || loaded.Routing.SegmentedMinCandidates != 4321 || loaded.Routing.SegmentedWindowSize != 72 {
		t.Fatalf("segmented selector defaults were not preserved: %#v", loaded.Routing)
	}
}

func TestLoadPersistedPreservesExplicitlyDisabledSegmentedSelector(t *testing.T) {
	cfg := testConfig(t)
	cfg.Routing.SegmentedSelectorEnabled = true
	value := toDomainConfig(cfg)
	value.Routing.SegmentedSelector = &settingsdomain.SegmentedSelectorConfig{
		ActiveEnabled: false, MinCandidates: 6000, WindowSize: 128,
	}
	repository := &runtimeSettingsRepositoryStub{value: value, found: true}
	loaded, _, _, err := LoadPersisted(context.Background(), cfg, repository)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Routing.SegmentedSelectorEnabled || loaded.Routing.SegmentedMinCandidates != 6000 || loaded.Routing.SegmentedWindowSize != 128 {
		t.Fatalf("explicit segmented selector settings were not preserved: %#v", loaded.Routing)
	}
}

func TestLegacyShadowSettingCannotEnableSegmentedSelector(t *testing.T) {
	var persisted settingsdomain.Config
	if err := json.Unmarshal([]byte(`{"Routing":{"SegmentedSelector":{"ActiveEnabled":false,"Enabled":true,"MinCandidates":3000,"WindowSize":64,"SamplePercent":100}}}`), &persisted); err != nil {
		t.Fatal(err)
	}
	loaded := applyDomainConfig(testConfig(t), persisted)
	if loaded.Routing.SegmentedSelectorEnabled {
		t.Fatal("legacy shadow-only setting enabled the authoritative segmented selector")
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

func TestLoadPersistedKeepsClearanceDefaultsForOlderPayload(t *testing.T) {
	cfg := testConfig(t)
	value := toDomainConfig(cfg)
	value.ProviderWeb.ClearanceMode = ""
	value.ProviderWeb.FlareSolverrURL = ""
	value.ProviderWeb.ClearanceTimeout = 0
	value.ProviderWeb.ClearanceRefresh = 0
	repository := &runtimeSettingsRepositoryStub{value: value, found: true}
	loaded, _, _, err := LoadPersisted(context.Background(), cfg, repository)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Provider.Web.ClearanceMode != cfg.Provider.Web.ClearanceMode || loaded.Provider.Web.FlareSolverrURL != cfg.Provider.Web.FlareSolverrURL || loaded.Provider.Web.ClearanceTimeout != cfg.Provider.Web.ClearanceTimeout || loaded.Provider.Web.ClearanceRefresh != cfg.Provider.Web.ClearanceRefresh {
		t.Fatalf("clearance config = %#v, want %#v", loaded.Provider.Web, cfg.Provider.Web)
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

func TestLoadPersistedBackfillsMissingServerConcurrency(t *testing.T) {
	cfg := testConfig(t)
	value := toDomainConfig(cfg)
	value.Server = settingsdomain.ServerConfig{}
	repository := &runtimeSettingsRepositoryStub{value: value, found: true}

	loaded, _, _, err := LoadPersisted(context.Background(), cfg, repository)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Server.MaxConcurrentRequests != cfg.Server.MaxConcurrentRequests {
		t.Fatalf("maxConcurrentRequests = %d, want %d", loaded.Server.MaxConcurrentRequests, cfg.Server.MaxConcurrentRequests)
	}
}

func TestLoadPersistedBackfillsMissingConsoleSection(t *testing.T) {
	cfg := testConfig(t)
	value := toDomainConfig(cfg)
	value.ProviderConsole = settingsdomain.ProviderConsoleConfig{}
	repository := &runtimeSettingsRepositoryStub{value: value, found: true}

	loaded, _, _, err := LoadPersisted(context.Background(), cfg, repository)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Provider.Console != cfg.Provider.Console {
		t.Fatalf("console = %#v, want %#v", loaded.Provider.Console, cfg.Provider.Console)
	}
}

func TestLoadPersistedRejectsPartiallyInvalidConsoleSection(t *testing.T) {
	cfg := testConfig(t)
	value := toDomainConfig(cfg)
	value.ProviderConsole.BaseURL = ""
	repository := &runtimeSettingsRepositoryStub{value: value, found: true}

	if _, _, _, err := LoadPersisted(context.Background(), cfg, repository); err == nil {
		t.Fatal("partially invalid Console settings were accepted")
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

func TestLoadPersistedKeepsYAMLFrontendWhenUnset(t *testing.T) {
	cfg := testConfig(t)
	cfg.Frontend.PublicAPIBaseURL = "http://yaml.example.com"
	value := toDomainConfig(cfg)
	value.Frontend = settingsdomain.FrontendConfig{}
	repository := &runtimeSettingsRepositoryStub{
		value: value,
		found: true, revision: 1, updatedAt: time.Now().UTC(),
	}
	loaded, _, _, err := LoadPersisted(context.Background(), cfg, repository)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Frontend.PublicAPIBaseURL != "http://yaml.example.com" || loaded.Frontend.PublicAPIBaseURLOverride != "" || loaded.Frontend.EffectivePublicAPIBaseURL() != "http://yaml.example.com" {
		t.Fatalf("frontend = %#v", loaded.Frontend)
	}
}

func TestUpdateEmptyFrontendOverrideFallsBackToYAML(t *testing.T) {
	cfg := testConfig(t)
	cfg.Frontend.PublicAPIBaseURL = "http://yaml.example.com"
	cfg.Frontend.PublicAPIBaseURLOverride = "http://runtime.example.com"
	repository := &runtimeSettingsRepositoryStub{}
	var applied config.Config
	service := NewService(cfg, time.Time{}, 0, repository, nil, func(next config.Config) { applied = next })
	input := service.Get().Config
	input.Frontend.PublicAPIBaseURL = ""
	if _, err := service.Update(context.Background(), 0, input); err != nil {
		t.Fatal(err)
	}
	if applied.Frontend.PublicAPIBaseURLOverride != "" || applied.Frontend.EffectivePublicAPIBaseURL() != "http://yaml.example.com" {
		t.Fatalf("frontend fallback = %#v", applied.Frontend)
	}
	if repository.value.Frontend.PublicAPIBaseURL != "" {
		t.Fatalf("persisted override = %q", repository.value.Frontend.PublicAPIBaseURL)
	}
}

func TestApplyDomainConfigAccountsDefaults(t *testing.T) {
	base := testConfig(t)
	// 旧持久化 JSON 无 Accounts 段时，应保持代码默认：关闭 + 10m + 1h。
	loaded := applyDomainConfig(base, settingsdomain.Config{
		Server: settingsdomain.ServerConfig{MaxConcurrentRequests: base.Server.MaxConcurrentRequests},
		ProviderBuild: settingsdomain.ProviderBuildConfig{
			BaseURL: base.Provider.Build.BaseURL, FallbackBaseURL: base.Provider.Build.FallbackBaseURL,
			ClientVersion: base.Provider.Build.ClientVersion, ClientIdentifier: base.Provider.Build.ClientIdentifier,
			TokenAuth: base.Provider.Build.TokenAuth, UserAgent: base.Provider.Build.UserAgent,
		},
		ProviderWeb: settingsdomain.ProviderWebConfig{
			BaseURL: base.Provider.Web.BaseURL, QuotaTimeout: base.Provider.Web.QuotaTimeout.Value(),
			StatsigMode: base.Provider.Web.StatsigMode, StatsigManualValue: base.Provider.Web.StatsigManualValue,
			StatsigSignerURL: base.Provider.Web.StatsigSignerURL,
			ChatTimeout:      base.Provider.Web.ChatTimeout.Value(), ImageTimeout: base.Provider.Web.ImageTimeout.Value(),
			VideoTimeout: base.Provider.Web.VideoTimeout.Value(), MediaConcurrency: base.Provider.Web.MediaConcurrency,
			AllowNSFW:           base.Provider.Web.AllowNSFW,
			RecoveryBackoffBase: base.Provider.Web.RecoveryBackoffBase.Value(), RecoveryBackoffMax: base.Provider.Web.RecoveryBackoffMax.Value(),
		},
		Batch: settingsdomain.BatchConfig{
			ImportConcurrency: base.Batch.ImportConcurrency, ConversionConcurrency: base.Batch.ConversionConcurrency,
			SyncConcurrency: base.Batch.SyncConcurrency, RefreshConcurrency: base.Batch.RefreshConcurrency,
			RandomDelay: func() *time.Duration { d := base.Batch.RandomDelay.Value(); return &d }(),
		},
		Media: settingsdomain.MediaConfig{
			MaxImageBytes: base.Media.MaxImageBytes, MaxTotalBytes: base.Media.MaxTotalBytes,
			CleanupThresholdPercent: base.Media.CleanupThresholdPercent, CleanupInterval: base.Media.CleanupInterval.Value(),
		},
		Routing: settingsdomain.RoutingConfig{
			StickyTTL: base.Routing.StickyTTL.Value(), CooldownBase: base.Routing.CooldownBase.Value(),
			CooldownMax: base.Routing.CooldownMax.Value(), CapacityWait: base.Routing.CapacityWait.Value(),
			MaxAttempts: base.Routing.MaxAttempts, PreferFreeBuild: base.Routing.PreferFreeBuild,
		},
		Audit: settingsdomain.AuditConfig{
			BufferSize: base.Audit.BufferSize, BatchSize: base.Audit.BatchSize, FlushInterval: base.Audit.FlushInterval.Value(),
		},
		ClientKeyDefaults: settingsdomain.ClientKeyDefaultsConfig{
			RPMLimit: base.ClientKeyDefaults.RPMLimit, MaxConcurrent: base.ClientKeyDefaults.MaxConcurrent,
		},
	})
	if loaded.Accounts.AutoCleanReauthEnabled || loaded.Accounts.AutoCleanIncludeDisabled {
		t.Fatalf("accounts flags should stay false: %#v", loaded.Accounts)
	}
	if loaded.Accounts.AutoCleanReauthInterval.Value() != 10*time.Minute || loaded.Accounts.AutoCleanReauthMinAge.Value() != time.Hour {
		t.Fatalf("accounts defaults = %#v", loaded.Accounts)
	}
	if loaded.Audit.CommitDelay.Value() != base.Audit.CommitDelay.Value() {
		t.Fatalf("audit commit delay = %s", loaded.Audit.CommitDelay.Value())
	}
}

func TestUpdateAuditCommitDelayRoundTrip(t *testing.T) {
	cfg := testConfig(t)
	repo := &runtimeSettingsRepositoryStub{}
	var applied config.Config
	service := NewService(cfg, time.Time{}, 0, repo, nil, func(next config.Config) { applied = next })
	input := service.Get().Config
	input.Audit.CommitDelayMS = 12
	snapshot, err := service.Update(context.Background(), service.Get().Revision, input)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Audit.CommitDelay.Value() != 12*time.Millisecond || snapshot.Config.Audit.CommitDelayMS != 12 {
		t.Fatalf("applied=%s snapshot=%d", applied.Audit.CommitDelay.Value(), snapshot.Config.Audit.CommitDelayMS)
	}
}

func TestUpdateRejectsNegativeAuditCommitDelay(t *testing.T) {
	cfg := testConfig(t)
	service := NewService(cfg, time.Time{}, 0, &runtimeSettingsRepositoryStub{}, nil, nil)
	input := service.Get().Config
	input.Audit.CommitDelayMS = -1
	if _, err := service.Update(context.Background(), service.Get().Revision, input); err == nil {
		t.Fatal("negative audit commit delay was accepted")
	}
}

func TestUpdateAccountsAutoCleanRoundTrip(t *testing.T) {
	cfg := testConfig(t)
	repo := &runtimeSettingsRepositoryStub{}
	var applied config.Config
	service := NewService(cfg, time.Time{}, 0, repo, nil, func(next config.Config) { applied = next })
	input := service.Get().Config
	input.Accounts = AccountsConfig{
		MarkBuildForbiddenReauth: true, MarkBuildForbiddenReauthProvided: true,
		BuildForbiddenReauthCodes: []string{" Permission-Denied ", "permission-denied", "team-access-denied"}, BuildForbiddenReauthCodesProvided: true,
		AutoCleanReauthEnabled: true, AutoCleanReauthInterval: "5m",
		AutoCleanReauthMinAge: "2h", AutoCleanIncludeDisabled: true,
	}
	snapshot, err := service.Update(context.Background(), service.Get().Revision, input)
	if err != nil {
		t.Fatal(err)
	}
	if !applied.Accounts.MarkBuildForbiddenReauth || !applied.Accounts.AutoCleanReauthEnabled || !applied.Accounts.AutoCleanIncludeDisabled {
		t.Fatalf("applied accounts = %#v", applied.Accounts)
	}
	if len(applied.Accounts.BuildForbiddenReauthCodes) != 2 || applied.Accounts.BuildForbiddenReauthCodes[0] != "permission-denied" || applied.Accounts.BuildForbiddenReauthCodes[1] != "team-access-denied" {
		t.Fatalf("normalized Build forbidden codes = %#v", applied.Accounts.BuildForbiddenReauthCodes)
	}
	if applied.Accounts.AutoCleanReauthInterval.Value() != 5*time.Minute || applied.Accounts.AutoCleanReauthMinAge.Value() != 2*time.Hour {
		t.Fatalf("applied accounts durations = %#v", applied.Accounts)
	}
	if !snapshot.Config.Accounts.MarkBuildForbiddenReauth || !snapshot.Config.Accounts.AutoCleanReauthEnabled || snapshot.Config.Accounts.AutoCleanReauthInterval != "5m" {
		t.Fatalf("snapshot accounts = %#v", snapshot.Config.Accounts)
	}
}

func TestUpdateWithoutAccountsPreservesCurrentAutoCleanConfig(t *testing.T) {
	cfg := testConfig(t)
	cfg.Accounts.AutoCleanReauthEnabled = true
	cfg.Accounts.MarkBuildForbiddenReauth = true
	cfg.Accounts.BuildForbiddenReauthCodes = []string{"custom-denial"}
	cfg.Accounts.AutoCleanReauthInterval = config.Duration(7 * time.Minute)
	cfg.Accounts.AutoCleanReauthMinAge = config.Duration(3 * time.Hour)
	cfg.Accounts.AutoCleanIncludeDisabled = true
	repo := &runtimeSettingsRepositoryStub{}
	var applied config.Config
	service := NewService(cfg, time.Time{}, 0, repo, nil, func(next config.Config) { applied = next })
	input := service.Get().Config
	input.Accounts = AccountsConfig{}
	input.AccountsProvided = false
	input.Server.MaxConcurrentRequests++
	if _, err := service.Update(context.Background(), 0, input); err != nil {
		t.Fatal(err)
	}
	if !applied.Accounts.MarkBuildForbiddenReauth || !applied.Accounts.AutoCleanReauthEnabled || !applied.Accounts.AutoCleanIncludeDisabled || applied.Accounts.AutoCleanReauthInterval.Value() != 7*time.Minute || applied.Accounts.AutoCleanReauthMinAge.Value() != 3*time.Hour {
		t.Fatalf("accounts changed by legacy update: %#v", applied.Accounts)
	}
}

func TestUpdateWithoutBuildForbiddenFieldPreservesCurrentPolicy(t *testing.T) {
	cfg := testConfig(t)
	cfg.Accounts.MarkBuildForbiddenReauth = true
	cfg.Accounts.BuildForbiddenReauthCodes = []string{"custom-denial"}
	var applied config.Config
	service := NewService(cfg, time.Time{}, 0, &runtimeSettingsRepositoryStub{}, nil, func(next config.Config) { applied = next })
	input := service.Get().Config
	input.Accounts.MarkBuildForbiddenReauth = false
	input.Accounts.MarkBuildForbiddenReauthProvided = false
	input.Accounts.BuildForbiddenReauthCodes = nil
	input.Accounts.BuildForbiddenReauthCodesProvided = false
	input.Server.MaxConcurrentRequests++
	if _, err := service.Update(context.Background(), 0, input); err != nil {
		t.Fatal(err)
	}
	if !applied.Accounts.MarkBuildForbiddenReauth || len(applied.Accounts.BuildForbiddenReauthCodes) != 1 || applied.Accounts.BuildForbiddenReauthCodes[0] != "custom-denial" {
		t.Fatalf("legacy update changed the Build forbidden-account policy: %#v", applied.Accounts)
	}
}

func TestLoadPersistedKeepsDefaultBuildForbiddenCodesForOlderPayload(t *testing.T) {
	cfg := testConfig(t)
	value := toDomainConfig(cfg)
	value.Accounts.BuildForbiddenReauthCodes = nil
	repository := &runtimeSettingsRepositoryStub{value: value, found: true}
	loaded, _, _, err := LoadPersisted(context.Background(), cfg, repository)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Accounts.BuildForbiddenReauthCodes) != 1 || loaded.Accounts.BuildForbiddenReauthCodes[0] != "permission-denied" {
		t.Fatalf("Build forbidden code defaults were not preserved: %#v", loaded.Accounts.BuildForbiddenReauthCodes)
	}
}

func TestUpdateRejectsInvalidBuildForbiddenCodes(t *testing.T) {
	for _, codes := range [][]string{{}, {"contains spaces"}, func() []string {
		values := make([]string, 33)
		for index := range values {
			values[index] = fmt.Sprintf("denial-%d", index)
		}
		return values
	}()} {
		cfg := testConfig(t)
		repository := &runtimeSettingsRepositoryStub{}
		service := NewService(cfg, time.Time{}, 0, repository, nil, nil)
		input := service.Get().Config
		input.Accounts.BuildForbiddenReauthCodes = codes
		input.Accounts.BuildForbiddenReauthCodesProvided = true
		if _, err := service.Update(context.Background(), 0, input); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("codes %#v: error = %v", codes, err)
		}
		if repository.found {
			t.Fatalf("invalid codes were persisted: %#v", codes)
		}
	}
}
