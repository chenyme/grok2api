package account

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestAutoCleanReauthRespectsMinAgeAndIncludeDisabled(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	service, repo := newAutoCleanTestService(t, now)

	aged := mustUpsert(t, repo, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "aged-reauth", SourceKey: "aged-reauth",
		EncryptedAccessToken: "x", Enabled: true, AuthStatus: accountdomain.AuthStatusReauthRequired,
		ReauthMarkedAt: ptrTime(now.Add(-2 * time.Hour)),
	})
	fresh := mustUpsert(t, repo, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "fresh-reauth", SourceKey: "fresh-reauth",
		EncryptedAccessToken: "x", Enabled: true, AuthStatus: accountdomain.AuthStatusReauthRequired,
		ReauthMarkedAt: ptrTime(now.Add(-10 * time.Minute)),
	})
	activePermanent := mustUpsert(t, repo, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "active-permanent", SourceKey: "active-permanent",
		EncryptedAccessToken: "x", EncryptedRefreshToken: "r", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
		RefreshPermanent: true, ExpiresAt: now.Add(time.Hour),
	})
	cooldownUntil := now.Add(time.Hour)
	cooldown := mustUpsert(t, repo, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "cooldown", SourceKey: "cooldown",
		EncryptedAccessToken: "x", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
		CooldownUntil: &cooldownUntil,
	})
	disabledAged := mustUpsert(t, repo, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "disabled-aged", SourceKey: "disabled-aged",
		EncryptedAccessToken: "x", Enabled: true, AuthStatus: accountdomain.AuthStatusReauthRequired,
		ReauthMarkedAt: ptrTime(now.Add(-3 * time.Hour)),
	})
	disabledAged.Enabled = false
	var err error
	disabledAged, err = repo.Update(ctx, disabledAged)
	if err != nil {
		t.Fatal(err)
	}

	// Flag off is a no-op.
	service.UpdateAutoCleanConfig(AutoCleanConfig{
		Enabled: false, Interval: 10 * time.Minute, MinAge: time.Hour,
	})
	if err := service.runAutoCleanReauth(ctx, service.autoCleanConfig()); err != nil {
		t.Fatal(err)
	}
	assertPresent(t, repo, aged.ID)
	assertPresent(t, repo, fresh.ID)
	assertPresent(t, repo, disabledAged.ID)

	// Enabled without include-disabled: only aged enabled reauth is deleted.
	service.UpdateAutoCleanConfig(AutoCleanConfig{
		Enabled: true, Interval: 10 * time.Minute, MinAge: time.Hour, IncludeDisabled: false,
	})
	if err := service.runAutoCleanReauth(ctx, service.autoCleanConfig()); err != nil {
		t.Fatal(err)
	}
	assertMissing(t, repo, aged.ID)
	assertPresent(t, repo, fresh.ID)
	assertPresent(t, repo, activePermanent.ID)
	assertPresent(t, repo, cooldown.ID)
	assertPresent(t, repo, disabledAged.ID)

	// Include disabled: aged disabled reauth is deleted; fresh remains.
	service.UpdateAutoCleanConfig(AutoCleanConfig{
		Enabled: true, Interval: 10 * time.Minute, MinAge: time.Hour, IncludeDisabled: true,
	})
	if err := service.runAutoCleanReauth(ctx, service.autoCleanConfig()); err != nil {
		t.Fatal(err)
	}
	assertMissing(t, repo, disabledAged.ID)
	assertPresent(t, repo, fresh.ID)
	assertPresent(t, repo, activePermanent.ID)
	assertPresent(t, repo, cooldown.ID)

	// Advance clock past minAge for the remaining fresh reauth.
	service.now = func() time.Time { return now.Add(2 * time.Hour) }
	if err := service.runAutoCleanReauth(ctx, service.autoCleanConfig()); err != nil {
		t.Fatal(err)
	}
	assertMissing(t, repo, fresh.ID)
	assertPresent(t, repo, activePermanent.ID)
	assertPresent(t, repo, cooldown.ID)
}

