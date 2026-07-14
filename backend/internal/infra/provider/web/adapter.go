package web

import (
	"context"
	"log/slog"
	"sync"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type Config struct {
	BaseURL             string
	StatsigMode         string
	StatsigManualValue  string
	StatsigSignerURL    string
	QuotaTimeoutSeconds int
	ChatTimeoutSeconds  int
	ImageTimeoutSeconds int
	VideoTimeoutSeconds int
	MaxInputImageBytes  int64
	AllowNSFW           bool
}

type Adapter struct {
	mu      sync.RWMutex
	cfg     Config
	egress  *infraegress.Manager
	cipher  *security.Cipher
	states  repository.ResponseRepository
	assets  provider.ImageAssetStore
	statsig *statsigSigner
	logger  *slog.Logger
}

func NewAdapter(cfg Config, egress *infraegress.Manager, cipher *security.Cipher, states repository.ResponseRepository, assets provider.ImageAssetStore) *Adapter {
	cfg = normalizedConfig(cfg)
	return &Adapter{cfg: cfg, egress: egress, cipher: cipher, states: states, assets: assets, statsig: newStatsigSigner(), logger: slog.Default()}
}

func (a *Adapter) SetLogger(logger *slog.Logger) {
	if logger != nil {
		a.logger = logger
	}
}

func (a *Adapter) log() *slog.Logger {
	if a.logger != nil {
		return a.logger
	}
	return slog.Default()
}

func normalizedConfig(cfg Config) Config {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://grok.com"
	}
	if cfg.StatsigMode == "" {
		cfg.StatsigMode = "url"
	}
	if cfg.StatsigSignerURL == "" {
		cfg.StatsigSignerURL = defaultStatsigSignerURL
	}
	if cfg.QuotaTimeoutSeconds <= 0 {
		cfg.QuotaTimeoutSeconds = 25
	}
	if cfg.ChatTimeoutSeconds <= 0 {
		cfg.ChatTimeoutSeconds = 120
	}
	if cfg.ImageTimeoutSeconds <= 0 {
		cfg.ImageTimeoutSeconds = 180
	}
	if cfg.VideoTimeoutSeconds <= 0 {
		cfg.VideoTimeoutSeconds = 900
	}
	if cfg.MaxInputImageBytes <= 0 {
		cfg.MaxInputImageBytes = 32 << 20
	}
	return cfg
}

func (a *Adapter) UpdateConfig(cfg Config) {
	cfg = normalizedConfig(cfg)
	a.mu.Lock()
	changed := a.cfg.StatsigMode != cfg.StatsigMode || a.cfg.StatsigManualValue != cfg.StatsigManualValue || a.cfg.StatsigSignerURL != cfg.StatsigSignerURL || a.cfg.BaseURL != cfg.BaseURL
	a.cfg = cfg
	a.mu.Unlock()
	if changed && a.statsig != nil {
		a.statsig.Clear()
	}
}

func (a *Adapter) config() Config {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg
}

func (a *Adapter) Provider() account.Provider { return account.ProviderWeb }

func (a *Adapter) QuotaMode(upstreamModel string) string {
	if spec, ok := Resolve(upstreamModel); ok {
		return spec.Mode
	}
	return ""
}

func (a *Adapter) TierOrder(upstreamModel string) []account.WebTier {
	spec, ok := Resolve(upstreamModel)
	if !ok {
		return nil
	}
	switch spec.MinimumTier {
	case account.WebTierHeavy:
		return []account.WebTier{account.WebTierHeavy}
	case account.WebTierSuper:
		return []account.WebTier{account.WebTierSuper, account.WebTierHeavy}
	default:
		return []account.WebTier{account.WebTierBasic, account.WebTierSuper, account.WebTierHeavy}
	}
}

func (a *Adapter) PricingModel(upstreamModel string) string {
	spec, ok := Resolve(upstreamModel)
	if ok {
		if spec.Capability == modeldomain.CapabilityChat {
			return "grok-4.5"
		}
		return spec.PublicID
	}
	return upstreamModel
}

func (a *Adapter) ListModels(_ context.Context, credential account.Credential) ([]string, error) {
	tier := credential.WebTier
	if tier == "" || tier == account.WebTierAuto || tier == account.WebTierBasic {
		tier = account.WebTierBasic
	}
	values := make([]string, 0, len(catalog))
	for _, spec := range catalog {
		if TierSupports(tier, spec.MinimumTier) {
			values = append(values, spec.UpstreamModel)
		}
	}
	return values, nil
}
