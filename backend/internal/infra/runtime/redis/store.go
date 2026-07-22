package redis

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	redisclient "github.com/redis/go-redis/v9"
)

const (
	concurrencyLeaseGrace       = time.Minute
	maxStickyBindingsPerAccount = 10000
	maxDeviceSessions           = 1000
	maxQuotaRecoveryEvents      = 100000
	maxQuotaRefreshDirty        = 100000
	observedModelStateTTL       = 30 * time.Minute
)

var rateScript = redisclient.NewScript(`
local current = redis.call('INCR', KEYS[1])
if current == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[2]) end
if current > tonumber(ARGV[1]) then return 0 end
return 1
`)

var acquireLeaseScript = redisclient.NewScript(`
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', ARGV[1])
if redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[2]) then return 0 end
redis.call('ZADD', KEYS[1], ARGV[3], ARGV[4])
redis.call('PEXPIRE', KEYS[1], ARGV[5])
return 1
`)

var releaseLeaseScript = redisclient.NewScript(`return redis.call('ZREM', KEYS[1], ARGV[1])`)

var releaseLockScript = redisclient.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0
`)

var setStickyScript = redisclient.NewScript(`
local old = redis.call('GET', KEYS[1])
if old and old ~= ARGV[1] then redis.call('ZREM', ARGV[3] .. old, KEYS[1]) end
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', ARGV[4])
redis.call('ZADD', KEYS[2], ARGV[5], KEYS[1])
local excess = redis.call('ZCARD', KEYS[2]) - tonumber(ARGV[6])
if excess > 0 then
  local stale = redis.call('ZRANGE', KEYS[2], 0, excess - 1)
  for _, key in ipairs(stale) do
    if redis.call('GET', key) == ARGV[1] then redis.call('DEL', key) end
    redis.call('ZREM', KEYS[2], key)
  end
end
if redis.call('PTTL', KEYS[2]) < tonumber(ARGV[2]) then redis.call('PEXPIRE', KEYS[2], ARGV[2]) end
return 1
`)

var bindStickyScript = redisclient.NewScript(`
local selected = redis.call('GET', KEYS[1])
if not selected then selected = ARGV[1] end
local accountSetKey = ARGV[3] .. selected
redis.call('SET', KEYS[1], selected, 'PX', ARGV[2])
redis.call('ZREMRANGEBYSCORE', accountSetKey, '-inf', ARGV[4])
redis.call('ZADD', accountSetKey, ARGV[5], KEYS[1])
local excess = redis.call('ZCARD', accountSetKey) - tonumber(ARGV[6])
if excess > 0 then
  local stale = redis.call('ZRANGE', accountSetKey, 0, excess - 1)
  for _, key in ipairs(stale) do
    if redis.call('GET', key) == selected then redis.call('DEL', key) end
    redis.call('ZREM', accountSetKey, key)
  end
end
if redis.call('PTTL', accountSetKey) < tonumber(ARGV[2]) then redis.call('PEXPIRE', accountSetKey, ARGV[2]) end
return selected
`)

var deleteStickyByAccountScript = redisclient.NewScript(`
local members = redis.call('ZRANGE', KEYS[1], 0, -1)
local deleted = 0
for _, key in ipairs(members) do
  if redis.call('GET', key) == ARGV[1] then
    deleted = deleted + redis.call('DEL', key)
  end
end
redis.call('DEL', KEYS[1])
return deleted
`)

var createDeviceSessionScript = redisclient.NewScript(`
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', ARGV[2])
if redis.call('ZCARD', KEYS[2]) >= tonumber(ARGV[4]) then return 0 end
if not redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[3], 'NX') then return -1 end
redis.call('ZADD', KEYS[2], ARGV[5], KEYS[1])
if redis.call('PTTL', KEYS[2]) < tonumber(ARGV[3]) then redis.call('PEXPIRE', KEYS[2], ARGV[3]) end
return 1
`)

var updateDeviceSessionScript = redisclient.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then return 0 end
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2], 'XX')
redis.call('ZADD', KEYS[2], ARGV[3], KEYS[1])
if redis.call('PTTL', KEYS[2]) < tonumber(ARGV[2]) then redis.call('PEXPIRE', KEYS[2], ARGV[2]) end
return 1
`)

