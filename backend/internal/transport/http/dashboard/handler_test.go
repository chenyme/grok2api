package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	dashboardapp "github.com/chenyme/grok2api/backend/internal/application/dashboard"
	dashboarddomain "github.com/chenyme/grok2api/backend/internal/domain/dashboard"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/gin-gonic/gin"
)

func TestHandlerReturnsDashboardContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := dashboardapp.NewService(&dashboardRepositoryStub{aggregate: dashboarddomain.Aggregate{
		Resources:       dashboarddomain.Resources{ActiveAccounts: 2, TotalAccounts: 3, BuildAccounts: 1, WebAccounts: 1, ConsoleAccounts: 1, EnabledModels: 4, TotalModels: 5, ActiveClientKeys: 6, TotalClientKeys: 7},
		Usage:           dashboarddomain.Usage{Requests: 10, SuccessfulRequests: 9, FailedRequests: 1, Tokens: 1200},
		Buckets:         []dashboarddomain.Bucket{{Index: 0, Requests: 10, Tokens: 1200}},
		ActivityBuckets: []dashboarddomain.ActivityBucket{{Index: 179, Requests: 10}},
		Providers:       []dashboarddomain.ProviderUsage{{Provider: "grok_build", Requests: 10, SuccessfulRequests: 9, Tokens: 1200}},
	}})
	router := gin.New()
	NewHandler(service).Register(router.Group("/api/admin/v1"))

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/admin/v1/dashboard?period=7d", nil)
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var envelope struct {
		Data struct {
			Period string `json:"period"`
			Usage  struct {
				Requests    int64   `json:"requests"`
				SuccessRate float64 `json:"successRate"`
			} `json:"usage"`
			Series    []seriesDTO        `json:"series"`
			Activity  []activityDTO      `json:"activity"`
			Providers []providerUsageDTO `json:"providers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.Period != "7d" || envelope.Data.Usage.Requests != 10 || envelope.Data.Usage.SuccessRate != 90 || len(envelope.Data.Series) != 7 || len(envelope.Data.Activity) != 180 || len(envelope.Data.Providers) != 1 {
		t.Fatalf("response = %#v", envelope.Data)
	}
}

func TestHandlerRejectsInvalidPeriod(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewHandler(dashboardapp.NewService(&dashboardRepositoryStub{})).Register(router.Group("/api/admin/v1"))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/v1/dashboard?period=365d", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestHandlerRejectsInvalidTimezone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewHandler(dashboardapp.NewService(&dashboardRepositoryStub{})).Register(router.Group("/api/admin/v1"))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/v1/dashboard?period=24h&timezone=invalid", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

type dashboardRepositoryStub struct {
	aggregate dashboarddomain.Aggregate
}

func (s *dashboardRepositoryStub) Snapshot(context.Context, repository.DashboardSnapshotWindow, time.Time) (dashboarddomain.Aggregate, error) {
	return s.aggregate, nil
}
