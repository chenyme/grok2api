package account

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type quotaResetRepository struct {
	repository.AccountRepository
	countBatchSizes []int
	resetBatchSizes []int
}

func (r *quotaResetRepository) CountProviderAccountsByIDs(_ context.Context, provider accountdomain.Provider, ids []uint64) (int64, error) {
	if provider != accountdomain.ProviderBuild {
		return 0, nil
	}
	r.countBatchSizes = append(r.countBatchSizes, len(ids))
	return int64(len(ids)), nil
}

func (r *quotaResetRepository) ResetQuotaState(_ context.Context, provider accountdomain.Provider, ids []uint64) error {
	if provider != accountdomain.ProviderBuild {
		return errors.New("unexpected provider")
	}
	r.resetBatchSizes = append(r.resetBatchSizes, len(ids))
	return nil
}

func TestNewQuotaViewFreeUsesObservedRollingTokens(t *testing.T) {
	quota := newQuotaView(&accountdomain.Billing{IsUnifiedBillingUser: true}, 250_000, nil, "grok-4.5-build-free", false)
	if quota.Type != QuotaTypeFree || quota.Unit != "tokens" || quota.Limit != 500_000 || quota.LimitKnown || quota.Confidence != "observed" {
		t.Fatalf("quota = %#v", quota)
	}
	if quota.Used != 250_000 || quota.Remaining != 250_000 || quota.UsagePercent != 50 || quota.WindowHours != 24 || !quota.Observed {
		t.Fatalf("quota = %#v", quota)
	}
}

func TestNewQuotaViewPaidUsesMonthlyBilling(t *testing.T) {
	quota := newQuotaView(&accountdomain.Billing{MonthlyLimit: 200, Used: 50, BillingPeriodStart: "start", BillingPeriodEnd: "end"}, 900_000, nil, "", false)
	if quota.Type != QuotaTypePaid || quota.Unit != "credits" || quota.Limit != 200 {
		t.Fatalf("quota = %#v", quota)
	}
	if quota.Used != 50 || quota.Remaining != 150 || quota.UsagePercent != 25 || quota.Observed || !quota.LimitKnown {
		t.Fatalf("quota = %#v", quota)
	}
}

func TestNewQuotaViewPaidShowsBillingProbeState(t *testing.T) {
	now := time.Now().UTC()
	next := now.Add(time.Hour)
	quota := newQuotaView(&accountdomain.Billing{MonthlyLimit: 100, Used: 100}, 0, &accountdomain.QuotaRecovery{
		Kind: accountdomain.QuotaRecoveryKindPaid, Status: accountdomain.QuotaRecoveryStatusExhausted,
		ExhaustedAt: &now, NextProbeAt: &next,
	}, "", false)
	if quota.Type != QuotaTypePaid || quota.Status != QuotaStatusWaitingReset || quota.NextProbeAt == nil {
		t.Fatalf("quota = %#v", quota)
	}
}

func TestNewQuotaViewUnknownWithoutBillingSnapshot(t *testing.T) {
	quota := newQuotaView(nil, 100, nil, "", false)
	if quota.Type != QuotaTypeUnknown {
		t.Fatalf("quota = %#v", quota)
	}
}

func TestNewQuotaViewEstimatesFreeFromObservedZeroBillingProfile(t *testing.T) {
	quota := newQuotaView(&accountdomain.Billing{PlanName: "Free", IsUnifiedBillingUser: true, TopUpMethod: "TOP_UP_METHOD_SAVED_PAYMENT_METHOD"}, 100, nil, "", false)
	if quota.Type != QuotaTypeFree || quota.Source != "billingProfile" || quota.Confidence != "estimated" || quota.Limit != 500_000 || quota.LimitKnown {
		t.Fatalf("quota = %#v", quota)
	}
}

func TestNewQuotaViewInfersFreeFromSuccessfulZeroBillingSnapshot(t *testing.T) {
	quota := newQuotaView(&accountdomain.Billing{
		IsUnifiedBillingUser: true, UsagePeriodType: "USAGE_PERIOD_TYPE_WEEKLY", SyncedAt: time.Now().UTC(),
	}, 100, nil, "", false)
	if quota.Type != QuotaTypeFree || quota.Source != "billingProfile" || quota.Confidence != "estimated" {
		t.Fatalf("quota = %#v", quota)
	}
}

