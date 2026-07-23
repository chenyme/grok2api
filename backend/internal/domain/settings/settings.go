package settings

import "time"

const (
	DefaultBuildResponseHeaderTimeout = 5 * time.Minute
	MinBuildResponseHeaderTimeout     = 30 * time.Second
	MaxBuildResponseHeaderTimeout     = 30 * time.Minute
)

// Config 表示可跨重启持久化并支持热加载的网关运行参数。
type Config struct {
	Server            ServerConfig
	ProviderBuild     ProviderBuildConfig
	ProviderWeb       ProviderWebConfig
	ProviderConsole   ProviderConsoleConfig
	Batch             BatchConfig
	Media             MediaConfig
	Frontend          FrontendConfig
	Routing           RoutingConfig
	Audit             AuditConfig
	ClientKeyDefaults ClientKeyDefaultsConfig
	Accounts          AccountsConfig
}

// ServerConfig 定义可热更新的推理入口容量参数。
type ServerConfig struct {
	MaxConcurrentRequests int
}

// FrontendConfig 定义公开 API 地址的运行时覆盖值；留空时使用配置文件值。
type FrontendConfig struct {
	PublicAPIBaseURL string
}

type ProviderConsoleConfig struct {
	BaseURL     string
	ChatTimeout time.Duration
}

type MediaConfig struct {
	MaxImageBytes           int64
	MaxTotalBytes           int64
	CleanupThresholdPercent int
	CleanupInterval         time.Duration
}

type ProviderWebConfig struct {
	BaseURL             string
	StatsigMode         string
	StatsigManualValue  string
	StatsigSignerURL    string
	ClearanceMode       string
	FlareSolverrURL     string
	ClearanceTimeout    time.Duration
	ClearanceRefresh    time.Duration
	QuotaTimeout        time.Duration
	ChatTimeout         time.Duration
	ImageTimeout        time.Duration
	VideoTimeout        time.Duration
	MediaConcurrency    int
	AllowNSFW           bool
	RecoveryBackoffBase time.Duration
	RecoveryBackoffMax  time.Duration
}

// BatchConfig 定义账号导入、转换、同步和凭据刷新的并发上限。
type BatchConfig struct {
	ImportConcurrency     int
	ConversionConcurrency int
	SyncConcurrency       int
	RefreshConcurrency    int
	RandomDelay           *time.Duration
}

// ProviderBuildConfig 定义 Grok Build CLI 上游协议标识。
type ProviderBuildConfig struct {
	BaseURL               string
	FallbackBaseURL       string
	ClientVersion         string
	ClientIdentifier      string
	TokenAuth             string
	UserAgent             string
	ResponseHeaderTimeout time.Duration
}

// RoutingConfig 定义会话粘性、冷却和故障切换边界。
type RoutingConfig struct {
	StickyTTL         time.Duration
	CooldownBase      time.Duration
	CooldownMax       time.Duration
	CapacityWait      time.Duration
	MaxAttempts       int
	PreferFreeBuild   bool
	SegmentedSelector *SegmentedSelectorConfig
}

// SegmentedSelectorConfig persists the bounded selector policy.
// A nil value means the stored settings predate this policy and startup defaults must be preserved.
type SegmentedSelectorConfig struct {
	// ActiveEnabled retains the original persisted field name so existing disabled policies remain disabled.
	ActiveEnabled bool
	MinCandidates int
	WindowSize    int
}

// AuditConfig 定义请求审计异步写入参数。
type AuditConfig struct {
	BufferSize    int
	BatchSize     int
	FlushInterval time.Duration
	CommitDelay   time.Duration
}

// ClientKeyDefaultsConfig 定义新建客户端密钥的默认限制。
type ClientKeyDefaultsConfig struct {
	RPMLimit      int
	MaxConcurrent int
}

// AccountsConfig 定义账号池后台维护策略；默认全部关闭。
type AccountsConfig struct {
	// MarkBuildForbiddenReauth marks high-confidence Grok Build permission denials as requiring reauthorization.
	MarkBuildForbiddenReauth bool
	// BuildForbiddenReauthCodes contains exact upstream error codes that opt into account invalidation.
	BuildForbiddenReauthCodes []string
	// AutoCleanReauthEnabled 为 true 时，周期性删除已标记 reauthRequired 且超过 minAge 的账号。
	AutoCleanReauthEnabled bool
	// AutoCleanReauthInterval 自动清理扫描间隔。
	AutoCleanReauthInterval time.Duration
	// AutoCleanReauthMinAge 仅删除 reauth_marked_at 早于该时长的 reauthRequired 账号。
	AutoCleanReauthMinAge time.Duration
	// AutoCleanIncludeDisabled 为 true 时，reauth 清理时包含 enabled=false 的账号。
	AutoCleanIncludeDisabled bool
}