var deleteDeviceSessionScript = redisclient.NewScript(`
redis.call('ZREM', KEYS[2], KEYS[1])
return redis.call('DEL', KEYS[1])
`)

var scheduleQuotaRecoveryScript = redisclient.NewScript(`
if not redis.call('ZSCORE', KEYS[1], ARGV[1]) and redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[4]) then return 0 end
redis.call('ZADD', KEYS[1], ARGV[2], ARGV[1])
redis.call('HSET', KEYS[2], ARGV[1], ARGV[3])
redis.call('HDEL', KEYS[3], ARGV[1])
return 1
`)

var ensureQuotaRecoveryScript = redisclient.NewScript(`
if redis.call('ZSCORE', KEYS[1], ARGV[1]) then return 2 end
if redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[4]) then return 0 end
redis.call('ZADD', KEYS[1], ARGV[2], ARGV[1])
redis.call('HSET', KEYS[2], ARGV[1], ARGV[3])
return 1
`)

var claimQuotaRecoveryScript = redisclient.NewScript(`
local values = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1], 'LIMIT', 0, ARGV[2])
local result = {}
for _, value in ipairs(values) do
  redis.call('ZADD', KEYS[1], ARGV[3], value)
  redis.call('HSET', KEYS[3], value, ARGV[4])
  table.insert(result, value)
  table.insert(result, redis.call('HGET', KEYS[2], value) or '0')
  table.insert(result, ARGV[4])
end
return result
`)

var ackQuotaRecoveryScript = redisclient.NewScript(`
if redis.call('HGET', KEYS[3], ARGV[1]) ~= ARGV[2] then return 0 end
redis.call('HDEL', KEYS[2], ARGV[1])
redis.call('HDEL', KEYS[3], ARGV[1])
return redis.call('ZREM', KEYS[1], ARGV[1])
`)

var markQuotaRefreshDirtyScript = redisclient.NewScript(`
local now = tonumber(ARGV[4])
local expired = redis.call('ZRANGEBYSCORE', KEYS[3], '-inf', now, 'LIMIT', 0, 1000)
for _, member in ipairs(expired) do
  redis.call('ZREM', KEYS[3], member)
  redis.call('ZREM', KEYS[2], member)
  redis.call('HDEL', KEYS[1], member)
end
local memberExpires = redis.call('ZSCORE', KEYS[3], ARGV[1])
if memberExpires and tonumber(memberExpires) <= now then
  redis.call('ZREM', KEYS[3], ARGV[1])
  redis.call('ZREM', KEYS[2], ARGV[1])
  redis.call('HDEL', KEYS[1], ARGV[1])
end
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', now)
if not redis.call('ZSCORE', KEYS[2], ARGV[1]) and redis.call('ZCARD', KEYS[2]) >= tonumber(ARGV[3]) then return 0 end
local generation = redis.call('HINCRBY', KEYS[1], ARGV[1], 1)
redis.call('ZADD', KEYS[2], ARGV[2], ARGV[1])
redis.call('ZADD', KEYS[3], ARGV[2], ARGV[1])
local latest = redis.call('ZREVRANGE', KEYS[3], 0, 0, 'WITHSCORES')
if #latest == 2 then
  redis.call('PEXPIREAT', KEYS[1], latest[2])
  redis.call('PEXPIREAT', KEYS[2], latest[2])
  redis.call('PEXPIREAT', KEYS[3], latest[2])
end
return generation
`)

var clearQuotaRefreshDirtyScript = redisclient.NewScript(`
local retentionExpires = redis.call('ZSCORE', KEYS[3], ARGV[1])
if not retentionExpires or tonumber(retentionExpires) <= tonumber(ARGV[3]) then
  redis.call('ZREM', KEYS[3], ARGV[1])
  redis.call('ZREM', KEYS[2], ARGV[1])
  redis.call('HDEL', KEYS[1], ARGV[1])
  return 0
end
local dirtyExpires = redis.call('ZSCORE', KEYS[2], ARGV[1])
if not dirtyExpires or tonumber(dirtyExpires) <= tonumber(ARGV[3]) then
  redis.call('ZREM', KEYS[2], ARGV[1])
  return 0
end
if tonumber(redis.call('HGET', KEYS[1], ARGV[1]) or '0') ~= tonumber(ARGV[2]) then return 0 end
redis.call('ZREM', KEYS[2], ARGV[1])
return 1
`)

