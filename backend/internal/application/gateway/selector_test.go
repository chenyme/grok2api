package gateway

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestSelectionUnavailableErrorClassification(t *testing.T) {
	tests := []struct {
		reason SelectionUnavailableReason
		status int
		code   string
	}{
		{reason: SelectionNoAccounts, status: http.StatusServiceUnavailable, code: "upstream_unavailable"},
		{reason: SelectionUnsupportedModel, status: http.StatusServiceUnavailable, code: "upstream_model_unavailable"},
		{reason: SelectionCooling, status: http.StatusTooManyRequests, code: "upstream_cooling"},
		{reason: SelectionModelCooling, status: http.StatusTooManyRequests, code: "upstream_model_cooling"},
		{reason: SelectionQuotaExhausted, status: http.StatusTooManyRequests, code: "upstream_quota_exhausted"},
		{reason: SelectionSaturated, status: http.StatusServiceUnavailable, code: "upstream_saturated"},
	}
	for _, test := range tests {
		t.Run(string(test.reason), func(t *testing.T) {
			failure := &SelectionUnavailableError{Reason: test.reason}
			if failure.HTTPStatus() != test.status || failure.Code() != test.code {
				t.Fatalf("status=%d code=%q", failure.HTTPStatus(), failure.Code())
			}
		})
	}
}

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
	lease, err := selector.Acquire(ctx, account.ProviderBuild, 0, "grok-test", "", "", map[uint64]bool{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Credential.ID != probe.ID || !lease.QuotaProbe {
		t.Fatalf("lease = %#v, want due probe account %d", lease, probe.ID)
	}
	lease.Release()

	lease, err = selector.Acquire(ctx, account.ProviderBuild, 0, "grok-test", "", "", map[uint64]bool{probe.ID: true}, false)
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

func TestSelectorQualityProbePinsAccountToRequestedEgressNode(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "selector-egress.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	egressNodes := relational.NewEgressRepository(database)
	firstNode, err := egressNodes.CreateEgressNode(ctx, egressdomain.Node{Name: "first", Scope: egressdomain.ScopeBuild, Enabled: true, EncryptedProxyURL: "first"})
	if err != nil {
		t.Fatal(err)
	}
	secondNode, err := egressNodes.CreateEgressNode(ctx, egressdomain.Node{Name: "second", Scope: egressdomain.ScopeBuild, Enabled: true, EncryptedProxyURL: "second"})
	if err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	first, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "first", SourceKey: "first", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1, EgressNodeID: firstNode.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "second", SourceKey: "second", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1, EgressNodeID: secondNode.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	lease, err := selector.AcquireForKeyOnEgressNode(ctx, account.ProviderBuild, 0, "grok-test", "", "", nil, false, clientkeydomain.AccountScope{}, secondNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Credential.ID != second.ID || lease.Credential.ID == first.ID {
		t.Fatalf("selected account=%d, want=%d on node=%d", lease.Credential.ID, second.ID, secondNode.ID)
	}
	lease.Release()

	if _, err := accounts.UpsertEgressLeaseBlock(ctx, account.EgressLeaseBlock{
		AccountID: second.ID, NodeID: secondNode.ID, Reason: "hard_tps", Version: "selector-lease-0001", CooldownUntil: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	selector = NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	if _, err := selector.AcquirePinnedForKey(ctx, account.ProviderBuild, second.ID, 0, "grok-test", "", true, clientkeydomain.AccountScope{}); err == nil {
		t.Fatal("ordinary pinned inference ignored the active egress lease block")
	} else {
		var unavailable *SelectionUnavailableError
		if !errors.As(err, &unavailable) || unavailable.Reason != SelectionCooling {
			t.Fatalf("ordinary pinned error = %v", err)
		}
	}
	recoveryLease, err := selector.AcquirePinnedForQualityProbe(ctx, account.ProviderBuild, second.ID, 0, "grok-test", "", clientkeydomain.AccountScope{})
	if err != nil {
		t.Fatalf("quality recovery did not bypass only the lease block: %v", err)
	}
	defer recoveryLease.Release()
	if recoveryLease.Credential.ID != second.ID || recoveryLease.Credential.EgressNodeID != secondNode.ID {
		t.Fatalf("quality recovery lease = %#v", recoveryLease.Credential)
	}
}

func TestSelectorQualityProbeBorrowsHealthyAccountForUnavailableNode(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "selector-egress-fallback.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	egressNodes := relational.NewEgressRepository(database)
	targetNode, err := egressNodes.CreateEgressNode(ctx, egressdomain.Node{Name: "target", Scope: egressdomain.ScopeBuild, Enabled: false, EncryptedProxyURL: "target"})
	if err != nil {
		t.Fatal(err)
	}
	healthyNode, err := egressNodes.CreateEgressNode(ctx, egressdomain.Node{Name: "healthy", Scope: egressdomain.ScopeBuild, Enabled: true, EncryptedProxyURL: "healthy"})
	if err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	_, _, err = accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "target-reauth", SourceKey: "target-reauth", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: account.AuthStatusReauthRequired, MaxConcurrent: 1, EgressNodeID: targetNode.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	healthy, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "healthy", SourceKey: "healthy", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1, EgressNodeID: healthyNode.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	lease, err := selector.AcquireForKeyOnEgressNode(ctx, account.ProviderBuild, 0, "grok-test", "", "ordinary-affinity", nil, false, clientkeydomain.AccountScope{}, targetNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Credential.ID != healthy.ID {
		t.Fatalf("selected account=%d, want borrowed healthy account=%d", lease.Credential.ID, healthy.ID)
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
			plan, err := selector.planCandidates(ctx, candidates, now, nil)
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
	if _, err := selector.Acquire(ctx, account.ProviderBuild, 0, "grok-test", "", "", map[uint64]bool{}, true); err == nil {
		t.Fatal("expected no account before next probe time")
	}
}

func TestSelectorQuotaRecoveryUsesFixedFreeAndUpstreamPaidReset(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "quota-recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	value, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "build", SourceKey: "build", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)

	paidPeriodEnd := now.Add(7 * time.Hour).Truncate(time.Second)
	selector.MarkPaymentQuotaExhausted(ctx, value, quotaRecoveryHints{Billing: &account.Billing{
		AccountID: value.ID, PlanCode: "super", MonthlyLimit: 100, BillingPeriodEnd: paidPeriodEnd.Format(time.RFC3339),
	}})
	recovery := requireQuotaRecovery(t, ctx, accounts, value.ID)
	if recovery.Kind != account.QuotaRecoveryKindPaid || recovery.NextProbeAt == nil || !recovery.NextProbeAt.Equal(paidPeriodEnd) {
		t.Fatalf("paid recovery = %#v", recovery)
	}

	freeStarted := time.Now().UTC()
	selector.MarkPaymentQuotaExhausted(ctx, value, quotaRecoveryHints{})
	recovery = requireQuotaRecovery(t, ctx, accounts, value.ID)
	if recovery.Kind != account.QuotaRecoveryKindFree {
		t.Fatalf("free recovery = %#v", recovery)
	}
	assertRecoveryDelay(t, recovery, freeStarted, defaultFreeQuotaRecoveryPause)

	freeStarted = time.Now().UTC()
	selector.MarkFreeQuotaExhausted(ctx, value, 100, 100)
	recovery = requireQuotaRecovery(t, ctx, accounts, value.ID)
	assertRecoveryDelay(t, recovery, freeStarted, defaultFreeQuotaRecoveryPause)
}

func TestSelectorModelQuotaUsesFixedFreeAndUpstreamPaidDelay(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "model-quota-recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	value, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "build", SourceKey: "model-quota-build", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1, ObservedModel: "grok-4.5-build-free",
	})
	if err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	freeStarted := time.Now().UTC()
	selector.MarkModelQuotaExhausted(ctx, value, &account.Billing{PlanName: "free"}, "free-model", time.Hour)
	freeCandidates, err := accounts.ListRoutingCandidates(ctx, account.ProviderBuild, 0, "free-model", "")
	if err != nil || len(freeCandidates) != 1 || freeCandidates[0].ModelQuotaBlock == nil {
		t.Fatalf("free candidates = %#v, err = %v", freeCandidates, err)
	}
	assertTimeDelay(t, freeCandidates[0].ModelQuotaBlock.CooldownUntil, freeStarted, defaultFreeQuotaRecoveryPause)

	paidStarted := time.Now().UTC()
	selector.MarkModelQuotaExhausted(ctx, value, &account.Billing{PlanName: "SuperGrok"}, "paid-model", 2*time.Hour)
	paidCandidates, err := accounts.ListRoutingCandidates(ctx, account.ProviderBuild, 0, "paid-model", "")
	if err != nil || len(paidCandidates) != 1 || paidCandidates[0].ModelQuotaBlock == nil {
		t.Fatalf("paid candidates = %#v, err = %v", paidCandidates, err)
	}
	assertTimeDelay(t, paidCandidates[0].ModelQuotaBlock.CooldownUntil, paidStarted, 2*time.Hour)
}

func TestSelectorModelQuotaPreservesSessionAffinity(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "model-quota-sticky.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "build", SourceKey: "build", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 100, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	fallback, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "fallback", SourceKey: "fallback", EncryptedAccessToken: "encrypted-fallback",
		Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 50, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	sticky := memory.NewStickyStore()
	expiresAt := time.Now().UTC().Add(time.Hour)
	for _, key := range []string{"model-a-session", "model-b-session"} {
		if err := sticky.Set(ctx, stickySessionKey(key), credential.ID, expiresAt); err != nil {
			t.Fatal(err)
		}
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), sticky, nil, time.Hour, time.Second, time.Minute)
	selector.MarkModelQuotaExhausted(ctx, credential, nil, "model-a", time.Hour)

	for _, key := range []string{"model-a-session", "model-b-session"} {
		accountID, ok, err := sticky.Get(ctx, stickySessionKey(key), time.Now().UTC())
		if err != nil || !ok || accountID != credential.ID {
			t.Fatalf("sticky %q = account %d, ok=%v, err=%v", key, accountID, ok, err)
		}
	}

	lease, err := selector.Acquire(ctx, account.ProviderBuild, 0, "model-a", "", "model-a-session", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Credential.ID != fallback.ID {
		t.Fatalf("blocked model session selected account %d, want fallback %d", lease.Credential.ID, fallback.ID)
	}
	lease.Release()
	if accountID, ok, err := sticky.Get(ctx, stickySessionKey("model-a-session"), time.Now().UTC()); err != nil || !ok || accountID != fallback.ID {
		t.Fatalf("affected sticky binding was not rebuilt: id=%d ok=%v err=%v", accountID, ok, err)
	}
	if accountID, ok, err := sticky.Get(ctx, stickySessionKey("model-b-session"), time.Now().UTC()); err != nil || !ok || accountID != credential.ID {
		t.Fatalf("unrelated sticky binding changed: id=%d ok=%v err=%v", accountID, ok, err)
	}
}

func requireQuotaRecovery(t *testing.T, ctx context.Context, accounts repository.AccountRepository, accountID uint64) account.QuotaRecovery {
	t.Helper()
	recovery, err := accounts.GetQuotaRecovery(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	return recovery
}

func assertRecoveryDelay(t *testing.T, recovery account.QuotaRecovery, started time.Time, delay time.Duration) {
	t.Helper()
	if recovery.NextProbeAt == nil {
		t.Fatalf("recovery has no next probe: %#v", recovery)
	}
	assertTimeDelay(t, *recovery.NextProbeAt, started, delay)
}

func assertTimeDelay(t *testing.T, actual, started time.Time, delay time.Duration) {
	t.Helper()
	want := started.Add(delay)
	if actual.Before(want.Add(-time.Second)) || actual.After(want.Add(2*time.Second)) {
		t.Fatalf("time = %s, want around %s", actual, want)
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
	if _, err := selector.Acquire(ctx, account.ProviderWeb, 0, "", "fast", "", nil, false); err == nil {
		t.Fatal("exhausted weekly pool must take precedence over a stale fast quota window")
	}
	if err := accounts.SaveQuotaWindows(ctx, value.ID, account.WebTierSuper, now, []account.QuotaWindow{
		{AccountID: value.ID, Mode: "weekly", Remaining: 8900, Total: 10000, UsagePercent: 11, ResetAt: &resetAt, SyncedAt: &now, Source: account.QuotaSourceUpstream},
		{AccountID: value.ID, Mode: "fast", Remaining: 0, Total: 30, ResetAt: &resetAt, SyncedAt: &now, Source: account.QuotaSourceUpstream},
	}); err != nil {
		t.Fatal(err)
	}
	selector.MarkQuotaStateChanged(account.ProviderWeb)
	lease, err := selector.Acquire(ctx, account.ProviderWeb, 0, "", "fast", "", nil, false)
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
	lease, err := selector.Acquire(ctx, account.ProviderBuild, 0, "", "", "", map[uint64]bool{}, true)
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
	lease, err := selector.Acquire(ctx, account.ProviderBuild, 0, "grok-premium", "", "", map[uint64]bool{}, true)
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
	if _, err := selector.Acquire(ctx, account.ProviderWeb, 0, "grok-chat", "fast", "", nil, false); err == nil {
		t.Fatal("exhausted fast mode should not be selected")
	}
	lease, err := selector.Acquire(ctx, account.ProviderWeb, 0, "grok-chat-auto", "auto", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Credential.ID != value.ID || lease.QuotaMode != "auto" {
		t.Fatalf("lease = %#v", lease)
	}
}

func TestSelectorUsesTierSpecificWebImageEditQuotaAndPrefersBasic(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "selector-web-image-edit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	models := relational.NewModelRepository(database)
	create := func(name string, tier account.WebTier, priority int) account.Credential {
		value, _, createErr := accounts.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: tier,
			Name: name, SourceKey: name, EncryptedAccessToken: "encrypted",
			AuthStatus: account.AuthStatusActive, Priority: priority, MaxConcurrent: 1,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		return value
	}
	basic := create("basic-edit", account.WebTierBasic, 1)
	super := create("super-edit", account.WebTierSuper, 100)
	now := time.Now().UTC()
	resetAt := now.Add(time.Hour)
	if err := accounts.SaveQuotaWindows(ctx, basic.ID, account.WebTierBasic, now, []account.QuotaWindow{
		{AccountID: basic.ID, Mode: account.QuotaModeWebImagePro, Remaining: 4, Total: 4, ResetAt: &resetAt, SyncedAt: &now, Source: account.QuotaSourceUpstream},
		{AccountID: basic.ID, Mode: account.QuotaModeWebImageEdit, Remaining: 0, Total: 0, ResetAt: &resetAt, SyncedAt: &now, Source: account.QuotaSourceUpstream},
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.SaveQuotaWindows(ctx, super.ID, account.WebTierSuper, now, []account.QuotaWindow{
		{AccountID: super.ID, Mode: account.QuotaModeWebImagePro, Remaining: 20, Total: 20, ResetAt: &resetAt, SyncedAt: &now, Source: account.QuotaSourceUpstream},
		{AccountID: super.ID, Mode: account.QuotaModeWebImageEdit, Remaining: 8, Total: 8, ResetAt: &resetAt, SyncedAt: &now, Source: account.QuotaSourceUpstream},
	}); err != nil {
		t.Fatal(err)
	}
	// Simulate an old Basic snapshot from before image editing was exposed to
	// that tier. Super already reports the capability.
	if err := models.ReplaceAccountCapabilities(ctx, basic.ID, []string{"grok-chat-fast"}, now); err != nil {
		t.Fatal(err)
	}
	if err := models.ReplaceAccountCapabilities(ctx, super.ID, []string{"grok-chat-fast", "imagine-image-edit"}, now); err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), staticTierOrder{order: []account.WebTier{
		account.WebTierBasic, account.WebTierSuper, account.WebTierHeavy,
	}}, time.Hour, time.Second, time.Minute)
	lease, err := selector.Acquire(ctx, account.ProviderWeb, 0, "imagine-image-edit", account.QuotaModeWebImageEdit, "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Credential.ID != basic.ID {
		t.Fatalf("selected account = %d, want stale-snapshot Basic %d", lease.Credential.ID, basic.ID)
	}
	if lease.QuotaMode != account.QuotaModeWebImagePro {
		t.Fatalf("quota mode = %q, want %q", lease.QuotaMode, account.QuotaModeWebImagePro)
	}
}

func TestSelectorAllowsBasicOnlyForConfirmedWebVideoQuota(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "selector-web-basic-video.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	models := relational.NewModelRepository(database)
	basic, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierBasic,
		Name: "basic-video", SourceKey: "basic-video", EncryptedAccessToken: "encrypted",
		AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := accounts.SaveQuotaWindows(ctx, basic.ID, account.WebTierBasic, now, []account.QuotaWindow{{
		AccountID: basic.ID, Mode: account.QuotaModeWebVideo720p, Remaining: 1, Total: 1,
		SyncedAt: &now, Source: account.QuotaSourceUpstream,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := models.ReplaceAccountCapabilities(ctx, basic.ID, []string{"grok-chat-fast"}, now); err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), webVideoTierOrder{}, time.Hour, time.Second, time.Minute)
	lease, err := selector.Acquire(ctx, account.ProviderWeb, 0, "grok-imagine-video", account.QuotaModeWebVideo720p, "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Credential.ID != basic.ID || lease.QuotaMode != account.QuotaModeWebVideo720p {
		t.Fatalf("720p Basic video lease = %#v", lease)
	}
	lease.Release()
	if _, err := selector.Acquire(ctx, account.ProviderWeb, 0, "grok-imagine-video", account.QuotaModeWebVideo, "", nil, false); err == nil {
		t.Fatal("Basic account was selected for an unverified Web video quota product")
	}
}

func TestSelectorWebCatalogCapabilityStillEnforcesTier(t *testing.T) {
	candidate := account.RoutingCandidate{
		Credential:           account.Credential{Provider: account.ProviderWeb, WebTier: account.WebTierBasic},
		ModelCapabilityKnown: true,
	}
	imageSelector := NewSelector(nil, memory.NewConcurrencyLimiter(), nil, staticTierOrder{order: []account.WebTier{
		account.WebTierBasic, account.WebTierSuper, account.WebTierHeavy,
	}}, time.Hour, time.Second, time.Minute)
	if !imageSelector.candidateSupportsModel(account.ProviderWeb, "imagine-image-edit", account.QuotaModeWebImageEdit, candidate) {
		t.Fatal("recognized Basic image-edit capability was blocked by a stale snapshot")
	}
	videoSelector := NewSelector(nil, memory.NewConcurrencyLimiter(), nil, staticTierOrder{order: []account.WebTier{
		account.WebTierSuper, account.WebTierHeavy,
	}}, time.Hour, time.Second, time.Minute)
	if videoSelector.candidateSupportsModel(account.ProviderWeb, "grok-imagine-video", account.QuotaModeWebVideo, candidate) {
		t.Fatal("Basic account bypassed the video tier requirement")
	}
}

func TestImageQuotaFinalizationKeepsEffectiveConsumptionFence(t *testing.T) {
	refreshMode, decrementMode, availabilityMode := quotaFinalizationModes(account.QuotaModeWebImagePro, account.QuotaGroupWebImagine)
	if refreshMode != account.QuotaGroupWebImagine || decrementMode != account.QuotaModeWebImagePro || availabilityMode != "" {
		t.Fatalf("refresh=%q decrement=%q availability=%q", refreshMode, decrementMode, availabilityMode)
	}
	refreshMode, decrementMode, availabilityMode = quotaFinalizationModes("weekly", account.QuotaGroupWebImagine)
	if refreshMode != "weekly" || decrementMode != "weekly" || availabilityMode != account.QuotaGroupWebImagine {
		t.Fatalf("shared weekly refresh=%q decrement=%q availability=%q", refreshMode, decrementMode, availabilityMode)
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
	lease, err := selector.Acquire(ctx, account.ProviderWeb, 0, "fast-prefer-best", "fast", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Credential.WebTier != account.WebTierHeavy {
		t.Fatalf("selected tier = %s", lease.Credential.WebTier)
	}
}

func TestSelectorEnforcesClientKeyAccountScopeAcrossProvidersAndTiers(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "selector-client-key-pool.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	create := func(value account.Credential) account.Credential {
		t.Helper()
		created, _, createErr := accounts.UpsertByIdentity(ctx, value)
		if createErr != nil {
			t.Fatal(createErr)
		}
		return created
	}
	buildFree := create(account.Credential{Provider: account.ProviderBuild, Name: "build-free", SourceKey: "build-free", EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 10, MaxConcurrent: 2})
	buildSuper := create(account.Credential{Provider: account.ProviderBuild, Name: "build-super", SourceKey: "build-super", EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 20, MaxConcurrent: 2})
	buildUnknown := create(account.Credential{Provider: account.ProviderBuild, Name: "build-unknown", SourceKey: "build-unknown", EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 100, MaxConcurrent: 2})
	now := time.Now().UTC()
	if err := accounts.SaveBilling(ctx, account.Billing{AccountID: buildFree.ID, PlanName: "Free", SyncedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.SaveBilling(ctx, account.Billing{AccountID: buildSuper.ID, PlanName: "SuperGrok", SyncedAt: now}); err != nil {
		t.Fatal(err)
	}

	webFree := create(account.Credential{Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierBasic, Name: "web-free", SourceKey: "web-free", EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 100, MaxConcurrent: 2})
	webSuper := create(account.Credential{Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierSuper, Name: "web-super", SourceKey: "web-super", EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 20, MaxConcurrent: 2})
	webHeavy := create(account.Credential{Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierHeavy, Name: "web-heavy", SourceKey: "web-heavy", EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 10, MaxConcurrent: 2})
	_ = create(account.Credential{Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierAuto, Name: "web-unknown", SourceKey: "web-unknown", EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 200, MaxConcurrent: 2})
	console := create(account.Credential{Provider: account.ProviderConsole, AuthType: account.AuthTypeSSO, Name: "console", SourceKey: "console", EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 10, MaxConcurrent: 2})

	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), staticTierOrder{order: []account.WebTier{account.WebTierBasic, account.WebTierSuper, account.WebTierHeavy}}, time.Hour, time.Second, time.Minute)
	providerScope := func(provider account.Provider) clientkeydomain.ProviderScope {
		switch provider {
		case account.ProviderBuild:
			return clientkeydomain.ProviderScopeBuild
		case account.ProviderWeb:
			return clientkeydomain.ProviderScopeWeb
		default:
			return clientkeydomain.ProviderScopeConsole
		}
	}
	assertSelected := func(provider account.Provider, tiers clientkeydomain.TierScope, excluded map[uint64]bool, want uint64) {
		t.Helper()
		scope := clientkeydomain.AccountScope{Providers: providerScope(provider), Tiers: tiers}
		lease, acquireErr := selector.AcquireForKey(ctx, provider, 0, "", "", "", excluded, false, scope)
		if acquireErr != nil {
			t.Fatal(acquireErr)
		}
		defer lease.Release()
		if lease.Credential.ID != want {
			t.Fatalf("provider %s tiers %d selected %d, want %d", provider, tiers, lease.Credential.ID, want)
		}
	}
	assertSelected(account.ProviderBuild, clientkeydomain.TierScopeFree, nil, buildFree.ID)
	assertSelected(account.ProviderBuild, clientkeydomain.TierScopeSuper, nil, buildSuper.ID)
	assertSelected(account.ProviderWeb, clientkeydomain.TierScopeFree, nil, webFree.ID)
	assertSelected(account.ProviderWeb, clientkeydomain.TierScopeSuper, nil, webSuper.ID)
	assertSelected(account.ProviderWeb, clientkeydomain.TierScopeSuper, map[uint64]bool{webSuper.ID: true}, webHeavy.ID)
	assertSelected(account.ProviderConsole, clientkeydomain.TierScopeFree, nil, console.ID)

	freeBuildScope := clientkeydomain.AccountScope{Providers: clientkeydomain.ProviderScopeBuild, Tiers: clientkeydomain.TierScopeFree}
	_, err = selector.AcquireForKey(ctx, account.ProviderBuild, 0, "", "", "", map[uint64]bool{buildFree.ID: true}, false, freeBuildScope)
	var unavailable *SelectionUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Code() != "client_key_account_scope_unavailable" || unavailable.Scope != freeBuildScope {
		t.Fatalf("scoped exhaustion error = %#v, err = %v", unavailable, err)
	}
	superBuildScope := clientkeydomain.AccountScope{Providers: clientkeydomain.ProviderScopeBuild, Tiers: clientkeydomain.TierScopeSuper}
	if _, err := selector.AcquirePinnedForKey(ctx, account.ProviderBuild, buildUnknown.ID, 0, "", "", true, superBuildScope); !errors.As(err, &unavailable) || unavailable.Code() != "client_key_account_scope_unavailable" {
		t.Fatalf("out-of-pool pinned error = %#v, err = %v", unavailable, err)
	}
	allKnownBuildTiers := clientkeydomain.AccountScope{Providers: clientkeydomain.ProviderScopeBuild, Tiers: clientkeydomain.TierScopeFree | clientkeydomain.TierScopeSuper}
	if _, err := selector.AcquirePinnedForKey(ctx, account.ProviderBuild, buildUnknown.ID, 0, "", "", true, allKnownBuildTiers); !errors.As(err, &unavailable) {
		t.Fatalf("unknown Build tier should be excluded: %v", err)
	}
	buildOnlyScope := clientkeydomain.AccountScope{Providers: clientkeydomain.ProviderScopeBuild, Tiers: clientkeydomain.TierScopeAll}
	if _, err := selector.AcquireForKey(ctx, account.ProviderWeb, 0, "", "", "", nil, false, buildOnlyScope); !errors.As(err, &unavailable) || unavailable.Code() != "client_key_account_scope_unavailable" {
		t.Fatalf("provider scope should fail closed: %#v, err = %v", unavailable, err)
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
	if _, err := selector.Acquire(ctx, account.ProviderBuild, 0, "", "", "", map[uint64]bool{}, true); !errors.Is(err, runtimeErr) {
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
	selector := NewSelector(nil, limiter, nil, nil, time.Hour, time.Second, time.Minute)
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
	if _, err := selector.planCandidates(context.Background(), values, time.Now().UTC(), nil); err != nil {
		t.Fatal(err)
	}
	if limiter.batchCalls != 1 {
		t.Fatalf("short snapshot cache made %d batch reads", limiter.batchCalls)
	}
	time.Sleep(concurrencySnapshotTTL + 5*time.Millisecond)
	if _, err := selector.planCandidates(context.Background(), values, time.Now().UTC(), nil); err != nil {
		t.Fatal(err)
	}
	if limiter.batchCalls != 2 {
		t.Fatalf("expired snapshot cache made %d batch reads", limiter.batchCalls)
	}
}

func TestCandidatePlanExcludesSaturatedAccounts(t *testing.T) {
	limiter := &batchConcurrencyLimiter{values: map[string]int{"account:1": 1, "account:2": 0}}
	selector := &Selector{concurrency: limiter, lastSelectedAt: make(map[uint64]time.Time)}
	values := []account.RoutingCandidate{
		{Credential: account.Credential{ID: 1, Priority: 100, MaxConcurrent: 1}},
		{Credential: account.Credential{ID: 2, Priority: 1, MaxConcurrent: 1}},
	}
	plan, err := selector.planCandidates(context.Background(), values, time.Now().UTC(), nil)
	if err != nil {
		t.Fatal(err)
	}
	first, ok := plan.Next()
	if !ok || first.Credential.ID != 2 {
		t.Fatalf("first candidate = %#v, want account 2", first)
	}
	if _, ok := plan.Next(); ok {
		t.Fatal("saturated account should not remain in the plan")
	}
}

func TestSelectionSessionReusesCandidatePlanAcrossAccountSwitches(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "selection-session.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	first, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "first", SourceKey: "first", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 20, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "second", SourceKey: "second", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 10, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	limiter := &batchConcurrencyLimiter{values: map[string]int{}}
	selector := NewSelector(accounts, limiter, memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	excluded := map[uint64]bool{}
	session, err := selector.beginSelectionSession(ctx, account.ProviderBuild, 0, "model", "", "", excluded, false)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := session.Acquire(ctx, excluded, false)
	if err != nil || lease == nil || lease.Credential.ID != first.ID {
		t.Fatalf("first lease = %#v, err = %v", lease, err)
	}
	lease.Release()
	session.RetryAccount(first.ID)
	first.Enabled = false
	if _, err := accounts.Update(ctx, first); err != nil {
		t.Fatal(err)
	}
	// Session 快照中的 first 已过期，Acquire 应跳过它并继续使用现有计划。
	lease, err = session.Acquire(ctx, excluded, false)
	if err != nil || lease == nil || lease.Credential.ID != second.ID {
		t.Fatalf("second lease = %#v, err = %v", lease, err)
	}
	lease.Release()
	if limiter.batchCalls != 1 {
		t.Fatalf("batch concurrency reads = %d, want 1", limiter.batchCalls)
	}
}

func TestSelectorEvictsOnlyChangedCandidate(t *testing.T) {
	key := candidateCacheKey{provider: account.ProviderBuild, upstreamModel: "model"}
	selector := &Selector{candidates: map[candidateCacheKey]candidateSnapshot{
		key: {values: []account.RoutingCandidate{
			{Credential: account.Credential{ID: 1, Provider: account.ProviderBuild}},
			{Credential: account.Credential{ID: 2, Provider: account.ProviderBuild}},
		}},
	}}
	selector.evictCandidate(account.ProviderBuild, 1)
	values := selector.candidates[key].values
	if len(values) != 1 || values[0].Credential.ID != 2 {
		t.Fatalf("remaining candidates = %#v", values)
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
	lease, err := selector.Acquire(ctx, account.ProviderBuild, 0, "grok-4.5", "", "existing-session", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Credential.ID != superAccount.ID {
		t.Fatalf("disabled strategy selected %d, want higher-priority Super %d", lease.Credential.ID, superAccount.ID)
	}
	lease.Release()

	selector.UpdatePreferFreeBuild(true)
	stickyLease, err := selector.Acquire(ctx, account.ProviderBuild, 0, "grok-4.5", "", "existing-session", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if stickyLease.Credential.ID != superAccount.ID {
		t.Fatalf("existing sticky session moved to %d, want Super %d", stickyLease.Credential.ID, superAccount.ID)
	}
	stickyLease.Release()

	freeLease, err := selector.Acquire(ctx, account.ProviderBuild, 0, "grok-4.5", "", "new-session", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if freeLease.Credential.ID != freeAccount.ID {
		t.Fatalf("enabled strategy selected %d, want Free %d", freeLease.Credential.ID, freeAccount.ID)
	}

	fallbackLease, err := selector.Acquire(ctx, account.ProviderBuild, 0, "grok-4.5", "", "", nil, false)
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

func TestCandidatePlanPrefersKnownRemainingQuota(t *testing.T) {
	values := []account.RoutingCandidate{
		{Credential: account.Credential{ID: 1, Priority: 100}},
		{Credential: account.Credential{ID: 2, Priority: 1}, QuotaWindow: &account.QuotaWindow{AccountID: 2, Mode: "console_image", Remaining: 2, Total: 5}},
	}
	scores := []candidateScore{{index: 0}, {index: 1, quotaKnown: true, quotaAvailable: true}}
	if !candidateScoreBetter(values, scores[1], scores[0]) {
		t.Fatal("known remaining quota did not outrank an unknown window")
	}
}

func TestCandidatePlanDoesNotTreatEstimatedQuotaAsAuthoritative(t *testing.T) {
	now := time.Now().UTC()
	selector := NewSelector(nil, memory.NewConcurrencyLimiter(), nil, nil, time.Hour, time.Second, time.Minute)
	values := []account.RoutingCandidate{
		{Credential: account.Credential{ID: 1, Priority: 100}, QuotaWindow: &account.QuotaWindow{AccountID: 1, Mode: "console_image", Remaining: 5, Total: 5, Source: account.QuotaSourceEstimated}},
		{Credential: account.Credential{ID: 2, Priority: 1}, QuotaWindow: &account.QuotaWindow{AccountID: 2, Mode: "console_image", Remaining: 1, Total: 5, Source: account.QuotaSourceUpstream}},
	}
	plan, err := selector.planCandidates(context.Background(), values, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, ok := plan.Next()
	if !ok || first.Credential.ID != 2 {
		t.Fatalf("first candidate = %#v, want authoritative account 2", first)
	}
}

func TestCandidatePlanDoesNotTreatLegacyDefaultQuotaAsAuthoritative(t *testing.T) {
	now := time.Now().UTC()
	selector := NewSelector(nil, memory.NewConcurrencyLimiter(), nil, nil, time.Hour, time.Second, time.Minute)
	values := []account.RoutingCandidate{
		{Credential: account.Credential{ID: 1, Priority: 100}, QuotaWindow: &account.QuotaWindow{AccountID: 1, Mode: "console_image", Remaining: 5, Total: 5, Source: account.QuotaSourceDefault}},
		{Credential: account.Credential{ID: 2, Priority: 1}, QuotaWindow: &account.QuotaWindow{AccountID: 2, Mode: "console_image", Remaining: 1, Total: 5, Source: account.QuotaSourceUpstream}},
	}
	plan, err := selector.planCandidates(context.Background(), values, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, ok := plan.Next()
	if !ok || first.Credential.ID != 2 {
		t.Fatalf("first candidate = %#v, want upstream-confirmed account 2", first)
	}
}

func TestSelectorConsumesOnlyMatchingQuotaSnapshot(t *testing.T) {
	key := candidateCacheKey{provider: account.ProviderWeb, upstreamModel: "chat", quotaMode: "fast"}
	values := []account.RoutingCandidate{{
		Credential: account.Credential{ID: 7}, QuotaWindow: &account.QuotaWindow{AccountID: 7, Mode: "fast", Remaining: 10},
	}}
	selector := &Selector{candidates: map[candidateCacheKey]candidateSnapshot{key: newCandidateSnapshot(values, time.Now().UTC().Add(time.Minute))}}
	original := selector.candidates[key].values
	selector.ConsumeQuota(account.ProviderWeb, 7, "fast", 3)
	if original[0].QuotaWindow == nil || original[0].QuotaWindow.Remaining != 10 {
		t.Fatalf("published snapshot was mutated: %#v", original[0].QuotaWindow)
	}
	consumed := selector.quotaConsumptionSnapshot(account.ProviderWeb)
	if quotaWindowExhausted(values[0], consumed) {
		t.Fatal("partially consumed quota was treated as exhausted")
	}
	selector.ConsumeQuota(account.ProviderWeb, 7, "other", 100)
	if quotaWindowExhausted(values[0], selector.quotaConsumptionSnapshot(account.ProviderWeb)) {
		t.Fatal("a different quota mode affected the candidate")
	}
	selector.ConsumeQuota(account.ProviderWeb, 7, "fast", 7)
	if !quotaWindowExhausted(values[0], selector.quotaConsumptionSnapshot(account.ProviderWeb)) {
		t.Fatal("fully consumed quota remained schedulable")
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
	first, err := selector.Acquire(ctx, account.ProviderBuild, 0, "model", "", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		lease *accountLease
		err   error
	}
	resultCh := make(chan result, 1)
	go func() {
		lease, acquireErr := selector.Acquire(ctx, account.ProviderBuild, 0, "model", "", "", nil, false)
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
	first, err := selector.Acquire(ctx, account.ProviderBuild, 0, "model", "", "stable-affinity", nil, false)
	if err != nil || first.Credential.ID != primary.ID {
		t.Fatalf("first lease = %#v, err = %v", first, err)
	}
	type result struct {
		lease *accountLease
		err   error
	}
	resultCh := make(chan result, 1)
	go func() {
		lease, acquireErr := selector.Acquire(ctx, account.ProviderBuild, 0, "model", "", "stable-affinity", nil, false)
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

func TestSelectionSessionStickyWaitsForBoundAccountCapacity(t *testing.T) {
	ctx := context.Background()
	sticky := memory.NewStickyStore()
	selector, primary, _ := newStickySelectorFixture(t, sticky, 300*time.Millisecond, true)
	firstSession, err := selector.beginSelectionSession(ctx, account.ProviderBuild, 0, "model", "", "stable-affinity", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	first, err := firstSession.Acquire(ctx, nil, false)
	if err != nil || first.Credential.ID != primary.ID {
		t.Fatalf("first lease = %#v, err = %v", first, err)
	}
	secondSession, err := selector.beginSelectionSession(ctx, account.ProviderBuild, 0, "model", "", "stable-affinity", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		lease *accountLease
		err   error
	}
	resultCh := make(chan result, 1)
	go func() {
		lease, acquireErr := secondSession.Acquire(ctx, nil, false)
		resultCh <- result{lease: lease, err: acquireErr}
	}()
	select {
	case value := <-resultCh:
		t.Fatalf("selection session bypassed the sticky account before capacity returned: %#v", value)
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
		t.Fatal("selection session did not wake after sticky capacity returned")
	}
}

func TestSelectionSessionStickyFallbackDoesNotRebind(t *testing.T) {
	ctx := context.Background()
	sticky := memory.NewStickyStore()
	selector, primary, fallback := newStickySelectorFixture(t, sticky, 20*time.Millisecond, true)
	firstSession, err := selector.beginSelectionSession(ctx, account.ProviderBuild, 0, "model", "", "stable-affinity", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	first, err := firstSession.Acquire(ctx, nil, false)
	if err != nil || first.Credential.ID != primary.ID {
		t.Fatalf("first lease = %#v, err = %v", first, err)
	}
	secondSession, err := selector.beginSelectionSession(ctx, account.ProviderBuild, 0, "model", "", "stable-affinity", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	temporary, err := secondSession.Acquire(ctx, nil, false)
	if err != nil || temporary.Credential.ID != fallback.ID {
		t.Fatalf("temporary lease = %#v, err = %v", temporary, err)
	}
	if boundID, ok, err := sticky.Get(ctx, stickySessionKey("stable-affinity"), time.Now().UTC()); err != nil || !ok || boundID != primary.ID {
		t.Fatalf("sticky binding changed after temporary fallback: id=%d ok=%v err=%v", boundID, ok, err)
	}
	temporary.Release()
	first.Release()
}

func TestSelectorStickySessionTemporaryFallbackDoesNotRebind(t *testing.T) {
	ctx := context.Background()
	sticky := memory.NewStickyStore()
	selector, primary, fallback := newStickySelectorFixture(t, sticky, 20*time.Millisecond, true)
	first, err := selector.Acquire(ctx, account.ProviderBuild, 0, "model", "", "stable-affinity", nil, false)
	if err != nil || first.Credential.ID != primary.ID {
		t.Fatalf("first lease = %#v, err = %v", first, err)
	}
	temporary, err := selector.Acquire(ctx, account.ProviderBuild, 0, "model", "", "stable-affinity", nil, false)
	if err != nil || temporary.Credential.ID != fallback.ID {
		t.Fatalf("temporary lease = %#v, err = %v", temporary, err)
	}
	if boundID, ok, err := sticky.Get(ctx, stickySessionKey("stable-affinity"), time.Now().UTC()); err != nil || !ok || boundID != primary.ID {
		t.Fatalf("sticky binding changed after temporary fallback: id=%d ok=%v err=%v", boundID, ok, err)
	}
	temporary.Release()
	first.Release()
	resumed, err := selector.Acquire(ctx, account.ProviderBuild, 0, "model", "", "stable-affinity", nil, false)
	if err != nil || resumed.Credential.ID != primary.ID {
		t.Fatalf("resumed sticky lease = %#v, err = %v", resumed, err)
	}
	resumed.Release()
}

func TestSelectorStickyHitRefreshesTTL(t *testing.T) {
	ctx := context.Background()
	sticky := newRecordingStickyStore()
	selector, _, _ := newStickySelectorFixture(t, sticky, 0, false)
	first, err := selector.Acquire(ctx, account.ProviderBuild, 0, "model", "", "stable-affinity", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	first.Release()
	time.Sleep(time.Millisecond)
	second, err := selector.Acquire(ctx, account.ProviderBuild, 0, "model", "", "stable-affinity", nil, false)
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
	if _, err := selector.Acquire(ctx, account.ProviderBuild, 0, "limited-model", "", "", nil, false); err == nil {
		t.Fatal("matching model cooldown was ignored")
	} else {
		var unavailable *SelectionUnavailableError
		if !errors.As(err, &unavailable) || unavailable.Reason != SelectionModelCooling || unavailable.RetryAfter < 30*time.Minute {
			t.Fatalf("error = %v", err)
		}
	}
	lease, err := selector.Acquire(ctx, account.ProviderBuild, 0, "other-model", "", "", nil, false)
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

func TestMarkMissingThinkingCoolsThenDisables(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "missing-thinking.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "no-think", SourceKey: "no-think", EncryptedAccessToken: "encrypted", Enabled: true,
		AuthStatus: account.AuthStatusActive, Priority: 10, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, 30*time.Second, 30*time.Minute, 500*time.Millisecond)
	before := time.Now().UTC()
	if action, err := selector.markMissingThinking(ctx, credential, time.Hour); err != nil || action != missingThinkingPenaltyCooled {
		t.Fatalf("first penalty = (%s, %v)", action, err)
	}
	first, err := accounts.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Enabled || first.LastError != lastErrorMissingThinking || first.CooldownUntil == nil {
		t.Fatalf("first strike = %#v", first)
	}
	if wait := first.CooldownUntil.Sub(before); wait < 50*time.Minute || wait > 70*time.Minute {
		t.Fatalf("first cooldown = %s", wait)
	}
	if action, err := selector.markMissingThinking(ctx, first, time.Hour); err != nil || action != missingThinkingPenaltyUnchanged {
		t.Fatalf("in-cooldown penalty = (%s, %v)", action, err)
	}
	stillCooling, err := accounts.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stillCooling.Enabled {
		t.Fatal("in-cooldown second call must not disable")
	}
	expired := time.Now().UTC().Add(-time.Second)
	stillCooling.CooldownUntil = &expired
	if action, err := selector.markMissingThinking(ctx, stillCooling, time.Hour); err != nil || action != missingThinkingPenaltyDisabled {
		t.Fatalf("second penalty = (%s, %v)", action, err)
	}
	second, err := accounts.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.Enabled {
		t.Fatalf("second strike after cooldown must disable, got %#v", second)
	}
	if second.LastError != lastErrorMissingThinkingDisabled {
		t.Fatalf("disabled last error = %q", second.LastError)
	}

	ok, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "recovered", SourceKey: "recovered", EncryptedAccessToken: "encrypted", Enabled: true,
		AuthStatus: account.AuthStatusActive, Priority: 10, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if action, err := selector.markMissingThinking(ctx, ok, time.Hour); err != nil || action != missingThinkingPenaltyCooled {
		t.Fatalf("recovery penalty = (%s, %v)", action, err)
	}
	cooled, err := accounts.Get(ctx, ok.ID)
	if err != nil {
		t.Fatal(err)
	}
	selector.MarkSuccess(ctx, cooled)
	kept, err := accounts.Get(ctx, ok.ID)
	if err != nil {
		t.Fatal(err)
	}
	if kept.LastError != lastErrorMissingThinking || kept.CooldownUntil != nil || kept.FailureCount != 0 {
		t.Fatalf("success must keep thinking strike and clear cooldown, got %#v", kept)
	}
}

func TestMarkFailureSoftNetworkCooldown(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "soft-network.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "soft", SourceKey: "soft", EncryptedAccessToken: "encrypted", Enabled: true,
		AuthStatus: account.AuthStatusActive, Priority: 10, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, 30*time.Second, 30*time.Minute, 500*time.Millisecond)
	before := time.Now().UTC()
	selector.MarkFailure(ctx, credential, 0, 0)
	updated, err := accounts.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.FailureCount != 0 {
		t.Fatalf("soft network failure count = %d, want 0", updated.FailureCount)
	}
	if updated.CooldownUntil == nil {
		t.Fatal("expected short cooldown")
	}
	cooldown := updated.CooldownUntil.Sub(before)
	if cooldown < 4*time.Second || cooldown > 6*time.Second {
		t.Fatalf("soft network cooldown = %s, want ~5s", cooldown)
	}

	selector.MarkFailure(ctx, updated, http.StatusTooManyRequests, 0)
	hard, err := accounts.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if hard.FailureCount != 1 {
		t.Fatalf("hard failure count = %d, want 1", hard.FailureCount)
	}
	if hard.CooldownUntil == nil || hard.CooldownUntil.Sub(time.Now().UTC()) < 20*time.Second {
		t.Fatalf("hard cooldown too short: %v", hard.CooldownUntil)
	}

	before = time.Now().UTC()
	if err := selector.markSoftFailure(ctx, hard, http.StatusGatewayTimeout, 0); err != nil {
		t.Fatal(err)
	}
	soft5xx, err := accounts.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if soft5xx.FailureCount != hard.FailureCount {
		t.Fatalf("soft 5xx failure count = %d, want preserved %d", soft5xx.FailureCount, hard.FailureCount)
	}
	if soft5xx.LastError != "upstream status 504" {
		t.Fatalf("soft 5xx diagnostic = %q", soft5xx.LastError)
	}
	if soft5xx.CooldownUntil == nil {
		t.Fatal("soft 5xx did not set cooldown")
	}
	cooldown = soft5xx.CooldownUntil.Sub(before)
	if cooldown < 4*time.Second || cooldown > 6*time.Second {
		t.Fatalf("soft 5xx cooldown = %s, want ~5s", cooldown)
	}

	before = time.Now().UTC()
	if err := selector.markSoftFailure(ctx, soft5xx, http.StatusServiceUnavailable, 12*time.Second); err != nil {
		t.Fatal(err)
	}
	softRetryAfter, err := accounts.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if softRetryAfter.FailureCount != hard.FailureCount {
		t.Fatalf("Retry-After soft failure count = %d, want preserved %d", softRetryAfter.FailureCount, hard.FailureCount)
	}
	if softRetryAfter.CooldownUntil == nil {
		t.Fatal("Retry-After soft failure did not set cooldown")
	}
	cooldown = softRetryAfter.CooldownUntil.Sub(before)
	if cooldown < 11*time.Second || cooldown > 13*time.Second {
		t.Fatalf("Retry-After soft cooldown = %s, want ~12s", cooldown)
	}

	before = time.Now().UTC()
	selector.MarkFailure(ctx, softRetryAfter, 0, 0)
	preserved, err := accounts.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if preserved.FailureCount != hard.FailureCount {
		t.Fatalf("ordinary soft failure count = %d, want preserved %d", preserved.FailureCount, hard.FailureCount)
	}
	if preserved.CooldownUntil == nil {
		t.Fatal("ordinary soft failure did not set cooldown")
	}
	cooldown = preserved.CooldownUntil.Sub(before)
	if cooldown < 4*time.Second || cooldown > 6*time.Second {
		t.Fatalf("ordinary soft cooldown = %s, want ~5s", cooldown)
	}

	before = time.Now().UTC()
	if err := selector.MarkFailureAfterSuccess(ctx, preserved, 0, 0); err != nil {
		t.Fatal(err)
	}
	reset, err := accounts.Get(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reset.FailureCount != 0 {
		t.Fatalf("after-success soft failure count = %d, want 0", reset.FailureCount)
	}
	if reset.CooldownUntil == nil {
		t.Fatal("after-success soft failure did not set cooldown")
	}
	cooldown = reset.CooldownUntil.Sub(before)
	if cooldown < 4*time.Second || cooldown > 6*time.Second {
		t.Fatalf("after-success soft cooldown = %s, want ~5s", cooldown)
	}
}

func TestConsoleRateLimitReconciliationUsesNonAccumulatingCooldown(t *testing.T) {
	tests := []struct {
		name             string
		status           int
		state            accountapp.RateLimitReconcileState
		err              error
		wantFailureCount int
	}{
		{name: "quota confirmed available", status: http.StatusTooManyRequests, state: accountapp.RateLimitReconcileAvailable, wantFailureCount: 3},
		{name: "another replica refreshing", status: http.StatusTooManyRequests, state: accountapp.RateLimitReconcileRefreshing, wantFailureCount: 3},
		{name: "quota probe inconclusive", status: http.StatusTooManyRequests, state: accountapp.RateLimitReconcileInconclusive, err: errors.New("usage probe failed"), wantFailureCount: 3},
		{name: "payment failure keeps hard penalty", status: http.StatusPaymentRequired, state: accountapp.RateLimitReconcileAvailable, wantFailureCount: 4},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "console-rate-limit-soft.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = database.Close() })
			if err := database.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			accounts := relational.NewAccountRepository(database)
			credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
				Provider: account.ProviderConsole, AuthType: account.AuthTypeSSO,
				Name: "console-soft", SourceKey: "console-soft", EncryptedAccessToken: "encrypted",
				Enabled: true, AuthStatus: account.AuthStatusActive,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := accounts.UpdateHealth(ctx, credential.ID, credential.Provider, 3, nil, "prior failures", false); err != nil {
				t.Fatal(err)
			}
			credential, err = accounts.Get(ctx, credential.ID)
			if err != nil {
				t.Fatal(err)
			}
			selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, 30*time.Second, 30*time.Minute, 500*time.Millisecond)
			service := &Service{selector: selector}
			before := time.Now().UTC()

			service.applyRateLimitReconciliation(ctx, credential, test.status, 12*time.Second, test.state, test.err)

			updated, err := accounts.Get(ctx, credential.ID)
			if err != nil {
				t.Fatal(err)
			}
			if updated.FailureCount != test.wantFailureCount {
				t.Fatalf("failure count = %d, want %d", updated.FailureCount, test.wantFailureCount)
			}
			if updated.CooldownUntil == nil {
				t.Fatal("expected Console 429 cooldown")
			}
			cooldown := updated.CooldownUntil.Sub(before)
			if test.status == http.StatusTooManyRequests {
				if cooldown < 11*time.Second || cooldown > 13*time.Second {
					t.Fatalf("cooldown = %s, want ~12s", cooldown)
				}
			} else if cooldown < 2*time.Minute {
				t.Fatalf("hard failure cooldown = %s, want at least 2m", cooldown)
			}
		})
	}
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

type webVideoTierOrder struct{}

func (webVideoTierOrder) TierOrder(account.Provider, string) []account.WebTier {
	return []account.WebTier{account.WebTierBasic, account.WebTierSuper, account.WebTierHeavy}
}

func (webVideoTierOrder) TierOrderForQuotaMode(_ account.Provider, _ string, quotaMode string) []account.WebTier {
	if quotaMode == account.QuotaModeWebVideo720p {
		return []account.WebTier{account.WebTierBasic, account.WebTierSuper, account.WebTierHeavy}
	}
	return []account.WebTier{account.WebTierSuper, account.WebTierHeavy}
}

func (f failingConcurrencyLimiter) Acquire(context.Context, string, int) (func(), bool, error) {
	return nil, false, f.err
}

func (f failingConcurrencyLimiter) Current(context.Context, string) (int, error) {
	return 0, nil
}
