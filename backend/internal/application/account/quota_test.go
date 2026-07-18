package account

import (
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestNewQuotaViewFreeUsesObservedRollingTokens(t *testing.T) {
	quota := newQuotaView(&accountdomain.Billing{IsUnifiedBillingUser: true}, 250_000, nil, "grok-4.5-build-free")
	if quota.Type != QuotaTypeFree || quota.Unit != "tokens" || quota.Limit != 1_000_000 || quota.LimitKnown || quota.Confidence != "observed" {
		t.Fatalf("quota = %#v", quota)
	}
	if quota.Used != 250_000 || quota.Remaining != 750_000 || quota.UsagePercent != 25 || quota.WindowHours != 24 || !quota.Observed {
		t.Fatalf("quota = %#v", quota)
	}
}

func TestNewQuotaViewPaidUsesMonthlyBilling(t *testing.T) {
	quota := newQuotaView(&accountdomain.Billing{MonthlyLimit: 200, Used: 50, BillingPeriodStart: "start", BillingPeriodEnd: "end"}, 900_000, nil, "")
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
	}, "")
	if quota.Type != QuotaTypePaid || quota.Status != QuotaStatusWaitingReset || quota.NextProbeAt == nil {
		t.Fatalf("quota = %#v", quota)
	}
}

func TestNewQuotaViewUnknownWithoutBillingSnapshot(t *testing.T) {
	quota := newQuotaView(nil, 100, nil, "")
	if quota.Type != QuotaTypeUnknown {
		t.Fatalf("quota = %#v", quota)
	}
}

func TestNewQuotaViewEstimatesFreeFromObservedZeroBillingProfile(t *testing.T) {
	quota := newQuotaView(&accountdomain.Billing{PlanName: "Free", IsUnifiedBillingUser: true, TopUpMethod: "TOP_UP_METHOD_SAVED_PAYMENT_METHOD"}, 100, nil, "")
	if quota.Type != QuotaTypeFree || quota.Source != "billingProfile" || quota.Confidence != "estimated" || quota.Limit != 1_000_000 || quota.LimitKnown {
		t.Fatalf("quota = %#v", quota)
	}
}

func TestNewQuotaViewTreatsZeroUsageSuperGrokAsPaid(t *testing.T) {
	quota := newQuotaView(&accountdomain.Billing{
		PlanName: "SuperGrok", IsUnifiedBillingUser: true,
		UsagePeriodType: "USAGE_PERIOD_TYPE_WEEKLY", CreditUsagePercent: 0,
	}, 0, nil, "")
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
	}, "")
	if quota.Type != QuotaTypeFree || quota.Status != QuotaStatusWaitingReset || !quota.Confirmed {
		t.Fatalf("quota = %#v", quota)
	}
	if quota.Used != 1_065_387 || quota.Limit != 1_000_000 || quota.Remaining != 0 || quota.NextProbeAt == nil || !quota.LimitKnown {
		t.Fatalf("quota = %#v", quota)
	}
}
