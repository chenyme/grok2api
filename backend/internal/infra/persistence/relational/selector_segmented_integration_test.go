package relational_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	redisruntime "github.com/chenyme/grok2api/backend/internal/infra/runtime/redis"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestPostgresRedisSegmentedSelectorIntegration(t *testing.T) {
	postgresDSN := os.Getenv("TEST_POSTGRES_DSN")
	redisAddress := os.Getenv("TEST_REDIS_ADDRESS")
	if postgresDSN == "" || redisAddress == "" {
		t.Skip("TEST_POSTGRES_DSN and TEST_REDIS_ADDRESS are required")
	}
	redisDatabase := 0
	if raw := os.Getenv("TEST_REDIS_DATABASE"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			t.Fatalf("TEST_REDIS_DATABASE = %q", raw)
		}
		redisDatabase = value
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	database, err := relational.OpenPostgres(ctx, postgresDSN, 30, 10)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("close PostgreSQL integration database: %v", err)
		}
	})
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	values := make([]account.Credential, 3000)
	identityPrefix := fmt.Sprintf("p4-%d", time.Now().UTC().UnixNano())
	for index := range values {
		values[index] = account.Credential{
			Provider: account.ProviderBuild, Name: fmt.Sprintf("p4-%04d", index+1),
			SourceKey: identityPrefix + "-" + strconv.Itoa(index+1), EncryptedAccessToken: "encrypted",
			AuthStatus: account.AuthStatusActive, Priority: 1_000_000, MaxConcurrent: 1,
		}
	}
	created, err := accounts.UpsertManyByIdentity(ctx, values)
	if err != nil {
		t.Fatal(err)
	}
	createdIDs := make([]uint64, len(created))
	for index, value := range created {
		createdIDs[index] = value.ID
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if _, err := accounts.DeleteMany(cleanupCtx, createdIDs); err != nil {
			t.Errorf("delete P4 integration accounts: %v", err)
		}
	})
	loadedBases, err := accounts.ListRoutingAccountBases(ctx, account.ProviderBuild, "")
	if err != nil {
		t.Fatal(err)
	}
	bases := make([]account.RoutingAccountBase, 0, len(values))
	for _, value := range loadedBases {
		if strings.HasPrefix(value.Credential.SourceKey, identityPrefix+"-") {
			bases = append(bases, value)
		}
	}
	if len(bases) != len(values) {
		t.Fatalf("routing account count = %d, want %d", len(bases), len(values))
	}

	store, err := redisruntime.Open(ctx, redisruntime.Config{
		Address: redisAddress, Username: os.Getenv("TEST_REDIS_USERNAME"), Password: os.Getenv("TEST_REDIS_PASSWORD"), Database: redisDatabase,
		KeyPrefix: identityPrefix + ":", ConcurrencyLease: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	limiter := redisruntime.NewConcurrencyLimiter(store)

	newSelector := func() *gateway.Selector {
		selector := gateway.NewSelector(accounts, limiter, store, nil, time.Hour, time.Second, time.Minute, 100*time.Millisecond)
		selector.UpdateSegmentedSelector(true, 3000, 64)
		return selector
	}
	saturate := func(t *testing.T, count int) {
		t.Helper()
		for index := 0; index < count; index++ {
			release, acquired, err := limiter.Acquire(ctx, repository.AccountConcurrencyKey(bases[index].Credential.ID), 1)
			if err != nil || !acquired {
				t.Fatalf("saturate account %d: acquired=%v err=%v", bases[index].Credential.ID, acquired, err)
			}
			t.Cleanup(release)
		}
	}

	t.Run("continues to the next window", func(t *testing.T) {
		saturate(t, 64)
		lease, err := newSelector().Acquire(ctx, account.ProviderBuild, "p4-model", "", "", nil, false)
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Release()
		if lease.Credential.ID != bases[64].Credential.ID {
			t.Fatalf("selected account = %d, want %d", lease.Credential.ID, bases[64].Credential.ID)
		}
	})

	t.Run("falls back after four saturated windows", func(t *testing.T) {
		saturate(t, 256)
		lease, err := newSelector().Acquire(ctx, account.ProviderBuild, "p4-model", "", "", nil, false)
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Release()
		if lease.Credential.ID != bases[256].Credential.ID {
			t.Fatalf("selected account = %d, want %d", lease.Credential.ID, bases[256].Credential.ID)
		}
	})

	t.Run("shares atomic capacity across instances", func(t *testing.T) {
		selectors := []*gateway.Selector{newSelector(), newSelector()}
		const workers = 32
		type result struct {
			accountID uint64
			release   func()
			err       error
		}
		start := make(chan struct{})
		results := make(chan result, workers)
		var wait sync.WaitGroup
		for index := range workers {
			wait.Add(1)
			go func(selector *gateway.Selector) {
				defer wait.Done()
				<-start
				lease, err := selector.Acquire(ctx, account.ProviderBuild, "p4-model", "", "", nil, false)
				if err != nil {
					results <- result{err: err}
					return
				}
				results <- result{accountID: lease.Credential.ID, release: lease.Release}
			}(selectors[index%len(selectors)])
		}
		close(start)
		wait.Wait()
		close(results)
		selected := make(map[uint64]struct{}, workers)
		for value := range results {
			if value.err != nil {
				t.Fatal(value.err)
			}
			t.Cleanup(value.release)
			if _, exists := selected[value.accountID]; exists {
				t.Fatalf("account %d exceeded shared concurrency capacity", value.accountID)
			}
			selected[value.accountID] = struct{}{}
		}
		if len(selected) != workers {
			t.Fatalf("selected accounts = %d, want %d", len(selected), workers)
		}
	})
}
