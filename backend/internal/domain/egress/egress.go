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
	ProbeError                  string
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
	ProbeError           string
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
	Status    ProbeStatus
	TestedAt  time.Time
	LatencyMS int
	ExitIP    string
	Error     string
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
	LastSyncedAt           *time.Time
	NextSyncAt             *time.Time
	LastSyncImported       int
	LastSyncError          string
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// OperationsConfig controls background probe and account assignment work. It
// defaults to a conservative disabled state for assignment mutations.
type OperationsConfig struct {
	ProbeIntervalSeconds      int
	AutoAssignEnabled         bool
	AutoBalanceEnabled        bool
	AssignmentIntervalSeconds int
	UpdatedAt                 time.Time
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
