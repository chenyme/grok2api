package account

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	redisruntime "github.com/chenyme/grok2api/backend/internal/infra/runtime/redis"
	redisclient "github.com/redis/go-redis/v9"
)

func TestRedisQuotaRefreshCrossInstanceTrailing(t *testing.T) {
	address := os.Getenv("TEST_REDIS_ADDRESS")
	if address == "" {
		t.Skip("TEST_REDIS_ADDRESS is not configured")
	}
	databaseNumber, err := redisTestDatabaseNumber()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cleanup := redisclient.NewClient(&redisclient.Options{
		Addr: address, Username: os.Getenv("TEST_REDIS_USERNAME"), Password: os.Getenv("TEST_REDIS_PASSWORD"), DB: databaseNumber,
	})
	defer cleanup.Close()
	if err := cleanup.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := cleanup.FlushDB(ctx).Err(); err != nil {
			t.Errorf("flush Redis test database: %v", err)
		}
	}()

	prefix := "grok2api:quota-refresh-integration:" + time.Now().UTC().Format("20060102150405.000000000") + ":"
	config := redisruntime.Config{
		Address: address, Username: os.Getenv("TEST_REDIS_USERNAME"), Password: os.Getenv("TEST_REDIS_PASSWORD"), Database: databaseNumber,
		KeyPrefix: prefix, ConcurrencyLease: time.Minute,
	}
	firstRuntime, err := redisruntime.Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer firstRuntime.Close()
	secondRuntime, err := redisruntime.Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer secondRuntime.Close()

	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "quota-refresh-redis-integration.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	credential, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO,
		Name: "redis-cross-instance", SourceKey: "redis-cross-instance", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive, WebTier: accountdomain.WebTierSuper,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &quotaCountingAdapter{modeStarted: make(chan struct{}, 4), modeRelease: make(chan struct{}, 4)}
	registry := provider.NewRegistry(adapter)
	first := NewService(accounts, nil, nil, nil, registry, nil, redisruntime.NewLockStore(firstRuntime))
	second := NewService(accounts, nil, nil, nil, registry, nil, redisruntime.NewLockStore(secondRuntime))
	first.SetQuotaRefreshCoordinator(firstRuntime)
	second.SetQuotaRefreshCoordinator(secondRuntime)

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{}, 2)
	go func() { first.RunWebQuotaRefresh(runCtx); done <- struct{}{} }()
	go func() { second.RunWebQuotaRefresh(runCtx); done <- struct{}{} }()
	t.Cleanup(func() {
		for range 4 {
			adapter.modeRelease <- struct{}{}
		}
		cancel()
		<-done
		<-done
	})

	first.QueueQuotaRefresh(credential.ID, "weekly")
	select {
	case <-adapter.modeStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("first Redis-backed refresh did not start")
	}
	second.QueueQuotaRefresh(credential.ID, "weekly")
	deadline := time.Now().Add(3 * time.Second)
	for {
		generation, dirty, generationErr := secondRuntime.QuotaRefreshGeneration(ctx, credential.ID, "weekly")
		if generationErr != nil {
			t.Fatal(generationErr)
		}
		if generation >= 2 && dirty {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("shared Redis generation = %d, dirty = %v", generation, dirty)
		}
		time.Sleep(10 * time.Millisecond)
	}
	adapter.modeRelease <- struct{}{}
	select {
	case <-adapter.modeStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("Redis-backed trailing refresh did not start")
	}
	adapter.modeRelease <- struct{}{}

	deadline = time.Now().Add(3 * time.Second)
	for adapter.modeCalls.Load() != 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if calls := adapter.modeCalls.Load(); calls != 2 {
		t.Fatalf("Redis-backed refresh calls = %d, want 2", calls)
	}
	time.Sleep(2 * webQuotaRefreshRetryInterval)
	if calls := adapter.modeCalls.Load(); calls != 2 {
		t.Fatalf("Redis-backed losing instance performed duplicate refresh: %d", calls)
	}
}

func redisTestDatabaseNumber() (int, error) {
	raw := os.Getenv("TEST_REDIS_DATABASE")
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("TEST_REDIS_DATABASE = %q", raw)
	}
	return value, nil
}
