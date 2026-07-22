package audit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestServiceCloseFlushesQueuedAudits(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit-service.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := relational.NewAuditRepository(database)
	service := NewService(repository, slog.Default(), 16, 8, time.Hour)
	service.Start()
	for index := range 5 {
		if err := service.Create(ctx, auditdomain.Record{RequestID: "queued-" + string(rune('a'+index)), ClientKeyID: 1, ModelRouteID: 1, StatusCode: 200, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	closeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := service.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	values, total, err := repository.List(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if total != 5 || len(values) != 5 {
		t.Fatalf("total = %d, values = %d", total, len(values))
	}
}

func TestAuditBatchRetriesTransientDatabaseFailure(t *testing.T) {
	repo := &flakyAuditRepository{failures: 5}
	service := NewService(repo, slog.Default(), 8, 4, time.Hour)
	service.Start()
	if !service.Record(auditdomain.Record{RequestID: "retry", StatusCode: 200}) {
		t.Fatal("record was not queued")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if attempts := repo.attempts.Load(); attempts != 6 {
		t.Fatalf("attempts = %d", attempts)
	}
}

func TestAuditBatchRecoversRepositoryPanicAndRetries(t *testing.T) {
	repo := &panicAuditRepository{}
	service := NewService(repo, slog.Default(), 8, 1, time.Hour)
	service.Start()
	if !service.Record(auditdomain.Record{RequestID: "panic-retry", StatusCode: 200}) {
		t.Fatal("record was not queued")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if attempts := repo.attempts.Load(); attempts != 2 {
		t.Fatalf("attempts = %d", attempts)
	}
}

type flakyAuditRepository struct {
	repository.AuditRepository
	failures int32
	attempts atomic.Int32
}

type panicAuditRepository struct {
	repository.AuditRepository
	attempts atomic.Int32
}

type summaryAuditRepository struct {
	repository.AuditRepository
	calls int
}

func (r *summaryAuditRepository) Summarize(context.Context, repository.AuditSummaryQuery) (auditdomain.Summary, error) {
	r.calls++
	return auditdomain.Summary{Requests: 1, SuccessfulRequests: 1}, nil
}

func TestSummaryCachesRepeatedAggregate(t *testing.T) {
	repo := &summaryAuditRepository{}
	service := NewService(repo, slog.Default(), 8, 4, time.Second)
	service.now = func() time.Time { return time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC) }

	if _, err := service.Summary(context.Background(), "", "24h", ListFilter{}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Summary(context.Background(), "", "24h", ListFilter{}); err != nil {
		t.Fatal(err)
	}
	if repo.calls != 1 {
		t.Fatalf("summary calls = %d", repo.calls)
	}
}

func TestSummaryFreshBypassesAggregateCache(t *testing.T) {
	repo := &summaryAuditRepository{}
	service := NewService(repo, slog.Default(), 8, 4, time.Second)
	service.now = func() time.Time { return time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC) }

	if _, err := service.Summary(context.Background(), "", "24h", ListFilter{}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SummaryFresh(context.Background(), "", "24h", ListFilter{}); err != nil {
		t.Fatal(err)
	}
	if repo.calls != 2 {
		t.Fatalf("summary calls = %d", repo.calls)
	}
}

func TestCreateDurableHonorsCallerDeadline(t *testing.T) {
	release := make(chan struct{})
	repo := &blockingAuditRepository{release: release}
	service := NewService(repo, slog.Default(), 8, 4, time.Second)
	service.Start()
	t.Cleanup(func() { closeAuditService(t, service) })
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := service.CreateDurable(ctx, auditdomain.Record{EventID: "deadline"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("deadline was not honored: %s", elapsed)
	}
	close(release)
}

func TestCreateRequiresStartedWriter(t *testing.T) {
	service := NewService(&toggleAuditRepository{}, slog.Default(), 8, 4, time.Second)
	err := service.Create(context.Background(), auditdomain.Record{EventID: "not-started"})
	if !errors.Is(err, ErrWriterUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestCloseStartsWriterAndFlushesPrestartRecords(t *testing.T) {
	repo := newGatedAuditRepository()
	close(repo.release)
	service := NewService(repo, slog.Default(), 8, 8, time.Hour)
	if !service.Record(auditdomain.Record{EventID: "prestart-record"}) {
		t.Fatal("record was not queued")
	}
	closeAuditService(t, service)
	if calls := repo.calls.Load(); calls != 1 {
		t.Fatalf("CreateBatch calls = %d", calls)
	}
	select {
	case <-repo.committed:
	default:
		t.Fatal("prestart record was not committed")
	}
}

func TestAcknowledgedWritesWaitForCommitAndCoalesce(t *testing.T) {
	repo := newGatedAuditRepository()
	service := NewService(repo, slog.Default(), 32, 32, time.Second)
	service.UpdateWriterConfig(32, time.Second, 100*time.Millisecond)
	service.Start()
	t.Cleanup(func() { closeAuditService(t, service) })

	const writeCount = 8
	start := make(chan struct{})
	results := make(chan error, writeCount)
	for index := range writeCount {
		go func() {
			<-start
			results <- service.Create(context.Background(), auditdomain.Record{EventID: fmt.Sprintf("coalesced-%d", index)})
		}()
	}
	close(start)

	var batch []auditdomain.Record
	select {
	case batch = <-repo.started:
	case <-time.After(time.Second):
		t.Fatal("batch did not start")
	}
	if len(batch) != writeCount {
		t.Fatalf("batch size = %d", len(batch))
	}
	select {
	case err := <-results:
		t.Fatalf("ack completed before commit: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	close(repo.release)
	for range writeCount {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if calls := repo.calls.Load(); calls != 1 {
		t.Fatalf("CreateBatch calls = %d", calls)
	}
}

func TestAcknowledgedWriteUsesCallerBudgetWhenQueueIsFull(t *testing.T) {
	repo := newGatedAuditRepository()
	service := NewService(repo, slog.Default(), 1, 1, time.Second)
	service.UpdateWriterConfig(1, time.Second, time.Millisecond)
	service.Start()
	t.Cleanup(func() { closeAuditService(t, service) })

	firstResult := make(chan error, 1)
	go func() {
		firstResult <- service.Create(context.Background(), auditdomain.Record{EventID: "evt_queue_budget_first_0001", RequestID: "first", ClientKeyID: 1, ModelRouteID: 1, StatusCode: 200, CreatedAt: time.Now().UTC()})
	}()
	select {
	case <-repo.started:
	case <-time.After(time.Second):
		t.Fatal("first batch did not start")
	}
	if !service.Record(auditdomain.Record{EventID: "evt_queue_budget_buffered_02", RequestID: "buffered", ClientKeyID: 1, ModelRouteID: 1, StatusCode: 200, CreatedAt: time.Now().UTC()}) {
		t.Fatal("failed to fill the audit queue")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ackResult := make(chan error, 1)
	go func() {
		ackResult <- service.Create(ctx, auditdomain.Record{EventID: "evt_queue_budget_waiting_003", RequestID: "waiting", ClientKeyID: 1, ModelRouteID: 1, StatusCode: 200, CreatedAt: time.Now().UTC()})
	}()
	select {
	case err := <-ackResult:
		t.Fatalf("acknowledged write returned before queue capacity recovered: %v", err)
	case <-time.After(2 * auditEnqueueWait):
	}

	close(repo.release)
	if err := <-firstResult; err != nil {
		t.Fatal(err)
	}
	if err := <-ackResult; err != nil {
		t.Fatal(err)
	}
}

func TestWriterIsolatesInvalidAuditRecordFromValidBatch(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit-invalid-isolation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	baseRepo := relational.NewAuditRepository(database)
	repo := &observedBatchAuditRepository{AuditRepository: baseRepo, sizes: make(chan int, 4)}
	service := NewService(repo, slog.Default(), 8, 8, time.Second)
	service.UpdateWriterConfig(8, time.Second, 100*time.Millisecond)
	service.UpdateLedgerConfig(LedgerConfig{Mode: LedgerModeObserve, FailureThreshold: 1, UnhealthyGrace: time.Hour, QueueHighWatermarkPercent: 90})
	droppedEvents := make(chan string, 1)
	service.SetDropObserver(func(eventIDs []string) {
		if len(eventIDs) > 0 {
			droppedEvents <- eventIDs[0]
		}
	})
	service.Start()
	t.Cleanup(func() { closeAuditService(t, service) })

	start := make(chan struct{})
	type result struct {
		name string
		err  error
	}
	results := make(chan result, 2)
	go func() {
		<-start
		results <- result{name: "valid", err: service.Create(ctx, auditdomain.Record{
			EventID: "evt_isolated_valid_record_001", RequestID: "valid", ClientKeyID: 1, ModelRouteID: 1,
			StatusCode: 200, CreatedAt: time.Now().UTC(),
		})}
	}()
	go func() {
		<-start
		results <- result{name: "invalid", err: service.Create(ctx, auditdomain.Record{
			EventID: "evt_isolated_invalid_record_02", RequestID: "invalid", ClientKeyID: 1, ModelRouteID: 1,
			StatusCode: 200, CreatedAt: time.Now().UTC(),
			Attempts: []auditdomain.Attempt{{Number: 0, Source: auditdomain.AttemptSourceCredential, Stage: "credential", StartedAt: time.Now().UTC()}},
		})}
	}()
	close(start)

	for range 2 {
		value := <-results
		switch value.name {
		case "valid":
			if value.err != nil {
				t.Fatalf("valid audit failed: %v", value.err)
			}
		case "invalid":
			if !errors.Is(value.err, repository.ErrInvalidRecord) {
				t.Fatalf("invalid audit error = %v", value.err)
			}
		}
	}
	if size := <-repo.sizes; size != 2 {
		t.Fatalf("initial batch size = %d, want 2", size)
	}
	_, total, err := baseRepo.List(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("persisted audits = %d, want 1", total)
	}
	select {
	case eventID := <-droppedEvents:
		if eventID != "evt_isolated_invalid_record_02" {
			t.Fatalf("dropped event = %q", eventID)
		}
	case <-time.After(time.Second):
		t.Fatal("drop observer was not notified")
	}
	if snapshot := service.LedgerSnapshot(); snapshot.Ready || !snapshot.Irrecoverable || snapshot.Dropped != 1 {
		t.Fatalf("ledger snapshot after rejected record = %#v", snapshot)
	}
	if err := service.CheckLedgerReady(); !errors.Is(err, ErrLedgerUnavailable) {
		t.Fatalf("irrecoverable drop did not block observe mode: %v", err)
	}
}

func TestCallerTimeoutDoesNotCancelQueuedSettlement(t *testing.T) {
	repo := newGatedAuditRepository()
	service := NewService(repo, slog.Default(), 8, 8, time.Second)
	service.UpdateWriterConfig(8, time.Second, time.Millisecond)
	service.Start()
	t.Cleanup(func() { closeAuditService(t, service) })

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- service.Create(ctx, auditdomain.Record{EventID: "caller-timeout"})
	}()
	select {
	case <-repo.started:
	case <-time.After(time.Second):
		t.Fatal("batch did not start")
	}
	if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	close(repo.release)
	select {
	case <-repo.committed:
	case <-time.After(time.Second):
		t.Fatal("timed-out write was not committed")
	}
	if err := service.Create(context.Background(), auditdomain.Record{EventID: "after-timeout"}); err != nil {
		t.Fatal(err)
	}
}

func TestLedgerReadinessObserveAndEnforceModes(t *testing.T) {
	initialNow := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	var nowNanos atomic.Int64
	nowNanos.Store(initialNow.UnixNano())
	repo := &toggleAuditRepository{err: errors.New("database unavailable")}
	service := NewService(repo, slog.Default(), 8, 4, time.Second)
	service.Start()
	t.Cleanup(func() {
		repo.setError(nil)
		closeAuditService(t, service)
	})
	service.now = func() time.Time { return time.Unix(0, nowNanos.Load()).UTC() }
	service.UpdateLedgerConfig(LedgerConfig{Mode: LedgerModeObserve, FailureThreshold: 1, UnhealthyGrace: time.Second, QueueHighWatermarkPercent: 90})
	writeCtx, writeCancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer writeCancel()
	if err := service.CreateDurable(writeCtx, auditdomain.Record{EventID: "observe"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("write error = %v", err)
	}
	nowNanos.Add(int64(2 * time.Second))
	if snapshot := service.LedgerSnapshot(); snapshot.Ready || snapshot.ConsecutiveFailures < 1 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if err := service.CheckLedgerReady(); err != nil {
		t.Fatalf("observe mode blocked traffic: %v", err)
	}

	service.UpdateLedgerConfig(LedgerConfig{Mode: LedgerModeEnforce, FailureThreshold: 1, UnhealthyGrace: time.Second, QueueHighWatermarkPercent: 90})
	if err := service.CheckLedgerReady(); !errors.Is(err, ErrLedgerUnavailable) {
		t.Fatalf("enforce readiness error = %v", err)
	}
	repo.setError(nil)
	recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer recoveryCancel()
	if err := service.CreateDurable(recoveryCtx, auditdomain.Record{EventID: "recovered"}); err != nil {
		t.Fatal(err)
	}
	if snapshot := service.LedgerSnapshot(); !snapshot.Ready || snapshot.ConsecutiveFailures != 0 {
		t.Fatalf("recovered snapshot = %#v", snapshot)
	}
}

func TestLedgerReadinessDetectsSustainedQueuePressure(t *testing.T) {
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	service := NewService(&toggleAuditRepository{}, slog.Default(), 2, 2, time.Hour)
	service.now = func() time.Time { return now }
	service.UpdateLedgerConfig(LedgerConfig{Mode: LedgerModeEnforce, FailureThreshold: 3, UnhealthyGrace: time.Second, QueueHighWatermarkPercent: 50})
	if !service.Record(auditdomain.Record{EventID: "queued"}) {
		t.Fatal("record was not queued")
	}
	if snapshot := service.LedgerSnapshot(); !snapshot.Ready {
		t.Fatalf("queue pressure should honor grace: %#v", snapshot)
	}
	now = now.Add(2 * time.Second)
	if err := service.CheckLedgerReady(); !errors.Is(err, ErrLedgerUnavailable) {
		t.Fatalf("queue pressure readiness error = %v", err)
	}
	<-service.queue
	service.recordLedgerSuccess()
	if snapshot := service.LedgerSnapshot(); !snapshot.Ready {
		t.Fatalf("drained queue did not restore readiness: %#v", snapshot)
	}
}

func TestCommitObserverRunsOnlyAfterDurableCommit(t *testing.T) {
	repo := &toggleAuditRepository{}
	service := NewService(repo, slog.Default(), 8, 4, time.Hour)
	service.Start()
	var mu sync.Mutex
	var committed []string
	service.SetCommitObserver(func(values []string) {
		mu.Lock()
		committed = append(committed, values...)
		mu.Unlock()
	})
	if err := service.CreateDurable(context.Background(), auditdomain.Record{EventID: "sync"}); err != nil {
		t.Fatal(err)
	}
	if !service.Record(auditdomain.Record{EventID: "batch"}) {
		t.Fatal("record was not queued")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(committed) != 2 || committed[0] != "sync" || committed[1] != "batch" {
		t.Fatalf("committed = %#v", committed)
	}
}

type toggleAuditRepository struct {
	repository.AuditRepository
	mu  sync.RWMutex
	err error
}

func (r *toggleAuditRepository) setError(err error) {
	r.mu.Lock()
	r.err = err
	r.mu.Unlock()
}

func (r *toggleAuditRepository) currentError() error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.err
}

func (r *toggleAuditRepository) Create(context.Context, auditdomain.Record) error {
	return r.currentError()
}

func (r *toggleAuditRepository) CreateBatch(context.Context, []auditdomain.Record) error {
	return r.currentError()
}

type blockingAuditRepository struct {
	repository.AuditRepository
	release <-chan struct{}
}

func (r *blockingAuditRepository) Create(ctx context.Context, _ auditdomain.Record) error {
	<-ctx.Done()
	return ctx.Err()
}

func (r *blockingAuditRepository) CreateBatch(ctx context.Context, _ []auditdomain.Record) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.release:
		return nil
	}
}

type gatedAuditRepository struct {
	repository.AuditRepository
	started   chan []auditdomain.Record
	release   chan struct{}
	committed chan struct{}
	calls     atomic.Int32
}

type observedBatchAuditRepository struct {
	repository.AuditRepository
	sizes chan int
}

func (r *observedBatchAuditRepository) CreateBatch(ctx context.Context, values []auditdomain.Record) error {
	select {
	case r.sizes <- len(values):
	default:
	}
	return r.AuditRepository.CreateBatch(ctx, values)
}

func newGatedAuditRepository() *gatedAuditRepository {
	return &gatedAuditRepository{
		started:   make(chan []auditdomain.Record, 8),
		release:   make(chan struct{}),
		committed: make(chan struct{}, 8),
	}
}

func (r *gatedAuditRepository) CreateBatch(ctx context.Context, values []auditdomain.Record) error {
	r.calls.Add(1)
	copyValues := append([]auditdomain.Record(nil), values...)
	select {
	case r.started <- copyValues:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-r.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case r.committed <- struct{}{}:
	default:
	}
	return nil
}

func closeAuditService(t *testing.T, service *Service) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := service.Close(ctx); err != nil {
		t.Errorf("close audit service: %v", err)
	}
}

func (r *panicAuditRepository) CreateBatch(context.Context, []auditdomain.Record) error {
	if r.attempts.Add(1) == 1 {
		panic("database driver panic")
	}
	return nil
}

func (r *flakyAuditRepository) CreateBatch(context.Context, []auditdomain.Record) error {
	attempt := r.attempts.Add(1)
	if attempt <= r.failures {
		return errors.New("temporary database error")
	}
	return nil
}

func TestSummaryUsesOfficialPricesAndExcludesUnknownModels(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit-summary.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := relational.NewAuditRepository(database)
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	if err := repository.CreateBatch(ctx, []auditdomain.Record{
		{RequestID: "priced", ClientKeyID: 1, ModelRouteID: 1, ModelUpstreamModel: "grok-build-0.1", StatusCode: 200, InputTokens: 1_000_000, CachedInputTokens: 200_000, OutputTokens: 500_000, TotalTokens: 1_500_000, EstimatedCostInUSDTicks: 36_800_000_000, PricingModel: "grok-build-0.1", PricingVersion: auditdomain.OfficialPricingAsOf, DurationMS: 100, CreatedAt: now.Add(-time.Hour)},
		{RequestID: "unknown", ClientKeyID: 1, ModelRouteID: 1, ModelUpstreamModel: "grok-4.5-build-free", StatusCode: 500, InputTokens: 100, OutputTokens: 50, TotalTokens: 150, DurationMS: 300, CreatedAt: now.Add(-2 * time.Hour)},
		{RequestID: "outside", ClientKeyID: 1, ModelRouteID: 1, ModelUpstreamModel: "grok-build-0.1", StatusCode: 200, TotalTokens: 999, CreatedAt: now.Add(-25 * time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}
	service := NewService(repository, slog.Default(), 16, 8, time.Hour)
	service.now = func() time.Time { return now }
	result, err := service.Summary(ctx, "", "24h", ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Usage.Requests != 2 || result.Usage.SuccessfulRequests != 1 || result.Usage.TotalTokens != 1_500_150 {
		t.Fatalf("usage = %#v", result.Usage)
	}
	if result.Usage.EstimatedCostInUSDTicks != 36_800_000_000 || result.Usage.PricedRequests != 1 || result.Usage.UnpricedRequests != 1 {
		t.Fatalf("pricing = %#v", result.Usage)
	}
	if result.Usage.AverageDurationMS != 200 || result.Usage.SuccessRate != 50 {
		t.Fatalf("rates = %#v", result.Usage)
	}
}

func TestListCursorKeepsStableOrderAcrossEqualSortValues(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit-sorted-cursor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAuditRepository(database)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	if err := repo.CreateBatch(ctx, []auditdomain.Record{
		{RequestID: "low", ClientKeyID: 1, ModelRouteID: 1, StatusCode: 200, TotalTokens: 50, CreatedAt: now.Add(-3 * time.Minute)},
		{RequestID: "equal-old", ClientKeyID: 1, ModelRouteID: 1, StatusCode: 200, TotalTokens: 100, CreatedAt: now.Add(-2 * time.Minute)},
		{RequestID: "equal-new", ClientKeyID: 1, ModelRouteID: 1, StatusCode: 200, TotalTokens: 100, CreatedAt: now.Add(-time.Minute)},
	}); err != nil {
		t.Fatal(err)
	}
	service := NewService(repo, slog.Default(), 8, 4, time.Hour)
	service.now = func() time.Time { return now }
	filter := ListFilter{Sort: repository.SortQuery{Field: "tokens", Direction: repository.SortDescending}}
	first, err := service.ListCursor(ctx, "", 2, "", "24h", filter)
	if err != nil || !first.HasMore || len(first.Items) != 2 || first.Items[0].RequestID != "equal-new" || first.Items[1].RequestID != "equal-old" || first.NextCursor == "" {
		t.Fatalf("first page = %#v, err = %v", first, err)
	}
	second, err := service.ListCursor(ctx, first.NextCursor, 2, "", "24h", filter)
	if err != nil || second.HasMore || len(second.Items) != 1 || second.Items[0].RequestID != "low" {
		t.Fatalf("second page = %#v, err = %v", second, err)
	}
	wrongSort := ListFilter{Sort: repository.SortQuery{Field: "duration", Direction: repository.SortDescending}}
	if _, err := service.ListCursor(ctx, first.NextCursor, 2, "", "24h", wrongSort); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("mismatched cursor error = %v", err)
	}
}
