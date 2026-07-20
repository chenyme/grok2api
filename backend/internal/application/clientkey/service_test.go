package clientkey

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestCreateUsesG2AClientKeyFormat(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "client-key.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	service := NewService(relational.NewClientKeyRepository(database), nil, nil, 60, 5, testCipher(t))
	created, err := service.Create(ctx, CreateInput{Name: "test", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Secret, "g2a_") {
		t.Fatalf("client key = %q", created.Secret)
	}
	prefix, ok := security.SplitClientKey(created.Secret)
	if !ok || prefix != created.Key.Prefix {
		t.Fatalf("parsed prefix = %q, key prefix = %q, ok = %v", prefix, created.Key.Prefix, ok)
	}
	values, total, err := service.List(ctx, 1, 20, created.Secret, ListFilter{})
	if err != nil || total != 1 || len(values) != 1 || values[0].ID != created.Key.ID {
		t.Fatalf("search by full client key values = %#v, total = %d, err = %v", values, total, err)
	}
	if values[0].EncryptedSecret != "" || values[0].SecretHash != "" {
		t.Fatal("客户端 Key 列表不应加载哈希或加密密文")
	}
	if _, err := service.Create(ctx, CreateInput{Name: "unlimited", Enabled: true, RPMLimit: -1}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("negative rpm error = %v", err)
	}
	zero := 0
	updated, err := service.Update(ctx, created.Key.ID, UpdateInput{MaxConcurrent: &zero})
	if err != nil || updated.MaxConcurrent != 0 {
		t.Fatalf("zero concurrency update = %#v, err = %v", updated, err)
	}
	revealed, err := service.RevealSecret(ctx, created.Key.ID)
	if err != nil || revealed != created.Secret {
		t.Fatalf("revealed secret = %q, err = %v", revealed, err)
	}
}

func TestUnlimitedRuntimeLimitsBypassLimiterStores(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "unlimited-runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewClientKeyRepository(database)
	service := NewService(repo, failingRateLimiter{}, failingConcurrencyLimiter{}, 60, 5, testCipher(t))
	created, err := service.Create(ctx, CreateInput{
		Name: "unlimited", Enabled: true,
		RPMUnlimited: true, ConcurrencyUnlimited: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Key.RPMLimit != 0 || created.Key.MaxConcurrent != 0 {
		t.Fatalf("persisted limits = rpm %d, concurrency %d", created.Key.RPMLimit, created.Key.MaxConcurrent)
	}
	value, release, err := service.Authenticate(ctx, created.Secret)
	if err != nil {
		t.Fatalf("authenticate unlimited key: %v", err)
	}
	if value.ID != created.Key.ID {
		t.Fatalf("authenticated key = %d, want %d", value.ID, created.Key.ID)
	}
	release()
}

func TestAuthenticateDistinguishesRuntimeStoreFailures(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "runtime-errors.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewClientKeyRepository(database)
	cipher := testCipher(t)
	created, err := NewService(repo, nil, nil, 60, 5, cipher).Create(ctx, CreateInput{Name: "test", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	rateFailure := NewService(repo, failingRateLimiter{}, successfulConcurrencyLimiter{}, 60, 5, cipher)
	if _, _, err := rateFailure.Authenticate(ctx, created.Secret); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("rate limiter error = %v", err)
	}
	concurrencyFailure := NewService(repo, successfulRateLimiter{}, failingConcurrencyLimiter{}, 60, 5, cipher)
	if _, _, err := concurrencyFailure.Authenticate(ctx, created.Secret); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("concurrency limiter error = %v", err)
	}
	persistenceFailure := NewService(failingClientKeyRepository{ClientKeyRepository: repo}, successfulRateLimiter{}, successfulConcurrencyLimiter{}, 60, 5, cipher)
	if _, _, err := persistenceFailure.Authenticate(ctx, created.Secret); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("client key repository error = %v", err)
	}
}

func TestBillingLimitUsesAtomicReservations(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "billing-limit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	keys := relational.NewClientKeyRepository(database)
	service := NewService(keys, successfulRateLimiter{}, successfulConcurrencyLimiter{}, 60, 5, testCipher(t))
	created, err := service.Create(ctx, CreateInput{Name: "limited", Enabled: true, BillingLimitUSDTicks: 6_000_000_000})
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := service.ReserveBilling(ctx, created.Key, "evt_client_key_reservation_0001", 2_000_000_000, time.Hour)
	if err != nil || !reserved {
		t.Fatal(err)
	}
	reserved, err = service.ReserveBilling(ctx, created.Key, "evt_client_key_reservation_0002", 4_000_000_000, time.Hour)
	if err != nil || !reserved {
		t.Fatalf("reserve remaining limit: reserved=%v err=%v", reserved, err)
	}
	if _, _, err := service.Authenticate(ctx, created.Secret); !errors.Is(err, ErrBillingLimit) {
		t.Fatalf("reserved billing limit error = %v", err)
	}
	if _, err := service.ReserveBilling(ctx, created.Key, "evt_client_key_reservation_0003", 1, time.Hour); !errors.Is(err, ErrBillingLimit) {
		t.Fatalf("billing limit error = %v", err)
	}
	if err := service.CancelBilling(ctx, "evt_client_key_reservation_0001"); err != nil {
		t.Fatal(err)
	}
	if reserved, err := service.ReserveBilling(ctx, created.Key, "evt_client_key_reservation_0003", 1_000_000_000, time.Hour); err != nil || !reserved {
		t.Fatalf("reserve after cancel: reserved=%v err=%v", reserved, err)
	}
	values, _, err := service.List(ctx, 1, 20, "", ListFilter{})
	if err != nil || len(values) != 1 || values[0].ReservedUsageUSDTicks != 5_000_000_000 {
		t.Fatalf("listed usage = %#v, err = %v", values, err)
	}
	unlimited, err := service.Create(ctx, CreateInput{Name: "unlimited", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if reserved, err := service.ReserveBilling(ctx, unlimited.Key, "evt_client_key_unlimited_0001", 100_000_000_000, time.Hour); err != nil || reserved {
		t.Fatalf("unlimited reservation = %v, err = %v", reserved, err)
	}
	_, unlimitedRelease, err := service.Authenticate(ctx, unlimited.Secret)
	if err != nil {
		t.Fatalf("authenticate unlimited key: %v", err)
	}
	unlimitedRelease()
}

func TestAuthenticateCachesUnlimitedKeyAndInvalidatesOnDisable(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "auth-cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	base := relational.NewClientKeyRepository(database)
	created, err := NewService(base, successfulRateLimiter{}, successfulConcurrencyLimiter{}, 60, 5, testCipher(t)).Create(ctx, CreateInput{Name: "cached", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	repository := &countingClientKeyRepository{ClientKeyRepository: base}
	service := NewService(repository, successfulRateLimiter{}, successfulConcurrencyLimiter{}, 60, 5, testCipher(t))
	for range 2 {
		_, release, err := service.Authenticate(ctx, created.Secret)
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	if repository.lookups != 1 {
		t.Fatalf("鉴权查询次数 = %d, want 1", repository.lookups)
	}
	if _, err := service.BatchSetEnabled(ctx, []uint64{created.Key.ID}, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Authenticate(ctx, created.Secret); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("停用后的鉴权错误 = %v", err)
	}
	if repository.lookups != 2 {
		t.Fatalf("缓存失效后的查询次数 = %d, want 2", repository.lookups)
	}
}

func testCipher(t *testing.T) *security.Cipher {
	t.Helper()
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return cipher
}

type failingRateLimiter struct{}

func (failingRateLimiter) Allow(context.Context, string, int, time.Time) (bool, error) {
	return false, errors.New("redis unavailable")
}

type successfulRateLimiter struct{}

func (successfulRateLimiter) Allow(context.Context, string, int, time.Time) (bool, error) {
	return true, nil
}

type failingConcurrencyLimiter struct{}

func (failingConcurrencyLimiter) Acquire(context.Context, string, int) (func(), bool, error) {
	return nil, false, errors.New("redis unavailable")
}
func (failingConcurrencyLimiter) Current(context.Context, string) (int, error) { return 0, nil }

type successfulConcurrencyLimiter struct{}

func (successfulConcurrencyLimiter) Acquire(context.Context, string, int) (func(), bool, error) {
	return func() {}, true, nil
}
func (successfulConcurrencyLimiter) Current(context.Context, string) (int, error) { return 0, nil }

type failingClientKeyRepository struct{ repository.ClientKeyRepository }

func (failingClientKeyRepository) GetByPrefix(context.Context, string) (clientkeydomain.Key, error) {
	return clientkeydomain.Key{}, errors.New("database unavailable")
}

type countingClientKeyRepository struct {
	repository.ClientKeyRepository
	lookups int
}

func (r *countingClientKeyRepository) GetByPrefix(ctx context.Context, prefix string) (clientkeydomain.Key, error) {
	r.lookups++
	return r.ClientKeyRepository.GetByPrefix(ctx, prefix)
}

var _ repository.RateLimiter = failingRateLimiter{}
var _ repository.ConcurrencyLimiter = failingConcurrencyLimiter{}
