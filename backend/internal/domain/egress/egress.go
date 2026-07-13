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
	ScopeAll      Scope = "all"
	ScopeBuild    Scope = "grok_build"
	ScopeWeb      Scope = "grok_web"
	ScopeWebAsset Scope = "grok_web_asset"
)

type Node struct {
	ID                        uint64
	Name                      string
	Scope                     Scope
	Enabled                   bool
	EncryptedProxyURL         string
	UserAgent                 string
	EncryptedCloudflareCookie string
	Health                    float64
	FailureCount              int
	CooldownUntil             *time.Time
	LastError                 string
	CreatedAt                 time.Time
	UpdatedAt                 time.Time
}

type PublicNode struct {
	ID               uint64
	Name             string
	Scope            Scope
	Enabled          bool
	ProxyConfigured  bool
	UserAgent        string
	CookieConfigured bool
	Health           float64
	FailureCount     int
	CooldownUntil    *time.Time
	LastError        string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}
