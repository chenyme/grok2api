package account

import (
	"crypto/sha256"
	"encoding/binary"
	"strings"
	"time"
)

// Provider 表示上游能力来源。
type Provider string

const (
	ProviderBuild   Provider = "grok_build"
	ProviderWeb     Provider = "grok_web"
	ProviderConsole Provider = "grok_console"
)

var providers = [...]Provider{ProviderBuild, ProviderWeb, ProviderConsole}

// Providers 返回按产品展示和后台维护顺序排列的稳定 Provider 集合。
func Providers() []Provider {
	return append([]Provider(nil), providers[:]...)
}

// IsValid 判断 Provider 是否属于当前系统固定支持的渠道。
func (p Provider) IsValid() bool {
	switch p {
	case ProviderBuild, ProviderWeb, ProviderConsole:
		return true
	default:
		return false
	}
}

// ModelNamespace 返回内部模型路由使用的稳定渠道命名空间。
func (p Provider) ModelNamespace() string {
	switch p {
	case ProviderBuild:
		return "Build"
	case ProviderWeb:
		return "Web"
	case ProviderConsole:
		return "Console"
	default:
		return ""
	}
}

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
	ID                        uint64
	Provider                  Provider
	AuthType                  AuthType
	Name                      string
	Email                     string
	UserID                    string
	TeamID                    string
	SourceKey                 string
	OIDCClientID              string
	EncryptedAccessToken      string
	EncryptedRefreshToken     string
	EncryptedCloudflareCookie string
	ExpiresAt                 time.Time
	RefreshDueAt              *time.Time
	LastRefreshAt             *time.Time
	RefreshFailureCount       int
	LastRefreshErrorCode      string
	RefreshPermanent          bool
	Enabled                   bool
	AuthStatus                AuthStatus
	Priority                  int
	MaxConcurrent             int
	MinimumRemaining          float64
	FailureCount              int
	CooldownUntil             *time.Time
	LastError                 string
	LastUsedAt                *time.Time
	ObservedModel             string
	ObservedModelAt           *time.Time
	WebTier                   WebTier
	WebTierSyncedAt           *time.Time
	LinkedAccountID           uint64
	LinkedAccountName         string
	LinkedProvider            Provider
	// BuildAPIFallback 仅对 grok_build 有效：账号级 XAI **推理** 回退标记。
	// 已标记时 models / responses create|compact / video 走 FallbackBaseURL；
	// Billing、stored GET/DELETE /responses/{id}、OAuth 与未知路径仍走主地址。
	// token refresh / SSO 转换 / 普通 upsert / 重启不得清除。
	BuildAPIFallback bool
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// CredentialRefreshDueAt 将账号稳定地分散到到期前 5~8 分钟，避免同批导入账号同时刷新。
func CredentialRefreshDueAt(accountID uint64, expiresAt time.Time) time.Time {
	if expiresAt.IsZero() {
		return time.Time{}
	}
	var identity [8]byte
	binary.BigEndian.PutUint64(identity[:], accountID)
	digest := sha256.Sum256(identity[:])
	jitterSeconds := binary.BigEndian.Uint16(digest[:2]) % 181
	return expiresAt.UTC().Add(-5*time.Minute - time.Duration(jitterSeconds)*time.Second)
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
	ProductCode  int
	UsagePercent float64
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
	PeriodType   string
	PeriodStart  string
	PeriodEnd    string
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
	OnDemandEnabled      *bool
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
	ModelQuotaBlock      *ModelQuotaBlock
	ModelCapabilityKnown bool
	SupportsModel        bool
}

// ModelQuotaBlock 表示账号的单模型配额暂不可用，不影响该账号上的其他模型。
type ModelQuotaBlock struct {
	AccountID     uint64
	UpstreamModel string
	Reason        string
	CooldownUntil time.Time
	UpdatedAt     time.Time
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

// IsPaid 判断 Billing 快照是否呈现 Super/paid 信号。
// 语义与 SQL accountPaidBillingPredicate 及 QuotaView paid 分支一致：
// 已确认的付费订阅名称，或任一付费额度字段为正，即为 paid。
// creditUsagePercent 只是当前周期使用率，Free 与 paid 都可能存在，不能参与判级。
// 无快照时应由调用方按 Unknown 处理，不得调用本方法。
func (b Billing) IsPaid() bool {
	return isPaidBillingPlan(b.PlanCode) || isPaidBillingPlan(b.PlanName) ||
		b.MonthlyLimit > 0 || b.OnDemandCap > 0 || b.OnDemandUsed > 0 || b.PrepaidBalance > 0
}

// HasFreeProfileSignal 仅接受明确的 Free/Basic 套餐名称。
// currentPeriod、unified billing、top-up 等字段在零使用量的付费账号上同样存在，
// 不能作为 Free 证据，否则会把刚开通的 SuperGrok 误判为 Free。
func (b Billing) HasFreeProfileSignal() bool {
	return isFreeBillingPlan(b.PlanCode) || isFreeBillingPlan(b.PlanName)
}

func normalizeBillingPlan(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.NewReplacer(" ", "", "_", "", "-", "", "+", "plus").Replace(value)
}

func isPaidBillingPlan(value string) bool {
	switch normalizeBillingPlan(value) {
	case "super", "supergrok", "supergrokpro", "supergrokheavy", "supergroklite",
		"grokpro", "xpremium", "xpremiumplus", "apikey":
		return true
	default:
		return false
	}
}

func isFreeBillingPlan(value string) bool {
	switch normalizeBillingPlan(value) {
	case "free", "grokfree", "freetier", "basic", "grokbasic", "xbasic":
		return true
	default:
		return false
	}
}

// IsKnownFreeBuild 判断候选是否是已确认的 Grok Build Free 账号。
// paid 信号优先，避免旧的响应模型或恢复记录把 Super 错分为 Free。
func (c RoutingCandidate) IsKnownFreeBuild() bool {
	if c.Credential.Provider != ProviderBuild {
		return false
	}
	if c.Billing != nil && c.Billing.IsPaid() {
		return false
	}
	if c.QuotaRecovery != nil && c.QuotaRecovery.Kind == QuotaRecoveryKindFree {
		return true
	}
	if strings.HasSuffix(strings.ToLower(strings.TrimSpace(c.Credential.ObservedModel)), "-build-free") {
		return true
	}
	return c.Billing != nil && c.Billing.HasFreeProfileSignal()
}

// IsExhausted 判断额度快照是否已达到账号保留阈值。
func (b Billing) IsExhausted(minimum float64) bool {
	if b.MonthlyLimit > 0 && b.Remaining() <= minimum {
		return true
	}
	return b.CreditUsagePercent >= 100 && (b.OnDemandCap > 0 || b.UsagePeriodType != "")
}
