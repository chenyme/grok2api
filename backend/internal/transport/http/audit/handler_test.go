package audit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
