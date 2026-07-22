package memory

import (
	"context"
	"testing"
	"time"
)

func TestQuotaRefreshCoordinatorCompareAndClear(t *testing.T) {
	coordinator := NewQuotaRefreshCoordinator()
	ctx := context.Background()
	first, err := coordinator.MarkQuotaRefreshDirty(ctx, 42, "fast", time.Hour)
	if err != nil || first != 1 {
		t.Fatalf("first generation = %d, err = %v", first, err)
	}
	second, err := coordinator.MarkQuotaRefreshDirty(ctx, 42, "fast", time.Hour)
	if err != nil || second != 2 {
		t.Fatalf("second generation = %d, err = %v", second, err)
	}
	if cleared, err := coordinator.ClearQuotaRefreshDirty(ctx, 42, "fast", first); err != nil || cleared {
		t.Fatalf("stale clear = %v, err = %v", cleared, err)
	}
	if cleared, err := coordinator.ClearQuotaRefreshDirty(ctx, 42, "fast", second); err != nil || !cleared {
		t.Fatalf("current clear = %v, err = %v", cleared, err)
	}
}

func TestQuotaRefreshCoordinatorExpiredLookupRemovesDirtyMembership(t *testing.T) {
	coordinator := NewQuotaRefreshCoordinator()
	ctx := context.Background()

	for _, accountID := range []uint64{41, 42} {
		if _, err := coordinator.MarkQuotaRefreshDirty(ctx, accountID, "fast", time.Hour); err != nil {
			t.Fatalf("mark account %d dirty: %v", accountID, err)
		}
		key := quotaRefreshKey(accountID, "fast")
		state := coordinator.values[key]
		state.expiresAt = time.Now().Add(-time.Second)
		coordinator.values[key] = state
	}

	if generation, dirty, err := coordinator.QuotaRefreshGeneration(ctx, 41, "fast"); err != nil || generation != 0 || dirty {
		t.Fatalf("expired generation = %d, dirty = %v, err = %v", generation, dirty, err)
	}
	if cleared, err := coordinator.ClearQuotaRefreshDirty(ctx, 42, "fast", 1); err != nil || cleared {
		t.Fatalf("expired clear = %v, err = %v", cleared, err)
	}
	if len(coordinator.dirty) != 0 {
		t.Fatalf("expired dirty membership retained: %v", coordinator.dirty)
	}
}

func TestQuotaRefreshCoordinatorListExcludesClearedAndExpiredState(t *testing.T) {
	coordinator := NewQuotaRefreshCoordinator()
	ctx := context.Background()

	clearedGeneration, err := coordinator.MarkQuotaRefreshDirty(ctx, 1, "fast", time.Hour)
	if err != nil {
		t.Fatalf("mark cleared state: %v", err)
	}
	if cleared, err := coordinator.ClearQuotaRefreshDirty(ctx, 1, "fast", clearedGeneration); err != nil || !cleared {
		t.Fatalf("clear state = %v, err = %v", cleared, err)
	}
	if _, err := coordinator.MarkQuotaRefreshDirty(ctx, 2, "fast", time.Hour); err != nil {
		t.Fatalf("mark expired state: %v", err)
	}
	expiredKey := quotaRefreshKey(2, "fast")
	expired := coordinator.values[expiredKey]
	expired.expiresAt = time.Now().Add(-time.Second)
	coordinator.values[expiredKey] = expired

	states, err := coordinator.ListQuotaRefreshDirty(ctx, time.Now(), 10)
	if err != nil {
		t.Fatalf("list dirty states: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("listed cleared or expired states: %+v", states)
	}
}

func TestQuotaRefreshCoordinatorCompactsStaleExpiryGenerations(t *testing.T) {
	coordinator := NewQuotaRefreshCoordinator()
	ctx := context.Background()
	const updates = 10000

	for range updates {
		if _, err := coordinator.MarkQuotaRefreshDirty(ctx, 42, "fast", 24*time.Hour); err != nil {
			t.Fatal(err)
		}
	}

	if len(coordinator.values) != 1 || len(coordinator.dirty) != 1 {
		t.Fatalf("coordinator state = values:%d dirty:%d", len(coordinator.values), len(coordinator.dirty))
	}
	if got, maxExpected := len(coordinator.expires), quotaRefreshExpiryCompactionMinStale+2; got > maxExpected {
		t.Fatalf("expiry heap retained %d stale generations, want at most %d", got, maxExpected)
	}
	if generation, dirty, err := coordinator.QuotaRefreshGeneration(ctx, 42, "fast"); err != nil || !dirty || generation != updates {
		t.Fatalf("generation = %d, dirty = %v, err = %v", generation, dirty, err)
	}
}
