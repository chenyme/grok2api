package middleware

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/transport/http/adminsession"
	"github.com/gin-gonic/gin"
)

func TestAdminAccessTokenPrefersBearerAndFallsBackToScopedCookie(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/admin/v1/models", nil)
	request.AddCookie(&http.Cookie{Name: adminsession.AccessCookieName, Value: "cookie-token"})

	if token, ok := adminAccessToken(request); !ok || token != "cookie-token" {
		t.Fatalf("cookie token = %q, ok = %v", token, ok)
	}

	request.Header.Set("Authorization", "Bearer header-token")
	if token, ok := adminAccessToken(request); !ok || token != "header-token" {
		t.Fatalf("header token = %q, ok = %v", token, ok)
	}

	request.Header.Set("Authorization", "Basic malformed")
	if token, ok := adminAccessToken(request); ok || token != "" {
		t.Fatalf("malformed explicit authorization fell back to cookie: token = %q, ok = %v", token, ok)
	}
}

func TestAdminAccessCookieRequiresSameOriginForUnsafeRequests(t *testing.T) {
	for _, test := range []struct {
		name      string
		method    string
		origin    string
		fetchSite string
		host      string
		ok        bool
	}{
		{name: "safe request", method: http.MethodGet, ok: true},
		{name: "cross site safe request", method: http.MethodGet, fetchSite: "cross-site", ok: false},
		{name: "same origin", method: http.MethodPost, origin: "https://admin.example.com", ok: true},
		{name: "same origin with port", method: http.MethodDelete, origin: "https://admin.example.com:8443", host: "admin.example.com:8443", ok: true},
		{name: "same origin metadata behind rewritten host", method: http.MethodPost, fetchSite: "same-origin", host: "backend:8000", ok: true},
		{name: "cross site metadata wins", method: http.MethodPost, origin: "https://admin.example.com", fetchSite: "cross-site", ok: false},
		{name: "missing origin", method: http.MethodPost, ok: false},
		{name: "cross origin", method: http.MethodPost, origin: "https://attacker.example.com", ok: false},
		{name: "opaque origin", method: http.MethodPut, origin: "null", ok: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, "https://admin.example.com/api/admin/v1/models", nil)
			if test.host != "" {
				request.Host = test.host
			}
			request.Header.Set("Origin", test.origin)
			request.Header.Set("Sec-Fetch-Site", test.fetchSite)
			request.AddCookie(&http.Cookie{Name: adminsession.AccessCookieName, Value: "cookie-token"})
			_, ok := adminAccessToken(request)
			if ok != test.ok {
				t.Fatalf("ok = %v, want %v", ok, test.ok)
			}
		})
	}
}

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
