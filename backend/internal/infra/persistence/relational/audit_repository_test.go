package relational

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	repositorypkg "github.com/chenyme/grok2api/backend/internal/repository"
)

func TestAuditRepositorySumTokensByAccountsSince(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := NewAuditRepository(database)
	now := time.Now().UTC()
	values := []audit.Record{
		{RequestID: "recent-1", ClientKeyID: 1, ModelRouteID: 1, AccountID: uint64Pointer(1), TotalTokens: 120, StatusCode: 200, CreatedAt: now.Add(-time.Hour)},
		{RequestID: "recent-2", ClientKeyID: 1, ModelRouteID: 1, AccountID: uint64Pointer(1), TotalTokens: 80, StatusCode: 200, CreatedAt: now.Add(-2 * time.Hour)},
		{RequestID: "old", ClientKeyID: 1, ModelRouteID: 1, AccountID: uint64Pointer(1), TotalTokens: 500, StatusCode: 200, CreatedAt: now.Add(-25 * time.Hour)},
		{RequestID: "other", ClientKeyID: 1, ModelRouteID: 1, AccountID: uint64Pointer(2), TotalTokens: 40, StatusCode: 200, CreatedAt: now.Add(-time.Hour)},
	}
	for _, value := range values {
		if err := repository.Create(ctx, value); err != nil {
			t.Fatal(err)
		}
	}
	totals, err := repository.SumTokensByAccountsSince(ctx, []uint64{1, 2}, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if totals[1] != 200 || totals[2] != 40 {
		t.Fatalf("totals = %#v", totals)
	}
}

func TestAuditRepositoryBatchAndCursor(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit-cursor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := NewAuditRepository(database)
	now := time.Now().UTC()
	values := []audit.Record{
		{RequestID: "cursor-old", ClientKeyID: 1, ModelRouteID: 1, StatusCode: 200, CreatedAt: now.Add(-48 * time.Hour)},
		{RequestID: "cursor-1", ClientKeyID: 1, ModelRouteID: 1, StatusCode: 200, CreatedAt: now.Add(-3 * time.Minute)},
		{RequestID: "cursor-2", ClientKeyID: 1, ModelRouteID: 1, StatusCode: 200, CreatedAt: now.Add(-2 * time.Minute)},
		{RequestID: "cursor-3", ClientKeyID: 1, ClientKeyName: "production", ModelRouteID: 1, ModelPublicID: "grok-test", ModelUpstreamModel: "grok-test-upstream", AccountName: "primary", EgressNodeID: uint64Pointer(42), EgressNodeName: "proxy-shanghai", EgressScope: "grok_web", EgressMode: audit.EgressModeProxy, StatusCode: 200, CreatedAt: now.Add(-time.Minute)},
	}
	if err := repository.CreateBatch(ctx, values); err != nil {
		t.Fatal(err)
	}
	sort := repositorypkg.SortQuery{Field: "createdAt", Direction: repositorypkg.SortDescending}
	first, hasMore, err := repository.ListCursor(ctx, repositorypkg.AuditCursorQuery{Limit: 2, Sort: sort})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || !hasMore || first[0].ID <= first[1].ID {
		t.Fatalf("first page = %#v, hasMore = %v", first, hasMore)
	}
	if first[0].ClientKeyName != "production" || first[0].ModelPublicID != "grok-test" || first[0].ModelUpstreamModel != "grok-test-upstream" || first[0].AccountName != "primary" || first[0].EgressNodeID == nil || *first[0].EgressNodeID != 42 || first[0].EgressNodeName != "proxy-shanghai" || first[0].EgressMode != audit.EgressModeProxy {
		t.Fatalf("audit snapshots = %#v", first[0])
	}
	matched, _, err := repository.ListCursor(ctx, repositorypkg.AuditCursorQuery{Limit: 10, Search: "proxy-shanghai", Sort: sort})
	if err != nil || len(matched) != 1 || matched[0].RequestID != "cursor-3" {
		t.Fatalf("egress search = %#v, err = %v", matched, err)
	}
	second, _, err := repository.ListCursor(ctx, repositorypkg.AuditCursorQuery{Cursor: &repositorypkg.SortCursor{ID: first[len(first)-1].ID, Value: first[len(first)-1].CreatedAt}, Limit: 2, Sort: sort})
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 2 || second[0].ID >= first[len(first)-1].ID {
		t.Fatalf("second page = %#v", second)
	}
}

func TestAuditRepositoryAtomicallyRecordsClientBillingUsage(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit-billing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	key := clientKeyModel{Name: "billing", Prefix: "billing", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 120, MaxConcurrent: 8}
	if err := database.db.WithContext(ctx).Create(&key).Error; err != nil {
		t.Fatal(err)
	}
	repository := NewAuditRepository(database)
	now := time.Now().UTC()
	if err := repository.CreateBatch(ctx, []audit.Record{
		{RequestID: "reported", ClientKeyID: key.ID, ModelRouteID: 1, StatusCode: 200, CostInUSDTicks: 20, EstimatedCostInUSDTicks: 90, CreatedAt: now},
		{RequestID: "estimated", ClientKeyID: key.ID, ModelRouteID: 1, StatusCode: 200, EstimatedCostInUSDTicks: 30, CreatedAt: now},
		{RequestID: "unpriced", ClientKeyID: key.ID, ModelRouteID: 1, StatusCode: 500, CreatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}
	var stored clientKeyModel
	if err := database.db.WithContext(ctx).First(&stored, key.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.BilledUsageUSDTicks != 50 {
		t.Fatalf("billed usage = %d", stored.BilledUsageUSDTicks)
	}
	if err := repository.Create(ctx, audit.Record{EventID: "evt_idempotent_billing_0001", RequestID: "idempotent", ClientKeyID: key.ID, ModelRouteID: 1, StatusCode: 200, EstimatedCostInUSDTicks: 40, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Create(ctx, audit.Record{EventID: "evt_idempotent_billing_0001", RequestID: "idempotent-retry", ClientKeyID: key.ID, ModelRouteID: 1, StatusCode: 200, EstimatedCostInUSDTicks: 40, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).First(&stored, key.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.BilledUsageUSDTicks != 90 {
		t.Fatalf("idempotent billed usage = %d", stored.BilledUsageUSDTicks)
	}
}

func TestAuditRepositorySettlesBillingReservationIdempotently(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit-reservation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	key := clientKeyModel{Name: "reserved", Prefix: "reserved", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 120, MaxConcurrent: 8, BillingLimitUSDTicks: 100}
	if err := database.db.WithContext(ctx).Create(&key).Error; err != nil {
		t.Fatal(err)
	}
	eventID := "evt_billing_reservation_0001"
	keys := NewClientKeyRepository(database)
	if reserved, err := keys.ReserveBillingUsage(ctx, key.ID, eventID, 80, time.Now().UTC().Add(time.Hour)); err != nil || !reserved {
		t.Fatal(err)
	}
	audits := NewAuditRepository(database)
	record := audit.Record{EventID: eventID, RequestID: "reserved-request", ClientKeyID: key.ID, ModelRouteID: 1, StatusCode: 200, EstimatedCostInUSDTicks: 30, CreatedAt: time.Now().UTC()}
	if err := audits.Create(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := audits.Create(ctx, record); err != nil {
		t.Fatal(err)
	}
	var stored clientKeyModel
	if err := database.db.WithContext(ctx).First(&stored, key.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.ReservedUsageUSDTicks != 0 || stored.BilledUsageUSDTicks != 30 {
		t.Fatalf("settled key = reserved %d, billed %d", stored.ReservedUsageUSDTicks, stored.BilledUsageUSDTicks)
	}
	if count := tableRowCount(t, database, "billing_reservations"); count != 0 {
		t.Fatalf("billing reservations = %d", count)
	}
}

func TestAuditRepositoryAllowsRepeatedExternalRequestIDs(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit-request-id.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := NewAuditRepository(database)
	now := time.Now().UTC()
	if err := repository.CreateBatch(ctx, []audit.Record{
		{RequestID: "caller-reused-id", ClientKeyID: 1, ModelRouteID: 1, StatusCode: 200, CreatedAt: now},
		{RequestID: "caller-reused-id", ClientKeyID: 1, ModelRouteID: 1, StatusCode: 200, CreatedAt: now.Add(time.Second)},
	}); err != nil {
		t.Fatal(err)
	}
	_, total, err := repository.List(ctx, 0, 10)
	if err != nil || total != 2 {
		t.Fatalf("total = %d, err = %v", total, err)
	}
}

func TestAuditRepositoryRoundTripsFailureAttempts(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit-attempts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := NewAuditRepository(database)
	now := time.Now().UTC()
	status := http.StatusBadGateway
	record := audit.Record{
		EventID: "evt_failure_attempts_0001", RequestID: "failure-attempts", ClientKeyID: 1, ModelRouteID: 1,
		StatusCode: http.StatusBadGateway, CreatedAt: now,
		Attempts: []audit.Attempt{
			{
				Number: 1, Source: audit.AttemptSourceTransport, Stage: "dns_lookup", AccountID: uint64Pointer(7), AccountName: "primary",
				Method: http.MethodPost, RequestPath: "/responses", UpstreamURL: "https://api.example.test/v1/responses", StartedAt: now.Add(-2 * time.Second), DurationMS: 125,
				TransportError: "lookup api.example.test: no such host", ErrorChain: []audit.ErrorFrame{{Type: "*url.Error", Message: "Post request failed"}, {Type: "*net.DNSError", Message: "no such host"}},
			},
			{
				Number: 2, Source: audit.AttemptSourceUpstreamHTTP, Stage: "upstream_response", AccountID: uint64Pointer(8), AccountName: "secondary",
				Method: http.MethodPost, RequestPath: "/responses", UpstreamURL: "https://api.example.test/v1/responses", StartedAt: now.Add(-time.Second), DurationMS: 250,
				UpstreamStatusCode: &status, UpstreamStatus: "502 Bad Gateway", ResponseHeaders: http.Header{"Content-Type": {"application/json"}, "X-Upstream": {"edge-a", "edge-b"}},
				ResponseBody: []byte{'{', '"', 'e', 'r', 'r', 'o', 'r', '"', ':', ' ', '"', 'f', 'a', 'i', 'l', 'e', 'd', '"', '}', 0xff}, ResponseBodyTruncated: true,
			},
		},
	}
	if err := repository.Create(ctx, record); err != nil {
		t.Fatal(err)
	}
	stored, err := repository.Get(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if stored.AttemptCount != 2 || len(stored.Attempts) != 2 {
		t.Fatalf("attempt count = %d, attempts = %#v", stored.AttemptCount, stored.Attempts)
	}
	if stored.Attempts[0].Number != 1 || stored.Attempts[0].Stage != "dns_lookup" || len(stored.Attempts[0].ErrorChain) != 2 {
		t.Fatalf("transport attempt = %#v", stored.Attempts[0])
	}
	httpAttempt := stored.Attempts[1]
	if httpAttempt.Number != 2 || httpAttempt.UpstreamStatusCode == nil || *httpAttempt.UpstreamStatusCode != status || string(httpAttempt.ResponseBody) != string(record.Attempts[1].ResponseBody) || !httpAttempt.ResponseBodyTruncated || len(httpAttempt.ResponseHeaders["X-Upstream"]) != 2 {
		t.Fatalf("HTTP attempt = %#v", httpAttempt)
	}
	if err := repository.Create(ctx, record); err != nil {
		t.Fatal(err)
	}
	if count := tableRowCount(t, database, "request_audit_attempts"); count != 2 {
		t.Fatalf("idempotent attempts = %d", count)
	}
	if _, err := repository.Get(ctx, 999); !errors.Is(err, repositorypkg.ErrNotFound) {
		t.Fatalf("missing audit error = %v", err)
	}
}

func TestAuditRepositoryNormalizesUntrustedUsage(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit-normalize.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := NewAuditRepository(database)
	if err := repository.Create(ctx, audit.Record{RequestID: "normalize", ClientKeyID: 1, ModelRouteID: 1, StatusCode: 200, MediaInputImages: -1, MediaOutputImages: -2, MediaOutputSeconds: -3, InputTokens: -1, TotalTokens: -2, DurationMS: -3, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	values, _, err := repository.List(ctx, 0, 1)
	if err != nil || len(values) != 1 {
		t.Fatalf("values = %#v, err = %v", values, err)
	}
	if values[0].MediaInputImages != 0 || values[0].MediaOutputImages != 0 || values[0].MediaOutputSeconds != 0 || values[0].InputTokens != 0 || values[0].TotalTokens != 0 || values[0].DurationMS != 0 {
		t.Fatalf("normalized audit = %#v", values[0])
	}
}

func TestAuditRepositorySummaryAppliesRangeAndGroupsPricingTier(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit-summary.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := NewAuditRepository(database)
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	values := []audit.Record{
		{RequestID: "standard", ClientKeyID: 1, ModelRouteID: 1, ModelPublicID: "public", ModelUpstreamModel: "grok-build-0.1", StatusCode: 200, Streaming: true, InputTokens: 100, CachedInputTokens: 20, OutputTokens: 50, ReasoningTokens: 10, TotalTokens: 150, EstimatedCostInUSDTicks: 1_840_000, PricingModel: "grok-build-0.1", PricingVersion: "2026-07-11", DurationMS: 100, CreatedAt: now.Add(-time.Hour)},
		{RequestID: "long", ClientKeyID: 1, ModelRouteID: 1, ModelPublicID: "public", ModelUpstreamModel: "grok-build-0.1", StatusCode: 500, Streaming: false, InputTokens: 210_000, ContextInputTokens: 210_000, OutputTokens: 100, TotalTokens: 210_100, DurationMS: 300, CreatedAt: now.Add(-2 * time.Hour)},
		{RequestID: "outside", ClientKeyID: 1, ModelRouteID: 1, ModelPublicID: "public", ModelUpstreamModel: "grok-build-0.1", StatusCode: 200, TotalTokens: 999, CreatedAt: now.Add(-8 * 24 * time.Hour)},
	}
	if err := repository.CreateBatch(ctx, values); err != nil {
		t.Fatal(err)
	}
	summary, err := repository.Summarize(ctx, repositorypkg.AuditSummaryQuery{Search: "public", Start: now.Add(-7 * 24 * time.Hour), End: now})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Requests != 2 || summary.SuccessfulRequests != 1 || summary.FailedRequests != 1 || summary.TotalTokens != 210_250 || summary.DurationMS != 400 {
		t.Fatalf("summary = %#v", summary)
	}
	if summary.EstimatedCostInUSDTicks != 1_840_000 || summary.PricedRequests != 1 || summary.UnpricedRequests != 1 || summary.PricedTokens != 150 || summary.UnpricedTokens != 210_100 {
		t.Fatalf("summary pricing = %#v", summary)
	}
}

func uint64Pointer(value uint64) *uint64 { return &value }
