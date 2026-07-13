package resultcache

import (
	"errors"
	"sync"
	"time"
)

type entry[V any] struct {
	value     V
	expiresAt time.Time
	storedAt  time.Time
}

type flight[V any] struct {
	done  chan struct{}
	value V
	err   error
}

// Cache 保存少量短生命周期计算结果；达到容量时淘汰最早写入的条目。
type Cache[K comparable, V any] struct {
	mu      sync.Mutex
	ttl     time.Duration
	maxSize int
	values  map[K]entry[V]
	loads   map[K]*flight[V]
}

func New[K comparable, V any](maxSize int, ttl time.Duration) *Cache[K, V] {
	if maxSize < 1 {
		maxSize = 1
	}
	if ttl <= 0 {
		ttl = time.Second
	}
	return &Cache[K, V]{ttl: ttl, maxSize: maxSize, values: make(map[K]entry[V], maxSize), loads: make(map[K]*flight[V])}
}

func (c *Cache[K, V]) Get(key K, now time.Time) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	value, ok := c.values[key]
	if !ok {
		var zero V
		return zero, false
	}
	if !now.Before(value.expiresAt) {
		delete(c.values, key)
		var zero V
		return zero, false
	}
	return value.value, true
}

// Load 合并同一键的并发加载，避免缓存过期瞬间重复执行昂贵查询。
func (c *Cache[K, V]) Load(key K, now time.Time, loader func() (V, error)) (V, error) {
	if value, ok := c.Get(key, now); ok {
		return value, nil
	}
	c.mu.Lock()
	if pending, ok := c.loads[key]; ok {
		c.mu.Unlock()
		<-pending.done
		return pending.value, pending.err
	}
	pending := &flight[V]{done: make(chan struct{})}
	c.loads[key] = pending
	c.mu.Unlock()

	defer func() {
		if recovered := recover(); recovered != nil {
			c.mu.Lock()
			pending.err = errors.New("缓存加载异常中断")
			delete(c.loads, key)
			close(pending.done)
			c.mu.Unlock()
			panic(recovered)
		}
	}()
	pending.value, pending.err = loader()
	c.mu.Lock()
	if pending.err == nil {
		c.setLocked(key, pending.value, now)
	}
	delete(c.loads, key)
	close(pending.done)
	c.mu.Unlock()
	return pending.value, pending.err
}

func (c *Cache[K, V]) Set(key K, value V, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setLocked(key, value, now)
}

func (c *Cache[K, V]) setLocked(key K, value V, now time.Time) {
	if _, exists := c.values[key]; !exists && len(c.values) >= c.maxSize {
		var oldestKey K
		var oldestAt time.Time
		found := false
		for candidateKey, candidate := range c.values {
			if !found || candidate.storedAt.Before(oldestAt) {
				oldestKey, oldestAt, found = candidateKey, candidate.storedAt, true
			}
		}
		if found {
			delete(c.values, oldestKey)
		}
	}
	c.values[key] = entry[V]{value: value, expiresAt: now.Add(c.ttl), storedAt: now}
}
