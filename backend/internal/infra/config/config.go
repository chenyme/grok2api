package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/pkg/signerurl"
	"gopkg.in/yaml.v3"
)

const (
	StatsigModeManual             = "manual"
	StatsigModeURL                = "url"
	DefaultStatsigSignerURL       = "https://grok.wodf.de/sign"
	RecommendedBuildClientVersion = "0.2.99"
	RecommendedBuildUserAgent     = "grok-shell/0.2.99 (linux; x86_64)"

	maxServerBodyBytes    = 256 << 20
	maxRequestTimeout     = 24 * time.Hour
	maxReadTimeout        = time.Hour
	maxRoutingTTL         = 30 * 24 * time.Hour
	maxRoutingCooldown    = 24 * time.Hour
	minAuditFlushInterval = 10 * time.Millisecond
	maxAuditFlushInterval = time.Minute
	maxAuditBufferSize    = 262144
	maxAuditBatchSize     = 4096
)

// Config 表示后端运行配置。
type Config struct {
	Server            ServerConfig            `yaml:"server"`
	Frontend          FrontendConfig          `yaml:"frontend"`
	Database          DatabaseConfig          `yaml:"database"`
	RuntimeStore      RuntimeStoreConfig      `yaml:"runtimeStore"`
	Auth              AuthConfig              `yaml:"auth"`
	Secrets           Secrets                 `yaml:"secrets"`
	BootstrapAdmin    BootstrapAdminConfig    `yaml:"bootstrapAdmin"`
	Provider          ProviderConfig          `yaml:"provider"`
	Batch             BatchConfig             `yaml:"-"`
	Media             MediaConfig             `yaml:"media"`
	Routing           RoutingConfig           `yaml:"routing"`
	Audit             AuditConfig             `yaml:"audit"`
	ClientKeyDefaults ClientKeyDefaultsConfig `yaml:"clientKeyDefaults"`
}

type ServerConfig struct {
	Listen                string   `yaml:"listen"`
	MaxBodyBytes          int64    `yaml:"maxBodyBytes"`
	MaxConcurrentRequests int      `yaml:"maxConcurrentRequests"`
	ReadTimeout           Duration `yaml:"readTimeout"`
	RequestTimeout        Duration `yaml:"requestTimeout"`
	SwaggerEnabled        bool     `yaml:"swaggerEnabled"`
}

type FrontendConfig struct {
	PublicAPIBaseURL         string `yaml:"publicApiBaseURL"`
	PublicAPIBaseURLOverride string `yaml:"-"`
	StaticPath               string `yaml:"staticPath"`
}

const DefaultPublicAPIBaseURL = "http://127.0.0.1:8000"

// EffectivePublicAPIBaseURL 按运行设置、配置文件、内置默认值的顺序解析公开地址。
func (c FrontendConfig) EffectivePublicAPIBaseURL() string {
	for _, value := range []string{c.PublicAPIBaseURLOverride, c.PublicAPIBaseURL} {
		if value = strings.TrimRight(strings.TrimSpace(value), "/"); value != "" {
			return value
		}
	}
	return DefaultPublicAPIBaseURL
}

type DatabaseConfig struct {
	Driver   string                 `yaml:"driver"`
	SQLite   SQLiteDatabaseConfig   `yaml:"sqlite"`
	Postgres PostgresDatabaseConfig `yaml:"postgres"`
}

type SQLiteDatabaseConfig struct {
	Path string `yaml:"path"`
}

type PostgresDatabaseConfig struct {
	DSN          string `yaml:"dsn"`
	MaxOpenConns int    `yaml:"maxOpenConns"`
	MaxIdleConns int    `yaml:"maxIdleConns"`
}

type RuntimeStoreConfig struct {
	Driver string             `yaml:"driver"`
	Redis  RedisRuntimeConfig `yaml:"redis"`
}

type RedisRuntimeConfig struct {
	Address   string `yaml:"address"`
	Username  string `yaml:"username"`
	Password  string `yaml:"password"`
	Database  int    `yaml:"database"`
	KeyPrefix string `yaml:"keyPrefix"`
	TLS       bool   `yaml:"tls"`
}

