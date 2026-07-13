package redis

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestRedisRuntimeStoreIntegration(t *testing.T) {
	address := os.Getenv("TEST_REDIS_ADDRESS")
	if address == "" {
		t.Skip("TEST_REDIS_ADDRESS is not configured")
	}
	ctx := context.Background()
	store, err := Open(ctx, Config{Address: address, KeyPrefix: "grok2api:test:" + time.Now().UTC().Format("150405.000000") + ":", ConcurrencyLease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if allowed, err := store.Allow(ctx, "key", 1, time.Now()); err != nil || !allowed {
		t.Fatalf("first rate allowance = %v, err = %v", allowed, err)
	}
	if allowed, err := store.Allow(ctx, "key", 1, time.Now()); err != nil || allowed {
		t.Fatalf("second rate allowance = %v, err = %v", allowed, err)
	}

	limiter := NewConcurrencyLimiter(store)
	release, acquired, err := limiter.Acquire(ctx, "account:1", 1)
	if err != nil || !acquired {
		t.Fatalf("concurrency acquire = %v, err = %v", acquired, err)
	}
	if _, acquired, err := limiter.Acquire(ctx, "account:1", 1); err != nil || acquired {
		t.Fatalf("duplicate concurrency acquire = %v, err = %v", acquired, err)
	}
	release()

	expiresAt := time.Now().UTC().Add(time.Minute)
	if err := store.Set(ctx, "sticky", 42, expiresAt); err != nil {
		t.Fatal(err)
	}
	if id, ok, err := store.Get(ctx, "sticky", time.Now().UTC()); err != nil || !ok || id != 42 {
		t.Fatalf("sticky = %d, %v, %v", id, ok, err)
	}
	if err := store.DeleteByAccount(ctx, 42); err != nil {
		t.Fatal(err)
	}

	deviceStore := NewDeviceSessionStore(store)
	session := account.DeviceSession{ID: "device", DeviceCode: "code", ExpiresAt: expiresAt}
	if err := deviceStore.Create(ctx, session); err != nil {
		t.Fatal(err)
	}
	if _, err := deviceStore.Get(ctx, session.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := deviceStore.Delete(ctx, session.ID); err != nil {
		t.Fatal(err)
	}

	lock := NewLockStore(store)
	unlock, acquired, err := lock.Acquire(ctx, "refresh:1", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("lock acquire = %v, err = %v", acquired, err)
	}
	if _, acquired, err := lock.Acquire(ctx, "refresh:1", time.Minute); err != nil || acquired {
		t.Fatalf("duplicate lock acquire = %v, err = %v", acquired, err)
	}
	unlock()

	dueAt := time.Now().UTC().Add(-time.Second)
	event := account.QuotaRecoveryEvent{AccountID: 42, Mode: "fast", DueAt: dueAt, Attempts: 3}
	if err := store.ScheduleQuotaRecovery(ctx, event); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimDueQuotaRecoveries(ctx, time.Now().UTC(), 10, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].Attempts != 3 {
		t.Fatalf("claimed quota recoveries = %#v, err = %v", claimed, err)
	}
	claimed[0].Attempts++
	claimed[0].DueAt = time.Now().UTC().Add(-time.Second)
	if err := store.RescheduleQuotaRecovery(ctx, claimed[0]); err != nil {
		t.Fatal(err)
	}
	claimed, err = store.ClaimDueQuotaRecoveries(ctx, time.Now().UTC(), 10, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].Attempts != 4 {
		t.Fatalf("rescheduled quota recoveries = %#v, err = %v", claimed, err)
	}
	if err := store.AckQuotaRecovery(ctx, claimed[0]); err != nil {
		t.Fatal(err)
	}

	listenerCtx, cancelListener := context.WithCancel(ctx)
	notified := make(chan struct{}, 1)
	listenerDone := make(chan error, 1)
	go func() {
		listenerDone <- store.ListenSettingsChanges(listenerCtx, func(context.Context) error {
			select {
			case notified <- struct{}{}:
			default:
			}
			return nil
		})
	}()
	deadline := time.NewTimer(3 * time.Second)
	publishTicker := time.NewTicker(25 * time.Millisecond)
	defer deadline.Stop()
	defer publishTicker.Stop()
	for {
		select {
		case <-publishTicker.C:
			if err := store.PublishSettingsChanged(ctx); err != nil {
				t.Fatal(err)
			}
		case <-notified:
			cancelListener()
			if err := <-listenerDone; err != nil {
				t.Fatal(err)
			}
			return
		case <-deadline.C:
			cancelListener()
			t.Fatal("settings change notification was not delivered")
		}
	}
}
