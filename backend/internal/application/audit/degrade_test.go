package audit

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
)

func TestDegradeSummaryClassifiesStreamingAnomalies(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "degrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAuditRepository(database)
	now := time.Now().UTC()
	first := int64(100)
	accountID := uint64(7)
	records := []auditdomain.Record{
		{RequestID: "hard-1", ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build", AccountID: &accountID, AccountName: "hot", EgressNodeName: "node-a", StatusCode: 200, Streaming: true, OutputTokens: 2000, FirstTokenMS: &first, DurationMS: 1100, CreatedAt: now.Add(-time.Hour)},
		{RequestID: "quality_skip_me", ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build", AccountID: &accountID, StatusCode: 200, Streaming: true, OutputTokens: 2000, FirstTokenMS: &first, DurationMS: 1100, CreatedAt: now.Add(-time.Hour)},
		{RequestID: "non-stream", ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build", AccountID: &accountID, StatusCode: 200, Streaming: false, OutputTokens: 2000, FirstTokenMS: &first, DurationMS: 1100, CreatedAt: now.Add(-time.Hour)},
	}
	if err := repo.CreateBatch(ctx, records); err != nil {
		t.Fatal(err)
	}
	service := NewService(repo, slog.Default(), 8, 4, time.Second)
	service.now = func() time.Time { return now }
	summary, err := service.DegradeSummary(ctx, "24h", DegradeThresholds{SoftTPS: 500, HardTPS: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Totals.Hits != 1 || summary.Totals.Accounts != 1 || summary.Totals.Hard != 1 {
		t.Fatalf("totals = %#v", summary.Totals)
	}
	if len(summary.Accounts) != 1 || summary.Accounts[0].ID != 7 || summary.Accounts[0].Hits != 1 {
		t.Fatalf("accounts = %#v", summary.Accounts)
	}
}

func TestDegradeSummaryRejectsUnknownWindow(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "degrade-window.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	service := NewService(relational.NewAuditRepository(database), slog.Default(), 8, 4, time.Second)
	if _, err := service.DegradeSummary(ctx, "3h", DegradeThresholds{}); err != ErrInvalidPeriod {
		t.Fatalf("error = %v", err)
	}
}
