package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