func TestMarkReauthRequiredSetsAnchorAndEditDoesNotReset(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 15, 0, 0, 0, time.UTC)
	service, repo := newAutoCleanTestService(t, now)

	value := mustUpsert(t, repo, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "anchor", SourceKey: "anchor",
		EncryptedAccessToken: "x", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err := service.MarkReauthRequired(ctx, value.ID, "token rejected"); err != nil {
		t.Fatal(err)
	}
	marked, err := repo.Get(ctx, value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if marked.AuthStatus != accountdomain.AuthStatusReauthRequired || marked.ReauthMarkedAt == nil {
		t.Fatalf("expected reauth anchor, got %#v", marked)
	}
	anchor := *marked.ReauthMarkedAt

	// Ordinary edit must not reset reauth_marked_at.
	marked.Name = "anchor-renamed"
	if _, err := repo.Update(ctx, marked); err != nil {
		t.Fatal(err)
	}
	afterEdit, err := repo.Get(ctx, value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterEdit.ReauthMarkedAt == nil || !afterEdit.ReauthMarkedAt.Equal(anchor) {
		t.Fatalf("reauth_marked_at reset by edit: before=%s after=%v", anchor, afterEdit.ReauthMarkedAt)
	}
}

func TestAutoCleanReauthMultiBatch(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 18, 0, 0, 0, time.UTC)
	service, repo := newAutoCleanTestService(t, now)

	const total = 105
	ids := make([]uint64, 0, total)
	for i := 0; i < total; i++ {
		value := mustUpsert(t, repo, accountdomain.Credential{
			Provider: accountdomain.ProviderBuild, Name: "batch-" + itoa(i), SourceKey: "batch-" + itoa(i),
			EncryptedAccessToken: "x", Enabled: true, AuthStatus: accountdomain.AuthStatusReauthRequired,
			ReauthMarkedAt: ptrTime(now.Add(-2 * time.Hour)),
		})
		ids = append(ids, value.ID)
	}

	// 直接验证 repo 分批：第一批最多 100，且 nextAfter 前进。
	candidates, err := repo.ListAutoCleanReauthCandidates(ctx, now.Add(-time.Hour), false, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := repo.DeleteAutoCleanReauthCandidates(ctx, now.Add(-time.Hour), false, candidates)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 100 || len(deleted) != 100 || candidates[len(candidates)-1] == 0 {
		t.Fatalf("first batch candidates=%d deleted=%d nextAfter=%d", len(candidates), len(deleted), candidates[len(candidates)-1])
	}
	remaining := 0
	for _, id := range ids {
		if _, getErr := repo.Get(ctx, id); getErr == nil {
			remaining++
		}
	}
	if remaining != 5 {
		t.Fatalf("remaining after first batch = %d", remaining)
	}

	// 应用层应扫完全部剩余。
	service.UpdateAutoCleanConfig(AutoCleanConfig{
		Enabled: true, Interval: 10 * time.Minute, MinAge: time.Hour,
	})
	if err := service.runAutoCleanReauth(ctx, service.autoCleanConfig()); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		assertMissing(t, repo, id)
	}
}

func TestSecondMarkReauthKeepsOriginalAnchor(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 16, 0, 0, 0, time.UTC)
	service, repo := newAutoCleanTestService(t, now)
	value := mustUpsert(t, repo, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "second-mark", SourceKey: "second-mark",
		EncryptedAccessToken: "x", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err := service.MarkReauthRequired(ctx, value.ID, "first"); err != nil {
		t.Fatal(err)
	}
	first, err := repo.Get(ctx, value.ID)
	if err != nil || first.ReauthMarkedAt == nil {
		t.Fatalf("first mark = %#v err=%v", first, err)
	}
	anchor := *first.ReauthMarkedAt
	time.Sleep(5 * time.Millisecond)
	if err := service.MarkReauthRequired(ctx, value.ID, "second"); err != nil {
		t.Fatal(err)
	}
	second, err := repo.Get(ctx, value.ID)
	if err != nil || second.ReauthMarkedAt == nil || !second.ReauthMarkedAt.Equal(anchor) {
		t.Fatalf("anchor reset: first=%s second=%v", anchor, second.ReauthMarkedAt)
	}
}

func TestRunAccountAutoCleanDoesNotDeleteOnEnableOrWake(t *testing.T) {
	now := time.Date(2026, 7, 20, 19, 0, 0, 0, time.UTC)
	service, repo := newAutoCleanTestService(t, now)
	aged := mustUpsert(t, repo, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "wake-aged", SourceKey: "wake-aged",
		EncryptedAccessToken: "x", Enabled: true, AuthStatus: accountdomain.AuthStatusReauthRequired,
		ReauthMarkedAt: ptrTime(now.Add(-2 * time.Hour)),
	})

	// 启用只写入配置并唤醒；本身不删除。
	service.UpdateAutoCleanConfig(AutoCleanConfig{
		Enabled: true, Interval: time.Minute, MinAge: time.Hour,
	})
	assertPresent(t, repo, aged.ID)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.RunAccountAutoClean(ctx)
	}()

	// 启动时会 drain 启动前 wake 并 arm timer；热更 wake 只重排 timer。
	time.Sleep(150 * time.Millisecond)
	assertPresent(t, repo, aged.ID)

	service.UpdateAutoCleanConfig(AutoCleanConfig{
		Enabled: true, Interval: time.Minute, MinAge: time.Hour, IncludeDisabled: true,
	})
	time.Sleep(150 * time.Millisecond)
	assertPresent(t, repo, aged.ID)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("auto-clean scheduler did not stop")
	}
	assertPresent(t, repo, aged.ID)
}

