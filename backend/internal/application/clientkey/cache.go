package clientkey

import (
	"sync"
	"time"

	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
)

const (
	keyTouchInterval       = time.Minute
	touchTrackerMaxEntries = 10000
	keyAuthCacheTTL        = time.Second
	keyAuthCacheMaxEntries = 10000
)

type cachedAuthKey struct {
	value     clientkeydomain.Key
	expiresAt time.Time
}

type authKeyCache struct {
	mu       sync.RWMutex
	byPrefix map[string]cachedAuthKey
}

func newAuthKeyCache() *authKeyCache {
	return &authKeyCache{byPrefix: make(map[string]cachedAuthKey)}
}

func (c *authKeyCache) get(prefix string, now time.Time) (clientkeydomain.Key, bool) {
	c.mu.RLock()
	entry, ok := c.byPrefix[prefix]
	c.mu.RUnlock()
	if !ok || !now.Before(entry.expiresAt) {
		if ok {
			c.mu.Lock()
			delete(c.byPrefix, prefix)
			c.mu.Unlock()
		}
		return clientkeydomain.Key{}, false
	}
	value := entry.value
	value.AllowedModels = append([]uint64(nil), entry.value.AllowedModels...)
	return value, true
}

func (c *authKeyCache) put(prefix string, value clientkeydomain.Key, now time.Time) {
	if prefix == "" || value.BillingLimitUSDTicks > 0 {
		return
	}
	value.EncryptedSecret = ""
	value.AllowedModels = append([]uint64(nil), value.AllowedModels...)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byPrefix[prefix] = cachedAuthKey{value: value, expiresAt: now.Add(keyAuthCacheTTL)}
	if len(c.byPrefix) <= keyAuthCacheMaxEntries {
		return
	}
	for candidate, entry := range c.byPrefix {
		if !now.Before(entry.expiresAt) {
			delete(c.byPrefix, candidate)
		}
	}
	for len(c.byPrefix) > keyAuthCacheMaxEntries {
		for candidate := range c.byPrefix {
			delete(c.byPrefix, candidate)
			break
		}
	}
}

func (c *authKeyCache) deleteID(id uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for prefix, entry := range c.byPrefix {
		if entry.value.ID == id {
			delete(c.byPrefix, prefix)
		}
	}
}

func (c *authKeyCache) deleteIDs(ids []uint64) {
	set := make(map[uint64]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for prefix, entry := range c.byPrefix {
		if _, ok := set[entry.value.ID]; ok {
			delete(c.byPrefix, prefix)
		}
	}
}

// touchTracker 合并非关键的最近使用时间写入。
type touchTracker struct {
	mu          sync.Mutex
	lastTouched map[uint64]time.Time
}

func newTouchTracker() *touchTracker {
	return &touchTracker{lastTouched: make(map[uint64]time.Time)}
}

func (c *touchTracker) deleteID(id uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.lastTouched, id)
}

func (c *touchTracker) deleteIDs(ids []uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		delete(c.lastTouched, id)
	}
}

func (c *touchTracker) shouldTouch(id uint64, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if last := c.lastTouched[id]; !last.IsZero() && now.Sub(last) < keyTouchInterval {
		return false
	}
	c.lastTouched[id] = now
	if len(c.lastTouched) > touchTrackerMaxEntries {
		var oldestID uint64
		var oldest time.Time
		for candidateID, touchedAt := range c.lastTouched {
			if oldestID == 0 || touchedAt.Before(oldest) {
				oldestID = candidateID
				oldest = touchedAt
			}
		}
		delete(c.lastTouched, oldestID)
	}
	return true
}
