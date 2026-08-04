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

func TestQualityGuardIdentityIsStableHiddenAndSystemManaged(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "quality-guard-identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := relational.NewClientKeyRepository(database)
	cipher := testCipher(t)
	service := NewService(repository, successfulRateLimiter{}, successfulConcurrencyLimiter{}, 60, 5, cipher)
	unused, err := service.EnsureQualityGuardIdentity(ctx, false)
	if err != nil || unused.ID != 0 {
		t.Fatalf("disabled fresh identity = %#v, err = %v", unused, err)
	}

	first, err := service.EnsureQualityGuardIdentity(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.EnsureQualityGuardIdentity(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == 0 || second.ID != first.ID || first.InternalKind != clientkeydomain.InternalKindQualityGuard || !first.Enabled || first.ProviderScope != clientkeydomain.ProviderScopeBuild {
		t.Fatalf("first = %#v, second = %#v", first, second)
	}
	if values, total, listErr := service.List(ctx, 1, 20, "", ListFilter{}); listErr != nil || total != 0 || len(values) != 0 {
		t.Fatalf("system identity leaked in list: values=%#v total=%d err=%v", values, total, listErr)
	}
	raw, err := cipher.Decrypt(first.EncryptedSecret)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, authErr := service.Authenticate(ctx, raw); !errors.Is(authErr, ErrInvalidKey) {
		t.Fatalf("system identity authenticated externally: %v", authErr)
	}
	if _, revealErr := service.RevealSecret(ctx, first.ID); !errors.Is(revealErr, ErrSystemManaged) {
		t.Fatalf("reveal error = %v", revealErr)
	}
	if _, updateErr := service.Update(ctx, first.ID, UpdateInput{}); !errors.Is(updateErr, ErrSystemManaged) {
		t.Fatalf("update error = %v", updateErr)
	}
	if deleteErr := service.Delete(ctx, first.ID); !errors.Is(deleteErr, ErrSystemManaged) {
		t.Fatalf("delete error = %v", deleteErr)
	}
	if _, batchErr := service.BatchSetEnabled(ctx, []uint64{first.ID}, false); !errors.Is(batchErr, ErrSystemManaged) {
		t.Fatalf("batch update error = %v", batchErr)
	}
	if _, batchErr := service.BatchDelete(ctx, []uint64{first.ID}); !errors.Is(batchErr, ErrSystemManaged) {
		t.Fatalf("batch delete error = %v", batchErr)
	}
	disabled, err := service.EnsureQualityGuardIdentity(ctx, false)
	if err != nil || disabled.ID != first.ID || disabled.Enabled {
		t.Fatalf("disabled = %#v, err = %v", disabled, err)
	}
	reenabled, err := service.EnsureQualityGuardIdentity(ctx, true)
	if err != nil || reenabled.ID != first.ID || !reenabled.Enabled {
		t.Fatalf("reenabled = %#v, err = %v", reenabled, err)
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

func TestCleanupExpiredBillingProtectsActiveRequest(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "active-billing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := relational.NewClientKeyRepository(database)
	service := NewService(repository, nil, nil, 60, 5, testCipher(t))
	created, err := service.Create(ctx, CreateInput{Name: "active", Enabled: true, BillingLimitUSDTicks: 100})
	if err != nil {
		t.Fatal(err)
	}
	const eventID = "evt_active_cleanup_protection"
	if reserved, reserveErr := service.ReserveBilling(ctx, created.Key, eventID, 40, time.Nanosecond); reserveErr != nil || !reserved {
		t.Fatalf("reserve: reserved=%v err=%v", reserved, reserveErr)
	}
	time.Sleep(time.Millisecond)
	if cleaned, cleanupErr := service.CleanupExpiredBilling(ctx, 10); cleanupErr != nil || cleaned != 0 {
		t.Fatalf("active cleanup: cleaned=%d err=%v", cleaned, cleanupErr)
	}
	service.CompleteBilling(eventID)
	if cleaned, cleanupErr := service.CleanupExpiredBilling(ctx, 10); cleanupErr != nil || cleaned != 1 {
		t.Fatalf("completed cleanup: cleaned=%d err=%v", cleaned, cleanupErr)
	}
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

func TestAccountScopePersistsAndAuthCacheInvalidatesOnChange(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "account-pool-auth-cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	base := relational.NewClientKeyRepository(database)
	service := NewService(base, successfulRateLimiter{}, successfulConcurrencyLimiter{}, 60, 5, testCipher(t))
	created, err := service.Create(ctx, CreateInput{Name: "scoped", Enabled: true, ProviderScope: clientkeydomain.ProviderScopeBuild | clientkeydomain.ProviderScopeWeb, TierScope: clientkeydomain.TierScopeFree})
	if err != nil {
		t.Fatal(err)
	}
	value, release, err := service.Authenticate(ctx, created.Secret)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if value.ProviderScope != clientkeydomain.ProviderScopeBuild|clientkeydomain.ProviderScopeWeb || value.TierScope != clientkeydomain.TierScopeFree {
		t.Fatalf("authenticated account scope = %+v", value.AccountScope())
	}
	consoleScope := clientkeydomain.ProviderScopeConsole
	superTier := clientkeydomain.TierScopeSuper
	if _, err := service.Update(ctx, created.Key.ID, UpdateInput{ProviderScope: &consoleScope, TierScope: &superTier}); err != nil {
		t.Fatal(err)
	}
	value, release, err = service.Authenticate(ctx, created.Secret)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if value.ProviderScope != clientkeydomain.ProviderScopeConsole || value.TierScope != clientkeydomain.TierScopeSuper {
		t.Fatalf("account scope after cache invalidation = %+v", value.AccountScope())
	}
	stored, err := base.Get(ctx, created.Key.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.ProviderScope = clientkeydomain.ProviderScopeWeb
	stored.TierScope = clientkeydomain.TierScopeFree
	if _, err := base.Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	value, release, err = service.Authenticate(ctx, created.Secret)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if value.ProviderScope != clientkeydomain.ProviderScopeConsole || value.TierScope != clientkeydomain.TierScopeSuper {
		t.Fatalf("cache unexpectedly changed before remote invalidation = %+v", value.AccountScope())
	}
	service.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationClientKeyChanged, ClientKeyID: created.Key.ID})
	value, release, err = service.Authenticate(ctx, created.Secret)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if value.ProviderScope != clientkeydomain.ProviderScopeWeb || value.TierScope != clientkeydomain.TierScopeFree {
		t.Fatalf("account scope after remote invalidation = %+v", value.AccountScope())
	}
	service.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationClientKeyChanged})
	service.authCache.mu.RLock()
	cacheEntries := len(service.authCache.byPrefix)
	service.authCache.mu.RUnlock()
	if cacheEntries != 0 {
		t.Fatalf("batch invalidation retained %d auth cache entries", cacheEntries)
	}
	invalid := clientkeydomain.ProviderScope(8)
	if _, err := service.Update(ctx, created.Key.ID, UpdateInput{ProviderScope: &invalid}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid account scope error = %v", err)
	}
	all, err := service.Create(ctx, CreateInput{Name: "legacy-default", Enabled: true})
	if err != nil || all.Key.ProviderScope != clientkeydomain.ProviderScopeAll || all.Key.TierScope != clientkeydomain.TierScopeAll {
		t.Fatalf("default account scope = %+v, err = %v", all.Key.AccountScope(), err)
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
