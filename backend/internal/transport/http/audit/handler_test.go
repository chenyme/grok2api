package audit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	auditapp "github.com/chenyme/grok2api/backend/internal/application/audit"
	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/gin-gonic/gin"
)

func TestAuditDetailReturnsCompleteTextAndBinaryBodies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit-handler.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := relational.NewAuditRepository(database)
	now := time.Now().UTC()
	status := http.StatusBadGateway
	if err := repository.Create(ctx, auditdomain.Record{
		EventID: "evt_audit_handler_0001", RequestID: "request-detail", ClientKeyID: 1, ModelRouteID: 1, StatusCode: status, CreatedAt: now,
		Attempts: []auditdomain.Attempt{
			{Number: 1, Source: auditdomain.AttemptSourceUpstreamHTTP, Stage: "upstream_response", StartedAt: now, UpstreamStatusCode: &status, ResponseHeaders: http.Header{"Content-Type": {"application/json"}}, ResponseBody: []byte(`{"error":"complete"}`), ResponseBodyTruncated: true},
			{Number: 2, Source: auditdomain.AttemptSourceUpstreamHTTP, Stage: "upstream_response", StartedAt: now, UpstreamStatusCode: &status, ResponseHeaders: http.Header{}, ResponseBody: []byte{0x00, 0xff, 0x01}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := auditapp.NewService(repository, slog.Default(), 8, 4, time.Second)
	router := gin.New()
	NewHandler(service).Register(router.Group("/api/admin/v1"))

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/v1/request-audits/1", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Data struct {
			Audit struct {
				AttemptCount int `json:"attemptCount"`
			} `json:"audit"`
			Attempts []struct {
				ResponseBody          string `json:"responseBody"`
				ResponseBodyEncoding  string `json:"responseBodyEncoding"`
				ResponseBodyTruncated bool   `json:"responseBodyTruncated"`
			} `json:"attempts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Data.Audit.AttemptCount != 2 || len(payload.Data.Attempts) != 2 {
		t.Fatalf("payload = %#v", payload)
	}
	if payload.Data.Attempts[0].ResponseBodyEncoding != "utf8" || payload.Data.Attempts[0].ResponseBody != `{"error":"complete"}` || !payload.Data.Attempts[0].ResponseBodyTruncated {
		t.Fatalf("text body = %#v", payload.Data.Attempts[0])
	}
	if payload.Data.Attempts[1].ResponseBodyEncoding != "base64" || payload.Data.Attempts[1].ResponseBody != base64.StdEncoding.EncodeToString([]byte{0x00, 0xff, 0x01}) {
		t.Fatalf("binary body = %#v", payload.Data.Attempts[1])
	}

	missing := httptest.NewRecorder()
	router.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/api/admin/v1/request-audits/999", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d, body = %s", missing.Code, missing.Body.String())
	}
}

func TestAuditResponseDerivesOutputThroughput(t *testing.T) {
	firstTokenMS := int64(250)
	response := newAuditResponse(auditdomain.Record{StatusCode: http.StatusOK, Streaming: true, FirstTokenMS: &firstTokenMS, DurationMS: 1250, OutputTokens: 80})
	if response.FirstTokenMS == nil || *response.FirstTokenMS != 250 || response.OutputTokensPerSecond == nil || *response.OutputTokensPerSecond != 80 {
		t.Fatalf("response = %#v", response)
	}

	unmeasured := newAuditResponse(auditdomain.Record{DurationMS: 1250, OutputTokens: 80})
	if unmeasured.OutputTokensPerSecond != nil {
		t.Fatalf("unmeasured throughput = %v", *unmeasured.OutputTokensPerSecond)
	}
}

func TestQualityGuardAuditListMarksOwnProbeWithoutExposingKeyIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "quality-guard-audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := relational.NewAuditRepository(database)
	now := time.Now().UTC()
	if err := repository.CreateBatch(ctx, []auditdomain.Record{
		{RequestID: "guard-probe", ClientKeyID: 7, ClientKeyName: "secret-guard-name", ModelRouteID: 1, Provider: "grok_build", StatusCode: 200, CreatedAt: now},
		{RequestID: "user-request", ClientKeyID: 8, ClientKeyName: "secret-user-name", ModelRouteID: 1, Provider: "grok_build", StatusCode: 200, CreatedAt: now.Add(-time.Second)},
	}); err != nil {
		t.Fatal(err)
	}
	service := auditapp.NewService(repository, slog.Default(), 8, 4, time.Second)
	router := gin.New()
	NewQualityGuardHandler(service, 7).RegisterQualityGuard(router.Group(""))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/request-audits?pageSize=20", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"requestId":"guard-probe","qualityProbe":true`) || !strings.Contains(body, `"requestId":"user-request","qualityProbe":false`) {
		t.Fatalf("probe markers missing: %s", body)
	}
	if strings.Contains(body, "clientKeyId") || strings.Contains(body, "clientKeyName") || strings.Contains(body, "secret-") {
		t.Fatalf("client key identity leaked: %s", body)
	}
}

func TestAuditResponseExplainsBillingWithoutChangingStoredTotal(t *testing.T) {
	estimated := newAuditResponse(auditdomain.Record{
		InputTokens: 100, CachedInputTokens: 20, OutputTokens: 50, ContextInputTokens: 100,
		EstimatedCostInUSDTicks: 1_840_000, PricingModel: "grok-build-0.1", PricingVersion: auditdomain.OfficialPricingAsOf,
	})
	if estimated.Billing == nil || estimated.Billing.Source != "official" || estimated.Billing.Method != "official_rates" || estimated.Billing.TotalInUSDTicks != 1_840_000 || len(estimated.Billing.Components) != 3 {
		t.Fatalf("estimated billing = %#v", estimated.Billing)
	}
	if estimated.Billing.Components[0].Kind != "uncached_input" || estimated.Billing.Components[1].Kind != "output" || estimated.Billing.Components[2].Kind != "cached_input" {
		t.Fatalf("billing component order = %#v", estimated.Billing.Components)
	}
	var componentTotal int64
	for _, component := range estimated.Billing.Components {
		componentTotal += component.SubtotalInUSDTicks
	}
	if componentTotal != estimated.Billing.TotalInUSDTicks {
		t.Fatalf("component total = %d, billing = %#v", componentTotal, estimated.Billing)
	}

	upstream := newAuditResponse(auditdomain.Record{CostInUSDTicks: 2_500_000})
	if upstream.Billing == nil || upstream.Billing.Source != "upstream" || upstream.Billing.Method != "upstream_reported" || upstream.Billing.TotalInUSDTicks != 2_500_000 || len(upstream.Billing.Components) != 0 {
		t.Fatalf("upstream billing = %#v", upstream.Billing)
	}

	historical := newAuditResponse(auditdomain.Record{
		InputTokens: 100, CachedInputTokens: 20, OutputTokens: 50,
		EstimatedCostInUSDTicks: 1_840_000, PricingModel: "grok-build-0.1", PricingVersion: "2026-07-11",
	})
	if historical.Billing == nil || historical.Billing.Method != "stored_estimate" || len(historical.Billing.Components) != 0 || historical.Billing.TotalInUSDTicks != 1_840_000 {
		t.Fatalf("historical billing = %#v", historical.Billing)
	}
}

func TestDegradeAccountsListsHardHitsAndRejectsUnknownWindow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "degrade-handler.db"))
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
	accountID := uint64(11)
	if err := repo.Create(ctx, auditdomain.Record{
		RequestID: "degrade-hard", ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build", AccountID: &accountID, AccountName: "noisy",
		EgressNodeName: "edge-1", StatusCode: 200, Streaming: true, OutputTokens: 2000, FirstTokenMS: &first, DurationMS: 1100, CreatedAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	NewHandler(auditapp.NewService(repo, slog.Default(), 8, 4, time.Second)).Register(router.Group("/api/admin/v1"))

	ok := httptest.NewRecorder()
	router.ServeHTTP(ok, httptest.NewRequest(http.MethodGet, "/api/admin/v1/request-audits/degrade-accounts?window=24h&softTPS=500&hardTPS=1000&failClosed=false&page=1&pageSize=20", nil))
	if ok.Code != http.StatusOK || !strings.Contains(ok.Body.String(), `"id":"11"`) || !strings.Contains(ok.Body.String(), `"hard":1`) ||
		!strings.Contains(ok.Body.String(), `"found":false`) || !strings.Contains(ok.Body.String(), `"deleted":1`) ||
		!strings.Contains(ok.Body.String(), `"accountPage":{"page":1,"pageSize":20,"total":1,"hasMore":false}`) {
		t.Fatalf("status=%d body=%s", ok.Code, ok.Body.String())
	}

	bad := httptest.NewRecorder()
	router.ServeHTTP(bad, httptest.NewRequest(http.MethodGet, "/api/admin/v1/request-audits/degrade-accounts?window=3h", nil))
	if bad.Code != http.StatusBadRequest || !strings.Contains(bad.Body.String(), "invalidAuditPeriod") {
		t.Fatalf("bad window status=%d body=%s", bad.Code, bad.Body.String())
	}
}
