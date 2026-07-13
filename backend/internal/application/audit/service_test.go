package audit

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
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
	repo := &flakyAuditRepository{failures: 2}
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
	if attempts := repo.attempts.Load(); attempts != 3 {
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
	repo := &blockingAuditRepository{}
	service := NewService(repo, slog.Default(), 8, 4, time.Second)
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
}

type blockingAuditRepository struct{ repository.AuditRepository }

func (r *blockingAuditRepository) Create(ctx context.Context, _ auditdomain.Record) error {
	<-ctx.Done()
	return ctx.Err()
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
