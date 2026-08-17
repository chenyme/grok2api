package audit

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
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
	summary, err := service.DegradeSummary(ctx, "24h", DegradeThresholds{SoftTPS: 500, HardTPS: 1000}, DegradeAccountFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Totals.Hits != 1 || summary.Totals.Accounts != 1 || summary.Totals.Hard != 1 {
		t.Fatalf("totals = %#v", summary.Totals)
	}
	if len(summary.Accounts) != 1 || summary.Accounts[0].ID != 7 || summary.Accounts[0].Hits != 1 {
		t.Fatalf("accounts = %#v", summary.Accounts)
	}
	if summary.Totals.Deleted != 1 || summary.Totals.Disabled != 0 || summary.Accounts[0].Found {
		t.Fatalf("deleted account state = totals %#v, account %#v", summary.Totals, summary.Accounts[0])
	}
}

func TestDegradeSummaryDoesNotDropOlderAnomalyAfterTwentyThousandNewerRows(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "degrade-large.db"))
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
	records := make([]auditdomain.Record, 0, 20_001)
	records = append(records, auditdomain.Record{
		RequestID: "older-real-anomaly", ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build",
		AccountID: &accountID, StatusCode: 200, Streaming: true, OutputTokens: 2_000,
		FirstTokenMS: &first, DurationMS: 1_100, CreatedAt: now.Add(-2 * time.Hour),
	})
	for index := 0; index < 20_000; index++ {
		records = append(records, auditdomain.Record{
			RequestID: fmt.Sprintf("newer-healthy-%05d", index), ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build",
			AccountID: &accountID, StatusCode: 200, Streaming: true, OutputTokens: 100,
			FirstTokenMS: &first, DurationMS: 1_100, CreatedAt: now.Add(-time.Minute - time.Duration(index)*time.Millisecond),
		})
	}
	if err := repo.CreateBatch(ctx, records); err != nil {
		t.Fatal(err)
	}
	service := NewService(repo, slog.Default(), 8, 4, time.Second)
	service.now = func() time.Time { return now }
	summary, err := service.DegradeSummary(ctx, "24h", DegradeThresholds{SoftTPS: 500, HardTPS: 1000}, DegradeAccountFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Totals.Hits != 1 || summary.Totals.Hard != 1 || len(summary.Events) != 1 || summary.Events[0].RequestID != "older-real-anomaly" {
		t.Fatalf("summary silently dropped older anomaly: totals=%#v events=%#v", summary.Totals, summary.Events)
	}
}

