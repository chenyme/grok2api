package memory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

const (
	maxEntries        = 10000
	maxDeviceSessions = 1000
	shardCount        = 64
)

type rateWindow struct {
	startedAt time.Time
	count     int
}

// RateLimiter 提供单实例固定分钟窗口限流。
type RateLimiter struct {
	shards [shardCount]rateShard
}

type rateShard struct {
	mu      sync.Mutex
	windows map[string]rateWindow
}

func NewRateLimiter() *RateLimiter {
	limiter := &RateLimiter{}
	for index := range limiter.shards {
		limiter.shards[index].windows = make(map[string]rateWindow)
	}
	return limiter
}

func (r *RateLimiter) Allow(_ context.Context, key string, limit int, now time.Time) (bool, error) {
	if limit <= 0 {
		return true, nil
	}
	shard := &r.shards[shardIndex(key)]
	shard.mu.Lock()
	defer shard.mu.Unlock()
	window := shard.windows[key]
	if window.startedAt.IsZero() || now.Sub(window.startedAt) >= time.Minute {
		window = rateWindow{startedAt: now, count: 0}
	}
	if window.count >= limit {
		shard.windows[key] = window
		return false, nil
	}
	window.count++
	shard.windows[key] = window
	if len(shard.windows) > maxEntriesPerShard() {
		cleanupRateShard(shard, now)
	}
	return true, nil
}

func cleanupRateShard(shard *rateShard, now time.Time) {
	for key, window := range shard.windows {
		if now.Sub(window.startedAt) >= time.Minute {
			delete(shard.windows, key)
		}
	}
	for len(shard.windows) > maxEntriesPerShard() {
		var oldestKey string
		var oldest time.Time
		for key, window := range shard.windows {
			if oldestKey == "" || window.startedAt.Before(oldest) {
				oldestKey = key
				oldest = window.startedAt
			}
		}
		delete(shard.windows, oldestKey)
	}
}

// ConcurrencyLimiter 提供单实例并发租约。
type ConcurrencyLimiter struct {
	shards [shardCount]concurrencyShard
}

type concurrencyShard struct {
	mu     sync.Mutex
	counts map[string]int
}

func NewConcurrencyLimiter() *ConcurrencyLimiter {
	limiter := &ConcurrencyLimiter{}
	for index := range limiter.shards {
		limiter.shards[index].counts = make(map[string]int)
	}
	return limiter
}

func (l *ConcurrencyLimiter) Acquire(_ context.Context, key string, limit int) (func(), bool, error) {
	if limit <= 0 {
		return func() {}, true, nil
	}
	shard := &l.shards[shardIndex(key)]
	shard.mu.Lock()
	if shard.counts[key] >= limit {
		shard.mu.Unlock()
		return nil, false, nil
	}
	shard.counts[key]++
	shard.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			shard.mu.Lock()
			defer shard.mu.Unlock()
			shard.counts[key]--
			if shard.counts[key] <= 0 {
				delete(shard.counts, key)
			}
		})
	}, true, nil
}

func (l *ConcurrencyLimiter) Current(_ context.Context, key string) (int, error) {
	shard := &l.shards[shardIndex(key)]
	shard.mu.Lock()
	defer shard.mu.Unlock()
	return shard.counts[key], nil
}

func (l *ConcurrencyLimiter) CurrentMany(_ context.Context, keys []string) (map[string]int, error) {
	values := make(map[string]int, len(keys))
	for _, key := range keys {
		shard := &l.shards[shardIndex(key)]
		shard.mu.Lock()
		values[key] = shard.counts[key]
		shard.mu.Unlock()
	}
	return values, nil
}

type stickyBinding struct {
	accountID uint64
	expiresAt time.Time
}

// StickyStore 提供有界的单实例会话粘滞状态。
type StickyStore struct {
	shards [shardCount]stickyShard
}

type stickyShard struct {
	mu       sync.Mutex
	bindings map[string]stickyBinding
}

func NewStickyStore() *StickyStore {
	store := &StickyStore{}
	for index := range store.shards {
		store.shards[index].bindings = make(map[string]stickyBinding)
	}
	return store
}

