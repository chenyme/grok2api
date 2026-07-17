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

// PublishSettingsChanged 发布运行设置失效通知，不在 Redis 中复制设置内容。
func (s *Store) PublishSettingsChanged(ctx context.Context) error {
	return s.client.Publish(ctx, s.key("events", "settings"), "reload").Err()
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
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	pipe := s.client.Pipeline()
	counts := make(map[string]*redisclient.IntCmd, len(keys))
	for _, key := range keys {
		redisKey := s.key("concurrency", key)
		pipe.ZRemRangeByScore(ctx, redisKey, "-inf", now)
		counts[key] = pipe.ZCard(ctx, redisKey)
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