func TestDegradeSummaryMatchesFailClosedShortWindowPolicy(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "degrade-policy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAuditRepository(database)
	now := time.Now().UTC()
	first := int64(50)
	accountID := uint64(9)
	if err := repo.Create(ctx, auditdomain.Record{
		RequestID: "short-window", ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build",
		AccountID: &accountID, StatusCode: 200, Streaming: true, OutputTokens: 200,
		FirstTokenMS: &first, DurationMS: 250, CreatedAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	service := NewService(repo, slog.Default(), 8, 4, time.Second)
	service.now = func() time.Time { return now }
	open, err := service.DegradeSummary(ctx, "1h", DegradeThresholds{SoftTPS: 500, HardTPS: 1000, FailClosed: false}, DegradeAccountFilter{})
	if err != nil {
		t.Fatal(err)
	}
	closed, err := service.DegradeSummary(ctx, "1h", DegradeThresholds{SoftTPS: 500, HardTPS: 1000, FailClosed: true}, DegradeAccountFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if open.Totals.Hard != 1 || open.Totals.Burst != 0 || closed.Totals.Hard != 0 || closed.Totals.Burst != 1 {
		t.Fatalf("open=%#v closed=%#v", open.Totals, closed.Totals)
	}
}

func TestDegradeSummaryUsesReasoningEvidenceForLateFlushFallback(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "degrade-reasoning.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAuditRepository(database)
	now := time.Now().UTC()
	first := int64(10000)
	accountID := uint64(9)
	records := []auditdomain.Record{
		{RequestID: "reasoning-late-flush", ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build", AccountID: &accountID, StatusCode: 200, Streaming: true, OutputTokens: 2000, ReasoningTokens: 1900, FirstTokenMS: &first, DurationMS: 10100, CreatedAt: now.Add(-2 * time.Minute)},
		{RequestID: "visible-output-burst", ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build", AccountID: &accountID, StatusCode: 200, Streaming: true, OutputTokens: 2000, FirstTokenMS: &first, DurationMS: 10100, CreatedAt: now.Add(-time.Minute)},
	}
	if err := repo.CreateBatch(ctx, records); err != nil {
		t.Fatal(err)
	}
	service := NewService(repo, slog.Default(), 8, 4, time.Second)
	service.now = func() time.Time { return now }
	summary, err := service.DegradeSummary(ctx, "1h", DegradeThresholds{SoftTPS: 500, HardTPS: 1000, FailClosed: true}, DegradeAccountFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Totals.Hits != 1 || summary.Totals.Burst != 1 || len(summary.Events) != 1 || summary.Events[0].RequestID != "visible-output-burst" {
		t.Fatalf("reasoning-aware summary = totals %#v events %#v", summary.Totals, summary.Events)
	}
}

func TestDegradeSummaryPaginatesAccountsInRepository(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "degrade-page.db"))
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
	for id := uint64(1); id <= 3; id++ {
		accountID := id
		if err := repo.Create(ctx, auditdomain.Record{
			RequestID: fmt.Sprintf("page-account-%d", id), ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build",
			AccountID: &accountID, StatusCode: 200, Streaming: true, OutputTokens: 2_000,
			FirstTokenMS: &first, DurationMS: 1_100, CreatedAt: now.Add(-time.Duration(id) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	service := NewService(repo, slog.Default(), 8, 4, time.Second)
	service.now = func() time.Time { return now }
	summary, err := service.DegradeSummary(ctx, "1h", DegradeThresholds{}, DegradeAccountFilter{Page: 2, PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if summary.AccountPage.Total != 3 || summary.AccountPage.HasMore || len(summary.Accounts) != 1 {
		t.Fatalf("page=%#v accounts=%#v", summary.AccountPage, summary.Accounts)
	}
}

func TestDegradeSummarySearchesEveryAccountIdentityField(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "degrade-search.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountValue, _, err := relational.NewAccountRepository(database).UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, AuthType: accountdomain.AuthTypeOAuth,
		Name: "Searchable Build Account", SourceKey: "searchable-build",
		EncryptedAccessToken: "encrypted-token", AuthStatus: accountdomain.AuthStatusActive, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	first := int64(100)
	if err := relational.NewAuditRepository(database).Create(ctx, auditdomain.Record{
		RequestID: "search-by-name", ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build",
		AccountID: &accountValue.ID, AccountName: "stale audit name", StatusCode: 200, Streaming: true,
		OutputTokens: 2_000, FirstTokenMS: &first, DurationMS: 1_100, CreatedAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	service := NewService(relational.NewAuditRepository(database), slog.Default(), 8, 4, time.Second)
	service.now = func() time.Time { return now }
	summary, err := service.DegradeSummary(ctx, "1h", DegradeThresholds{}, DegradeAccountFilter{Search: "searchable build"})
	if err != nil {
		t.Fatal(err)
	}
	if summary.AccountPage.Total != 1 || len(summary.Accounts) != 1 || summary.Accounts[0].ID != accountValue.ID {
		t.Fatalf("account search result = %#v", summary)
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
	if _, err := service.DegradeSummary(ctx, "3h", DegradeThresholds{}, DegradeAccountFilter{}); err != ErrInvalidPeriod {
		t.Fatalf("error = %v", err)
	}
}