var listQuotaRefreshDirtyScript = redisclient.NewScript(`
local limit = tonumber(ARGV[2])
local now = tonumber(ARGV[1])
local expired = redis.call('ZRANGEBYSCORE', KEYS[3], '-inf', now, 'LIMIT', 0, 1000)
for _, member in ipairs(expired) do
  redis.call('ZREM', KEYS[3], member)
  redis.call('ZREM', KEYS[2], member)
  redis.call('HDEL', KEYS[1], member)
end
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', now)
local members = redis.call('ZRANGEBYSCORE', KEYS[2], '(' .. ARGV[1], '+inf', 'LIMIT', 0, limit)
local result = {}
for _, member in ipairs(members) do
  local retentionExpires = redis.call('ZSCORE', KEYS[3], member)
  local generation = redis.call('HGET', KEYS[1], member)
  if retentionExpires and tonumber(retentionExpires) > now and generation then
    table.insert(result, member)
    table.insert(result, generation)
  end
end
return result
`)

var setObservedModelStateScript = redisclient.NewScript(`
local previous = redis.call('HGET', KEYS[1], 'observed_at')
if previous and tonumber(previous) > tonumber(ARGV[2]) then return 0 end
redis.call('HSET', KEYS[1], 'model', ARGV[1], 'observed_at', ARGV[2])
redis.call('PEXPIRE', KEYS[1], ARGV[3])
return 1
`)

var quotaRefreshStateScript = redisclient.NewScript(`
local generation = redis.call('HGET', KEYS[1], ARGV[1]) or '0'
local retentionExpires = redis.call('ZSCORE', KEYS[3], ARGV[1])
if not retentionExpires or tonumber(retentionExpires) <= tonumber(ARGV[2]) then
  redis.call('ZREM', KEYS[3], ARGV[1])
  redis.call('ZREM', KEYS[2], ARGV[1])
  redis.call('HDEL', KEYS[1], ARGV[1])
  generation = '0'
  return {generation, '0'}
end
local dirtyExpires = redis.call('ZSCORE', KEYS[2], ARGV[1])
local dirty = '0'
if dirtyExpires and tonumber(dirtyExpires) > tonumber(ARGV[2]) then
  dirty = '1'
elseif dirtyExpires then
  redis.call('ZREM', KEYS[2], ARGV[1])
end
return {generation, dirty}
`)

var rescheduleQuotaRecoveryScript = redisclient.NewScript(`
if redis.call('HGET', KEYS[3], ARGV[1]) ~= ARGV[4] then return 0 end
redis.call('ZADD', KEYS[1], ARGV[2], ARGV[1])
redis.call('HSET', KEYS[2], ARGV[1], ARGV[3])
redis.call('HDEL', KEYS[3], ARGV[1])
return 1
`)

// Config 表示 Redis 运行态存储的启动配置。
type Config struct {
	Address          string
	Username         string
	Password         string
	Database         int
	KeyPrefix        string
	TLS              bool
	ConcurrencyLease time.Duration
}

// Store 实现多实例共享的限流、并发租约、粘滞路由、Device OAuth 会话和分布式锁。
type Store struct {
	client           *redisclient.Client
	prefix           string
	concurrencyLease time.Duration
}

// Open 连接 Redis；选中的 Redis 不可用时直接返回启动错误。
func Open(ctx context.Context, cfg Config) (*Store, error) {
	options := &redisclient.Options{Addr: cfg.Address, Username: cfg.Username, Password: cfg.Password, DB: cfg.Database}
	if cfg.TLS {
		options.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	client := redisclient.NewClient(options)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("连接 Redis: %w", err)
	}
	lease := cfg.ConcurrencyLease
	if lease <= 0 {
		lease = 3 * time.Hour
	}
	return &Store{client: client, prefix: cfg.KeyPrefix, concurrencyLease: lease}, nil
}

