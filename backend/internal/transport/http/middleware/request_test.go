package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
	"github.com/gin-gonic/gin"
)

func TestValidRequestID(t *testing.T) {
	valid := []string{"req_123", "550e8400-e29b-41d4-a716-446655440000", "trace:span.1"}
	for _, value := range valid {
		if !validRequestID(value) {
			t.Fatalf("request ID %q should be valid", value)
		}
	}
	invalid := []string{"", "contains space", "含中文", string(make([]byte, maxRequestIDLength+1))}
	for _, value := range invalid {
		if validRequestID(value) {
			t.Fatalf("request ID %q should be invalid", value)
		}
	}
}

func TestMaxBodyBytesLimitsAllRequestBodies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(MaxBodyBytes(4))
	router.POST("/", func(c *gin.Context) {
		_, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.Status(http.StatusRequestEntityTooLarge)
			return
		}
		c.Status(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("12345"))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestObserveBodyMemoryRecordsActualAndDeclaredBytes(t *testing.T) {
	registry := perfmetrics.NewRegistry()
	previous := perfmetrics.Default
	perfmetrics.Default = registry
	t.Cleanup(func() { perfmetrics.Default = previous })

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(ObserveBodyMemory())
	router.POST("/v1/responses", func(c *gin.Context) {
		if _, err := io.ReadAll(c.Request.Body); err != nil {
			c.Status(http.StatusBadRequest)
			return
		}
		c.Status(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("12345"))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d", response.Code)
	}

	samples := registry.CollectAndReset()
	assertBodyMetric(t, samples, "http_request_body_total", "responses", "known_length", 1, 1, 1)
	assertBodyMetric(t, samples, "http_request_body_declared_bytes", "responses", "known_length", 1, 5, 5)
	assertBodyMetric(t, samples, "http_request_body_read_bytes", "responses", "known_length", 1, 5, 5)
	assertBodyMetric(t, samples, "http_request_body_active_peak_bytes", "responses", "known_length", 1, 5, 5)
	assertBodyGauge(t, samples, "http_request_body_active_bytes", 0)
}

func TestObserveBodyMemoryMeasuresConcurrentRetention(t *testing.T) {
	registry := perfmetrics.NewRegistry()
	previous := perfmetrics.Default
	perfmetrics.Default = registry
	t.Cleanup(func() { perfmetrics.Default = previous })

	gin.SetMode(gin.TestMode)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	router := gin.New()
	router.Use(newBodyMemoryObserver().Middleware())
	router.POST("/v1/messages", func(c *gin.Context) {
		_, _ = io.ReadAll(c.Request.Body)
		started <- struct{}{}
		<-release
		c.Status(http.StatusNoContent)
	})

	done := make(chan struct{}, 2)
	for range 2 {
		go func() {
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("1234"))
			router.ServeHTTP(response, request)
			if response.Code != http.StatusNoContent {
				t.Errorf("status = %d", response.Code)
			}
			done <- struct{}{}
		}()
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("request did not retain its body")
		}
	}
	activeSamples := registry.CollectAndReset()
	assertBodyGauge(t, activeSamples, "http_request_body_active_bytes", 8)
	close(release)
	for range 2 {
		<-done
	}

	samples := registry.CollectAndReset()
	assertBodyMetric(t, samples, "http_request_body_active_peak_bytes", "messages", "known_length", 2, 12, 8)
	assertBodyGauge(t, samples, "http_request_body_active_bytes", 0)
}

func TestObserveBodyMemoryReleasesPressureAfterRecoveredPanic(t *testing.T) {
	registry := perfmetrics.NewRegistry()
	previous := perfmetrics.Default
	perfmetrics.Default = registry
	t.Cleanup(func() { perfmetrics.Default = previous })

	gin.SetMode(gin.TestMode)
	observer := newBodyMemoryObserver()
	router := gin.New()
	router.Use(gin.Recovery(), observer.Middleware())
	router.POST("/v1/responses", func(c *gin.Context) {
		_, _ = io.ReadAll(c.Request.Body)
		panic("boom")
	})
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("12345"))
	router.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", response.Code)
	}
	if current := observer.active.Load(); current != 0 {
		t.Fatalf("active body bytes after recovered panic = %d", current)
	}

	samples := registry.CollectAndReset()
	assertBodyMetric(t, samples, "http_request_body_total", "responses", "known_length", 1, 1, 1)
	assertBodyMetric(t, samples, "http_request_body_read_bytes", "responses", "known_length", 1, 5, 5)
	assertBodyGauge(t, samples, "http_request_body_active_bytes", 0)
}

func assertBodyMetric(t *testing.T, samples []perfmetrics.Sample, name, operation, stage string, count uint64, total, maximum int64) {
	t.Helper()
	for _, sample := range samples {
		if sample.Name == name && sample.Labels.Operation == operation && sample.Labels.Stage == stage {
			if sample.Count != count || sample.Total != total || sample.Maximum != maximum {
				t.Fatalf("%s sample = %#v", name, sample)
			}
			return
		}
	}
	t.Fatalf("metric %s/%s/%s not found: %#v", name, operation, stage, samples)
}

func assertBodyGauge(t *testing.T, samples []perfmetrics.Sample, name string, value int64) {
	t.Helper()
	for _, sample := range samples {
		if sample.Name == name {
			if !sample.HasGauge || sample.Gauge != value {
				t.Fatalf("%s gauge = %#v", name, sample)
			}
			return
		}
	}
	t.Fatalf("gauge %s not found: %#v", name, samples)
}

func BenchmarkRequestBodyObservation(b *testing.B) {
	payload := strings.Repeat("x", 4<<10)
	for _, observed := range []bool{false, true} {
		name := "plain"
		if observed {
			name = "observed"
		}
		b.Run(name, func(b *testing.B) {
			gin.SetMode(gin.TestMode)
			router := gin.New()
			if observed {
				router.Use(ObserveBodyMemory())
			}
			router.POST("/v1/responses", func(c *gin.Context) {
				_, _ = io.Copy(io.Discard, c.Request.Body)
				c.Status(http.StatusNoContent)
			})
			b.ReportAllocs()
			b.RunParallel(func(worker *testing.PB) {
				for worker.Next() {
					request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(payload))
					router.ServeHTTP(httptest.NewRecorder(), request)
				}
			})
		})
	}
}

func TestSecurityHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(SecurityHeaders())
	router.GET("/", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	for name, expected := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
		"Permissions-Policy":     "camera=(), microphone=(), geolocation=()",
	} {
		if value := response.Header().Get(name); value != expected {
			t.Fatalf("%s = %q", name, value)
		}
	}
}
