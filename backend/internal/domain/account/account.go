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

// LinkedAccount 表示同一上游用户在另一 Provider 下的弱关联账号。
// 关联只用于出口身份与管理端展示，不共享额度、健康或路由状态。
type LinkedAccount struct {
	ID       uint64
	Provider Provider
	Name     string
	Email    string
	UserID   string
}

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

// BuildRouteMode 控制 Build 账号的推理地址；模型、Billing 与 OAuth 不受影响。
type BuildRouteMode string

// CurrentWebTermsVersion 是 Grok Web 当前要求接受的产品服务协议版本。
// accounts.x.ai 的账号协议使用独立版本，不与该值混用。
const CurrentWebTermsVersion = 5

const (
	BuildRouteAuto  BuildRouteMode = "auto"
	BuildRouteBuild BuildRouteMode = "build"
	BuildRouteXAI   BuildRouteMode = "xai"
)

func (m BuildRouteMode) IsValid() bool {
	switch m {
	case BuildRouteAuto, BuildRouteBuild, BuildRouteXAI:
		return true
	default:
		return false
	}
}

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
	// ReauthMarkedAt 仅在切入 reauthRequired 时写入；恢复 active 时清空。自动清理以该时刻为 minAge 锚点。
	ReauthMarkedAt            *time.Time
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
	// EgressIdentity 是不含凭据和个人信息的稳定出口身份。
	// 关联到同一 Web 账号的 Build/Console 只共享该值，不共享任何运行状态。
	EgressIdentity string
	// WebNSFWEnabledAt 记录 Grok Web 上游首次确认 NSFW 已成功开启的时间。
	// 普通导入、额度同步和凭据更新不得清除。
	WebNSFWEnabledAt *time.Time
	// WebTermsAcceptedAt 记录 Grok Web 上游确认当前版本完整服务协议已接受的时间。
	// 关联渠道只共享该展示状态，不获得修改 Web 资料的能力。
	WebTermsAcceptedAt *time.Time
	// WebTermsAcceptedVersion 记录已完成的 Grok Web 产品协议版本。
	// 历史数据默认为 0，必须补执行当前版本后才视为完整接受。
	WebTermsAcceptedVersion int
	// WebBirthDateSetAt 记录 Grok Web 上游首次确认生日已设置的时间。
	// 该字段用于避免批量脚本重复请求不可修改的生日接口。
	WebBirthDateSetAt *time.Time
	LinkedAccountID   uint64
	LinkedAccountName string
	LinkedProvider    Provider
	LinkedAccounts    []LinkedAccount
	// BuildAPIFallback 仅记录 grok_build 曾因当次 Build 403 成功回退到 XAI。
	// 它不参与路由；每个新请求仍先走 Build，只有当次严格 403 才可尝试 XAI。
	// token refresh / SSO 转换 / 普通 upsert / 重启不得清除。
	BuildAPIFallback bool
	// BuildRouteMode 是管理员设置的账号级推理地址策略。
	// auto 使用 bot flag / Build 403 自动规则；build 与 xai 分别强制单一地址。
	BuildRouteMode BuildRouteMode
	// BuildSuperEntitled 仅对 grok_build 有效：管理员已确认该账号具备 Super/1.5 entitlement。
	// 不替代 Billing 快照，不等同于 BuildAPIFallback，也不表示请求应走 XAI。
	// 普通导入/upsert/token refresh/SSO 转换不得清除；仅显式管理员 PATCH 可改。
	BuildSuperEntitled bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
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
// 注意：零 Billing + 管理员确认的 BuildSuperEntitled 由 IsBuildSuper 统一判定，不经本方法。
func (b Billing) IsPaid() bool {
	return isPaidBillingPlan(b.PlanCode) || isPaidBillingPlan(b.PlanName) ||
		b.MonthlyLimit > 0 || b.OnDemandCap > 0 || b.OnDemandUsed > 0 || b.PrepaidBalance > 0
}

// HasFreeProfileSignal accepts an explicit Free or Basic plan name.
func (b Billing) HasFreeProfileSignal() bool {
	return isFreeBillingPlan(b.PlanCode) || isFreeBillingPlan(b.PlanName)
}

// HasInferredFreeProfileSignal accepts a successful zero-value billing snapshot
// with no plan name. The upstream omits the Free plan name in this response.
// A non-zero usage or paid balance remains unknown unless another Free signal
// is available, preventing ordinary weekly billing from being misclassified.
func (b Billing) HasInferredFreeProfileSignal() bool {
	if b.SyncedAt.IsZero() || strings.TrimSpace(b.PlanCode) != "" || strings.TrimSpace(b.PlanName) != "" {
		return false
	}
	return b.MonthlyLimit == 0 && b.Used == 0 &&
		b.OnDemandCap == 0 && b.OnDemandUsed == 0 &&
		b.PrepaidBalance == 0 && b.CreditUsagePercent == 0
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

// IsBuildSuper 判定 Grok Build 账号是否为 Super：Billing IsPaid 或管理员确认 BuildSuperEntitled。
// 非 Build Provider 恒为 false。与 SQL accountBuildSuperPredicate 语义一致。
func IsBuildSuper(credential Credential, billing *Billing) bool {
	if credential.Provider != ProviderBuild {
		return false
	}
	if credential.BuildSuperEntitled {
		return true
	}
	return billing != nil && billing.IsPaid()
}

// IsKnownFreeBuild 判断候选是否是已确认的 Grok Build Free 账号。
// Super（Billing paid 或 BuildSuperEntitled）优先，避免旧的响应模型或恢复记录把 Super 错分为 Free。
func (c RoutingCandidate) IsKnownFreeBuild() bool {
	if c.Credential.Provider != ProviderBuild {
		return false
	}
	if IsBuildSuper(c.Credential, c.Billing) {
		return false
	}
	if c.QuotaRecovery != nil && c.QuotaRecovery.Kind == QuotaRecoveryKindFree {
		return true
	}
	if strings.HasSuffix(strings.ToLower(strings.TrimSpace(c.Credential.ObservedModel)), "-build-free") {
		return true
	}
	return c.Billing != nil && (c.Billing.HasFreeProfileSignal() || c.Billing.HasInferredFreeProfileSignal())
}

// IsExhausted 判断额度快照是否已达到账号保留阈值。
func (b Billing) IsExhausted(minimum float64) bool {
	if b.MonthlyLimit > 0 && b.Remaining() <= minimum {
		return true
	}
	return b.CreditUsagePercent >= 100 && (b.OnDemandCap > 0 || b.UsagePeriodType != "")
}
