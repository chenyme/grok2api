package httpserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadinessEndpointReturnsStructuredDegradedStateAsReady(t *testing.T) {
	router := New(Dependencies{
		RequestTimeout: time.Second,
		MaxBodyBytes:   1024,
		Readiness: func(context.Context) ReadinessSnapshot {
			return ReadinessSnapshot{
				Ready: true, State: "degraded", UpdatedAt: time.Now().UTC(),
				Components: map[string]ReadinessComponent{
					"grok_build": {State: "ready"},
					"grok_web":   {State: "unavailable"},
				},
			}
		},
	})
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	var body ReadinessSnapshot
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Ready || body.State != "degraded" || body.Components["grok_build"].State != "ready" {
		t.Fatalf("body = %#v", body)
	}
}

func TestReadinessEndpointReturns503WhileReconciling(t *testing.T) {
	router := New(Dependencies{RequestTimeout: time.Second, MaxBodyBytes: 1024, Readiness: func(context.Context) ReadinessSnapshot {
		return ReadinessSnapshot{Ready: false, State: "reconciling", UpdatedAt: time.Now().UTC()}
	}})
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), `"state":"reconciling"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestInferenceTrafficIsRejectedWhileReconciling(t *testing.T) {
	router := New(Dependencies{RequestTimeout: time.Second, MaxBodyBytes: 1024, TrafficReady: func() bool { return false }})
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), `"code":"service_reconciling"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestSystemInfoRequiresAdminAuthentication(t *testing.T) {
	router := New(Dependencies{RequestTimeout: time.Second, MaxBodyBytes: 1024, PublicAPIBaseURL: "https://api.example.com"})
	request := httptest.NewRequest(http.MethodGet, "/api/admin/v1/system", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
}

func TestFrontendStaticFilesAndSPAFallback(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<html>app</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "assets", "app.js"), []byte("console.log('app')"), 0o600); err != nil {
		t.Fatal(err)
	}
	router := New(Dependencies{Logger: slog.Default(), RequestTimeout: time.Second, MaxBodyBytes: 1024, FrontendStaticPath: root})

	for _, test := range []struct {
		path        string
		status      int
		body        string
		cachePrefix string
	}{
		{path: "/assets/app.js", status: http.StatusOK, body: "console.log('app')", cachePrefix: "public"},
		{path: "/dashboard", status: http.StatusOK, body: "<html>app</html>", cachePrefix: "no-cache"},
		{path: "/assets/missing.js", status: http.StatusNotFound},
		{path: "/api/admin/v1/missing", status: http.StatusNotFound},
		{path: "/swagger/index.html", status: http.StatusNotFound},
	} {
		t.Run(test.path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d", recorder.Code, test.status)
			}
			if test.body != "" && !strings.Contains(recorder.Body.String(), test.body) {
				t.Fatalf("body = %q", recorder.Body.String())
			}
			if test.cachePrefix != "" && !strings.HasPrefix(recorder.Header().Get("Cache-Control"), test.cachePrefix) {
				t.Fatalf("cache-control = %q", recorder.Header().Get("Cache-Control"))
			}
		})
	}
}

func TestSwaggerRegistrationFollowsStartupConfig(t *testing.T) {
	disabled := New(Dependencies{Logger: slog.Default(), RequestTimeout: time.Second, MaxBodyBytes: 1024})
	disabledRequest := httptest.NewRequest(http.MethodGet, "/swagger/doc.json", nil)
	disabledRecorder := httptest.NewRecorder()
	disabled.ServeHTTP(disabledRecorder, disabledRequest)
	if disabledRecorder.Code != http.StatusNotFound {
		t.Fatalf("disabled swagger status = %d, want %d", disabledRecorder.Code, http.StatusNotFound)
	}

	enabled := New(Dependencies{Logger: slog.Default(), RequestTimeout: time.Second, MaxBodyBytes: 1024, SwaggerEnabled: true})
	enabledRequest := httptest.NewRequest(http.MethodGet, "/swagger/doc.json", nil)
	enabledRecorder := httptest.NewRecorder()
	enabled.ServeHTTP(enabledRecorder, enabledRequest)
	if enabledRecorder.Code != http.StatusOK {
		t.Fatalf("enabled swagger status = %d, want %d", enabledRecorder.Code, http.StatusOK)
	}
	var document struct {
		Info struct {
			Title string `json:"title"`
		} `json:"info"`
	}
	if err := json.Unmarshal(enabledRecorder.Body.Bytes(), &document); err != nil {
		t.Fatalf("decode swagger document: %v", err)
	}
	if document.Info.Title != "Grok2API" {
		t.Fatalf("swagger title = %q, want %q", document.Info.Title, "Grok2API")
	}
}
