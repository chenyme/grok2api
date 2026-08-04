package middleware

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/gin-gonic/gin"
)

func TestClientRuntimeStoreFailureUsesServiceUnavailable(t *testing.T) {
	err := errors.Join(clientkeyapp.ErrRuntimeUnavailable, errors.New("redis unavailable"))
	if status := clientErrorStatus(err); status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", status)
	}
	if code := clientErrorCode(err); code != "runtime_store_unavailable" {
		t.Fatalf("code = %q", code)
	}
	if message := clientErrorMessage(err); message == err.Error() {
		t.Fatal("runtime implementation detail leaked to client")
	}
}

func TestQualityGuardAuthIsScopedBearerToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(QualityGuardAuth("scoped-secret"))
	router.GET("/probe", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	for _, test := range []struct {
		header string
		status int
	}{
		{header: "Bearer scoped-secret", status: http.StatusNoContent},
		{header: "Bearer wrong-secret", status: http.StatusUnauthorized},
		{header: "", status: http.StatusUnauthorized},
	} {
		request := httptest.NewRequest(http.MethodGet, "/probe", nil)
		request.Header.Set("Authorization", test.header)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != test.status {
			t.Fatalf("header %q status = %d, want %d", test.header, response.Code, test.status)
		}
	}
}

func TestBearerTokenAcceptsCaseInsensitiveSchemeAndWhitespace(t *testing.T) {
	token, ok := bearerToken("  bearer\tsecret-token  ")
	if !ok || token != "secret-token" {
		t.Fatalf("token = %q, ok = %v", token, ok)
	}
	for _, value := range []string{"", "Bearer", "Basic token", "Bearer token extra"} {
		if _, ok := bearerToken(value); ok {
			t.Fatalf("header %q unexpectedly accepted", value)
		}
	}
}
