package egress

import "time"

type Mode string

const (
	ModeDirect Mode = "direct"
	ModeSingle Mode = "single"
	ModePool   Mode = "pool"
)

type Scope string

const (
	ScopeBuild    Scope = "grok_build"
	ScopeWeb      Scope = "grok_web"
	ScopeConsole  Scope = "grok_console"
	ScopeWebAsset Scope = "grok_web_asset"
)

type Node struct {
	ID                          uint64
	Name                        string
	Scope                       Scope
	Enabled                     bool
	ProxyPool                   bool
	SourceID                    uint64
	SourceKey                   string
	AccountCapacity             int
	EncryptedProxyURL           string
	UserAgent                   string
	EncryptedCloudflareCookie   string
	ClearanceRefreshedAt        *time.Time
	ClearanceFingerprint        string
	ClearanceBindingFingerprint string
	Health                      float64
	FailureCount                int
	CooldownUntil               *time.Time
	LastError                   string
	ProbeStatus                 ProbeStatus
	LastProbedAt                *time.Time
	ProbeLatencyMS              int
	ExitIP                      string
	ExitCountry                 string
	ProbeError                  string
	ProbeProvider               ProbeProvider
	IPv4Probe                   ProbeFamilyResult
	IPv6Probe                   ProbeFamilyResult
	AssignedAccountCount        int
	CreatedAt                   time.Time
	UpdatedAt                   time.Time
}