func (s *Store) Close() error { return s.client.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.client.Ping(ctx).Err() }

func (s *Store) key(namespace, key string) string { return s.prefix + namespace + ":" + key }

func (s *Store) GetObservedModelState(ctx context.Context, accountID uint64) (repository.ObservedModelState, bool, error) {
	if accountID == 0 {
		return repository.ObservedModelState{}, false, nil
	}
	values, err := s.client.HMGet(ctx, s.key("observed-model", strconv.FormatUint(accountID, 10)), "model", "observed_at").Result()
	if err != nil {
		return repository.ObservedModelState{}, false, err
	}
	if len(values) != 2 || values[0] == nil || values[1] == nil {
		return repository.ObservedModelState{}, false, nil
	}
	model, ok := values[0].(string)
	if !ok || strings.TrimSpace(model) == "" {
		return repository.ObservedModelState{}, false, nil
	}
	observedMillis, err := strconv.ParseInt(fmt.Sprint(values[1]), 10, 64)
	if err != nil || observedMillis <= 0 {
		return repository.ObservedModelState{}, false, nil
	}
	return repository.ObservedModelState{Model: model, ObservedAt: time.UnixMilli(observedMillis).UTC()}, true, nil
}

func (s *Store) SetObservedModelState(ctx context.Context, accountID uint64, value repository.ObservedModelState, ttl time.Duration) error {
	if accountID == 0 || strings.TrimSpace(value.Model) == "" || value.ObservedAt.IsZero() {
		return nil
	}
	if ttl <= 0 {
		ttl = observedModelStateTTL
	}
	return setObservedModelStateScript.Run(ctx, s.client,
		[]string{s.key("observed-model", strconv.FormatUint(accountID, 10))},
		strings.TrimSpace(value.Model), value.ObservedAt.UTC().UnixMilli(), ttl.Milliseconds()).Err()
}

// PublishSettingsChanged 发布运行设置失效通知，不在 Redis 中复制设置内容。
func (s *Store) PublishSettingsChanged(ctx context.Context) error {
	return s.client.Publish(ctx, s.key("events", "settings"), "reload").Err()
}

func (s *Store) PublishInvalidation(ctx context.Context, event repository.InvalidationEvent) error {
	if !event.Valid() {
		return errors.New("invalid invalidation event")
	}
	if event.PublishedAt.IsZero() {
		event.PublishedAt = time.Now().UTC()
	}
	revision, err := s.client.Incr(ctx, s.key("invalidation-revision", string(event.Layer())+":"+string(event.Provider))).Result()
	if err != nil {
		return err
	}
	event.Revision = uint64(revision)
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return s.client.Publish(ctx, s.key("events", "invalidation"), payload).Err()
}

func (s *Store) ListenInvalidations(ctx context.Context, handler func(context.Context, repository.InvalidationEvent) error) error {
	pubsub := s.client.Subscribe(ctx, s.key("events", "invalidation"))
	defer pubsub.Close()
	if _, err := pubsub.Receive(ctx); err != nil {
		return err
	}
	channel := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case message, ok := <-channel:
			if !ok {
				return errors.New("Redis invalidation channel closed")
			}
			var event repository.InvalidationEvent
			if err := json.Unmarshal([]byte(message.Payload), &event); err != nil || !event.Valid() {
				continue
			}
			if err := handler(ctx, event); err != nil {
				return err
			}
		}
	}
}

// ListenSettingsChanges 监听设置变更并调用重载函数，go-redis 会在连接中断后自动重连。
func (s *Store) ListenSettingsChanges(ctx context.Context, handler func(context.Context) error) error {
	pubsub := s.client.Subscribe(ctx, s.key("events", "settings"))
	defer pubsub.Close()
	if _, err := pubsub.Receive(ctx); err != nil {
		return err
	}
	channel := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-channel:
			if !ok {
				return errors.New("Redis 设置通知通道已关闭")
			}
			if err := handler(ctx); err != nil {
				return err
			}
		}
	}
}

