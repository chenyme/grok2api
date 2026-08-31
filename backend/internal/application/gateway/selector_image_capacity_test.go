package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

func TestImageProFairnessCoversEveryIdentityBeforePriorityRepeats(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	window := func(accountID uint64) *account.QuotaWindow {
		return &account.QuotaWindow{
			AccountID: accountID, Mode: account.QuotaModeWebImagePro, Remaining: 10,
			SyncedAt: &now, Source: account.QuotaSourceUpstream,
		}
	}
	values := []account.RoutingCandidate{
		{Credential: account.Credential{ID: 30, Provider: account.ProviderWeb, Priority: 1000, WebTier: account.WebTierSuper, MaxConcurrent: 1}, QuotaWindow: window(30), SupportsModel: true, ModelCapabilityKnown: true},
		{Credential: account.Credential{ID: 20, Provider: account.ProviderWeb, Priority: 10, WebTier: account.WebTierBasic, MaxConcurrent: 1}, QuotaWindow: window(20), SupportsModel: true, ModelCapabilityKnown: true},
		{Credential: account.Credential{ID: 10, Provider: account.ProviderWeb, Priority: -10, WebTier: account.WebTierBasic, MaxConcurrent: 1}, QuotaWindow: window(10), SupportsModel: true, ModelCapabilityKnown: true},
	}
	selector := NewSelector(nil, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	tierOrder := []account.WebTier{account.WebTierSuper, account.WebTierBasic}
	selected := make([]uint64, 0, 6)
	for range 6 {
		plan, err := selector.planCandidates(context.Background(), values, time.Now().UTC(), tierOrder)
		if err != nil {
			t.Fatal(err)
		}
		candidate, ok := plan.Next()
		if !ok {
			t.Fatal("image_pro fairness returned no candidate")
		}
		lease, err := selector.claimAccountSlot(context.Background(), candidate.Credential)
		if err != nil {
			t.Fatal(err)
		}
		if lease == nil {
			t.Fatal("image_pro fairness candidate was unexpectedly saturated")
		}
		selected = append(selected, lease.Credential.ID)
		lease.Release()
	}
	want := []uint64{10, 20, 30, 10, 20, 30}
	for index := range want {
		if selected[index] != want[index] {
			t.Fatalf("selection sequence = %v, want %v", selected, want)
		}
	}
}

func TestImageProFairnessBypassesPrioritySegmentedPlanner(t *testing.T) {
	t.Parallel()
	selector := NewSelector(nil, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	selector.UpdateSegmentedSelector(true, 100, 8)
	if request := selector.nextSegmentedActiveRequest(account.ProviderWeb, "grok-imagine-image-2.0", account.QuotaModeWebImagePro, 100); request != nil {
		t.Fatalf("image_pro selected priority-segmented planner: %#v", request)
	}
	if request := selector.nextSegmentedActiveRequest(account.ProviderBuild, "other", "", 100); request == nil {
		t.Fatal("non-image capacity route unexpectedly bypassed segmented planner")
	}
	if selector.ImageCapacityFairnessPolicy() != account.ImageProFairnessPolicy {
		t.Fatalf("fairness policy = %q", selector.ImageCapacityFairnessPolicy())
	}
}
