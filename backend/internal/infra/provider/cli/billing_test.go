package cli

import (
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestParseBillingMonthlyPayload(t *testing.T) {
	value, err := parseBilling([]byte(`{"config":{"monthlyLimit":{"val":100},"used":{"val":25},"onDemandCap":{"val":0},"billingPeriodStart":"2026-07-01T00:00:00Z","billingPeriodEnd":"2026-08-01T00:00:00Z"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if value.MonthlyLimit != 100 || value.Used != 25 || value.CreditUsagePercent != 25 {
		t.Fatalf("billing = %#v", value)
	}
}

func TestParseBillingCreditsPayload(t *testing.T) {
	value, err := parseBilling([]byte(`{"subscription":{"code":"super","name":"Super Plan"},"config":{"currentPeriod":{"start":"2026-07-08T00:00:00Z","end":"2026-07-15T00:00:00Z"},"onDemandCap":{"val":50},"onDemandUsed":{"val":12.5}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if value.OnDemandCap != 50 || value.OnDemandUsed != 12.5 || value.Used != 0 || value.CreditUsagePercent != 25 {
		t.Fatalf("billing = %#v", value)
	}
	if value.UsagePeriodStart != "2026-07-08T00:00:00Z" || value.UsagePeriodEnd != "2026-07-15T00:00:00Z" {
		t.Fatalf("usage period = %q - %q", value.UsagePeriodStart, value.UsagePeriodEnd)
	}
	if value.PlanCode != "super" || value.PlanName != "Super Plan" {
		t.Fatalf("plan = %q / %q", value.PlanCode, value.PlanName)
	}
}

func TestParseBillingMatchesObservedBuildPayloads(t *testing.T) {
	monthly, err := parseBilling([]byte(`{"config":{"monthlyLimit":{"val":0},"used":{"val":0},"onDemandCap":{"val":0},"billingPeriodStart":"2026-07-01T00:00:00+00:00","billingPeriodEnd":"2026-08-01T00:00:00+00:00","history":[{"billingCycle":{"year":2026,"month":6},"includedUsed":{"val":0},"onDemandUsed":{"val":0},"totalUsed":{"val":0}}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(monthly.History) != 1 || monthly.History[0].Year != 2026 || monthly.History[0].Month != 6 {
		t.Fatalf("monthly = %#v", monthly)
	}

	credits, err := parseBilling([]byte(`{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-07-08T00:00:00+00:00","end":"2026-07-15T00:00:00+00:00"},"onDemandCap":{"val":0},"onDemandUsed":{"val":0},"isUnifiedBillingUser":true,"prepaidBalance":{"val":0},"topUpMethod":"TOP_UP_METHOD_SAVED_PAYMENT_METHOD","billingPeriodStart":"2026-07-08T00:00:00+00:00","billingPeriodEnd":"2026-07-15T00:00:00+00:00"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !credits.IsUnifiedBillingUser || credits.UsagePeriodType != "USAGE_PERIOD_TYPE_WEEKLY" || credits.UsagePeriodEnd != "2026-07-15T00:00:00+00:00" || credits.TopUpMethod != "TOP_UP_METHOD_SAVED_PAYMENT_METHOD" {
		t.Fatalf("credits = %#v", credits)
	}
}

func TestMergeBillingSnapshotsUsesWeeklyUsagePeriod(t *testing.T) {
	monthly := mergeBillingSnapshots(
		account.Billing{MonthlyLimit: 15_000, Used: 197, CreditUsagePercent: 1.313, BillingPeriodStart: "2026-07-01T00:00:00Z", BillingPeriodEnd: "2026-08-01T00:00:00Z"},
		account.Billing{CreditUsagePercent: 5, UsagePeriodType: "USAGE_PERIOD_TYPE_WEEKLY", UsagePeriodStart: "2026-07-12T04:52:00Z", UsagePeriodEnd: "2026-07-19T04:52:00Z"},
	)
	if monthly.MonthlyLimit != 15_000 || monthly.Used != 197 || monthly.CreditUsagePercent != 5 {
		t.Fatalf("merged billing = %#v", monthly)
	}
	if monthly.UsagePeriodType != "USAGE_PERIOD_TYPE_WEEKLY" || monthly.UsagePeriodEnd != "2026-07-19T04:52:00Z" || monthly.BillingPeriodEnd != "2026-08-01T00:00:00Z" {
		t.Fatalf("usage period = %#v", monthly)
	}
}
