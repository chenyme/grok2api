package system

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	updatecheckapp "github.com/chenyme/grok2api/backend/internal/application/updatecheck"
	"github.com/gin-gonic/gin"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestHandlerReturnsOnlyPublicFrontendConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewHandler(func() string { return "https://api.example.com/" }, updatecheckapp.NewService("v3.0.0", nil)).Register(router.Group("/api/admin/v1"))
	request := httptest.NewRequest(http.MethodGet, "/api/admin/v1/system", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var payload struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Data) != 1 || payload.Data["publicApiBaseURL"] != "https://api.example.com" {
		t.Fatalf("data = %#v", payload.Data)
	}
}

func TestHandlerReturnsAndChecksVersion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"tag_name":"v3.0.1","body":"Notes"}`))}, nil
	})}
	router := gin.New()
	updates := updatecheckapp.NewService("v3.0.0", client)
	NewHandler(nil, updates).Register(router.Group("/api/admin/v1"))

	for _, test := range []struct {
		method string
		path   string
		status string
	}{
		{method: http.MethodGet, path: "/api/admin/v1/system/version", status: "unchecked"},
		{method: http.MethodPost, path: "/api/admin/v1/system/update/check", status: "update_available"},
	} {
		request := httptest.NewRequest(test.method, test.path, nil)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s %s status = %d", test.method, test.path, recorder.Code)
		}
		var payload struct {
			Data struct {
				CurrentVersion string `json:"currentVersion"`
				Status         string `json:"status"`
			} `json:"data"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Data.CurrentVersion != "v3.0.0" || payload.Data.Status != test.status {
			t.Fatalf("%s %s data = %#v", test.method, test.path, payload.Data)
		}
	}
}
