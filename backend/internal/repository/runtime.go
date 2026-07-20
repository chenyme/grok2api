package repository

import (
	"context"
	"strconv"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// AccountConcurrencyKey 返回账号推理租约使用的统一运行态键。
// 账号维护任务必须复用该键判断是否仍有请求占用，避免复制协议字符串后发生漂移。
func AccountConcurrencyKey(accountID uint64) string {
	return "account:" + strconv.FormatUint(accountID, 10)
}

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

// StickySessionRepository 定义有过期时间的会话账号粘滞状态。
type StickySessionRepository interface {
	Get(ctx context.Context, affinityKey string, now time.Time) (uint64, bool, error)
	// Bind 原子保留已有有效绑定并刷新有效期；仅在绑定不存在或已过期时采用 proposedAccountID。
	Bind(ctx context.Context, affinityKey string, proposedAccountID uint64, now, expiresAt time.Time) (accountID uint64, err error)
	// Set 强制替换绑定，仅用于原账号已经确定不再适合当前请求时重新绑定。
	Set(ctx context.Context, affinityKey string, accountID uint64, expiresAt time.Time) error
	DeleteByAccount(ctx context.Context, accountID uint64) error
}

// ReasoningReplayRepository 保存无状态多轮所需的上一轮可回放 output items。
// key 边界为 model + sessionKey；sessionKey 应使用已隔离的 PromptCacheKey。
type ReasoningReplayRepository interface {
	Get(ctx context.Context, model, sessionKey string, now time.Time, ttl time.Duration) (items [][]byte, ok bool, err error)
	Set(ctx context.Context, model, sessionKey string, items [][]byte, expiresAt time.Time) error
	Delete(ctx context.Context, model, sessionKey string) error
}

// DeviceSessionRepository 定义短期 Device OAuth 会话状态。
type DeviceSessionRepository interface {
	Create(ctx context.Context, value account.DeviceSession) error
	Get(ctx context.Context, id string, now time.Time) (account.DeviceSession, error)
	Update(ctx context.Context, value account.DeviceSession) error
	Delete(ctx context.Context, id string) error
}

// DistributedLock 定义跨实例的短期互斥租约，用于避免同一账号维护任务被并发执行。
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