func (s *Store) Allow(ctx context.Context, key string, limit int, _ time.Time) (bool, error) {
	if limit <= 0 {
		return true, nil
	}
	result, err := rateScript.Run(ctx, s.client, []string{s.key("rate", key)}, limit, time.Minute.Milliseconds()).Int()
	return result == 1, err
}

func (s *Store) acquireConcurrency(ctx context.Context, key string, limit int) (func(), bool, error) {
	if limit <= 0 {
		return func() {}, true, nil
	}
	token, err := randomToken()
	if err != nil {
		return nil, false, err
	}
	now := time.Now().UTC()
	expiresAt := now.Add(s.concurrencyLease)
	redisKey := s.key("concurrency", key)
	result, err := acquireLeaseScript.Run(ctx, s.client, []string{redisKey}, now.UnixMilli(), limit, expiresAt.UnixMilli(), token, (s.concurrencyLease + concurrencyLeaseGrace).Milliseconds()).Int()
	if err != nil || result != 1 {
		return nil, false, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = releaseLeaseScript.Run(releaseCtx, s.client, []string{redisKey}, token).Err()
		})
	}, true, nil
}

func (s *Store) Current(ctx context.Context, key string) (int, error) {
	redisKey := s.key("concurrency", key)
	now := time.Now().UTC().UnixMilli()
	pipe := s.client.TxPipeline()
	pipe.ZRemRangeByScore(ctx, redisKey, "-inf", strconv.FormatInt(now, 10))
	count := pipe.ZCard(ctx, redisKey)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}
	return int(count.Val()), nil
}