func newAutoCleanTestService(t *testing.T, now time.Time) (*Service, *relational.AccountRepository) {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "auto-clean.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(database)
	service := NewService(repo, nil, nil, memory.NewStickyStore(), nil, nil, nil)
	service.now = func() time.Time { return now }
	return service, repo
}

func mustUpsert(t *testing.T, repo *relational.AccountRepository, value accountdomain.Credential) accountdomain.Credential {
	t.Helper()
	out, _, err := repo.UpsertByIdentity(context.Background(), value)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertMissing(t *testing.T, repo *relational.AccountRepository, id uint64) {
	t.Helper()
	if _, err := repo.Get(context.Background(), id); err == nil {
		t.Fatalf("account %d still present", id)
	} else if !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("account %d get error: %v", id, err)
	}
}

func assertPresent(t *testing.T, repo *relational.AccountRepository, id uint64) {
	t.Helper()
	if _, err := repo.Get(context.Background(), id); err != nil {
		t.Fatalf("account %d missing or error: %v", id, err)
	}
}

func ptrTime(value time.Time) *time.Time { return &value }

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for value > 0 {
		i--
		buf[i] = byte('0' + value%10)
		value /= 10
	}
	return string(buf[i:])
}

func TestUpdateAutoCleanConfigClamps(t *testing.T) {
	service, _ := newAutoCleanTestService(t, time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC))
	service.UpdateAutoCleanConfig(AutoCleanConfig{
		Enabled: true, Interval: 30 * time.Second, MinAge: 10 * time.Second, IncludeDisabled: true,
	})
	cfg := service.autoCleanConfig()
	if cfg.Interval != time.Minute || cfg.MinAge != time.Minute || !cfg.IncludeDisabled || !cfg.Enabled {
		t.Fatalf("low clamp = %#v", cfg)
	}
	service.UpdateAutoCleanConfig(AutoCleanConfig{
		Enabled: false, Interval: 2 * time.Hour, MinAge: 40 * 24 * time.Hour,
	})
	cfg = service.autoCleanConfig()
	if cfg.Interval != time.Hour || cfg.MinAge != 30*24*time.Hour || cfg.Enabled {
		t.Fatalf("high clamp = %#v", cfg)
	}
	if got := autoCleanInterval(AutoCleanConfig{Enabled: false, Interval: time.Minute}); got != time.Hour {
		t.Fatalf("disabled interval = %s", got)
	}
	if got := autoCleanInterval(AutoCleanConfig{Enabled: true, Interval: 5 * time.Minute}); got != 5*time.Minute {
		t.Fatalf("enabled interval = %s", got)
	}
}

