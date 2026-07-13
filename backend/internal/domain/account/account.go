package account

import "time"

// Provider 表示上游能力来源。
type Provider string

const (
	ProviderBuild Provider = "grok_build"
	ProviderWeb   Provider = "grok_web"
)

type AuthType string

const (
	AuthTypeOAuth AuthType = "oauth"
	AuthTypeSSO   AuthType = "sso"
)

type WebTier string

const (
	WebTierAuto  WebTier = "auto"
	WebTierBasic WebTier = "basic"
	WebTierSuper WebTier = "super"
	WebTierHeavy WebTier = "heavy"
)

const (
	DefaultPriority         = 1
	DefaultMaxConcurrent    = 8
	DefaultMinimumRemaining = 0
	MaxConcurrent           = 256
)

// AuthStatus 表示账号凭据的认证状态。
type AuthStatus string

const (
	AuthStatusActive         AuthStatus = "active"
	AuthStatusReauthRequired AuthStatus = "reauthRequired"
)

// Credential 表示持久化的上游 OAuth 账号。
type Credential struct {
	ID                    uint64
	Provider              Provider
	AuthType              AuthType
	Name                  string
	Email                 string
	UserID                string
	TeamID                string
	SourceKey             string
	OIDCClientID          string
	EncryptedAccessToken  string
	EncryptedRefreshToken string
	ExpiresAt             time.Time
	Enabled               bool
	AuthStatus            AuthStatus
	Priority              int
	MaxConcurrent         int
	MinimumRemaining      float64
	FailureCount          int
	CooldownUntil         *time.Time
	LastError             string
	LastUsedAt            *time.Time
	ObservedModel         string
	ObservedModelAt       *time.Time
	WebTier               WebTier
	WebTierSyncedAt       *time.Time
	LinkedAccountID       uint64
	LinkedAccountName     string
	LinkedProvider        Provider
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

type QuotaSource string

const (
	QuotaSourceDefault   QuotaSource = "default"
	QuotaSourceEstimated QuotaSource = "estimated"
	QuotaSourceUpstream  QuotaSource = "upstream"
)

// QuotaWindow 表示 Grok Web 单个模式的请求额度窗口。
type QuotaWindow struct {
	AccountID     uint64
	Mode          string
	Remaining     int
	Total         int
	UsagePercent  float64
	Breakdown     []QuotaBreakdown
	WindowSeconds int
	ResetAt       *time.Time
	SyncedAt      *time.Time
	Source        QuotaSource
	UpdatedAt     time.Time
}

// QuotaBreakdown 保存上游周额度中的产品枚举及其使用百分比。
type QuotaBreakdown struct {
	ProductCode  int     `json:"productCode"`
	UsagePercent float64 `json:"usagePercent"`
}

const (
	QuotaProductThirdParty = 0
	QuotaProductAPI        = 1
	QuotaProductBuild      = 2
	QuotaProductPlugins    = 3
	QuotaProductChat       = 4
	QuotaProductImagine    = 5
	QuotaProductVoice      = 6
)

type QuotaRecoveryEvent struct {
	AccountID  uint64
	Mode       string
	DueAt      time.Time
	Attempts   int
	ClaimToken string
}

type BillingHistoryEntry struct {
	Year         int
	Month        int
	IncludedUsed float64
	OnDemandUsed float64
	TotalUsed    float64
}

// Billing 表示账号最近一次额度快照。
type Billing struct {
	AccountID            uint64
	PlanCode             string
	PlanName             string
	MonthlyLimit         float64
	Used                 float64
	OnDemandCap          float64
	OnDemandUsed         float64
	PrepaidBalance       float64
	CreditUsagePercent   float64
	IsUnifiedBillingUser bool
	TopUpMethod          string
	UsagePeriodType      string
	UsagePeriodStart     string
	UsagePeriodEnd       string
	BillingPeriodStart   string
	BillingPeriodEnd     string
	History              []BillingHistoryEntry
	SyncedAt             time.Time
}

// PeriodEnd 返回上游账期结束时间，无法解析时返回 false。
func (b Billing) PeriodEnd() (time.Time, bool) {
	if b.CreditUsagePercent >= 100 {
		if value, ok := parseBillingTime(b.UsagePeriodEnd); ok {
			return value, true
		}
	}
	return parseBillingTime(b.BillingPeriodEnd)
}

func parseBillingTime(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	value, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return value.UTC(), true
}

// QuotaRecoveryKind 区分需要真实流量探测的 Free 额度和需要 Billing 探测的付费账期。
type QuotaRecoveryKind string

const (
	QuotaRecoveryKindFree QuotaRecoveryKind = "free"
	QuotaRecoveryKindPaid QuotaRecoveryKind = "paid"
)

// QuotaRecoveryStatus 表示 Free 额度耗尽后的持久化恢复状态。
type QuotaRecoveryStatus string

const (
	QuotaRecoveryStatusActive    QuotaRecoveryStatus = "active"
	QuotaRecoveryStatusExhausted QuotaRecoveryStatus = "exhausted"
	QuotaRecoveryStatusProbing   QuotaRecoveryStatus = "probing"
)

// QuotaRecovery 保存额度耗尽后的单次恢复探测状态。
type QuotaRecovery struct {
	AccountID       uint64
	Kind            QuotaRecoveryKind
	Status          QuotaRecoveryStatus
	ConfirmedUsed   int64
	ConfirmedLimit  int64
	ExhaustedAt     *time.Time
	NextProbeAt     *time.Time
	LastConfirmedAt *time.Time
	UpdatedAt       time.Time
}

// RoutingCandidate 聚合账号选择热路径所需的持久化快照。
type RoutingCandidate struct {
	Credential           Credential
	Billing              *Billing
	QuotaWindow          *QuotaWindow
	QuotaRecovery        *QuotaRecovery
	ModelCapabilityKnown bool
	SupportsModel        bool
}

// DeviceSession 表示一次短期 Device OAuth 授权流程。
type DeviceSession struct {
	ID                      string
	DeviceCode              string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	Interval                time.Duration
	NextPollAt              time.Time
	ExpiresAt               time.Time
}

// Remaining 返回当前月剩余额度。
func (b Billing) Remaining() float64 {
	remaining := b.MonthlyLimit - b.Used
	if remaining < 0 {
		return 0
	}
	return remaining
}

// IsExhausted 判断额度快照是否已达到账号保留阈值。
func (b Billing) IsExhausted(minimum float64) bool {
	if b.MonthlyLimit > 0 && b.Remaining() <= minimum {
		return true
	}
	return b.CreditUsagePercent >= 100 && (b.OnDemandCap > 0 || b.UsagePeriodType != "")
}