func (s *Store) CurrentMany(ctx context.Context, keys []string) (map[string]int, error) {
	values := make(map[string]int, len(keys))
	if len(keys) == 0 {
		return values, nil
	}
	now := "(" + strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	pipe := s.client.Pipeline()
	counts := make(map[string]*redisclient.IntCmd, len(keys))
	for _, key := range keys {
		redisKey := s.key("concurrency", key)
		counts[key] = pipe.ZCount(ctx, redisKey, now, "+inf")
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	for key, count := range counts {
		values[key] = int(count.Val())
	}
	return values, nil
}

func (s *Store) Get(ctx context.Context, key string, now time.Time) (uint64, bool, error) {
	value, err := s.client.Get(ctx, s.key("sticky", key)).Result()
	if errors.Is(err, redisclient.Nil) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	id, err := strconv.ParseUint(value, 10, 64)
	return id, err == nil, err
}

func (s *Store) Bind(ctx context.Context, key string, proposedAccountID uint64, now, expiresAt time.Time) (uint64, error) {
	if key == "" || proposedAccountID == 0 {
		return proposedAccountID, nil
	}
	ttl := time.Until(expiresAt)
	if ttl <= 0 {
		return proposedAccountID, nil
	}
	id := strconv.FormatUint(proposedAccountID, 10)
	value, err := bindStickyScript.Run(
		ctx,
		s.client,
		[]string{s.key("sticky", key)},
		id,
		ttl.Milliseconds(),
		s.prefix+"sticky-account:",
		now.UnixMilli(),
		expiresAt.UnixMilli(),
		maxStickyBindingsPerAccount,
	).Uint64()
	if err != nil {
		return 0, err
	}
	return value, nil
}

func (s *Store) Set(ctx context.Context, key string, accountID uint64, expiresAt time.Time) error {
	ttl := time.Until(expiresAt)
	if ttl <= 0 {
		return nil
	}
	id := strconv.FormatUint(accountID, 10)
	bindingKey := s.key("sticky", key)
	accountSetPrefix := s.prefix + "sticky-account:"
	accountSetKey := accountSetPrefix + id
	now := time.Now().UTC()
	return setStickyScript.Run(ctx, s.client, []string{bindingKey, accountSetKey}, id, ttl.Milliseconds(), accountSetPrefix, now.UnixMilli(), expiresAt.UnixMilli(), maxStickyBindingsPerAccount).Err()
}

func (s *Store) DeleteByAccount(ctx context.Context, accountID uint64) error {
	id := strconv.FormatUint(accountID, 10)
	return deleteStickyByAccountScript.Run(ctx, s.client, []string{s.key("sticky-account", id)}, id).Err()
}

func (s *Store) ScheduleQuotaRecovery(ctx context.Context, value account.QuotaRecoveryEvent) error {
	if value.AccountID == 0 || value.Mode == "" || value.DueAt.IsZero() {
		return fmt.Errorf("额度恢复事件无效")
	}
	member := strconv.FormatUint(value.AccountID, 10) + ":" + value.Mode
	result, err := scheduleQuotaRecoveryScript.Run(ctx, s.client, []string{s.key("quota-recovery", "events"), s.key("quota-recovery", "attempts"), s.key("quota-recovery", "claims")}, member, value.DueAt.UnixMilli(), max(0, value.Attempts), maxQuotaRecoveryEvents).Int()
	if err != nil {
		return err
	}
	if result == 0 {
		return fmt.Errorf("额度恢复队列已满")
	}
	return nil
}

func (s *Store) MarkQuotaRefreshDirty(ctx context.Context, accountID uint64, mode string, ttl time.Duration) (uint64, error) {
	mode = strings.TrimSpace(mode)
	if accountID == 0 || mode == "" || ttl <= 0 {
		return 0, fmt.Errorf("quota refresh identity is invalid")
	}
	member := strconv.FormatUint(accountID, 10) + ":" + mode
	now := time.Now().UTC()
	expiresAt := now.Add(ttl)
	generation, err := markQuotaRefreshDirtyScript.Run(ctx, s.client,
		[]string{s.key("quota-refresh", "generations"), s.key("quota-refresh", "dirty"), s.key("quota-refresh", "expiry")},
		member, expiresAt.UnixMilli(), maxQuotaRefreshDirty, now.UnixMilli(),
	).Uint64()
	if err != nil {
		return 0, err
	}
	if generation == 0 {
		return 0, fmt.Errorf("quota refresh dirty set is full")
	}
	return generation, nil
}

func (s *Store) QuotaRefreshGeneration(ctx context.Context, accountID uint64, mode string) (uint64, bool, error) {
	member := strconv.FormatUint(accountID, 10) + ":" + strings.TrimSpace(mode)
	values, err := quotaRefreshStateScript.Run(ctx, s.client,
		[]string{s.key("quota-refresh", "generations"), s.key("quota-refresh", "dirty"), s.key("quota-refresh", "expiry")}, member, time.Now().UTC().UnixMilli(),
	).StringSlice()
	if err != nil {
		return 0, false, err
	}
	if len(values) != 2 {
		return 0, false, fmt.Errorf("quota refresh state response is invalid")
	}
	generation, err := strconv.ParseUint(values[0], 10, 64)
	return generation, values[1] == "1", err
}

func (s *Store) ClearQuotaRefreshDirty(ctx context.Context, accountID uint64, mode string, generation uint64) (bool, error) {
	member := strconv.FormatUint(accountID, 10) + ":" + strings.TrimSpace(mode)
	result, err := clearQuotaRefreshDirtyScript.Run(ctx, s.client,
		[]string{s.key("quota-refresh", "generations"), s.key("quota-refresh", "dirty"), s.key("quota-refresh", "expiry")},
		member, generation, time.Now().UTC().UnixMilli(),
	).Int()
	return result == 1, err
}

func (s *Store) ListQuotaRefreshDirty(ctx context.Context, now time.Time, limit int) ([]repository.QuotaRefreshDirty, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	values, err := listQuotaRefreshDirtyScript.Run(ctx, s.client,
		[]string{s.key("quota-refresh", "generations"), s.key("quota-refresh", "dirty"), s.key("quota-refresh", "expiry")},
		now.UnixMilli(), limit,
	).StringSlice()
	if err != nil {
		return nil, err
	}
	result := make([]repository.QuotaRefreshDirty, 0, min(limit, len(values)/2))
	for index := 0; index+1 < len(values); index += 2 {
		member := values[index]
		separator := strings.IndexByte(member, ':')
		if separator <= 0 || separator == len(member)-1 {
			continue
		}
		accountID, parseErr := strconv.ParseUint(member[:separator], 10, 64)
		generation, generationErr := strconv.ParseUint(values[index+1], 10, 64)
		if parseErr != nil || generationErr != nil || accountID == 0 || generation == 0 {
			continue
		}
		result = append(result, repository.QuotaRefreshDirty{AccountID: accountID, Mode: member[separator+1:], Generation: generation})
	}
	return result, nil
}

func (s *Store) EnsureQuotaRecovery(ctx context.Context, value account.QuotaRecoveryEvent) error {
	if value.AccountID == 0 || value.Mode == "" || value.DueAt.IsZero() {
		return fmt.Errorf("额度恢复事件无效")
	}
	member := strconv.FormatUint(value.AccountID, 10) + ":" + value.Mode
	result, err := ensureQuotaRecoveryScript.Run(ctx, s.client, []string{s.key("quota-recovery", "events"), s.key("quota-recovery", "attempts")}, member, value.DueAt.UnixMilli(), max(0, value.Attempts), maxQuotaRecoveryEvents).Int()
	if err != nil {
		return err
	}
	if result == 0 {
		return fmt.Errorf("额度恢复队列已满")
	}
	return nil
}

func (s *Store) ClaimDueQuotaRecoveries(ctx context.Context, now time.Time, limit int, lease time.Duration) ([]account.QuotaRecoveryEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	claimToken, err := randomToken()
	if err != nil {
		return nil, err
	}
	values, err := claimQuotaRecoveryScript.Run(ctx, s.client, []string{s.key("quota-recovery", "events"), s.key("quota-recovery", "attempts"), s.key("quota-recovery", "claims")}, now.UnixMilli(), limit, now.Add(lease).UnixMilli(), claimToken).StringSlice()
	if err != nil {
		return nil, err
	}
	result := make([]account.QuotaRecoveryEvent, 0, len(values)/3)
	for index := 0; index+2 < len(values); index += 3 {
		raw := values[index]
		idText, mode, ok := strings.Cut(raw, ":")
		id, parseErr := strconv.ParseUint(idText, 10, 64)
		attempts, attemptsErr := strconv.Atoi(values[index+1])
		if ok && parseErr == nil && id > 0 && mode != "" {
			if attemptsErr != nil || attempts < 0 {
				attempts = 0
			}
			result = append(result, account.QuotaRecoveryEvent{AccountID: id, Mode: mode, DueAt: now, Attempts: attempts, ClaimToken: values[index+2]})
		}
	}
	return result, nil
}

func (s *Store) AckQuotaRecovery(ctx context.Context, value account.QuotaRecoveryEvent) error {
	member := strconv.FormatUint(value.AccountID, 10) + ":" + value.Mode
	result, err := ackQuotaRecoveryScript.Run(ctx, s.client, []string{s.key("quota-recovery", "events"), s.key("quota-recovery", "attempts"), s.key("quota-recovery", "claims")}, member, value.ClaimToken).Int()
	if err != nil {
		return err
	}
	if result == 0 {
		return repository.ErrConflict
	}
	return nil
}

func (s *Store) RescheduleQuotaRecovery(ctx context.Context, value account.QuotaRecoveryEvent) error {
	member := strconv.FormatUint(value.AccountID, 10) + ":" + value.Mode
	result, err := rescheduleQuotaRecoveryScript.Run(ctx, s.client, []string{s.key("quota-recovery", "events"), s.key("quota-recovery", "attempts"), s.key("quota-recovery", "claims")}, member, value.DueAt.UnixMilli(), max(0, value.Attempts), value.ClaimToken).Int()
	if err != nil {
		return err
	}
	if result == 0 {
		return repository.ErrConflict
	}
	return nil
}

func (s *Store) Create(ctx context.Context, value account.DeviceSession) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	ttl := time.Until(value.ExpiresAt)
	if ttl <= 0 {
		return repository.ErrNotFound
	}
	now := time.Now().UTC()
	result, err := createDeviceSessionScript.Run(ctx, s.client, []string{s.key("device", value.ID), s.key("device-index", "sessions")}, payload, now.UnixMilli(), ttl.Milliseconds(), maxDeviceSessions, value.ExpiresAt.UnixMilli()).Int()
	if err != nil {
		return err
	}
	if result != 1 {
		return repository.ErrConflict
	}
	return nil
}

