package memory

import (
	"container/heap"
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/repository"
)

const (
	maxQuotaRefreshDirty                 = 100000
	quotaRefreshExpiryCompactionMinStale = 1024
)

type quotaRefreshDirtyState struct {
	accountID  uint64
	mode       string
	generation uint64
	expiresAt  time.Time
	dirty      bool
}

type QuotaRefreshCoordinator struct {
	mu      sync.Mutex
	values  map[string]quotaRefreshDirtyState
	dirty   map[string]struct{}
	expires quotaRefreshExpiryHeap
}

func NewQuotaRefreshCoordinator() *QuotaRefreshCoordinator {
	return &QuotaRefreshCoordinator{values: make(map[string]quotaRefreshDirtyState), dirty: make(map[string]struct{})}
}

type quotaRefreshExpiry struct {
	key        string
	generation uint64
	expiresAt  time.Time
}

type quotaRefreshExpiryHeap []quotaRefreshExpiry

func (h quotaRefreshExpiryHeap) Len() int           { return len(h) }
func (h quotaRefreshExpiryHeap) Less(i, j int) bool { return h[i].expiresAt.Before(h[j].expiresAt) }
func (h quotaRefreshExpiryHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *quotaRefreshExpiryHeap) Push(value any)    { *h = append(*h, value.(quotaRefreshExpiry)) }
func (h *quotaRefreshExpiryHeap) Pop() any {
	values := *h
	last := values[len(values)-1]
	*h = values[:len(values)-1]
	return last
}

func (c *QuotaRefreshCoordinator) MarkQuotaRefreshDirty(_ context.Context, accountID uint64, mode string, ttl time.Duration) (uint64, error) {
	mode = strings.TrimSpace(mode)
	if accountID == 0 || mode == "" || ttl <= 0 {
		return 0, fmt.Errorf("quota refresh identity is invalid")
	}
	now := time.Now().UTC()
	key := quotaRefreshKey(accountID, mode)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(now)
	state := c.values[key]
	if _, dirty := c.dirty[key]; !dirty && len(c.dirty) >= maxQuotaRefreshDirty {
		return 0, fmt.Errorf("quota refresh dirty set is full")
	}
	state.accountID = accountID
	state.mode = mode
	state.generation++
	state.expiresAt = now.Add(ttl)
	state.dirty = true
	c.values[key] = state
	c.dirty[key] = struct{}{}
	heap.Push(&c.expires, quotaRefreshExpiry{key: key, generation: state.generation, expiresAt: state.expiresAt})
	c.compactExpiryHeapLocked()
	return state.generation, nil
}

func (c *QuotaRefreshCoordinator) QuotaRefreshGeneration(_ context.Context, accountID uint64, mode string) (uint64, bool, error) {
	now := time.Now().UTC()
	key := quotaRefreshKey(accountID, strings.TrimSpace(mode))
	c.mu.Lock()
	defer c.mu.Unlock()
	state, ok := c.values[key]
	if !ok || !now.Before(state.expiresAt) {
		delete(c.values, key)
		delete(c.dirty, key)
		c.compactExpiryHeapLocked()
		return 0, false, nil
	}
	return state.generation, state.dirty, nil
}

func (c *QuotaRefreshCoordinator) ClearQuotaRefreshDirty(_ context.Context, accountID uint64, mode string, generation uint64) (bool, error) {
	key := quotaRefreshKey(accountID, strings.TrimSpace(mode))
	now := time.Now().UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	state, ok := c.values[key]
	if !ok || !now.Before(state.expiresAt) {
		delete(c.values, key)
		delete(c.dirty, key)
		c.compactExpiryHeapLocked()
		return false, nil
	}
	if state.generation != generation {
		return false, nil
	}
	state.dirty = false
	c.values[key] = state
	delete(c.dirty, key)
	return true, nil
}

func (c *QuotaRefreshCoordinator) ListQuotaRefreshDirty(_ context.Context, now time.Time, limit int) ([]repository.QuotaRefreshDirty, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(now)
	values := make([]repository.QuotaRefreshDirty, 0, min(limit, len(c.dirty)))
	for key := range c.dirty {
		state, ok := c.values[key]
		if !ok || !state.dirty || !now.Before(state.expiresAt) {
			delete(c.dirty, key)
			if ok && !now.Before(state.expiresAt) {
				delete(c.values, key)
			}
			continue
		}
		values = append(values, repository.QuotaRefreshDirty{AccountID: state.accountID, Mode: state.mode, Generation: state.generation})
	}
	sort.Slice(values, func(i, j int) bool {
		if values[i].AccountID != values[j].AccountID {
			return values[i].AccountID < values[j].AccountID
		}
		return values[i].Mode < values[j].Mode
	})
	if len(values) > limit {
		values = values[:limit]
	}
	c.compactExpiryHeapLocked()
	return values, nil
}

func (c *QuotaRefreshCoordinator) pruneLocked(now time.Time) {
	for c.expires.Len() > 0 && !now.Before(c.expires[0].expiresAt) {
		expired := heap.Pop(&c.expires).(quotaRefreshExpiry)
		state, ok := c.values[expired.key]
		if !ok || state.generation != expired.generation || !state.expiresAt.Equal(expired.expiresAt) {
			continue
		}
		delete(c.values, expired.key)
		delete(c.dirty, expired.key)
	}
}

// compactExpiryHeapLocked bounds stale generation entries without adding a
// second index. Rebuilding is amortized and retains exactly one expiry per key.
func (c *QuotaRefreshCoordinator) compactExpiryHeapLocked() {
	if len(c.expires) <= len(c.values)*2+quotaRefreshExpiryCompactionMinStale {
		return
	}
	values := make(quotaRefreshExpiryHeap, 0, len(c.values))
	for key, state := range c.values {
		values = append(values, quotaRefreshExpiry{key: key, generation: state.generation, expiresAt: state.expiresAt})
	}
	heap.Init(&values)
	c.expires = values
}

func quotaRefreshKey(accountID uint64, mode string) string {
	return fmt.Sprintf("%d:%s", accountID, mode)
}
