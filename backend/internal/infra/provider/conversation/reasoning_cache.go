package conversation

import (
	"sync"
	"time"
)

const (
	defaultReasoningCacheCapacity = 4096
	defaultReasoningCacheTTL      = 30 * time.Minute
)

type reasoningCacheEntry struct {
	item      responseItem
	createdAt time.Time
}

type ReasoningCache struct {
	mu       sync.RWMutex
	entries  map[string]reasoningCacheEntry
	order    []string
	capacity int
	ttl      time.Duration
}

var globalReasoningCache = newReasoningCache(defaultReasoningCacheCapacity, defaultReasoningCacheTTL)

func newReasoningCache(capacity int, ttl time.Duration) *ReasoningCache {
	if capacity <= 0 {
		capacity = defaultReasoningCacheCapacity
	}
	if ttl <= 0 {
		ttl = defaultReasoningCacheTTL
	}
	return &ReasoningCache{
		entries:  make(map[string]reasoningCacheEntry, capacity),
		order:    make([]string, 0, capacity),
		capacity: capacity,
		ttl:      ttl,
	}
}

// RememberReasoningForEnvelope 检查响应 envelope 中是否同时包含 reasoning 与 function_call 项。
// 若存在，将该轮次的所有 reasoning 项与同批次的所有 call_id 建立关联缓存。
func RememberReasoningForEnvelope(envelope responseEnvelope) {
	if len(envelope.Output) == 0 {
		return
	}
	var reasoningItems []responseItem
	var callIDs []string

	for _, item := range envelope.Output {
		if item.Type == "reasoning" {
			reasoningItems = append(reasoningItems, item)
		} else if item.Type == "function_call" && item.CallID != "" {
			callIDs = append(callIDs, item.CallID)
		}
	}

	if len(reasoningItems) == 0 || len(callIDs) == 0 {
		return
	}

	// 取最后一个 reasoning 项（最完整的思维链证明）
	targetReasoning := reasoningItems[len(reasoningItems)-1]
	for _, callID := range callIDs {
		globalReasoningCache.Set(callID, targetReasoning)
	}
}

func (c *ReasoningCache) Set(callID string, item responseItem) {
	if callID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	// 如果已存在，更新
	if _, exists := c.entries[callID]; exists {
		c.entries[callID] = reasoningCacheEntry{item: item, createdAt: now}
		return
	}

	// 如果超量，淘汰最早的条目或过期条目
	if len(c.order) >= c.capacity {
		c.evictOldestLocked(now)
	}

	c.entries[callID] = reasoningCacheEntry{item: item, createdAt: now}
	c.order = append(c.order, callID)
}

func (c *ReasoningCache) Get(callID string) (responseItem, bool) {
	if callID == "" {
		return responseItem{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, exists := c.entries[callID]
	if !exists {
		return responseItem{}, false
	}
	if time.Since(entry.createdAt) > c.ttl {
		return responseItem{}, false
	}
	return entry.item, true
}

func (c *ReasoningCache) evictOldestLocked(now time.Time) {
	// 先扫描清除过期条目
	newOrder := c.order[:0]
	for _, id := range c.order {
		entry, exists := c.entries[id]
		if !exists {
			continue
		}
		if now.Sub(entry.createdAt) > c.ttl {
			delete(c.entries, id)
			continue
		}
		newOrder = append(newOrder, id)
	}
	c.order = newOrder

	// 若仍满，则淘汰队列头部最老的一批
	if len(c.order) >= c.capacity {
		removeCount := c.capacity / 8
		if removeCount < 1 {
			removeCount = 1
		}
		for i := 0; i < removeCount && i < len(c.order); i++ {
			delete(c.entries, c.order[i])
		}
		c.order = c.order[removeCount:]
	}
}

// GetReasoningForCall 查询指定 callID 的缓存 reasoning 证明项
func GetReasoningForCall(callID string) (responseItem, bool) {
	return globalReasoningCache.Get(callID)
}