func TestAutoCleanSkipsActiveInferenceLease(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 22, 0, 0, 0, time.UTC)
	service, repo := newAutoCleanTestService(t, now)
	limiter := memory.NewConcurrencyLimiter()
	service.SetConcurrencyLimiter(limiter)
	value := mustUpsert(t, repo, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "active-lease", SourceKey: "active-lease",
		EncryptedAccessToken: "x", Enabled: true, AuthStatus: accountdomain.AuthStatusReauthRequired,
		ReauthMarkedAt: ptrTime(now.Add(-2 * time.Hour)),
	})
	release, acquired, err := limiter.Acquire(ctx, repository.AccountConcurrencyKey(value.ID), 1)
	if err != nil || !acquired {
		t.Fatalf("acquire lease: acquired=%v err=%v", acquired, err)
	}
	service.UpdateAutoCleanConfig(AutoCleanConfig{Enabled: true, Interval: time.Minute, MinAge: time.Hour})
	if err := service.runAutoCleanReauth(ctx, service.autoCleanConfig()); err != nil {
		t.Fatal(err)
	}
	assertPresent(t, repo, value.ID)
	release()
	if err := service.runAutoCleanReauth(ctx, service.autoCleanConfig()); err != nil {
		t.Fatal(err)
	}
	assertMissing(t, repo, value.ID)
}

func TestAutoCleanConfigRevisionRejectsOldTimerAndUnchangedUpdateDoesNotWake(t *testing.T) {
	service, _ := newAutoCleanTestService(t, time.Date(2026, 7, 20, 23, 0, 0, 0, time.UTC))
	service.UpdateAutoCleanConfig(AutoCleanConfig{Enabled: true, Interval: 5 * time.Minute, MinAge: time.Hour})
	select {
	case <-service.autoCleanWake:
	default:
		t.Fatal("initial config update did not wake scheduler")
	}
	cfg, revision := service.autoCleanSnapshot()
	service.UpdateAutoCleanConfig(cfg)
	select {
	case <-service.autoCleanWake:
		t.Fatal("unchanged config woke scheduler")
	default:
	}
	service.UpdateAutoCleanConfig(AutoCleanConfig{Enabled: true, Interval: 5 * time.Minute, MinAge: 2 * time.Hour})
	if service.autoCleanRevisionCurrent(revision, cfg) {
		t.Fatal("old timer revision remained executable after config update")
	}
}

type deniedAutoCleanLock struct{}

func (deniedAutoCleanLock) Acquire(context.Context, string, time.Duration) (func(), bool, error) {
	return nil, false, nil
}

func TestAutoCleanSkipsWhenDistributedLockIsHeld(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)
	service, repo := newAutoCleanTestService(t, now)
	service.refreshLock = deniedAutoCleanLock{}
	value := mustUpsert(t, repo, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "locked", SourceKey: "locked",
		EncryptedAccessToken: "x", Enabled: true, AuthStatus: accountdomain.AuthStatusReauthRequired,
		ReauthMarkedAt: ptrTime(now.Add(-2 * time.Hour)),
	})
	service.UpdateAutoCleanConfig(AutoCleanConfig{Enabled: true, Interval: time.Minute, MinAge: time.Hour})
	if err := service.runAutoCleanReauth(ctx, service.autoCleanConfig()); err != nil {
		t.Fatal(err)
	}
	assertPresent(t, repo, value.ID)
}

type endlessAutoCleanRepository struct {
	repository.AccountRepository
	listCalls   int
	deleteCalls int
	deletedIDs  []uint64
}

func (r *endlessAutoCleanRepository) ListAutoCleanReauthCandidates(_ context.Context, _ time.Time, _ bool, afterID uint64, _ int) ([]uint64, error) {
	r.listCalls++
	ids := make([]uint64, autoCleanReauthBatchSize)
	for index := range ids {
		ids[index] = afterID + uint64(index) + 1
	}
	return ids, nil
}

func (r *endlessAutoCleanRepository) DeleteAutoCleanReauthCandidates(_ context.Context, _ time.Time, _ bool, ids []uint64) ([]uint64, error) {
	r.deleteCalls++
	r.deletedIDs = append(r.deletedIDs, ids...)
	return append([]uint64(nil), ids...), nil
}

func TestAutoCleanLimitsWorkPerTick(t *testing.T) {
	repo := &endlessAutoCleanRepository{}
	service := NewService(repo, nil, nil, nil, nil, nil, nil)
	service.UpdateAutoCleanConfig(AutoCleanConfig{Enabled: true, Interval: time.Minute, MinAge: time.Hour})
	if err := service.runAutoCleanReauth(context.Background(), service.autoCleanConfig()); err != nil {
		t.Fatal(err)
	}
	if repo.listCalls != autoCleanReauthMaxDeletes || repo.deleteCalls != autoCleanReauthMaxDeletes {
		t.Fatalf("calls list=%d delete=%d", repo.listCalls, repo.deleteCalls)
	}
}

