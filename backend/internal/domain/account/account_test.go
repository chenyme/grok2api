package account

import (
	"testing"
	"time"
)

func TestBillingIsPaidMatchesSQLSignals(t *testing.T) {
	if (Billing{}).IsPaid() {
		t.Fatal("empty billing must be Unknown/free, not paid")
	}
	for _, billing := range []Billing{
		{MonthlyLimit: 1},
		{OnDemandCap: 0.01},
		{OnDemandUsed: 1},
		{PrepaidBalance: 5},
		{PlanName: "SuperGrok"},
		{PlanName: "SuperGrok Heavy"},
		{PlanCode: "supergrok_lite"},
		{PlanName: "X Premium+"},
	} {
		if !billing.IsPaid() {
			t.Fatalf("expected paid for %#v", billing)
		}
	}
	if (Billing{Used: 100, CreditUsagePercent: 42.5, PlanName: "free"}).IsPaid() {
		t.Fatal("usage percentage must not mark a Free account as paid")
	}
	if (Billing{CreditUsagePercent: 42.5}).IsPaid() {
		t.Fatal("usage percentage without a subscription tier must remain unknown")
	}
	if (Billing{IsUnifiedBillingUser: true, UsagePeriodType: "USAGE_PERIOD_TYPE_WEEKLY"}).HasFreeProfileSignal() {
		t.Fatal("generic weekly billing fields must not mark an account as free")
	}
	if !(Billing{PlanName: "Free"}).HasFreeProfileSignal() || !(Billing{PlanCode: "x_basic"}).HasFreeProfileSignal() {
		t.Fatal("known restricted tiers should be recognized as free/basic")
	}
	now := time.Now().UTC()
	if !(Billing{SyncedAt: now, IsUnifiedBillingUser: true, UsagePeriodType: "USAGE_PERIOD_TYPE_WEEKLY"}).HasInferredFreeProfileSignal() {
		t.Fatal("a successful zero-value billing snapshot should be inferred as free")
	}
	for _, billing := range []Billing{
		{IsUnifiedBillingUser: true, UsagePeriodType: "USAGE_PERIOD_TYPE_WEEKLY"},
		{SyncedAt: now, MonthlyLimit: 1},
		{SyncedAt: now, CreditUsagePercent: 1},
		{SyncedAt: now, PlanName: "SuperGrok"},
	} {
		if billing.HasInferredFreeProfileSignal() {
			t.Fatalf("billing must remain non-inferred: %#v", billing)
		}
	}
}

func TestBuildRouteModeValidation(t *testing.T) {
	for _, value := range []BuildRouteMode{BuildRouteAuto, BuildRouteBuild, BuildRouteXAI} {
		if !value.IsValid() {
			t.Fatalf("valid route mode rejected: %q", value)
		}
	}
	for _, value := range []BuildRouteMode{"", "primary", "fallback"} {
		if value.IsValid() {
			t.Fatalf("invalid route mode accepted: %q", value)
		}
	}
}

func TestIsBuildSuper(t *testing.T) {
	paid := Billing{MonthlyLimit: 100}
	zeroFree := Billing{IsUnifiedBillingUser: true}
	if !IsBuildSuper(Credential{Provider: ProviderBuild, BuildSuperEntitled: true}, &zeroFree) {
		t.Fatal("entitlement must make zero-billing Build Super")
	}
	if !IsBuildSuper(Credential{Provider: ProviderBuild}, &paid) {
		t.Fatal("paid billing must make Build Super")
	}
	if IsBuildSuper(Credential{Provider: ProviderBuild}, &zeroFree) {
		t.Fatal("zero billing without entitlement is not Super")
	}
	if IsBuildSuper(Credential{Provider: ProviderWeb, BuildSuperEntitled: true}, &paid) {
		t.Fatal("Web must ignore BuildSuperEntitled")
	}
}

func TestRoutingCandidateIsKnownFreeBuild(t *testing.T) {
	freeBilling := Billing{PlanName: "Free"}
	paidBilling := Billing{PlanName: "SuperGrok"}
	freeRecovery := QuotaRecovery{Kind: QuotaRecoveryKindFree}
	tests := []struct {
		name      string
		candidate RoutingCandidate
		want      bool
	}{
		{name: "billing profile", candidate: RoutingCandidate{Credential: Credential{Provider: ProviderBuild}, Billing: &freeBilling}, want: true},
		{name: "inferred zero billing profile", candidate: RoutingCandidate{Credential: Credential{Provider: ProviderBuild}, Billing: &Billing{SyncedAt: time.Now().UTC(), IsUnifiedBillingUser: true, UsagePeriodType: "USAGE_PERIOD_TYPE_WEEKLY"}}, want: true},
		{name: "ambiguous weekly profile", candidate: RoutingCandidate{Credential: Credential{Provider: ProviderBuild}, Billing: &Billing{IsUnifiedBillingUser: true, UsagePeriodType: "USAGE_PERIOD_TYPE_WEEKLY"}}},
		{name: "observed response model", candidate: RoutingCandidate{Credential: Credential{Provider: ProviderBuild, ObservedModel: "grok-4.5-build-free"}}, want: true},
		{name: "quota recovery", candidate: RoutingCandidate{Credential: Credential{Provider: ProviderBuild}, QuotaRecovery: &freeRecovery}, want: true},
		{name: "paid overrides stale free signal", candidate: RoutingCandidate{Credential: Credential{Provider: ProviderBuild, ObservedModel: "grok-4.5-build-free"}, Billing: &paidBilling}},
		{name: "entitlement overrides free profile", candidate: RoutingCandidate{Credential: Credential{Provider: ProviderBuild, BuildSuperEntitled: true}, Billing: &freeBilling}},
		{name: "entitlement overrides free recovery", candidate: RoutingCandidate{Credential: Credential{Provider: ProviderBuild, BuildSuperEntitled: true}, QuotaRecovery: &freeRecovery}},
		{name: "entitlement overrides observed free model", candidate: RoutingCandidate{Credential: Credential{Provider: ProviderBuild, BuildSuperEntitled: true, ObservedModel: "grok-4.5-build-free"}}},
		{name: "unknown build", candidate: RoutingCandidate{Credential: Credential{Provider: ProviderBuild}}},
		{name: "web is never build free", candidate: RoutingCandidate{Credential: Credential{Provider: ProviderWeb, ObservedModel: "grok-4.5-build-free"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.candidate.IsKnownFreeBuild(); got != test.want {
				t.Fatalf("IsKnownFreeBuild() = %t, want %t", got, test.want)
			}
		})
	}
}

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
