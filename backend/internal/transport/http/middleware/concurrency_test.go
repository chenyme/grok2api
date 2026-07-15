package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestConcurrencyGateHotReloadsAndReleasesCapacity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	started := make(chan struct{})
	release := make(chan struct{})
	router := gin.New()
	gate := NewConcurrencyGate(1)
	router.Use(gate.Middleware())
	router.GET("/v1/responses", func(c *gin.Context) {
		if c.Query("block") == "true" {
			close(started)
			<-release
		}
		c.Status(http.StatusOK)
	})

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/responses?block=true", nil))
		if recorder.Code != http.StatusOK {
			t.Errorf("first status = %d", recorder.Code)
		}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first request did not acquire capacity")
	}

	overflow := httptest.NewRecorder()
	router.ServeHTTP(overflow, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
	if overflow.Code != http.StatusServiceUnavailable || overflow.Header().Get("Retry-After") != "1" {
		t.Fatalf("overflow status = %d, retry-after = %q", overflow.Code, overflow.Header().Get("Retry-After"))
	}

	gate.UpdateLimit(2)
	expanded := httptest.NewRecorder()
	router.ServeHTTP(expanded, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
	if expanded.Code != http.StatusOK {
		t.Fatalf("expanded status = %d", expanded.Code)
	}
	gate.UpdateLimit(1)
	reduced := httptest.NewRecorder()
	router.ServeHTTP(reduced, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
	if reduced.Code != http.StatusServiceUnavailable {
		t.Fatalf("reduced status = %d", reduced.Code)
	}
	close(release)
	<-firstDone

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("released status = %d", recorder.Code)
	}
}
