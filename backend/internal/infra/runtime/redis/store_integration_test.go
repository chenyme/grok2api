package redis

import (
	"context"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	redisclient "github.com/redis/go-redis/v9"
)

func TestRedisRuntimeStoreIntegration(t *testing.T) {
	address := os.Getenv("TEST_REDIS_ADDRESS")
	if address == "" {
		t.Skip("TEST_REDIS_ADDRESS is not configured")
	}
	database := 0
	if rawDatabase := os.Getenv("TEST_REDIS_DATABASE"); rawDatabase != "" {
		parsed, parseErr := strconv.Atoi(rawDatabase)
		if parseErr != nil || parsed < 0 {
			t.Fatalf("TEST_REDIS_DATABASE = %q", rawDatabase)
		}
		database = parsed
	}
	ctx := context.Background()
	store, err := Open(ctx, Config{
		Address: address, Username: os.Getenv("TEST_REDIS_USERNAME"), Password: os.Getenv("TEST_REDIS_PASSWORD"), Database: database,
		KeyPrefix: "grok2api:test:" + time.Now().UTC().Format("150405.000000") + ":", ConcurrencyLease: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if os.Getenv("TEST_REDIS_FLUSH_DATABASE") == "1" {
		defer func() {
			if err := store.client.FlushDB(ctx).Err(); err != nil {
				t.Errorf("flush Redis test database: %v", err)
			}
		}()
	}

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
	concurrencyKey := store.key("concurrency", "snapshot")
	now := time.Now().UTC()
	if err := store.client.ZAdd(ctx, concurrencyKey,
		redisclient.Z{Score: float64(now.Add(-time.Minute).UnixMilli()), Member: "expired"},
		redisclient.Z{Score: float64(now.Add(time.Minute).UnixMilli()), Member: "active"},
	).Err(); err != nil {
		t.Fatal(err)
	}
	concurrency, err := store.CurrentMany(ctx, []string{"snapshot"})
	if err != nil || concurrency["snapshot"] != 1 {
		t.Fatalf("read-only concurrency snapshot = %#v, err = %v", concurrency, err)
	}
	if remaining, err := store.client.ZCard(ctx, concurrencyKey).Result(); err != nil || remaining != 2 {
		t.Fatalf("concurrency snapshot mutated lease set: remaining=%d err=%v", remaining, err)
	}

	expiresAt := time.Now().UTC().Add(time.Minute)
	if err := store.Set(ctx, "sticky", 42, expiresAt); err != nil {
		t.Fatal(err)
	}
	if id, ok, err := store.Get(ctx, "sticky", time.Now().UTC()); err != nil || !ok || id != 42 {
		t.Fatalf("sticky = %d, %v, %v", id, ok, err)
	}
	if id, err := store.Bind(ctx, "sticky", 7, time.Now().UTC(), time.Now().UTC().Add(2*time.Minute)); err != nil || id != 42 {
		t.Fatalf("atomic sticky bind = %d, err = %v", id, err)
	}
	if err := store.Set(ctx, "sticky", 7, time.Now().UTC().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteByAccount(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Get(ctx, "sticky", time.Now().UTC()); err != nil || ok {
		t.Fatalf("deleted sticky remains available: ok=%v err=%v", ok, err)
	}

	observedAt := time.Now().UTC()
	if err := store.SetObservedModelState(ctx, 42, repository.ObservedModelState{Model: "grok-4.5-build-free", ObservedAt: observedAt}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if value, ok, err := store.GetObservedModelState(ctx, 42); err != nil || !ok || value.Model != "grok-4.5-build-free" || !value.ObservedAt.Equal(observedAt.Truncate(time.Millisecond)) {
		t.Fatalf("observed model state = %#v, ok=%v, err=%v", value, ok, err)
	}
	if err := store.SetObservedModelState(ctx, 42, repository.ObservedModelState{Model: "stale-model", ObservedAt: observedAt.Add(-time.Second)}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if value, ok, err := store.GetObservedModelState(ctx, 42); err != nil || !ok || value.Model != "grok-4.5-build-free" {
		t.Fatalf("stale observed model state replaced value = %#v, ok=%v, err=%v", value, ok, err)
	}

	const bindWorkers = 16
	start := make(chan struct{})
	results := make(chan uint64, bindWorkers)
	errors := make(chan error, bindWorkers)
	var bindGroup sync.WaitGroup
	for index := range bindWorkers {
		bindGroup.Add(1)
		go func(accountID uint64) {
			defer bindGroup.Done()
			<-start
			id, err := store.Bind(ctx, "sticky-race", accountID, time.Now().UTC(), time.Now().UTC().Add(time.Minute))
			results <- id
			errors <- err
		}(uint64(index + 1))
	}
	close(start)
	bindGroup.Wait()
	close(results)
	close(errors)
	var winner uint64
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	for id := range results {
		if winner == 0 {
			winner = id
		}
		if id != winner {
			t.Fatalf("concurrent bind returned multiple accounts: first=%d current=%d", winner, id)
		}
	}
	if winner == 0 {
		t.Fatal("concurrent bind did not select an account")
	}
	if err := store.DeleteByAccount(ctx, winner); err != nil {
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

	firstGeneration, err := store.MarkQuotaRefreshDirty(ctx, 42, "fast", 200*time.Millisecond)
	if err != nil || firstGeneration != 1 {
		t.Fatalf("first quota refresh generation = %d, err = %v", firstGeneration, err)
	}
	if cleared, err := store.ClearQuotaRefreshDirty(ctx, 42, "fast", firstGeneration); err != nil || !cleared {
		t.Fatalf("clear first quota refresh generation = %v, err = %v", cleared, err)
	}
	if generation, dirty, err := store.QuotaRefreshGeneration(ctx, 42, "fast"); err != nil || generation != firstGeneration || dirty {
		t.Fatalf("cleared quota refresh state = generation %d, dirty %v, err %v", generation, dirty, err)
	}
	secondGeneration, err := store.MarkQuotaRefreshDirty(ctx, 42, "fast", 200*time.Millisecond)
	if err != nil || secondGeneration != 2 {
		t.Fatalf("second quota refresh generation = %d, err = %v", secondGeneration, err)
	}
	if cleared, err := store.ClearQuotaRefreshDirty(ctx, 42, "fast", firstGeneration); err != nil || cleared {
		t.Fatalf("stale quota refresh clear = %v, err = %v", cleared, err)
	}

	member := "42:fast"
	generationKey := store.key("quota-refresh", "generations")
	dirtyKey := store.key("quota-refresh", "dirty")
	expiryKey := store.key("quota-refresh", "expiry")
	if _, err := store.MarkQuotaRefreshDirty(ctx, 43, "fast", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if generation, dirty, err := store.QuotaRefreshGeneration(ctx, 42, "fast"); err != nil || generation != 0 || dirty {
		t.Fatalf("expired quota refresh state = generation %d, dirty %v, err %v", generation, dirty, err)
	}
	if exists, err := store.client.HExists(ctx, generationKey, member).Result(); err != nil || exists {
		t.Fatalf("expired generation retained = %v, err = %v", exists, err)
	}
	if score, err := store.client.ZScore(ctx, dirtyKey, member).Result(); err != redisclient.Nil {
		t.Fatalf("expired dirty member score = %f, err = %v", score, err)
	}
	if score, err := store.client.ZScore(ctx, expiryKey, member).Result(); err != redisclient.Nil {
		t.Fatalf("expired expiry member score = %f, err = %v", score, err)
	}

	if _, err := store.MarkQuotaRefreshDirty(ctx, 44, "fast", 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkQuotaRefreshDirty(ctx, 45, "fast", time.Minute); err != nil {
		t.Fatal(err)
	}
	if ttl, err := store.client.PTTL(ctx, generationKey).Result(); err != nil || ttl < 90*time.Minute {
		t.Fatalf("short-lived member shortened generation key TTL: ttl = %s, err = %v", ttl, err)
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

func TestRedisInvalidationBusIntegration(t *testing.T) {
	address := os.Getenv("TEST_REDIS_ADDRESS")
	if address == "" {
		t.Skip("TEST_REDIS_ADDRESS is not configured")
	}
	database := 0
	if rawDatabase := os.Getenv("TEST_REDIS_DATABASE"); rawDatabase != "" {
		parsed, err := strconv.Atoi(rawDatabase)
		if err != nil || parsed < 0 {
			t.Fatalf("TEST_REDIS_DATABASE = %q", rawDatabase)
		}
		database = parsed
	}
	ctx := context.Background()
	store, err := Open(ctx, Config{
		Address: address, Username: os.Getenv("TEST_REDIS_USERNAME"), Password: os.Getenv("TEST_REDIS_PASSWORD"), Database: database,
		KeyPrefix: "grok2api:invalidation-test:" + time.Now().UTC().Format("150405.000000") + ":", ConcurrencyLease: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if os.Getenv("TEST_REDIS_FLUSH_DATABASE") == "1" {
		defer func() {
			if err := store.client.FlushDB(ctx).Err(); err != nil {
				t.Errorf("flush Redis test database: %v", err)
			}
		}()
	}

	listenerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	received := make(chan repository.InvalidationEvent, 4)
	done := make(chan error, 1)
	go func() {
		done <- store.ListenInvalidations(listenerCtx, func(_ context.Context, event repository.InvalidationEvent) error {
			received <- event
			return nil
		})
	}()

	event := repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: account.ProviderBuild, SourceInstance: "instance-a"}
	deadline := time.NewTimer(3 * time.Second)
	ticker := time.NewTicker(25 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	var first repository.InvalidationEvent
	waiting := true
	for waiting {
		select {
		case <-ticker.C:
			if err := store.PublishInvalidation(ctx, event); err != nil {
				t.Fatal(err)
			}
		case first = <-received:
			waiting = false
		case <-deadline.C:
			t.Fatal("invalidation notification was not delivered")
		}
	}
	if first.Revision == 0 || first.Layer() != repository.InvalidationLayerBase {
		t.Fatalf("first invalidation = %#v", first)
	}
	if err := store.client.Publish(ctx, store.key("events", "invalidation"), "not-json").Err(); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishInvalidation(ctx, event); err != nil {
		t.Fatal(err)
	}
	select {
	case second := <-received:
		if second.Revision <= first.Revision {
			t.Fatalf("revisions did not advance: first=%d second=%d", first.Revision, second.Revision)
		}
	case <-time.After(time.Second):
		t.Fatal("second invalidation notification was not delivered")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
