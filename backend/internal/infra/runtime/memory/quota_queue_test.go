package memory

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestQuotaRecoveryQueueClaimsInDueOrderAndReschedules(t *testing.T) {
	ctx := context.Background()
	queue := NewQuotaRecoveryQueue()
	now := time.Now().UTC()
	late := account.QuotaRecoveryEvent{AccountID: 2, Mode: "auto", DueAt: now.Add(time.Minute)}
	due := account.QuotaRecoveryEvent{AccountID: 1, Mode: "fast", DueAt: now.Add(-time.Second)}
	if err := queue.ScheduleQuotaRecovery(ctx, late); err != nil {
		t.Fatal(err)
	}
	if err := queue.ScheduleQuotaRecovery(ctx, due); err != nil {
		t.Fatal(err)
	}
	values, err := queue.ClaimDueQuotaRecoveries(ctx, now, 10, time.Minute)
	if err != nil || len(values) != 1 || values[0].AccountID != due.AccountID {
		t.Fatalf("values = %#v, err = %v", values, err)
	}
	claimedDue := values[0]
	claimedDue.DueAt = now.Add(2 * time.Minute)
	if err := queue.RescheduleQuotaRecovery(ctx, claimedDue); err != nil {
		t.Fatal(err)
	}
	values, err = queue.ClaimDueQuotaRecoveries(ctx, now.Add(90*time.Second), 10, time.Minute)
	if err != nil || len(values) != 1 || values[0].AccountID != late.AccountID {
		t.Fatalf("values = %#v, err = %v", values, err)
	}
}

func TestEnsureQuotaRecoveryPreservesExistingBackoffAndClaim(t *testing.T) {
	ctx := context.Background()
	queue := NewQuotaRecoveryQueue()
	now := time.Now().UTC()
	event := account.QuotaRecoveryEvent{AccountID: 1, Mode: "fast", DueAt: now.Add(-time.Second), Attempts: 3}
	if err := queue.ScheduleQuotaRecovery(ctx, event); err != nil {
		t.Fatal(err)
	}
	claimed, err := queue.ClaimDueQuotaRecoveries(ctx, now, 1, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].ClaimToken == "" {
		t.Fatalf("claimed = %#v, err = %v", claimed, err)
	}
	if err := queue.EnsureQuotaRecovery(ctx, account.QuotaRecoveryEvent{AccountID: 1, Mode: "fast", DueAt: now}); err != nil {
		t.Fatal(err)
	}
	claimed[0].DueAt = now.Add(2 * time.Minute)
	claimed[0].Attempts++
	if err := queue.RescheduleQuotaRecovery(ctx, claimed[0]); err != nil {
		t.Fatalf("ensure overwrote active claim: %v", err)
	}
}

func TestScheduleQuotaRecoveryPreservesActiveClaim(t *testing.T) {
	ctx := context.Background()
	queue := NewQuotaRecoveryQueue()
	now := time.Now().UTC()
	event := account.QuotaRecoveryEvent{AccountID: 1, Mode: "console", DueAt: now.Add(-time.Second), Attempts: 2}
	if err := queue.ScheduleQuotaRecovery(ctx, event); err != nil {
		t.Fatal(err)
	}
	claimed, err := queue.ClaimDueQuotaRecoveries(ctx, now, 1, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].ClaimToken == "" {
		t.Fatalf("claimed = %#v, err = %v", claimed, err)
	}
	if err := queue.ScheduleQuotaRecovery(ctx, account.QuotaRecoveryEvent{AccountID: 1, Mode: "console", DueAt: now.Add(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	claimed[0].DueAt = now.Add(24 * time.Hour)
	claimed[0].Attempts++
	if err := queue.RescheduleQuotaRecovery(ctx, claimed[0]); err != nil {
		t.Fatalf("concurrent schedule overwrote active claim: %v", err)
	}
}

func TestCancelQuotaRecoveryRemovesOnlyUnclaimedEvent(t *testing.T) {
	ctx := context.Background()
	queue := NewQuotaRecoveryQueue()
	now := time.Now().UTC()
	if err := queue.ScheduleQuotaRecovery(ctx, account.QuotaRecoveryEvent{AccountID: 1, Mode: "console", DueAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := queue.CancelQuotaRecovery(ctx, 1, "console"); err != nil {
		t.Fatal(err)
	}
	values, err := queue.ClaimDueQuotaRecoveries(ctx, now.Add(2*time.Hour), 1, time.Minute)
	if err != nil || len(values) != 0 {
		t.Fatalf("cancelled event was claimable: %#v, err = %v", values, err)
	}

	if err := queue.ScheduleQuotaRecovery(ctx, account.QuotaRecoveryEvent{AccountID: 2, Mode: "console", DueAt: now.Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	claimed, err := queue.ClaimDueQuotaRecoveries(ctx, now, 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claimed = %#v, err = %v", claimed, err)
	}
	if err := queue.CancelQuotaRecovery(ctx, 2, "console"); err != nil {
		t.Fatal(err)
	}
	claimed[0].DueAt = now.Add(time.Hour)
	if err := queue.RescheduleQuotaRecovery(ctx, claimed[0]); err != nil {
		t.Fatalf("cancel removed active claim: %v", err)
	}
}