type PublicNode struct {
	ID                   uint64
	Name                 string
	Scope                Scope
	Enabled              bool
	ProxyConfigured      bool
	ProxyPool            bool
	SourceID             uint64
	AccountCapacity      int
	UserAgent            string
	CookieConfigured     bool
	AccountBoundProxy    bool
	Health               float64
	FailureCount         int
	CooldownUntil        *time.Time
	LastError            string
	ProbeStatus          ProbeStatus
	LastProbedAt         *time.Time
	ProbeLatencyMS       int
	ExitIP               string
	ExitCountry          string
	ProbeError           string
	ProbeProvider        ProbeProvider
	IPv4Probe            ProbeFamilyResult
	IPv6Probe            ProbeFamilyResult
	AssignedAccountCount int
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type ProbeStatus string

const (
	ProbeStatusUnknown   ProbeStatus = "unknown"
	ProbeStatusHealthy   ProbeStatus = "healthy"
	ProbeStatusUnhealthy ProbeStatus = "unhealthy"
)

func (value ProbeStatus) IsValid() bool {
	switch value {
	case ProbeStatusUnknown, ProbeStatusHealthy, ProbeStatusUnhealthy:
		return true
	default:
		return false
	}
}

// ProbeResult contains only operational metadata. It never stores or exposes
// proxy credentials.
type ProbeResult struct {
	Status      ProbeStatus
	TestedAt    time.Time
	LatencyMS   int
	ExitIP      string
	ExitCountry string
	Error       string
	Provider    ProbeProvider
	IPv4        ProbeFamilyResult
	IPv6        ProbeFamilyResult
}

// ProbeFamilyResult stores one address family's independent connectivity
// result. A zero TestedAt represents a family that has not been tested yet.
type ProbeFamilyResult struct {
	Status      ProbeStatus
	TestedAt    time.Time
	LatencyMS   int
	ExitIP      string
	ExitCountry string
	Error       string
}

// SubscriptionImportFilter restricts which candidates can be written when a
// subscription or pasted proxy list is imported. A zero value leaves import
// behavior unchanged and avoids a preflight probe.
type SubscriptionImportFilter struct {
	MaxLatencyMS int
	Countries    []string
}

func (value SubscriptionImportFilter) Active() bool {
	return value.MaxLatencyMS > 0 || len(value.Countries) > 0
}

// SubscriptionSource stores a write-only remote proxy subscription. The URL
// remains encrypted at rest and must never be returned by management APIs.
type SubscriptionSource struct {
	ID                     uint64
	Name                   string
	Scope                  Scope
	Enabled                bool
	EncryptedURL           string
	RefreshIntervalSeconds int
	DefaultAccountCapacity int
	ImportFilter           SubscriptionImportFilter
	LastSyncedAt           *time.Time
	NextSyncAt             *time.Time
	LastSyncImported       int
	LastSyncError          string
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

type PublicSubscriptionSource struct {
	ID                     uint64
	Name                   string
	Scope                  Scope
	Enabled                bool
	URLConfigured          bool
	RefreshIntervalSeconds int
	DefaultAccountCapacity int
	ImportFilter           SubscriptionImportFilter
	LastSyncedAt           *time.Time
	NextSyncAt             *time.Time
	LastSyncImported       int
	LastSyncError          string
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// FallbackMode controls what happens when no primary egress node can be
// acquired for a request scope. The default is deliberately none so upgrades
// preserve the existing fail-closed behavior.
type FallbackMode string

const (
	FallbackModeNone   FallbackMode = "none"
	FallbackModeDirect FallbackMode = "direct"
	FallbackModeFixed  FallbackMode = "fixed"
)

func (value FallbackMode) IsValid() bool {
	switch value {
	case FallbackModeNone, FallbackModeDirect, FallbackModeFixed:
		return true
	default:
		return false
	}
}

// Normalized maps the zero value left by pre-fallback database rows to the
// conservative disabled mode.
func (value FallbackMode) Normalized() FallbackMode {
	if value == "" {
		return FallbackModeNone
	}
	return value
}

type FallbackConfig struct {
	Mode   FallbackMode
	NodeID uint64
}

type ProbeProvider string

const (
	ProbeProviderIPInfo     ProbeProvider = "ipinfo"
	ProbeProviderCloudflare ProbeProvider = "cloudflare"
)

func (value ProbeProvider) IsValid() bool {
	return value == ProbeProviderIPInfo || value == ProbeProviderCloudflare
}

func (value ProbeProvider) Normalized() ProbeProvider {
	if !value.IsValid() {
		return ProbeProviderCloudflare
	}
	return value
}

// OperationsConfig controls background probe, account assignment, and egress
// fallback work. It defaults to a conservative disabled state for mutations
// and fallback routing.
type OperationsConfig struct {
	ProbeProvider             ProbeProvider
	ProbeIntervalSeconds      int
	AutoAssignEnabled         bool
	AutoBalanceEnabled        bool
	AssignmentIntervalSeconds int
	Fallbacks                 map[Scope]FallbackConfig
	UpdatedAt                 time.Time
}

func DefaultOperationsConfig() OperationsConfig {
	return OperationsConfig{
		ProbeProvider:             ProbeProviderCloudflare,
		ProbeIntervalSeconds:      900,
		AssignmentIntervalSeconds: 300,
		Fallbacks: map[Scope]FallbackConfig{
			ScopeBuild:    {Mode: FallbackModeNone},
			ScopeWeb:      {Mode: FallbackModeNone},
			ScopeConsole:  {Mode: FallbackModeNone},
			ScopeWebAsset: {Mode: FallbackModeNone},
		},
	}
}

// FallbackFor always returns a canonical, safe fallback value. It accepts
// sparse maps so older callers and historical records remain compatible.
func (value OperationsConfig) FallbackFor(scope Scope) FallbackConfig {
	fallback := value.Fallbacks[scope]
	fallback.Mode = fallback.Mode.Normalized()
	if fallback.Mode != FallbackModeFixed {
		fallback.NodeID = 0
	}
	return fallback
}

// SupportsScope reports whether a node can serve requests for the supplied
// scope. Console may intentionally reuse a Web browser proxy, and Web assets
// inherit a Web node when no asset-specific node is required.
func SupportsScope(nodeScope, requestScope Scope) bool {
	if nodeScope == requestScope {
		return true
	}
	return (requestScope == ScopeWebAsset || requestScope == ScopeConsole) && nodeScope == ScopeWeb
}
