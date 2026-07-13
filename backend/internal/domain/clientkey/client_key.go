package clientkey

import "time"

const (
	DefaultRPMLimit      = 120
	DefaultMaxConcurrent = 8
	MaxRPMLimit          = 100000
	MaxConcurrent        = 1024
	MaxBillingLimitTicks = 9_000_000_000_000_000
)

// Key 表示下游客户端调用凭据及其限制。
type Key struct {
	ID                    uint64
	Name                  string
	Prefix                string
	SecretHash            string
	EncryptedSecret       string
	Enabled               bool
	ExpiresAt             *time.Time
	RPMLimit              int
	MaxConcurrent         int
	BillingLimitUSDTicks  int64
	BilledUsageUSDTicks   int64
	ReservedUsageUSDTicks int64
	AllowedModels         []uint64
	LastUsedAt            *time.Time
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// IsAvailable 判断客户端 Key 当前是否可用。
func (k Key) IsAvailable(now time.Time) bool {
	if !k.Enabled {
		return false
	}
	return k.ExpiresAt == nil || now.Before(*k.ExpiresAt)
}