func (s *Store) GetDevice(ctx context.Context, id string, now time.Time) (account.DeviceSession, error) {
	payload, err := s.client.Get(ctx, s.key("device", id)).Bytes()
	if errors.Is(err, redisclient.Nil) {
		return account.DeviceSession{}, repository.ErrNotFound
	}
	if err != nil {
		return account.DeviceSession{}, err
	}
	var value account.DeviceSession
	if err := json.Unmarshal(payload, &value); err != nil {
		return account.DeviceSession{}, err
	}
	if !now.Before(value.ExpiresAt) {
		_ = deleteDeviceSessionScript.Run(ctx, s.client, []string{s.key("device", id), s.key("device-index", "sessions")}).Err()
		return account.DeviceSession{}, repository.ErrNotFound
	}
	return value, nil
}

func (s *Store) Update(ctx context.Context, value account.DeviceSession) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	ttl := time.Until(value.ExpiresAt)
	if ttl <= 0 {
		return repository.ErrNotFound
	}
	result, err := updateDeviceSessionScript.Run(ctx, s.client, []string{s.key("device", value.ID), s.key("device-index", "sessions")}, payload, ttl.Milliseconds(), value.ExpiresAt.UnixMilli()).Int()
	if err != nil {
		return err
	}
	if result != 1 {
		return repository.ErrNotFound
	}
	return nil
}