type activeKeyConcurrency struct {
	active map[string]struct{}
	all    bool
}

func (*activeKeyConcurrency) Acquire(context.Context, string, int) (func(), bool, error) {
	return func() {}, true, nil
}

func (l *activeKeyConcurrency) Current(_ context.Context, key string) (int, error) {
	if l.all {
		return 1, nil
	}
	if _, ok := l.active[key]; ok {
		return 1, nil
	}
	return 0, nil
}

func (l *activeKeyConcurrency) CurrentMany(_ context.Context, keys []string) (map[string]int, error) {
	values := make(map[string]int, len(keys))
	for _, key := range keys {
		current, _ := l.Current(context.Background(), key)
		values[key] = current
	}
	return values, nil
}

func TestAutoCleanActiveOnlyPagesDoNotConsumeDeleteBudget(t *testing.T) {
	repo := &endlessAutoCleanRepository{}
	service := NewService(repo, nil, nil, nil, nil, nil, nil)
	active := make(map[string]struct{}, 2*autoCleanReauthBatchSize)
	for id := uint64(1); id <= 2*autoCleanReauthBatchSize; id++ {
		active[repository.AccountConcurrencyKey(id)] = struct{}{}
	}
	service.SetConcurrencyLimiter(&activeKeyConcurrency{active: active})
	service.UpdateAutoCleanConfig(AutoCleanConfig{Enabled: true, Interval: time.Minute, MinAge: time.Hour})
	if err := service.runAutoCleanReauth(context.Background(), service.autoCleanConfig()); err != nil {
		t.Fatal(err)
	}
	if repo.listCalls != autoCleanReauthMaxDeletes+2 || repo.deleteCalls != autoCleanReauthMaxDeletes {
		t.Fatalf("calls list=%d delete=%d", repo.listCalls, repo.deleteCalls)
	}
	if len(repo.deletedIDs) == 0 || repo.deletedIDs[0] != 2*autoCleanReauthBatchSize+1 {
		t.Fatalf("first deleted id=%v", repo.deletedIDs)
	}
}

func TestAutoCleanActiveOnlySourceIsBoundedByScanBudget(t *testing.T) {
	repo := &endlessAutoCleanRepository{}
	service := NewService(repo, nil, nil, nil, nil, nil, nil)
	service.SetConcurrencyLimiter(&activeKeyConcurrency{all: true})
	service.UpdateAutoCleanConfig(AutoCleanConfig{Enabled: true, Interval: time.Minute, MinAge: time.Hour})
	if err := service.runAutoCleanReauth(context.Background(), service.autoCleanConfig()); err != nil {
		t.Fatal(err)
	}
	if repo.listCalls != autoCleanReauthMaxScans || repo.deleteCalls != 0 {
		t.Fatalf("calls list=%d delete=%d", repo.listCalls, repo.deleteCalls)
	}
}

type configChangingConcurrency struct {
	service *Service
	once    bool
}

func (c *configChangingConcurrency) Acquire(context.Context, string, int) (func(), bool, error) {
	return func() {}, true, nil
}

func (c *configChangingConcurrency) Current(context.Context, string) (int, error) {
	if !c.once {
		c.once = true
		c.service.UpdateAutoCleanConfig(AutoCleanConfig{Enabled: true, Interval: 5 * time.Minute, MinAge: 2 * time.Hour})
	}
	return 0, nil
}

func TestAutoCleanPolicyChangeAbortsBeforeDelete(t *testing.T) {
	repo := &endlessAutoCleanRepository{}
	service := NewService(repo, nil, nil, nil, nil, nil, nil)
	service.SetConcurrencyLimiter(&configChangingConcurrency{service: service})
	service.UpdateAutoCleanConfig(AutoCleanConfig{Enabled: true, Interval: 5 * time.Minute, MinAge: time.Hour})
	if err := service.runAutoCleanReauth(context.Background(), service.autoCleanConfig()); err != nil {
		t.Fatal(err)
	}
	if repo.deleteCalls != 0 {
		t.Fatalf("delete calls after policy change=%d", repo.deleteCalls)
	}
}
