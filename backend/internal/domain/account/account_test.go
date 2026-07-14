package account

import (
	"testing"
	"time"
)

func TestBillingIsExhaustedForOnDemandCredits(t *testing.T) {
	if !(Billing{OnDemandCap: 50, CreditUsagePercent: 100}).IsExhausted(0) {
		t.Fatal("expected exhausted on-demand billing")
	}
	if (Billing{CreditUsagePercent: 100}).IsExhausted(0) {
		t.Fatal("billing without a reported limit should not be treated as exhausted")
	}
	if !(Billing{CreditUsagePercent: 100, UsagePeriodType: "USAGE_PERIOD_TYPE_WEEKLY"}).IsExhausted(0) {
		t.Fatal("expected exhausted weekly usage period")
	}
}

func TestBillingPeriodEndMatchesExhaustedLimit(t *testing.T) {
	monthlyEnd := "2026-08-01T00:00:00Z"
	weeklyEnd := "2026-07-19T00:00:00Z"
	weekly := Billing{MonthlyLimit: 15_000, Used: 197, CreditUsagePercent: 100, UsagePeriodType: "USAGE_PERIOD_TYPE_WEEKLY", UsagePeriodEnd: weeklyEnd, BillingPeriodEnd: monthlyEnd}
	if value, ok := weekly.PeriodEnd(); !ok || value.Format(time.RFC3339) != weeklyEnd {
		t.Fatalf("weekly period end = %v, %v", value, ok)
	}
	monthly := Billing{MonthlyLimit: 15_000, Used: 15_000, CreditUsagePercent: 5, UsagePeriodType: "USAGE_PERIOD_TYPE_WEEKLY", UsagePeriodEnd: weeklyEnd, BillingPeriodEnd: monthlyEnd}
	if value, ok := monthly.PeriodEnd(); !ok || value.Format(time.RFC3339) != monthlyEnd {
		t.Fatalf("monthly period end = %v, %v", value, ok)
	}
}
