package memory

import (
	"context"
	"sync"
	"time"
)

type reasoningReplayEntry struct {
	items     [][]byte
	expiresAt time.Time
	storedAt  time.Time
}

// ReasoningReplayStore 提供单实例有界推理回放缓存。
type ReasoningReplayStore struct {
	mu       sync.Mutex
	maxSize  int
	values   map[string]reasoningReplayEntry
	ttlSlide bool
}

// NewReasoningReplayStore 创建内存推理回放仓储；maxSize 为全局条目上限。
func NewReasoningReplayStore(maxSize int) *ReasoningReplayStore {
	if maxSize < 1 {
		maxSize = 10240
	}
	return &ReasoningReplayStore{maxSize: maxSize, values: make(map[string]reasoningReplayEntry, maxSize), ttlSlide: true}
}

func reasoningReplayMapKey(model, sessionKey string) string {
	return model + "\x00" + sessionKey
}

func cloneReplayItems(items [][]byte) [][]byte {
	if len(items) == 0 {
		return nil
	}
	cloned := make([][]byte, 0, len(items))
	for _, item := range items {
		cloned = append(cloned, append([]byte(nil), item...))
	}
	return cloned
}

func (s *ReasoningReplayStore) Get(_ context.Context, model, sessionKey string, now time.Time) ([][]byte, bool, error) {
	if model == "" || sessionKey == "" {
		return nil, false, nil
	}
	key := reasoningReplayMapKey(model, sessionKey)
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.values[key]
	if !ok {
		return nil, false, nil
	}
	if !now.Before(entry.expiresAt) {
		delete(s.values, key)
		return nil, false, nil
	}
	if s.ttlSlide {
		ttl := entry.expiresAt.Sub(entry.storedAt)
		if ttl <= 0 {
			ttl = time.Hour
		}
		entry.expiresAt = now.Add(ttl)
		entry.storedAt = now
		s.values[key] = entry
	}
	return cloneReplayItems(entry.items), true, nil
}

func (s *ReasoningReplayStore) Set(_ context.Context, model, sessionKey string, items [][]byte, expiresAt time.Time) error {
	if model == "" || sessionKey == "" || len(items) == 0 || expiresAt.IsZero() {
		return nil
	}
	key := reasoningReplayMapKey(model, sessionKey)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.values[key]; !exists {
		s.evictLocked(now)
	}
	s.values[key] = reasoningReplayEntry{items: cloneReplayItems(items), expiresAt: expiresAt, storedAt: now}
	return nil
}

func (s *ReasoningReplayStore) Delete(_ context.Context, model, sessionKey string) error {
	if model == "" || sessionKey == "" {
		return nil
	}
	key := reasoningReplayMapKey(model, sessionKey)
	s.mu.Lock()
	delete(s.values, key)
	s.mu.Unlock()
	return nil
}

func (s *ReasoningReplayStore) evictLocked(now time.Time) {
	for key, entry := range s.values {
		if !now.Before(entry.expiresAt) {
			delete(s.values, key)
		}
	}
	for len(s.values) >= s.maxSize {
		var oldestKey string
		var oldest time.Time
		for key, entry := range s.values {
			if oldestKey == "" || entry.storedAt.Before(oldest) {
				oldestKey = key
				oldest = entry.storedAt
			}
		}
		if oldestKey == "" {
			return
		}
		delete(s.values, oldestKey)
	}
}
