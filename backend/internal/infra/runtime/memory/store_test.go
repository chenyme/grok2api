package memory

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestRateAndConcurrencyLimits(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	rate := NewRateLimiter()
	if allowed, _ := rate.Allow(ctx, "key", 1, now); !allowed {
		t.Fatal("第一次请求应被允许")
	}
	if allowed, _ := rate.Allow(ctx, "key", 1, now); allowed {
		t.Fatal("同一分钟的第二次请求应被拒绝")
	}

	concurrency := NewConcurrencyLimiter()
	release, acquired, _ := concurrency.Acquire(ctx, "key", 1)
	if !acquired {
		t.Fatal("第一次并发租约应成功")
	}
	if _, acquired, _ := concurrency.Acquire(ctx, "key", 1); acquired {
		t.Fatal("超过并发上限的租约不应成功")
	}
	release()
	if _, acquired, _ := concurrency.Acquire(ctx, "key", 1); !acquired {
		t.Fatal("释放后应能再次获取租约")
	}
	values, err := concurrency.CurrentMany(ctx, []string{"key", "missing"})
	if err != nil || values["key"] != 1 || values["missing"] != 0 {
		t.Fatalf("并发快照 = %#v, err = %v", values, err)
	}
}

func TestStickyStoreExpires(t *testing.T) {
	ctx := context.Background()
	store := NewStickyStore()
	now := time.Now()
	if err := store.Set(ctx, "session", 42, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if id, ok, _ := store.Get(ctx, "session", now); !ok || id != 42 {
		t.Fatalf("粘滞账号 = %d, %v", id, ok)
	}
	if _, ok, _ := store.Get(ctx, "session", now.Add(2*time.Second)); ok {
		t.Fatal("过期粘滞绑定仍然可用")
	}
}

func TestDeviceSessionStoreCleansExpiredAndStaysBounded(t *testing.T) {
	ctx := context.Background()
	store := NewDeviceSessionStore()
	now := time.Now().UTC()
	if err := store.Create(ctx, account.DeviceSession{ID: "expired", ExpiresAt: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	for index := range maxDeviceSessions + 1 {
		id := fmt.Sprintf("session-%d", index)
		if err := store.Create(ctx, account.DeviceSession{ID: id, ExpiresAt: now.Add(time.Duration(index+1) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, exists := store.sessions["expired"]; exists {
		t.Fatal("expired device session was not removed")
	}
	if len(store.sessions) != maxDeviceSessions {
		t.Fatalf("device sessions = %d, want %d", len(store.sessions), maxDeviceSessions)
	}
}
