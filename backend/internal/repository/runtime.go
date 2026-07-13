package repository

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// RateLimiter 定义客户端 RPM 限制边界。
type RateLimiter interface {
	Allow(ctx context.Context, key string, limit int, now time.Time) (bool, error)
}

// ConcurrencyLimiter 定义客户端和账号并发租约边界。
type ConcurrencyLimiter interface {
	Acquire(ctx context.Context, key string, limit int) (release func(), acquired bool, err error)
	Current(ctx context.Context, key string) (int, error)
}

// ConcurrencySnapshotReader 批量读取并发租约快照；调度器会优先使用它减少远程运行态往返。
type ConcurrencySnapshotReader interface {
	CurrentMany(ctx context.Context, keys []string) (map[string]int, error)
}

// StickySessionRepository 定义有过期时间的 prompt_cache_key 账号粘滞状态。
type StickySessionRepository interface {
	Get(ctx context.Context, promptCacheKey string, now time.Time) (uint64, bool, error)
	Set(ctx context.Context, promptCacheKey string, accountID uint64, expiresAt time.Time) error
	DeleteByAccount(ctx context.Context, accountID uint64) error
}

// DeviceSessionRepository 定义短期 Device OAuth 会话状态。
type DeviceSessionRepository interface {
	Create(ctx context.Context, value account.DeviceSession) error
	Get(ctx context.Context, id string, now time.Time) (account.DeviceSession, error)
	Update(ctx context.Context, value account.DeviceSession) error
	Delete(ctx context.Context, id string) error
}

// DistributedLock 定义跨实例的短期互斥租约，用于避免同一凭据被并发刷新。
type DistributedLock interface {
	Acquire(ctx context.Context, key string, ttl time.Duration) (release func(), acquired bool, err error)
}

// SettingsChangeBus 在多实例之间传递运行设置已变更的通知，设置内容仍以数据库为准。
type SettingsChangeBus interface {
	PublishSettingsChanged(ctx context.Context) error
	ListenSettingsChanges(ctx context.Context, handler func(context.Context) error) error
}

// QuotaRecoveryQueue 保存分模式额度的到期探测事件，支持多实例原子认领。
type QuotaRecoveryQueue interface {
	ScheduleQuotaRecovery(ctx context.Context, value account.QuotaRecoveryEvent) error
	EnsureQuotaRecovery(ctx context.Context, value account.QuotaRecoveryEvent) error
	ClaimDueQuotaRecoveries(ctx context.Context, now time.Time, limit int, lease time.Duration) ([]account.QuotaRecoveryEvent, error)
	AckQuotaRecovery(ctx context.Context, value account.QuotaRecoveryEvent) error
	RescheduleQuotaRecovery(ctx context.Context, value account.QuotaRecoveryEvent) error
}
