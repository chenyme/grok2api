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
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
)

func testDependencies() Dependencies {
	return Dependencies{RequestTimeout: time.Second, MaxBodyBytes: 1024, ConcurrencyGate: middleware.NewConcurrencyGate(1024)}
}

func TestReadinessEndpointReturnsStructuredDegradedStateAsReady(t *testing.T) {
	deps := testDependencies()
	deps.Readiness = func(context.Context) ReadinessSnapshot {
		return ReadinessSnapshot{
			Ready: true, State: "degraded", UpdatedAt: time.Now().UTC(),
			Components: map[string]ReadinessComponent{
				"grok_build": {State: "ready"},
				"grok_web":   {State: "unavailable"},
			},
		}
	}
	router := New(deps)
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
	deps := testDependencies()
	deps.Readiness = func(context.Context) ReadinessSnapshot {
		return ReadinessSnapshot{Ready: false, State: "reconciling", UpdatedAt: time.Now().UTC()}
	}
	router := New(deps)
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), `"state":"reconciling"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestInferenceTrafficIsRejectedWhileReconciling(t *testing.T) {
	deps := testDependencies()
	deps.TrafficReady = func() bool { return false }
	router := New(deps)
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set(middleware.UpstreamRequestDispositionHeader, "not-submitted")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), `"code":"service_reconciling"`) || recorder.Header().Get(middleware.UpstreamRequestDispositionHeader) != "" {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestImageTrafficReadyGateProvesNotSubmittedBeforeAuth(t *testing.T) {
	for _, path := range []string{"/v1/images/generations", "/v1/images/edits"} {
		t.Run(path, func(t *testing.T) {
			deps := testDependencies()
			deps.TrafficReady = func() bool { return false }
			router := New(deps)
			request := httptest.NewRequest(http.MethodPost, path, nil)
			request.Header.Set("Authorization", "Bearer invalid-before-auth")
			request.Header.Set(middleware.UpstreamRequestDispositionHeader, "submitted")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			assertImageNotSubmitted(t, recorder)
		})
	}
}

func TestImageConcurrencyGateProvesNotSubmittedBeforeReadinessAndAuth(t *testing.T) {
	for _, path := range []string{"/v1/images/generations", "/v1/images/edits"} {
		t.Run(path, func(t *testing.T) {
			deps := testDependencies()
			deps.ConcurrencyGate = middleware.NewConcurrencyGate(1)
			entered := make(chan struct{})
			release := make(chan struct{})
			var once, releaseOnce sync.Once
			releaseGate := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(releaseGate)
			deps.TrafficReady = func() bool {
				once.Do(func() { close(entered) })
				<-release
				return false
			}
			router := New(deps)
			firstDone := make(chan int, 1)
			go func() {
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
				firstDone <- recorder.Code
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("first request did not enter readiness behind concurrency gate")
			}

			request := httptest.NewRequest(http.MethodPost, path, nil)
			request.Header.Set("Authorization", "Bearer invalid-before-auth")
			request.Header.Set(middleware.UpstreamRequestDispositionHeader, "submitted")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			assertImageNotSubmitted(t, recorder)
			if recorder.Header().Get("Retry-After") != "1" {
				t.Fatalf("retry-after = %q", recorder.Header().Get("Retry-After"))
			}

			nonImageRequest := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			nonImageRequest.Header.Set(middleware.UpstreamRequestDispositionHeader, "not-submitted")
			nonImage := httptest.NewRecorder()
			router.ServeHTTP(nonImage, nonImageRequest)
			if nonImage.Code != http.StatusServiceUnavailable || !strings.Contains(nonImage.Body.String(), `"code":"server_overloaded"`) || nonImage.Header().Get(middleware.UpstreamRequestDispositionHeader) != "" {
				t.Fatalf("non-image status=%d headers=%#v body=%s", nonImage.Code, nonImage.Header(), nonImage.Body.String())
			}

			releaseGate()
			if status := <-firstDone; status != http.StatusServiceUnavailable {
				t.Fatalf("first status = %d", status)
			}
		})
	}
}

func assertImageNotSubmitted(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get(middleware.UpstreamRequestDispositionHeader) != "not-submitted" || !strings.Contains(recorder.Body.String(), `"code":"upstream_not_submitted"`) {
		t.Fatalf("status=%d headers=%#v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
	}
}

func TestSystemEndpointsRequireAdminAuthentication(t *testing.T) {
	deps := testDependencies()
	deps.PublicAPIBaseURL = "https://api.example.com"
	router := New(deps)
	for _, route := range []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/api/admin/v1/system"},
		{method: http.MethodGet, path: "/api/admin/v1/system/version"},
		{method: http.MethodPost, path: "/api/admin/v1/system/update/check"},
		{method: http.MethodGet, path: "/api/admin/v1/client-keys/1/concurrency"},
		{method: http.MethodGet, path: "/api/admin/v1/client-keys/1/image-capacity-attestation?routeId=1"},
		{method: http.MethodPost, path: "/api/admin/v1/models/1/accounts"},
	} {
		request := httptest.NewRequest(route.method, route.path, nil)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status = %d, want %d", route.method, route.path, recorder.Code, http.StatusUnauthorized)
		}
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
	deps := testDependencies()
	deps.Logger = slog.Default()
	deps.FrontendStaticPath = root
	router := New(deps)

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
	disabledDeps := testDependencies()
	disabledDeps.Logger = slog.Default()
	disabled := New(disabledDeps)
	disabledRequest := httptest.NewRequest(http.MethodGet, "/swagger/doc.json", nil)
	disabledRecorder := httptest.NewRecorder()
	disabled.ServeHTTP(disabledRecorder, disabledRequest)
	if disabledRecorder.Code != http.StatusNotFound {
		t.Fatalf("disabled swagger status = %d, want %d", disabledRecorder.Code, http.StatusNotFound)
	}

	enabledDeps := testDependencies()
	enabledDeps.Logger = slog.Default()
	enabledDeps.SwaggerEnabled = true
	enabled := New(enabledDeps)
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
