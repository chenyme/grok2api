package resultcache

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCacheExpiresAndEvictsOldestEntry(t *testing.T) {
	now := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
	cache := New[string, int](2, time.Minute)
	cache.Set("first", 1, now)
	cache.Set("second", 2, now.Add(time.Second))
	cache.Set("third", 3, now.Add(2*time.Second))

	if _, ok := cache.Get("first", now.Add(3*time.Second)); ok {
		t.Fatal("oldest cache entry was not evicted")
	}
	if value, ok := cache.Get("third", now.Add(3*time.Second)); !ok || value != 3 {
		t.Fatalf("third entry = %d, %v", value, ok)
	}
	if _, ok := cache.Get("second", now.Add(2*time.Minute)); ok {
		t.Fatal("expired cache entry was returned")
	}
}

func TestCacheCoalescesConcurrentLoads(t *testing.T) {
	now := time.Now()
	cache := New[string, int](2, time.Minute)
	var calls atomic.Int32
	start := make(chan struct{})
	var wait sync.WaitGroup
	results := make([]int, 2)
	wait.Add(2)
	for index := range results {
		go func() {
			defer wait.Done()
			<-start
			value, err := cache.Load("shared", now, func() (int, error) {
				calls.Add(1)
				time.Sleep(10 * time.Millisecond)
				return 42, nil
			})
			if err != nil {
				t.Errorf("load: %v", err)
			}
			results[index] = value
		}()
	}
	close(start)
	wait.Wait()
	if calls.Load() != 1 || results[0] != 42 || results[1] != 42 {
		t.Fatalf("calls = %d, results = %#v", calls.Load(), results)
	}
}
