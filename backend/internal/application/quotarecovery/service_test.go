package quotarecovery

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestRunDueClaimsBoundedBatchAndProcessesConcurrently(t *testing.T) {
	now := time.Now().UTC()
	events := make([]accountdomain.QuotaRecoveryEvent, defaultRecoveryWorkers)
	for index := range events {
		events[index] = accountdomain.QuotaRecoveryEvent{AccountID: uint64(index + 1), Mode: "fast", DueAt: now}
	}
	queue := &quotaQueueStub{claimed: events}
	syncer := &quotaSyncStub{}
	service := NewService(testLogger(), queue, syncer, time.Second, time.Minute)

	service.runDue(context.Background(), now)

	if queue.claimLimit != defaultRecoveryWorkers || queue.claimLease != recoveryClaimLease {
		t.Fatalf("claim limit = %d, lease = %s", queue.claimLimit, queue.claimLease)
	}
	if syncer.maxConcurrent != defaultRecoveryWorkers {
		t.Fatalf("max concurrent probes = %d", syncer.maxConcurrent)
	}
	if queue.acked != defaultRecoveryWorkers {
		t.Fatalf("acked events = %d", queue.acked)
	}
}

func TestReconcileDueRestoresMissingQueueEvents(t *testing.T) {
	now := time.Now().UTC()
	queue := &quotaQueueStub{}
	syncer := &quotaSyncStub{due: []accountdomain.QuotaWindow{{AccountID: 7, Mode: "expert", Remaining: 0, ResetAt: &now}}}
	service := NewService(testLogger(), queue, syncer, time.Second, time.Minute)

	service.reconcileDue(context.Background(), now)

	if len(queue.scheduled) != 1 || queue.scheduled[0].AccountID != 7 || queue.scheduled[0].Mode != "expert" {
		t.Fatalf("scheduled = %#v", queue.scheduled)
	}
}

func TestRunOneKeepsPredictedRecoveryWindow(t *testing.T) {
	now := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	queue := &quotaQueueStub{}
	syncer := &quotaSyncStub{window: accountdomain.QuotaWindow{Mode: "console", Remaining: 0}}
	service := NewService(testLogger(), queue, syncer, 30*time.Second, 30*time.Minute)

	service.runOne(context.Background(), now, accountdomain.QuotaRecoveryEvent{AccountID: 7, Mode: "console", DueAt: now, ClaimToken: "claim"})

	if len(queue.rescheduled) != 1 || !queue.rescheduled[0].DueAt.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("rescheduled = %#v", queue.rescheduled)
	}
}

func TestRunOnePreservesNonConsoleWindowDuration(t *testing.T) {
	now := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	queue := &quotaQueueStub{}
	syncer := &quotaSyncStub{window: accountdomain.QuotaWindow{Mode: "fast", Remaining: 0, WindowSeconds: 3600}}
	service := NewService(testLogger(), queue, syncer, 30*time.Second, 30*time.Minute)

	service.runOne(context.Background(), now, accountdomain.QuotaRecoveryEvent{AccountID: 8, Mode: "fast", DueAt: now, ClaimToken: "claim"})

	if len(queue.rescheduled) != 1 || !queue.rescheduled[0].DueAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("rescheduled = %#v", queue.rescheduled)
	}
}

type quotaQueueStub struct {
	mu          sync.Mutex
	claimed     []accountdomain.QuotaRecoveryEvent
	claimLimit  int
	claimLease  time.Duration
	acked       int
	scheduled   []accountdomain.QuotaRecoveryEvent
	ensured     []accountdomain.QuotaRecoveryEvent
	rescheduled []accountdomain.QuotaRecoveryEvent
	cancelled   []accountdomain.QuotaRecoveryEvent
}

func (q *quotaQueueStub) EnsureQuotaRecovery(_ context.Context, value accountdomain.QuotaRecoveryEvent) error {
	q.mu.Lock()
	q.ensured = append(q.ensured, value)
	q.scheduled = append(q.scheduled, value)
	q.mu.Unlock()
	return nil
}

func (q *quotaQueueStub) ScheduleQuotaRecovery(_ context.Context, value accountdomain.QuotaRecoveryEvent) error {
	q.mu.Lock()
	q.scheduled = append(q.scheduled, value)
	q.mu.Unlock()
	return nil
}

func (q *quotaQueueStub) CancelQuotaRecovery(_ context.Context, accountID uint64, mode string) error {
	q.mu.Lock()
	q.cancelled = append(q.cancelled, accountdomain.QuotaRecoveryEvent{AccountID: accountID, Mode: mode})
	q.mu.Unlock()
	return nil
}

func (q *quotaQueueStub) ClaimDueQuotaRecoveries(_ context.Context, _ time.Time, limit int, lease time.Duration) ([]accountdomain.QuotaRecoveryEvent, error) {
	q.claimLimit, q.claimLease = limit, lease
	return q.claimed, nil
}

func (q *quotaQueueStub) AckQuotaRecovery(_ context.Context, _ accountdomain.QuotaRecoveryEvent) error {
	q.mu.Lock()
	q.acked++
	q.mu.Unlock()
	return nil
}

func (q *quotaQueueStub) RescheduleQuotaRecovery(_ context.Context, value accountdomain.QuotaRecoveryEvent) error {
	q.mu.Lock()
	q.rescheduled = append(q.rescheduled, value)
	q.mu.Unlock()
	return nil
}

type quotaSyncStub struct {
	mu            sync.Mutex
	current       int
	maxConcurrent int
	due           []accountdomain.QuotaWindow
	window        accountdomain.QuotaWindow
}

func (s *quotaSyncStub) ProbeQuotaMode(_ context.Context, accountID uint64, mode string) (accountdomain.QuotaWindow, error) {
	s.mu.Lock()
	s.current++
	if s.current > s.maxConcurrent {
		s.maxConcurrent = s.current
	}
	s.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	s.mu.Lock()
	s.current--
	s.mu.Unlock()
	if s.window.Mode != "" {
		window := s.window
		window.AccountID = accountID
		return window, nil
	}
	return accountdomain.QuotaWindow{AccountID: accountID, Mode: mode, Remaining: 1}, nil
}

func (s *quotaSyncStub) ListDueQuotaWindows(context.Context, time.Time, int) ([]accountdomain.QuotaWindow, error) {
	return s.due, nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
