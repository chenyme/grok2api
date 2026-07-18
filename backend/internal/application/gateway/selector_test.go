package gateway

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestSelectorPrioritizesDueQuotaProbeOnce(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "selector.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}

	accounts := relational.NewAccountRepository(database)
	probe, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "probe", SourceKey: "probe", EncryptedAccessToken: "encrypted", Enabled: true,
		AuthStatus: account.AuthStatusActive, Priority: 10, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	active, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "active", SourceKey: "active", EncryptedAccessToken: "encrypted", Enabled: true,
		AuthStatus: account.AuthStatusActive, Priority: 200, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	due := now.Add(-time.Minute)
	if err := accounts.SaveQuotaRecovery(ctx, account.QuotaRecovery{
		AccountID: probe.ID, Kind: account.QuotaRecoveryKindFree, Status: account.QuotaRecoveryStatusExhausted,
		ConfirmedUsed: 1_065_387, ConfirmedLimit: 1_000_000,
		ExhaustedAt: &now, NextProbeAt: &due, LastConfirmedAt: &now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	lease, err := selector.Acquire(ctx, account.ProviderBuild, "grok-test", "", "", map[uint64]bool{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Credential.ID != probe.ID || !lease.QuotaProbe {
		t.Fatalf("lease = %#v, want due probe account %d", lease, probe.ID)
	}
	lease.Release()

	lease, err = selector.Acquire(ctx, account.ProviderBuild, "grok-test", "", "", map[uint64]bool{probe.ID: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Credential.ID != active.ID || lease.QuotaProbe {
		t.Fatalf("lease = %#v, want active account %d", lease, active.ID)
	}
	lease.Release()

	selector.MarkSuccess(ctx, probe)
	if _, err := accounts.GetQuotaRecovery(ctx, probe.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("quota recovery should be cleared, err = %v", err)
	}
}

func BenchmarkSelectorCandidatePlanning(b *testing.B) {
	ctx := context.Background()
	limiter := memory.NewConcurrencyLimiter()
	selector := NewSelector(nil, limiter, nil, nil, time.Hour, time.Second, time.Minute)
	now := time.Now().UTC()
	candidates := make([]account.RoutingCandidate, 3000)
	for index := range candidates {
		id := uint64(index + 1)
		billing := account.Billing{
			AccountID: id, MonthlyLimit: 1_000_000, Used: float64(index % 1000), SyncedAt: now.Add(-time.Duration(index%60) * time.Minute),
		}
		candidates[index] = account.RoutingCandidate{
			Credential: account.Credential{
				ID: id, Provider: account.ProviderBuild, AuthStatus: account.AuthStatusActive,
				Priority: index % 10, MaxConcurrent: account.DefaultMaxConcurrent,
			},
			Billing: &billing, ModelCapabilityKnown: true, SupportsModel: true,
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(parallel *testing.PB) {
		for parallel.Next() {
			values := append([]account.RoutingCandidate(nil), candidates...)
			plan, err := selector.planCandidates(ctx, values, now, nil)
			if err != nil {
				b.Fatal(err)
			}
			if _, ok := plan.Next(); !ok {
				b.Fatal("候选计划为空")
			}
		}
	})
}

func TestSelectorSkipsQuotaProbeBeforeDue(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "selector.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}

	accounts := relational.NewAccountRepository(database)
	value, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "waiting", SourceKey: "waiting", EncryptedAccessToken: "encrypted", Enabled: true,
		AuthStatus: account.AuthStatusActive, Priority: 100, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	next := now.Add(time.Hour)
	if err := accounts.SaveQuotaRecovery(ctx, account.QuotaRecovery{
		AccountID: value.ID, Kind: account.QuotaRecoveryKindFree, Status: account.QuotaRecoveryStatusExhausted,
		NextProbeAt: &next, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	if _, err := selector.Acquire(ctx, account.ProviderBuild, "grok-test", "", "", map[uint64]bool{}, true); err == nil {
		t.Fatal("expected no account before next probe time")
	}
}

func TestSelectorUsesPaidWeeklyPoolAsWebQuotaGate(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "weekly-web.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	value, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: "paid-web", SourceKey: "paid-web",
		EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	resetAt := now.Add(7 * 24 * time.Hour)
	if err := accounts.SaveQuotaWindows(ctx, value.ID, account.WebTierSuper, now, []account.QuotaWindow{
		{AccountID: value.ID, Mode: "weekly", Remaining: 0, Total: 10000, UsagePercent: 100, ResetAt: &resetAt, SyncedAt: &now, Source: account.QuotaSourceUpstream},
		{AccountID: value.ID, Mode: "fast", Remaining: 30, Total: 30, ResetAt: &resetAt, SyncedAt: &now, Source: account.QuotaSourceUpstream},
	}); err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	if _, err := selector.Acquire(ctx, account.ProviderWeb, "", "fast", "", nil, false); err == nil {
		t.Fatal("exhausted weekly pool must take precedence over a stale fast quota window")
	}
	if err := accounts.SaveQuotaWindows(ctx, value.ID, account.WebTierSuper, now, []account.QuotaWindow{
		{AccountID: value.ID, Mode: "weekly", Remaining: 8900, Total: 10000, UsagePercent: 11, ResetAt: &resetAt, SyncedAt: &now, Source: account.QuotaSourceUpstream},
		{AccountID: value.ID, Mode: "fast", Remaining: 0, Total: 30, ResetAt: &resetAt, SyncedAt: &now, Source: account.QuotaSourceUpstream},
	}); err != nil {
		t.Fatal(err)
	}
	selector.MarkQuotaStateChanged(account.ProviderWeb)
	lease, err := selector.Acquire(ctx, account.ProviderWeb, "", "fast", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.QuotaMode != "weekly" {
		t.Fatalf("quota mode = %q, want weekly", lease.QuotaMode)
	}
}

func TestSelectorClaimsPaidBillingProbeAfterPeriodEnd(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "paid-probe.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	value, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "paid", SourceKey: "paid", EncryptedAccessToken: "encrypted", AuthStatus: account.AuthStatusActive, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	due := now.Add(-time.Minute)
	if err := accounts.SaveQuotaRecovery(ctx, account.QuotaRecovery{AccountID: value.ID, Kind: account.QuotaRecoveryKindPaid, Status: account.QuotaRecoveryStatusExhausted, NextProbeAt: &due, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	lease, err := selector.Acquire(ctx, account.ProviderBuild, "", "", "", map[uint64]bool{}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if !lease.QuotaProbe || lease.QuotaProbeKind != account.QuotaRecoveryKindPaid {
		t.Fatalf("lease = %#v", lease)
	}
}

func TestSelectorOnlyUsesAccountsSupportingRequestedModel(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "selector-model.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}

	accounts := relational.NewAccountRepository(database)
	models := relational.NewModelRepository(database)
	unsupported, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "basic", SourceKey: "basic", EncryptedAccessToken: "encrypted", AuthStatus: account.AuthStatusActive,
		Priority: 500, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	supported, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "premium", SourceKey: "premium", EncryptedAccessToken: "encrypted", AuthStatus: account.AuthStatusActive,
		Priority: 100, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := accounts.SaveBilling(ctx, account.Billing{AccountID: unsupported.ID, IsUnifiedBillingUser: true, SyncedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.SaveBilling(ctx, account.Billing{AccountID: supported.ID, MonthlyLimit: 100, SyncedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := models.ReplaceAccountCapabilities(ctx, unsupported.ID, []string{"grok-basic"}, now); err != nil {
		t.Fatal(err)
	}
	if err := models.ReplaceAccountCapabilities(ctx, supported.ID, []string{"grok-basic", "grok-premium"}, now); err != nil {
		t.Fatal(err)
	}

	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	selector.UpdatePreferFreeBuild(true)
	lease, err := selector.Acquire(ctx, account.ProviderBuild, "grok-premium", "", "", map[uint64]bool{}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Credential.ID != supported.ID {
		t.Fatalf("selected account = %d, want %d", lease.Credential.ID, supported.ID)
	}
}

func TestSelectorKeepsWebQuotaModesIsolated(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "selector-web-quota.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	value, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierSuper,
		Name: "web", SourceKey: "web", EncryptedAccessToken: "encrypted", AuthStatus: account.AuthStatusActive, MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	resetAt := now.Add(time.Hour)
	if err := accounts.SaveQuotaWindows(ctx, value.ID, account.WebTierSuper, now, []account.QuotaWindow{
		{AccountID: value.ID, Mode: "fast", Remaining: 0, Total: 20, ResetAt: &resetAt, Source: account.QuotaSourceUpstream},
		{AccountID: value.ID, Mode: "auto", Remaining: 5, Total: 10, ResetAt: &resetAt, Source: account.QuotaSourceUpstream},
	}); err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	if _, err := selector.Acquire(ctx, account.ProviderWeb, "grok-chat", "fast", "", nil, false); err == nil {
		t.Fatal("exhausted fast mode should not be selected")
	}
	lease, err := selector.Acquire(ctx, account.ProviderWeb, "grok-chat-auto", "auto", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Credential.ID != value.ID || lease.QuotaMode != "auto" {
		t.Fatalf("lease = %#v", lease)
	}
}

func TestSelectorHonorsWebTierPoolOrderBeforeAccountPriority(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "selector-web-tier.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	for index, tier := range []account.WebTier{account.WebTierBasic, account.WebTierSuper, account.WebTierHeavy} {
		if _, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: tier,
			Name: string(tier), SourceKey: string(tier), EncryptedAccessToken: "encrypted",
			AuthStatus: account.AuthStatusActive, Priority: 300 - index*100, MaxConcurrent: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), staticTierOrder{order: []account.WebTier{account.WebTierHeavy, account.WebTierSuper, account.WebTierBasic}}, time.Hour, time.Second, time.Minute)
	selector.UpdatePreferFreeBuild(true)
	lease, err := selector.Acquire(ctx, account.ProviderWeb, "fast-prefer-best", "fast", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Credential.WebTier != account.WebTierHeavy {
		t.Fatalf("selected tier = %s", lease.Credential.WebTier)
	}
}

func TestSelectorPropagatesConcurrencyStoreFailure(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "selector-runtime-error.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	if _, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "active", SourceKey: "active", EncryptedAccessToken: "encrypted",
		AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	}); err != nil {
		t.Fatal(err)
	}

	runtimeErr := errors.New("runtime store unavailable")
	selector := NewSelector(accounts, failingConcurrencyLimiter{err: runtimeErr}, memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	if _, err := selector.Acquire(ctx, account.ProviderBuild, "", "", "", map[uint64]bool{}, true); !errors.Is(err, runtimeErr) {
		t.Fatalf("Acquire error = %v, want wrapped runtime error", err)
	}
}

func TestStickySessionKeyIsFixedLengthAndStable(t *testing.T) {
	first := stickySessionKey("affinity-key")
	if len(first) != 64 || first != stickySessionKey("affinity-key") {
		t.Fatalf("sticky key = %q", first)
	}
	if first == stickySessionKey("another-key") {
		t.Fatal("different prompt cache keys produced the same sticky key")
	}
	if stickySessionKey("") != "" {
		t.Fatal("empty prompt cache key should remain empty")
	}
}

func TestSelectorUsesBatchConcurrencySnapshot(t *testing.T) {
	limiter := &batchConcurrencyLimiter{values: map[string]int{"account:1": 2, "account:2": 1}}
	selector := &Selector{concurrency: limiter, lastSelectedAt: make(map[uint64]time.Time)}
	values := []account.RoutingCandidate{
		{Credential: account.Credential{ID: 1, Priority: 1}},
		{Credential: account.Credential{ID: 2, Priority: 1}},
	}
	plan, err := selector.planCandidates(context.Background(), values, time.Now().UTC(), nil)
	if err != nil {
		t.Fatal(err)
	}
	first, ok := plan.Next()
	if limiter.batchCalls != 1 || limiter.currentCalls != 0 || !ok || first.Credential.ID != 2 {
		t.Fatalf("batchCalls=%d currentCalls=%d values=%#v", limiter.batchCalls, limiter.currentCalls, values)
	}
}

func TestSelectorPreferFreeBuildHotReloadAndSaturationFallback(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "free-first.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	freeAccount, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "free", SourceKey: "free", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 1, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	superAccount, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "super", SourceKey: "super", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 100, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := accounts.SaveBilling(ctx, account.Billing{AccountID: freeAccount.ID, PlanName: "free", SyncedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.SaveBilling(ctx, account.Billing{AccountID: superAccount.ID, MonthlyLimit: 140, SyncedAt: now}); err != nil {
		t.Fatal(err)
	}

	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	lease, err := selector.Acquire(ctx, account.ProviderBuild, "grok-4.5", "", "existing-session", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Credential.ID != superAccount.ID {
		t.Fatalf("disabled strategy selected %d, want higher-priority Super %d", lease.Credential.ID, superAccount.ID)
	}
	lease.Release()

	selector.UpdatePreferFreeBuild(true)
	stickyLease, err := selector.Acquire(ctx, account.ProviderBuild, "grok-4.5", "", "existing-session", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if stickyLease.Credential.ID != superAccount.ID {
		t.Fatalf("existing sticky session moved to %d, want Super %d", stickyLease.Credential.ID, superAccount.ID)
	}
	stickyLease.Release()

	freeLease, err := selector.Acquire(ctx, account.ProviderBuild, "grok-4.5", "", "new-session", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if freeLease.Credential.ID != freeAccount.ID {
		t.Fatalf("enabled strategy selected %d, want Free %d", freeLease.Credential.ID, freeAccount.ID)
	}

	fallbackLease, err := selector.Acquire(ctx, account.ProviderBuild, "grok-4.5", "", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer fallbackLease.Release()
	defer freeLease.Release()
	if fallbackLease.Credential.ID != superAccount.ID {
		t.Fatalf("saturated Free selected %d, want Super fallback %d", fallbackLease.Credential.ID, superAccount.ID)
	}
}

func TestCandidatePlanPreservesSelectorOrdering(t *testing.T) {
	now := time.Now().UTC()
	limiter := &batchConcurrencyLimiter{values: map[string]int{"account:2": 1}}
	selector := &Selector{concurrency: limiter, lastSelectedAt: map[uint64]time.Time{6: now}}
	newCandidate := func(id uint64, tier account.WebTier, priority int, known, supported bool) account.RoutingCandidate {
		return account.RoutingCandidate{
			Credential: account.Credential{ID: id, WebTier: tier, Priority: priority},
			Billing: &account.Billing{
				AccountID: id, MonthlyLimit: 100, SyncedAt: now,
			},
			ModelCapabilityKnown: known, SupportsModel: supported,
		}
	}
	values := []account.RoutingCandidate{
		newCandidate(5, account.WebTierHeavy, 100, false, false),
		newCandidate(4, account.WebTierSuper, 100, true, true),
		newCandidate(3, account.WebTierHeavy, 9, true, true),
		newCandidate(2, account.WebTierHeavy, 10, true, true),
		newCandidate(6, account.WebTierHeavy, 10, true, true),
		newCandidate(1, account.WebTierHeavy, 10, true, true),
	}
	plan, err := selector.planCandidates(context.Background(), values, now, []account.WebTier{account.WebTierHeavy, account.WebTierSuper})
	if err != nil {
		t.Fatal(err)
	}
	ordered := make([]uint64, 0, len(values))
	for candidate, ok := plan.Next(); ok; candidate, ok = plan.Next() {
		ordered = append(ordered, candidate.Credential.ID)
	}
	if expected := []uint64{1, 6, 2, 3, 4, 5}; !slices.Equal(ordered, expected) {
		t.Fatalf("候选顺序 = %v, want %v", ordered, expected)
	}
}

func TestSelectorConsumesOnlyMatchingQuotaSnapshot(t *testing.T) {
	key := candidateCacheKey{provider: account.ProviderWeb, upstreamModel: "chat", quotaMode: "fast"}
	selector := &Selector{candidates: map[candidateCacheKey]candidateSnapshot{
		key: {values: []account.RoutingCandidate{{
			Credential: account.Credential{ID: 7}, QuotaWindow: &account.QuotaWindow{AccountID: 7, Mode: "fast", Remaining: 10},
		}}},
	}}
	selector.ConsumeQuota(account.ProviderWeb, 7, "fast", 3)
	window := selector.candidates[key].values[0].QuotaWindow
	if window == nil || window.Remaining != 7 {
		t.Fatalf("quota window = %#v", window)
	}
}

func TestSelectorWaitsBrieflyForAccountCapacity(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "capacity-wait.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	if _, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "capacity", SourceKey: "capacity", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	}); err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute, 300*time.Millisecond)
	first, err := selector.Acquire(ctx, account.ProviderBuild, "model", "", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		lease *accountLease
		err   error
	}
	resultCh := make(chan result, 1)
	go func() {
		lease, acquireErr := selector.Acquire(ctx, account.ProviderBuild, "model", "", "", nil, false)
		resultCh <- result{lease: lease, err: acquireErr}
	}()
	select {
	case value := <-resultCh:
		t.Fatalf("second acquire returned before capacity release: %v", value.err)
	case <-time.After(30 * time.Millisecond):
	}
	first.Release()
	select {
	case value := <-resultCh:
		if value.err != nil || value.lease == nil {
			t.Fatalf("second acquire lease=%v err=%v", value.lease, value.err)
		}
		value.lease.Release()
	case <-time.After(time.Second):
		t.Fatal("second acquire did not wake after capacity release")
	}
}

func TestSelectorStickySessionWaitsForBoundAccountCapacity(t *testing.T) {
	ctx := context.Background()
	sticky := memory.NewStickyStore()
	selector, primary, _ := newStickySelectorFixture(t, sticky, 300*time.Millisecond, true)
	first, err := selector.Acquire(ctx, account.ProviderBuild, "model", "", "stable-affinity", nil, false)
	if err != nil || first.Credential.ID != primary.ID {
		t.Fatalf("first lease = %#v, err = %v", first, err)
	}
	type result struct {
		lease *accountLease
		err   error
	}
	resultCh := make(chan result, 1)
	go func() {
		lease, acquireErr := selector.Acquire(ctx, account.ProviderBuild, "model", "", "stable-affinity", nil, false)
		resultCh <- result{lease: lease, err: acquireErr}
	}()
	select {
	case value := <-resultCh:
		t.Fatalf("sticky request bypassed the bound account before capacity returned: %#v", value)
	case <-time.After(30 * time.Millisecond):
	}
	first.Release()
	select {
	case value := <-resultCh:
		if value.err != nil || value.lease == nil || value.lease.Credential.ID != primary.ID {
			t.Fatalf("sticky lease = %#v, err = %v", value.lease, value.err)
		}
		value.lease.Release()
	case <-time.After(time.Second):
		t.Fatal("sticky request did not wake after bound capacity returned")
	}
}

func TestSelectorStickySessionTemporaryFallbackDoesNotRebind(t *testing.T) {
	ctx := context.Background()
	sticky := memory.NewStickyStore()
	selector, primary, fallback := newStickySelectorFixture(t, sticky, 20*time.Millisecond, true)
	first, err := selector.Acquire(ctx, account.ProviderBuild, "model", "", "stable-affinity", nil, false)
	if err != nil || first.Credential.ID != primary.ID {
		t.Fatalf("first lease = %#v, err = %v", first, err)
	}
	temporary, err := selector.Acquire(ctx, account.ProviderBuild, "model", "", "stable-affinity", nil, false)
	if err != nil || temporary.Credential.ID != fallback.ID {
		t.Fatalf("temporary lease = %#v, err = %v", temporary, err)
	}
	if boundID, ok, err := sticky.Get(ctx, stickySessionKey("stable-affinity"), time.Now().UTC()); err != nil || !ok || boundID != primary.ID {
		t.Fatalf("sticky binding changed after temporary fallback: id=%d ok=%v err=%v", boundID, ok, err)
	}
	temporary.Release()
	first.Release()
	resumed, err := selector.Acquire(ctx, account.ProviderBuild, "model", "", "stable-affinity", nil, false)
	if err != nil || resumed.Credential.ID != primary.ID {
		t.Fatalf("resumed sticky lease = %#v, err = %v", resumed, err)
	}
	resumed.Release()
}

func TestSelectorStickyHitRefreshesTTL(t *testing.T) {
	ctx := context.Background()
	sticky := newRecordingStickyStore()
	selector, _, _ := newStickySelectorFixture(t, sticky, 0, false)
	first, err := selector.Acquire(ctx, account.ProviderBuild, "model", "", "stable-affinity", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	first.Release()
	time.Sleep(time.Millisecond)
	second, err := selector.Acquire(ctx, account.ProviderBuild, "model", "", "stable-affinity", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	second.Release()
	expiries := sticky.Expiries()
	if len(expiries) < 2 || !expiries[len(expiries)-1].After(expiries[0]) {
		t.Fatalf("sticky expiry was not refreshed: %v", expiries)
	}
}

func newStickySelectorFixture(t *testing.T, sticky repository.StickySessionRepository, capacityWait time.Duration, withFallback bool) (*Selector, account.Credential, account.Credential) {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "sticky-selector.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	primary, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "primary", SourceKey: "primary", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 100, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	var fallback account.Credential
	if withFallback {
		fallback, _, err = accounts.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderBuild, Name: "fallback", SourceKey: "fallback", EncryptedAccessToken: "encrypted",
			Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 1, MaxConcurrent: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return NewSelector(accounts, memory.NewConcurrencyLimiter(), sticky, nil, time.Hour, time.Second, time.Minute, capacityWait), primary, fallback
}

func TestSelectorAppliesPersistedCooldownOnlyToMatchingModel(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "model-cooldown.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "model-cooling", SourceKey: "model-cooling", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().UTC().Add(time.Hour)
	if err := accounts.UpsertModelQuotaBlock(ctx, account.ModelQuotaBlock{AccountID: credential.ID, UpstreamModel: "limited-model", Reason: "test", CooldownUntil: until}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.UpsertModelQuotaBlock(ctx, account.ModelQuotaBlock{AccountID: credential.ID, UpstreamModel: "limited-model", Reason: "shorter", CooldownUntil: time.Now().UTC().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	if _, err := selector.Acquire(ctx, account.ProviderBuild, "limited-model", "", "", nil, false); err == nil {
		t.Fatal("matching model cooldown was ignored")
	} else {
		var unavailable *SelectionUnavailableError
		if !errors.As(err, &unavailable) || unavailable.Reason != SelectionModelCooling || unavailable.RetryAfter < 30*time.Minute {
			t.Fatalf("error = %v", err)
		}
	}
	lease, err := selector.Acquire(ctx, account.ProviderBuild, "other-model", "", "", nil, false)
	if err != nil {
		t.Fatalf("other model was blocked: %v", err)
	}
	lease.Release()
}

type failingConcurrencyLimiter struct{ err error }

type recordingStickyStore struct {
	*memory.StickyStore
	mu       sync.Mutex
	expiries []time.Time
}

func newRecordingStickyStore() *recordingStickyStore {
	return &recordingStickyStore{StickyStore: memory.NewStickyStore()}
}

func (s *recordingStickyStore) Bind(ctx context.Context, key string, accountID uint64, now, expiresAt time.Time) (uint64, error) {
	s.mu.Lock()
	s.expiries = append(s.expiries, expiresAt)
	s.mu.Unlock()
	return s.StickyStore.Bind(ctx, key, accountID, now, expiresAt)
}

func (s *recordingStickyStore) Expiries() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.expiries...)
}

type batchConcurrencyLimiter struct {
	values       map[string]int
	batchCalls   int
	currentCalls int
}

func (l *batchConcurrencyLimiter) Acquire(context.Context, string, int) (func(), bool, error) {
	return func() {}, true, nil
}

func (l *batchConcurrencyLimiter) Current(context.Context, string) (int, error) {
	l.currentCalls++
	return 0, nil
}

func (l *batchConcurrencyLimiter) CurrentMany(_ context.Context, keys []string) (map[string]int, error) {
	l.batchCalls++
	values := make(map[string]int, len(keys))
	for _, key := range keys {
		values[key] = l.values[key]
	}
	return values, nil
}

type staticTierOrder struct{ order []account.WebTier }

func (value staticTierOrder) TierOrder(account.Provider, string) []account.WebTier {
	return value.order
}

func (f failingConcurrencyLimiter) Acquire(context.Context, string, int) (func(), bool, error) {
	return nil, false, f.err
}

func (f failingConcurrencyLimiter) Current(context.Context, string) (int, error) {
	return 0, nil
}