func (s *Store) Delete(ctx context.Context, id string) error {
	return deleteDeviceSessionScript.Run(ctx, s.client, []string{s.key("device", id), s.key("device-index", "sessions")}).Err()
}

func (s *Store) acquireLock(ctx context.Context, key string, ttl time.Duration) (func(), bool, error) {
	token, err := randomToken()
	if err != nil {
		return nil, false, err
	}
	redisKey := s.key("lock", key)
	acquired, err := s.client.SetNX(ctx, redisKey, token, ttl).Result()
	if err != nil || !acquired {
		return nil, acquired, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = releaseLockScript.Run(releaseCtx, s.client, []string{redisKey}, token).Err()
		})
	}, true, nil
}

func randomToken() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

// DeviceSessionStore 适配 DeviceSessionRepository，避免与 StickySessionRepository 的 Get 签名冲突。
type DeviceSessionStore struct{ store *Store }

func NewDeviceSessionStore(store *Store) *DeviceSessionStore {
	return &DeviceSessionStore{store: store}
}
func (s *DeviceSessionStore) Create(ctx context.Context, value account.DeviceSession) error {
	return s.store.Create(ctx, value)
}
func (s *DeviceSessionStore) Get(ctx context.Context, id string, now time.Time) (account.DeviceSession, error) {
	return s.store.GetDevice(ctx, id, now)
}
func (s *DeviceSessionStore) Update(ctx context.Context, value account.DeviceSession) error {
	return s.store.Update(ctx, value)
}
func (s *DeviceSessionStore) Delete(ctx context.Context, id string) error {
	return s.store.Delete(ctx, id)
}

// ConcurrencyLimiter 适配 ConcurrencyLimiter，避免与 DistributedLock 的 Acquire 签名冲突。
type ConcurrencyLimiter struct{ store *Store }

func NewConcurrencyLimiter(store *Store) *ConcurrencyLimiter {
	return &ConcurrencyLimiter{store: store}
}
func (l *ConcurrencyLimiter) Acquire(ctx context.Context, key string, limit int) (func(), bool, error) {
	return l.store.acquireConcurrency(ctx, key, limit)
}
func (l *ConcurrencyLimiter) Current(ctx context.Context, key string) (int, error) {
	return l.store.Current(ctx, key)
}
func (l *ConcurrencyLimiter) CurrentMany(ctx context.Context, keys []string) (map[string]int, error) {
	return l.store.CurrentMany(ctx, keys)
}

// LockStore 适配 DistributedLock。
type LockStore struct{ store *Store }

func NewLockStore(store *Store) *LockStore { return &LockStore{store: store} }
func (l *LockStore) Acquire(ctx context.Context, key string, ttl time.Duration) (func(), bool, error) {
	return l.store.acquireLock(ctx, strings.TrimSpace(key), ttl)
}