func TestNewQuotaViewTreatsZeroUsageSuperGrokAsPaid(t *testing.T) {
	quota := newQuotaView(&accountdomain.Billing{
		PlanName: "SuperGrok", IsUnifiedBillingUser: true,
		UsagePeriodType: "USAGE_PERIOD_TYPE_WEEKLY", CreditUsagePercent: 0,
	}, 0, nil, "", false)
	if quota.Type != QuotaTypePaid || quota.Unit != "percent" || quota.Limit != 100 || quota.Remaining != 100 || quota.Confidence != "observed" {
		t.Fatalf("quota = %#v", quota)
	}
}

func TestNewQuotaViewUsesConfirmedExhaustion(t *testing.T) {
	now := time.Now().UTC()
	next := now.Add(24 * time.Hour)
	quota := newQuotaView(&accountdomain.Billing{}, 250_000, &accountdomain.QuotaRecovery{
		Status: accountdomain.QuotaRecoveryStatusExhausted, ConfirmedUsed: 1_065_387,
		ConfirmedLimit: 1_000_000, ExhaustedAt: &now, NextProbeAt: &next, LastConfirmedAt: &now,
	}, "", false)
	if quota.Type != QuotaTypeFree || quota.Status != QuotaStatusWaitingReset || !quota.Confirmed {
		t.Fatalf("quota = %#v", quota)
	}
	if quota.Used != 1_065_387 || quota.Limit != 1_000_000 || quota.Remaining != 0 || quota.NextProbeAt == nil || !quota.LimitKnown {
		t.Fatalf("quota = %#v", quota)
	}
}

func TestNewQuotaViewBuildSuperEntitlementOverridesFreeSignals(t *testing.T) {
	now := time.Now().UTC()
	next := now.Add(time.Hour)
	// 零 Billing profile + entitlement → paid/Super，不伪造额度。
	quota := newQuotaView(&accountdomain.Billing{IsUnifiedBillingUser: true, TopUpMethod: "TOP_UP_METHOD_SAVED_PAYMENT_METHOD"}, 100, nil, "grok-4.5-build-free", true)
	if quota.Type != QuotaTypePaid || quota.Source != "buildSuperEntitlement" || quota.Confidence != "confirmed" || !quota.Confirmed {
		t.Fatalf("entitlement quota = %#v", quota)
	}
	if quota.LimitKnown || quota.Used != 0 || quota.Limit != 0 || quota.Unit != "" {
		t.Fatalf("must not fabricate limits: %#v", quota)
	}
	// Free recovery 不得把 entitlement 账号降回 Free。
	quota = newQuotaView(&accountdomain.Billing{}, 250_000, &accountdomain.QuotaRecovery{
		Kind: accountdomain.QuotaRecoveryKindFree, Status: accountdomain.QuotaRecoveryStatusExhausted,
		ConfirmedUsed: 1_000_000, ConfirmedLimit: 1_000_000, ExhaustedAt: &now, NextProbeAt: &next,
	}, "", true)
	if quota.Type != QuotaTypePaid || quota.Source != "buildSuperEntitlement" {
		t.Fatalf("entitlement must override free recovery: %#v", quota)
	}
	// Billing paid 仍优先展示真实额度。
	quota = newQuotaView(&accountdomain.Billing{MonthlyLimit: 200, Used: 50}, 0, nil, "", true)
	if quota.Type != QuotaTypePaid || quota.Source != "upstreamBilling" || quota.Limit != 200 {
		t.Fatalf("paid billing must win: %#v", quota)
	}
}