type AuthConfig struct {
	AccessTokenTTL  Duration `yaml:"accessTokenTTL"`
	RefreshTokenTTL Duration `yaml:"refreshTokenTTL"`
	SecureCookies   bool     `yaml:"secureCookies"`
}

type ProviderConfig struct {
	Build   BuildProviderConfig   `yaml:"build"`
	Web     WebProviderConfig     `yaml:"web"`
	Console ConsoleProviderConfig `yaml:"console"`
}

type BuildProviderConfig struct {
	BaseURL          string `yaml:"baseURL"`
	ClientVersion    string `yaml:"clientVersion"`
	ClientIdentifier string `yaml:"clientIdentifier"`
	TokenAuth        string `yaml:"tokenAuth"`
	UserAgent        string `yaml:"userAgent"`
}

type WebProviderConfig struct {
	BaseURL             string   `yaml:"baseURL"`
	StatsigMode         string   `yaml:"-"`
	StatsigManualValue  string   `yaml:"-"`
	StatsigSignerURL    string   `yaml:"-"`
	QuotaTimeout        Duration `yaml:"quotaTimeout"`
	ChatTimeout         Duration `yaml:"chatTimeout"`
	ImageTimeout        Duration `yaml:"imageTimeout"`
	VideoTimeout        Duration `yaml:"videoTimeout"`
	MediaConcurrency    int      `yaml:"mediaConcurrency"`
	AllowNSFW           bool     `yaml:"allowNSFW"`
	RecoveryBackoffBase Duration `yaml:"recoveryBackoffBase"`
	RecoveryBackoffMax  Duration `yaml:"recoveryBackoffMax"`
}

type ConsoleProviderConfig struct {
	BaseURL     string   `yaml:"baseURL"`
	UserAgent   string   `yaml:"userAgent"`
	ChatTimeout Duration `yaml:"chatTimeout"`
}

// BatchConfig 定义可热加载的账号批量任务并发上限。
type BatchConfig struct {
	ImportConcurrency     int
	ConversionConcurrency int
	SyncConcurrency       int
	RefreshConcurrency    int
	RandomDelay           Duration
}

type MediaConfig struct {
	Driver                  string           `yaml:"driver"`
	MaxImageBytes           int64            `yaml:"-"`
	MaxTotalBytes           int64            `yaml:"-"`
	CleanupThresholdPercent int              `yaml:"-"`
	CleanupInterval         Duration         `yaml:"-"`
	Local                   LocalMediaConfig `yaml:"local"`
}

type LocalMediaConfig struct {
	Path string `yaml:"path"`
}

type RoutingConfig struct {
	StickyTTL    Duration `yaml:"stickyTTL"`
	CooldownBase Duration `yaml:"cooldownBase"`
	CooldownMax  Duration `yaml:"cooldownMax"`
	CapacityWait Duration `yaml:"capacityWait"`
	MaxAttempts  int      `yaml:"maxAttempts"`
}

type AuditConfig struct {
	BufferSize    int      `yaml:"bufferSize"`
	BatchSize     int      `yaml:"batchSize"`
	FlushInterval Duration `yaml:"flushInterval"`
}

type ClientKeyDefaultsConfig struct {
	RPMLimit      int `yaml:"rpmLimit"`
	MaxConcurrent int `yaml:"maxConcurrent"`
}

type Secrets struct {
	JWTSecret               string `yaml:"jwtSecret"`
	CredentialEncryptionKey string `yaml:"credentialEncryptionKey"`
}

type BootstrapAdminConfig struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// Duration 支持在 YAML 中使用 10m、1h 等可读时间格式。
type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

func (d Duration) Value() time.Duration { return time.Duration(d) }

func (d Duration) String() string {
	value := d.Value().String()
	if strings.HasSuffix(value, "m0s") {
		value = strings.TrimSuffix(value, "0s")
	}
	if strings.HasSuffix(value, "h0m") {
		value = strings.TrimSuffix(value, "0m")
	}
	return value
}

