package clientkey

import (
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

const (
	DefaultRPMLimit      = 120
	DefaultMaxConcurrent = 8
	MaxRPMLimit          = 100000
	MaxConcurrent        = 1024
	MaxBillingLimitTicks = 9_000_000_000_000_000
)

const InternalKindQualityGuard = "quality_guard"

type ProviderScope uint8

const (
	ProviderScopeBuild ProviderScope = 1 << iota
	ProviderScopeWeb
	ProviderScopeConsole
	ProviderScopeAll = ProviderScopeBuild | ProviderScopeWeb | ProviderScopeConsole
)

type TierScope uint8

const (
	TierScopeFree TierScope = 1 << iota
	TierScopeSuper
	TierScopeUnknown
	TierScopeAll = TierScopeFree | TierScopeSuper | TierScopeUnknown
)

type AccountTier uint8

const (
	AccountTierFree AccountTier = iota + 1
	AccountTierSuper
	AccountTierUnknown
)

type AccountScope struct {
	Providers ProviderScope
	Tiers     TierScope
}

func ParseProviderScopeValues(values []string) (ProviderScope, bool) {
	if len(values) == 0 {
		return 0, false
	}
	var scope ProviderScope
	for _, value := range values {
		switch value {
		case "all":
			if len(values) != 1 {
				return 0, false
			}
			return ProviderScopeAll, true
		case string(account.ProviderBuild):
			scope |= ProviderScopeBuild
		case string(account.ProviderWeb):
			scope |= ProviderScopeWeb
		case string(account.ProviderConsole):
			scope |= ProviderScopeConsole
		default:
			return 0, false
		}
	}
	return NormalizeProviderScope(scope)
}

func ParseTierScopeValues(values []string) (TierScope, bool) {
	if len(values) == 0 {
		return 0, false
	}
	var scope TierScope
	for _, value := range values {
		switch value {
		case "all":
			if len(values) != 1 {
				return 0, false
			}
			return TierScopeAll, true
		case "free":
			scope |= TierScopeFree
		case "super":
			scope |= TierScopeSuper
		default:
			return 0, false
		}
	}
	return NormalizeTierScope(scope)
}

func (s ProviderScope) Values() []string {
	value, valid := NormalizeProviderScope(s)
	if !valid || value == ProviderScopeAll {
		return []string{"all"}
	}
	values := make([]string, 0, 3)
	if value&ProviderScopeBuild != 0 {
		values = append(values, string(account.ProviderBuild))
	}
	if value&ProviderScopeWeb != 0 {
		values = append(values, string(account.ProviderWeb))
	}
	if value&ProviderScopeConsole != 0 {
		values = append(values, string(account.ProviderConsole))
	}
	return values
}

func (s TierScope) Values() []string {
	value, valid := NormalizeTierScope(s)
	if !valid || value == TierScopeAll {
		return []string{"all"}
	}
	values := make([]string, 0, 2)
	if value&TierScopeFree != 0 {
		values = append(values, "free")
	}
	if value&TierScopeSuper != 0 {
		values = append(values, "super")
	}
	return values
}

func NormalizeProviderScope(value ProviderScope) (ProviderScope, bool) {
	if value == 0 {
		return ProviderScopeAll, true
	}
	if value&^ProviderScopeAll != 0 {
		return value, false
	}
	return value, true
}

func NormalizeTierScope(value TierScope) (TierScope, bool) {
	if value == 0 {
		return TierScopeAll, true
	}
	switch value {
	case TierScopeFree, TierScopeSuper, TierScopeFree | TierScopeSuper, TierScopeAll:
		return value, true
	default:
		return value, false
	}
}

func NormalizeAccountScope(value AccountScope) (AccountScope, bool) {
	providers, providersValid := NormalizeProviderScope(value.Providers)
	tiers, tiersValid := NormalizeTierScope(value.Tiers)
	return AccountScope{Providers: providers, Tiers: tiers}, providersValid && tiersValid
}

func (s ProviderScope) Allows(provider account.Provider) bool {
	value, valid := NormalizeProviderScope(s)
	if !valid {
		return false
	}
	switch provider {
	case account.ProviderBuild:
		return value&ProviderScopeBuild != 0
	case account.ProviderWeb:
		return value&ProviderScopeWeb != 0
	case account.ProviderConsole:
		return value&ProviderScopeConsole != 0
	default:
		return false
	}
}

func (s TierScope) Allows(tier AccountTier) bool {
	value, valid := NormalizeTierScope(s)
	if !valid {
		return false
	}
	switch tier {
	case AccountTierFree:
		return value&TierScopeFree != 0
	case AccountTierSuper:
		return value&TierScopeSuper != 0
	case AccountTierUnknown:
		return value&TierScopeUnknown != 0
	default:
		return false
	}
}

func (s AccountScope) AllowsProvider(provider account.Provider) bool {
	value, valid := NormalizeAccountScope(s)
	return valid && value.Providers.Allows(provider)
}

// AllowsAccount applies provider restrictions to every channel while tier
// restrictions apply only to channels with a reliable tier classification.
func (s AccountScope) AllowsAccount(provider account.Provider, tier AccountTier) bool {
	value, valid := NormalizeAccountScope(s)
	if !valid || !value.Providers.Allows(provider) {
		return false
	}
	if provider == account.ProviderConsole {
		return true
	}
	return value.Tiers.Allows(tier)
}

func (s AccountScope) IsRestricted() bool {
	value, valid := NormalizeAccountScope(s)
	return valid && (value.Providers != ProviderScopeAll || value.Tiers != TierScopeAll)
}

func (k Key) AccountScope() AccountScope {
	value, _ := NormalizeAccountScope(AccountScope{Providers: k.ProviderScope, Tiers: k.TierScope})
	return value
}

// Key 表示下游客户端调用凭据及其限制。
type Key struct {
	ID              uint64
	Name            string
	Prefix          string
	SecretHash      string
	EncryptedSecret string
	// InternalKind marks server-managed identities that must never be exposed
	// through the public client-key lifecycle or accepted by external auth.
	InternalKind          string
	Enabled               bool
	ExpiresAt             *time.Time
	RPMLimit              int
	MaxConcurrent         int
	BillingLimitUSDTicks  int64
	BilledUsageUSDTicks   int64
	ReservedUsageUSDTicks int64
	// AllowModelAliases enables discovery and use of dynamically generated reasoning-effort aliases.
	// Registered compatibility aliases remain available to avoid breaking existing clients.
	AllowModelAliases bool
	AllowedModels     []uint64
	// ProviderScope and TierScope narrow routing without adding request-time storage lookups.
	ProviderScope ProviderScope
	TierScope     TierScope
	LastUsedAt    *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// IsAvailable 判断客户端 Key 当前是否可用。
func (k Key) IsAvailable(now time.Time) bool {
	if !k.Enabled {
		return false
	}
	return k.ExpiresAt == nil || now.Before(*k.ExpiresAt)
}

func (k Key) AllowsModel(modelID uint64) bool {
	if len(k.AllowedModels) == 0 {
		return true
	}
	for _, allowed := range k.AllowedModels {
		if allowed == modelID {
			return true
		}
	}
	return false
}