func (s *StickyStore) Get(_ context.Context, promptCacheKey string, now time.Time) (uint64, bool, error) {
	shard := &s.shards[shardIndex(promptCacheKey)]
	shard.mu.Lock()
	defer shard.mu.Unlock()
	binding, ok := shard.bindings[promptCacheKey]
	if !ok {
		return 0, false, nil
	}
	if !now.Before(binding.expiresAt) {
		delete(shard.bindings, promptCacheKey)
		return 0, false, nil
	}
	return binding.accountID, true, nil
}

func (s *StickyStore) Set(_ context.Context, promptCacheKey string, accountID uint64, expiresAt time.Time) error {
	if promptCacheKey == "" {
		return nil
	}
	shard := &s.shards[shardIndex(promptCacheKey)]
	shard.mu.Lock()
	defer shard.mu.Unlock()
	shard.bindings[promptCacheKey] = stickyBinding{accountID: accountID, expiresAt: expiresAt}
	if len(shard.bindings) > maxEntriesPerShard() {
		for key, binding := range shard.bindings {
			if time.Now().After(binding.expiresAt) {
				delete(shard.bindings, key)
			}
		}
		for len(shard.bindings) > maxEntriesPerShard() {
			var oldestKey string
			var oldest time.Time
			for key, binding := range shard.bindings {
				if oldestKey == "" || binding.expiresAt.Before(oldest) {
					oldestKey = key
					oldest = binding.expiresAt
				}
			}
			delete(shard.bindings, oldestKey)
		}
	}
	return nil
}

func (s *StickyStore) DeleteByAccount(_ context.Context, accountID uint64) error {
	for index := range s.shards {
		shard := &s.shards[index]
		shard.mu.Lock()
		for key, binding := range shard.bindings {
			if binding.accountID == accountID {
				delete(shard.bindings, key)
			}
		}
		shard.mu.Unlock()
	}
	return nil
}

func shardIndex(key string) uint32 {
	const offset32 = uint32(2166136261)
	const prime32 = uint32(16777619)
	hash := offset32
	for index := 0; index < len(key); index++ {
		hash ^= uint32(key[index])
		hash *= prime32
	}
	return hash % shardCount
}

func maxEntriesPerShard() int { return (maxEntries + shardCount - 1) / shardCount }

// DeviceSessionStore 保存不会跨重启恢复的短期 OAuth 会话。
type DeviceSessionStore struct {
	mu       sync.Mutex
	sessions map[string]account.DeviceSession
}

func NewDeviceSessionStore() *DeviceSessionStore {
	return &DeviceSessionStore{sessions: make(map[string]account.DeviceSession)}
}

func (s *DeviceSessionStore) Create(_ context.Context, value account.DeviceSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	for id, session := range s.sessions {
		if !now.Before(session.ExpiresAt) {
			delete(s.sessions, id)
		}
	}
	if _, exists := s.sessions[value.ID]; !exists && len(s.sessions) >= maxDeviceSessions {
		var earliestID string
		var earliestExpiry time.Time
		for id, session := range s.sessions {
			if earliestID == "" || session.ExpiresAt.Before(earliestExpiry) {
				earliestID = id
				earliestExpiry = session.ExpiresAt
			}
		}
		delete(s.sessions, earliestID)
	}
	s.sessions[value.ID] = value
	return nil
}

func (s *DeviceSessionStore) Get(_ context.Context, id string, now time.Time) (account.DeviceSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.sessions[id]
	if !ok || !now.Before(value.ExpiresAt) {
		delete(s.sessions, id)
		return account.DeviceSession{}, repository.ErrNotFound
	}
	return value, nil
}

func (s *DeviceSessionStore) Update(_ context.Context, value account.DeviceSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[value.ID]; !ok {
		return repository.ErrNotFound
	}
	s.sessions[value.ID] = value
	return nil
}

func (s *DeviceSessionStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
	return nil
}

// LockStore 提供单实例非阻塞短期锁。
type LockStore struct {
	mu    sync.Mutex
	locks map[string]string
}

func NewLockStore() *LockStore { return &LockStore{locks: make(map[string]string)} }

func (s *LockStore) Acquire(_ context.Context, key string, _ time.Duration) (func(), bool, error) {
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, false, err
	}
	token := hex.EncodeToString(tokenBytes)
	s.mu.Lock()
	if _, exists := s.locks[key]; exists {
		s.mu.Unlock()
		return nil, false, nil
	}
	s.locks[key] = token
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.locks[key] == token {
				delete(s.locks, key)
			}
		})
	}, true, nil
}