// Load 从 YAML 加载启动配置，并为非敏感运行参数补充代码默认值。
func Load(path string) (Config, error) {
	cfg := defaultConfig()
	loadedFrom := ""
	if strings.TrimSpace(path) != "" {
		data, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Config{}, fmt.Errorf("读取配置文件: %w", err)
		}
		if err == nil {
			loadedFrom = path
			decoder := yaml.NewDecoder(bytes.NewReader(data))
			decoder.KnownFields(true)
			if err := decoder.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
				return Config{}, fmt.Errorf("解析配置文件: %w", err)
			}
			var extra any
			if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
				if err != nil {
					return Config{}, fmt.Errorf("解析配置文件: %w", err)
				}
				return Config{}, errors.New("配置文件只能包含一个 YAML 文档")
			}
		}
	}
	if loadedFrom != "" {
		if err := resolveRelativePaths(&cfg, loadedFrom); err != nil {
			return Config{}, err
		}
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func resolveRelativePaths(cfg *Config, configPath string) error {
	absoluteConfigPath, err := filepath.Abs(configPath)
	if err != nil {
		return fmt.Errorf("解析配置文件路径: %w", err)
	}
	baseDir := filepath.Dir(absoluteConfigPath)
	if cfg.Database.Driver == "sqlite" {
		path := strings.TrimSpace(cfg.Database.SQLite.Path)
		if path != "" && !filepath.IsAbs(path) {
			cfg.Database.SQLite.Path = filepath.Clean(filepath.Join(baseDir, path))
		}
	}
	mediaPath := strings.TrimSpace(cfg.Media.Local.Path)
	if mediaPath != "" && !filepath.IsAbs(mediaPath) {
		cfg.Media.Local.Path = filepath.Clean(filepath.Join(baseDir, mediaPath))
	}
	staticPath := strings.TrimSpace(cfg.Frontend.StaticPath)
	if staticPath != "" && !filepath.IsAbs(staticPath) {
		cfg.Frontend.StaticPath = filepath.Clean(filepath.Join(baseDir, staticPath))
	}
	return nil
}

// Validate 校验启动所需的安全配置和运行边界。
func (c Config) Validate() error {
	if strings.TrimSpace(c.Server.Listen) == "" {
		return errors.New("server.listen 不能为空")
	}
	if c.Server.MaxBodyBytes <= 0 || c.Server.MaxBodyBytes > maxServerBodyBytes {
		return fmt.Errorf("server.maxBodyBytes 必须在 1 到 %d 字节之间", maxServerBodyBytes)
	}
	if c.Server.ReadTimeout.Value() <= 0 || c.Server.ReadTimeout.Value() > maxReadTimeout {
		return errors.New("server.readTimeout 必须大于零且不超过 1 小时")
	}
	if c.Server.RequestTimeout.Value() <= 0 || c.Server.RequestTimeout.Value() > maxRequestTimeout {
		return errors.New("server.requestTimeout 必须大于零且不超过 24 小时")
	}
	if c.Server.MaxConcurrentRequests < 1 || c.Server.MaxConcurrentRequests > 100000 {
		return errors.New("server.maxConcurrentRequests 必须在 1 到 100000 之间")
	}
	for _, item := range []struct {
		name  string
		value string
	}{
		{name: "frontend.publicApiBaseURL", value: c.Frontend.PublicAPIBaseURL},
		{name: "frontend.publicApiBaseURL 运行设置", value: c.Frontend.PublicAPIBaseURLOverride},
	} {
		if publicBase := strings.TrimSpace(item.value); publicBase != "" {
			publicAPIURL, err := url.ParseRequestURI(publicBase)
			if err != nil || (publicAPIURL.Scheme != "http" && publicAPIURL.Scheme != "https") || publicAPIURL.Host == "" || publicAPIURL.User != nil || publicAPIURL.RawQuery != "" || publicAPIURL.Fragment != "" {
				return fmt.Errorf("%s 必须是不含凭据、查询参数和片段的 HTTP(S) URL", item.name)
			}
		}
	}
	switch c.Database.Driver {
	case "sqlite":
		if strings.TrimSpace(c.Database.SQLite.Path) == "" {
			return errors.New("database.sqlite.path 不能为空")
		}
	case "postgres":
		if strings.TrimSpace(c.Database.Postgres.DSN) == "" {
			return errors.New("database.postgres.dsn 不能为空")
		}
		if c.Database.Postgres.MaxOpenConns < 1 || c.Database.Postgres.MaxOpenConns > 1000 || c.Database.Postgres.MaxIdleConns < 0 || c.Database.Postgres.MaxIdleConns > c.Database.Postgres.MaxOpenConns {
			return errors.New("database.postgres 连接池配置无效")
		}
	default:
		return errors.New("database.driver 必须是 sqlite 或 postgres")
	}
	switch c.RuntimeStore.Driver {
	case "memory":
	case "redis":
		if strings.TrimSpace(c.RuntimeStore.Redis.Address) == "" {
			return errors.New("runtimeStore.redis.address 不能为空")
		}
		if c.RuntimeStore.Redis.Database < 0 || c.RuntimeStore.Redis.Database > 1024 {
			return errors.New("runtimeStore.redis.database 必须在 0 到 1024 之间")
		}
		if prefix := strings.TrimSpace(c.RuntimeStore.Redis.KeyPrefix); prefix == "" || len(prefix) > 128 {
			return errors.New("runtimeStore.redis.keyPrefix 必须在 1 到 128 个字符之间")
		}
	default:
		return errors.New("runtimeStore.driver 必须是 memory 或 redis")
	}
	if c.Media.Driver != "local" {
		return errors.New("media.driver 当前仅支持 local")
	}
	if strings.TrimSpace(c.Media.Local.Path) == "" {
		return errors.New("media.local.path 不能为空")
	}
	if c.Media.MaxImageBytes < 1<<20 || c.Media.MaxImageBytes > 32<<20 {
		return errors.New("media.maxImageBytes 必须在 1 MiB 到 32 MiB 之间")
	}
	if c.Media.MaxTotalBytes < c.Media.MaxImageBytes || c.Media.MaxTotalBytes > 1<<40 {
		return errors.New("media.maxTotalBytes 必须不小于单图上限且不超过 1 TiB")
	}
	if c.Media.CleanupThresholdPercent < 50 || c.Media.CleanupThresholdPercent > 95 {
		return errors.New("media.cleanupThresholdPercent 必须在 50 到 95 之间")
	}
	if c.Media.CleanupInterval.Value() < time.Minute || c.Media.CleanupInterval.Value() > 24*time.Hour {
		return errors.New("media.cleanupInterval 必须在 1 分钟到 24 小时之间")
	}
	if len(c.Secrets.JWTSecret) < 32 {
		return errors.New("secrets.jwtSecret 至少需要 32 个字符")
	}
	if isExampleSecret(c.Secrets.JWTSecret) {
		return errors.New("secrets.jwtSecret 不能使用示例占位值")
	}
	if !validCredentialEncryptionKey(c.Secrets.CredentialEncryptionKey) {
		return errors.New("secrets.credentialEncryptionKey 必须是 Base64 编码的 32 字节密钥")
	}
	if isExampleSecret(c.BootstrapAdmin.Password) {
		return errors.New("bootstrapAdmin.password 不能使用示例占位值")
	}
	if c.Auth.AccessTokenTTL.Value() <= 0 || c.Auth.RefreshTokenTTL.Value() <= 0 {
		return errors.New("JWT 有效期必须大于零")
	}
	providerURL, err := url.ParseRequestURI(strings.TrimSpace(c.Provider.Build.BaseURL))
	if err != nil || providerURL.Scheme == "" || providerURL.Host == "" {
		return errors.New("provider.build.baseURL 必须是有效 URL")
	}
	if strings.TrimSpace(c.Provider.Build.ClientVersion) == "" || strings.TrimSpace(c.Provider.Build.ClientIdentifier) == "" || strings.TrimSpace(c.Provider.Build.TokenAuth) == "" || strings.TrimSpace(c.Provider.Build.UserAgent) == "" {
		return errors.New("provider.build 客户端标识不能为空")
	}
	webURL, err := url.ParseRequestURI(strings.TrimSpace(c.Provider.Web.BaseURL))
	if err != nil || webURL.Scheme != "https" || webURL.Host == "" || webURL.User != nil {
		return errors.New("provider.web.baseURL 必须是无凭据的 HTTPS URL")
	}
	switch c.Provider.Web.StatsigMode {
	case StatsigModeManual:
		if !validStatsigID(c.Provider.Web.StatsigManualValue) {
			return errors.New("provider.web 手动 x-statsig-id 格式无效")
		}
	case StatsigModeURL:
		if err := signerurl.Validate(c.Provider.Web.StatsigSignerURL); err != nil {
			return fmt.Errorf("provider.web Statsig 签名 URL 无效: %w", err)
		}
	default:
		return errors.New("provider.web Statsig 模式必须是 manual 或 url")
	}
	if c.Provider.Web.QuotaTimeout.Value() < time.Second || c.Provider.Web.QuotaTimeout.Value() > 2*time.Minute ||
		c.Provider.Web.ChatTimeout.Value() < 5*time.Second || c.Provider.Web.ChatTimeout.Value() > 30*time.Minute ||
		c.Provider.Web.ImageTimeout.Value() < 5*time.Second || c.Provider.Web.ImageTimeout.Value() > 30*time.Minute ||
		c.Provider.Web.VideoTimeout.Value() < time.Minute || c.Provider.Web.VideoTimeout.Value() > 2*time.Hour {
		return errors.New("provider.web 上游超时配置无效")
	}
	if c.Provider.Web.MediaConcurrency < 1 || c.Provider.Web.MediaConcurrency > 64 {
		return errors.New("provider.web 媒体并发必须在 1 到 64 之间")
	}
	consoleURL, err := url.ParseRequestURI(strings.TrimSpace(c.Provider.Console.BaseURL))
	if err != nil || consoleURL.Scheme != "https" || consoleURL.Host == "" || consoleURL.User != nil {
		return errors.New("provider.console.baseURL 必须是无凭据的 HTTPS URL")
	}
	if userAgent := strings.TrimSpace(c.Provider.Console.UserAgent); len(userAgent) < 1 || len(userAgent) > 512 {
		return errors.New("provider.console.userAgent 长度必须在 1 到 512 个字符之间")
	}
	if c.Provider.Console.ChatTimeout.Value() < 5*time.Second || c.Provider.Console.ChatTimeout.Value() > 30*time.Minute {
		return errors.New("provider.console.chatTimeout 必须在 5 秒到 30 分钟之间")
	}
	if c.Batch.ImportConcurrency < 1 || c.Batch.ImportConcurrency > 50 ||
		c.Batch.ConversionConcurrency < 1 || c.Batch.ConversionConcurrency > 50 ||
		c.Batch.SyncConcurrency < 1 || c.Batch.SyncConcurrency > 50 ||
		c.Batch.RefreshConcurrency < 1 || c.Batch.RefreshConcurrency > 50 {
		return errors.New("批量任务并发必须在 1 到 50 之间")
	}
	if c.Batch.RandomDelay.Value() < 0 || c.Batch.RandomDelay.Value() > 5*time.Second {
		return errors.New("批量任务随机延迟必须在 0 到 5 秒之间")
	}
	if c.Provider.Web.RecoveryBackoffBase.Value() < 5*time.Second || c.Provider.Web.RecoveryBackoffMax.Value() < c.Provider.Web.RecoveryBackoffBase.Value() || c.Provider.Web.RecoveryBackoffMax.Value() > 6*time.Hour {
		return errors.New("provider.web 恢复退避配置无效")
	}
	if c.Routing.StickyTTL.Value() <= 0 || c.Routing.StickyTTL.Value() > maxRoutingTTL || c.Routing.CooldownBase.Value() <= 0 || c.Routing.CooldownMax.Value() < c.Routing.CooldownBase.Value() || c.Routing.CooldownMax.Value() > maxRoutingCooldown || c.Routing.CapacityWait.Value() <= 0 || c.Routing.CapacityWait.Value() > 5*time.Second || c.Routing.MaxAttempts < 1 || c.Routing.MaxAttempts > 10 {
		return errors.New("routing 配置无效")
	}
	if c.Audit.BufferSize < 1 || c.Audit.BufferSize > maxAuditBufferSize || c.Audit.BatchSize < 1 || c.Audit.BatchSize > maxAuditBatchSize || c.Audit.BatchSize > c.Audit.BufferSize || c.Audit.FlushInterval.Value() < minAuditFlushInterval || c.Audit.FlushInterval.Value() > maxAuditFlushInterval {
		return errors.New("audit 队列和批量写入配置无效")
	}
	if c.ClientKeyDefaults.RPMLimit < 1 || c.ClientKeyDefaults.RPMLimit > clientkeydomain.MaxRPMLimit || c.ClientKeyDefaults.MaxConcurrent < 1 || c.ClientKeyDefaults.MaxConcurrent > clientkeydomain.MaxConcurrent {
		return errors.New("clientKeyDefaults 超出允许范围")
	}
	return nil
}

func defaultConfig() Config {
	return Config{
		Server: ServerConfig{
			Listen:                "127.0.0.1:8000",
			MaxBodyBytes:          32 << 20,
			MaxConcurrentRequests: 1024,
			ReadTimeout:           Duration(15 * time.Minute),
			RequestTimeout:        Duration(2 * time.Hour),
		},
		Frontend: FrontendConfig{PublicAPIBaseURL: DefaultPublicAPIBaseURL, StaticPath: "./frontend/dist"},
		Database: DatabaseConfig{
			Driver:   "sqlite",
			SQLite:   SQLiteDatabaseConfig{Path: "./data/backend.db"},
			Postgres: PostgresDatabaseConfig{MaxOpenConns: 50, MaxIdleConns: 10},
		},
		RuntimeStore: RuntimeStoreConfig{
			Driver: "memory",
			Redis:  RedisRuntimeConfig{Address: "127.0.0.1:6379", KeyPrefix: "grok2api:"},
		},
		Auth: AuthConfig{
			AccessTokenTTL:  Duration(15 * time.Minute),
			RefreshTokenTTL: Duration(30 * 24 * time.Hour),
		},
		Provider: ProviderConfig{
			Build: BuildProviderConfig{
				BaseURL: "https://cli-chat-proxy.grok.com/v1", ClientVersion: RecommendedBuildClientVersion,
				ClientIdentifier: "grok-shell", TokenAuth: "xai-grok-cli",
				UserAgent: RecommendedBuildUserAgent,
			},
			Web: WebProviderConfig{
				BaseURL: "https://grok.com", StatsigMode: StatsigModeURL, StatsigSignerURL: DefaultStatsigSignerURL,
				QuotaTimeout: Duration(25 * time.Second),
				ChatTimeout:  Duration(2 * time.Minute), ImageTimeout: Duration(3 * time.Minute),
				VideoTimeout:     Duration(15 * time.Minute),
				MediaConcurrency: 4, RecoveryBackoffBase: Duration(30 * time.Second),
				RecoveryBackoffMax: Duration(30 * time.Minute),
			},
			Console: ConsoleProviderConfig{
				BaseURL: "https://console.x.ai", UserAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36",
				ChatTimeout: Duration(5 * time.Minute),
			},
		},
		Batch: BatchConfig{
			ImportConcurrency: 25, ConversionConcurrency: 25, SyncConcurrency: 25,
			RefreshConcurrency: 25, RandomDelay: Duration(500 * time.Millisecond),
		},
		Media: MediaConfig{
			Driver: "local", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30,
			CleanupThresholdPercent: 80, CleanupInterval: Duration(10 * time.Minute),
			Local: LocalMediaConfig{Path: "./data/media"},
		},
		Routing: RoutingConfig{
			StickyTTL:    Duration(time.Hour),
			CooldownBase: Duration(30 * time.Second),
			CooldownMax:  Duration(30 * time.Minute),
			CapacityWait: Duration(500 * time.Millisecond),
			MaxAttempts:  3,
		},
		Audit:             AuditConfig{BufferSize: 16384, BatchSize: 256, FlushInterval: Duration(250 * time.Millisecond)},
		ClientKeyDefaults: ClientKeyDefaultsConfig{RPMLimit: clientkeydomain.DefaultRPMLimit, MaxConcurrent: clientkeydomain.DefaultMaxConcurrent},
	}
}

func validStatsigID(value string) bool {
	value = strings.TrimSpace(value)
	decoded, err := base64.RawStdEncoding.DecodeString(value)
	if err != nil {
		decoded, err = base64.StdEncoding.DecodeString(value)
	}
	return err == nil && len(decoded) == 70
}

func validCredentialEncryptionKey(value string) bool {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	return err == nil && len(decoded) == 32
}

func isExampleSecret(value string) bool {
	switch strings.TrimSpace(value) {
	case "replace-with-at-least-32-characters", "replace-with-base64-key", "replace-with-a-strong-password":
		return true
	default:
		return false
	}
}