func TestBatchResetQuotaStatePreservesBillingAndClearsLocalBlocks(t *testing.T) {
	ctx := context.Background()
	service, accounts := openAccountService(t)
	credential, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "build", SourceKey: "quota-reset-build",
		EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	next := now.Add(24 * time.Hour)
	if err := accounts.SaveBilling(ctx, accountdomain.Billing{AccountID: credential.ID, PlanName: "free", Used: 42, SyncedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.SaveQuotaRecovery(ctx, accountdomain.QuotaRecovery{
		AccountID: credential.ID, Kind: accountdomain.QuotaRecoveryKindFree, Status: accountdomain.QuotaRecoveryStatusExhausted,
		ExhaustedAt: &now, NextProbeAt: &next, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.UpsertModelQuotaBlock(ctx, accountdomain.ModelQuotaBlock{
		AccountID: credential.ID, UpstreamModel: "grok-test", Reason: "model_quota_depleted", CooldownUntil: next,
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.UpsertModelQuotaBlock(ctx, accountdomain.ModelQuotaBlock{
		AccountID: credential.ID, UpstreamModel: "grok-denied", Reason: "model_access_denied", CooldownUntil: next,
	}); err != nil {
		t.Fatal(err)
	}

	reset, err := service.BatchResetQuotaState(ctx, []uint64{credential.ID})
	if err != nil || reset != 1 {
		t.Fatalf("reset = %d, err = %v", reset, err)
	}
	if _, err := accounts.GetQuotaRecovery(ctx, credential.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("quota recovery should be cleared, err = %v", err)
	}
	billing, err := accounts.GetBilling(ctx, credential.ID)
	if err != nil || billing.Used != 42 {
		t.Fatalf("billing = %#v, err = %v", billing, err)
	}
	candidates, err := accounts.ListRoutingCandidates(ctx, accountdomain.ProviderBuild, 0, "grok-test", "")
	if err != nil || len(candidates) != 1 || candidates[0].ModelQuotaBlock != nil {
		t.Fatalf("candidates = %#v, err = %v", candidates, err)
	}
	candidates, err = accounts.ListRoutingCandidates(ctx, accountdomain.ProviderBuild, 0, "grok-denied", "")
	if err != nil || len(candidates) != 1 || candidates[0].ModelQuotaBlock == nil || candidates[0].ModelQuotaBlock.Reason != "model_access_denied" {
		t.Fatalf("access denial must be preserved, candidates = %#v, err = %v", candidates, err)
	}
}

func TestResetAllBuildQuotaStateOnlyProcessesEnabledBuildAccounts(t *testing.T) {
	ctx := context.Background()
	service, accounts := openAccountService(t)
	create := func(providerValue accountdomain.Provider, sourceKey string, enabled bool) accountdomain.Credential {
		value, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
			Provider: providerValue, Name: sourceKey, SourceKey: sourceKey,
			EncryptedAccessToken: "encrypted", Enabled: enabled, AuthStatus: accountdomain.AuthStatusActive,
		})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	enabledBuild := create(accountdomain.ProviderBuild, "reset-all-enabled", true)
	disabledBuild := create(accountdomain.ProviderBuild, "reset-all-disabled", false)
	web := create(accountdomain.ProviderWeb, "reset-all-web", true)
	disabledBuild.Enabled = false
	if _, err := accounts.Update(ctx, disabledBuild); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	next := now.Add(24 * time.Hour)
	for _, value := range []accountdomain.Credential{enabledBuild, disabledBuild, web} {
		if err := accounts.SaveQuotaRecovery(ctx, accountdomain.QuotaRecovery{
			AccountID: value.ID, Kind: accountdomain.QuotaRecoveryKindFree, Status: accountdomain.QuotaRecoveryStatusExhausted,
			ExhaustedAt: &now, NextProbeAt: &next, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}

	reset, err := service.ResetAllBuildQuotaState(ctx)
	if err != nil || reset != 1 {
		t.Fatalf("reset = %d, err = %v", reset, err)
	}
	if _, err := accounts.GetQuotaRecovery(ctx, enabledBuild.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("enabled Build recovery should be cleared, err = %v", err)
	}
	for _, value := range []accountdomain.Credential{disabledBuild, web} {
		if _, err := accounts.GetQuotaRecovery(ctx, value.ID); err != nil {
			t.Fatalf("recovery for account %d should remain, err = %v", value.ID, err)
		}
	}
}

func TestBatchResetQuotaStateChunksMoreThanAdminPageLimit(t *testing.T) {
	repo := &quotaResetRepository{}
	service := &Service{accounts: repo}
	ids := make([]uint64, 2501)
	for index := range ids {
		ids[index] = uint64(index + 1)
	}

	reset, err := service.BatchResetQuotaState(context.Background(), ids)
	if err != nil || reset != len(ids) {
		t.Fatalf("reset = %d, err = %v", reset, err)
	}
	want := []int{500, 500, 500, 500, 500, 1}
	if !slices.Equal(repo.countBatchSizes, want) || !slices.Equal(repo.resetBatchSizes, want) {
		t.Fatalf("count batches = %v, reset batches = %v, want %v", repo.countBatchSizes, repo.resetBatchSizes, want)
	}
}
