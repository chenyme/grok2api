package cli

import (
	"encoding/base64"
	"testing"
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

	credits, err := parseBilling([]byte(`{"onDemandEnabled":false,"subscriptionTier":"SuperGrok Heavy","config":{"creditUsagePercent":42.5,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-07-08T00:00:00+00:00","end":"2026-07-15T00:00:00+00:00"},"onDemandCap":{"val":0},"onDemandUsed":{"val":0},"isUnifiedBillingUser":true,"prepaidBalance":{"val":0},"topUpMethod":"TOP_UP_METHOD_SAVED_PAYMENT_METHOD","history":[{"period":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-07-01T00:00:00Z","end":"2026-07-08T00:00:00Z"},"onDemandUsed":{"val":120}}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !credits.IsUnifiedBillingUser || credits.OnDemandEnabled == nil || *credits.OnDemandEnabled || credits.CreditUsagePercent != 42.5 || credits.UsagePeriodType != "USAGE_PERIOD_TYPE_WEEKLY" || credits.UsagePeriodEnd != "2026-07-15T00:00:00+00:00" || credits.TopUpMethod != "TOP_UP_METHOD_SAVED_PAYMENT_METHOD" || credits.PlanCode != "" || credits.PlanName != "SuperGrok Heavy" {
		t.Fatalf("credits = %#v", credits)
	}
	if len(credits.History) != 1 || credits.History[0].PeriodType != "USAGE_PERIOD_TYPE_WEEKLY" || credits.History[0].PeriodStart != "2026-07-01T00:00:00Z" || credits.History[0].PeriodEnd != "2026-07-08T00:00:00Z" || credits.History[0].OnDemandUsed != 120 {
		t.Fatalf("credits history = %#v", credits.History)
	}
	if !credits.IsPaid() {
		t.Fatal("explicit SuperGrok tier must remain paid even when numeric limits are zero")
	}
}

func TestParseSubscriptionTierAndJWTFallback(t *testing.T) {
	tier, err := parseSubscriptionTier([]byte(`{"user":{"subscriptionTier":"SuperGrokPro"}}`))
	if err != nil || tier != "SuperGrokPro" {
		t.Fatalf("tier = %q err=%v", tier, err)
	}
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"tier":5}`))
	if got := subscriptionTierFromJWT("header." + claims + ".signature"); got != "supergrok_heavy" {
		t.Fatalf("JWT tier = %q", got)
	}
}
