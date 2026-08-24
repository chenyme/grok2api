package adminauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	adminapp "github.com/chenyme/grok2api/backend/internal/application/adminauth"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/transport/http/adminsession"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

func TestSessionCookiesAuthenticateRefreshAndProtectedRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}

	service := adminapp.NewService(
		relational.NewAdminRepository(database),
		relational.NewAdminSessionRepository(database),
		security.NewTokenService("12345678901234567890123456789012"),
		15*time.Minute,
		30*24*time.Hour,
	)
	if err := service.Bootstrap(ctx, "admin", "password123"); err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	handler := NewHandler(service, true)
	root := router.Group("/api/admin/v1")
	handler.RegisterPublic(root)
	protected := root.Group("")
	protected.Use(middleware.AdminAuth(service))
	handler.RegisterAuthenticated(protected)

	login := httptest.NewRecorder()
	loginRequest := httptest.NewRequest(http.MethodPost, "/api/admin/v1/auth/login", strings.NewReader(`{"username":"admin","password":"password123"}`))
	loginRequest.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(login, loginRequest)
	if login.Code != http.StatusOK {
		t.Fatalf("login status = %d, body = %s", login.Code, login.Body.String())
	}
	if strings.Contains(login.Body.String(), `"refreshToken":`) {
		t.Fatalf("login response exposed refresh token: %s", login.Body.String())
	}
	cookies := login.Result().Cookies()
	accessCookie := cookieNamed(cookies, adminsession.AccessCookieName)
	refreshCookie := cookieNamed(cookies, adminsession.RefreshCookieName)
	if accessCookie == nil || accessCookie.Path != adminsession.AccessCookiePath || !accessCookie.HttpOnly || !accessCookie.Secure || accessCookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("unexpected access cookie: %#v", accessCookie)
	}
	if refreshCookie == nil || refreshCookie.Path != adminsession.RefreshCookiePath || !refreshCookie.HttpOnly || !refreshCookie.Secure || refreshCookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("unexpected refresh cookie: %#v", refreshCookie)
	}

	me := httptest.NewRecorder()
	meRequest := httptest.NewRequest(http.MethodGet, "/api/admin/v1/me", nil)
	meRequest.AddCookie(accessCookie)
	router.ServeHTTP(me, meRequest)
	if me.Code != http.StatusOK {
		t.Fatalf("cookie-authenticated me status = %d, body = %s", me.Code, me.Body.String())
	}

	refresh := httptest.NewRecorder()
	refreshRequest := httptest.NewRequest(http.MethodPost, "/api/admin/v1/auth/refresh", strings.NewReader(`{}`))
	refreshRequest.Header.Set("Content-Type", "application/json")
	refreshRequest.AddCookie(refreshCookie)
	router.ServeHTTP(refresh, refreshRequest)
	if refresh.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, body = %s", refresh.Code, refresh.Body.String())
	}
	if strings.Contains(refresh.Body.String(), `"refreshToken":`) {
		t.Fatalf("refresh response exposed refresh token: %s", refresh.Body.String())
	}
	rotatedAccessCookie := cookieNamed(refresh.Result().Cookies(), adminsession.AccessCookieName)
	rotatedRefreshCookie := cookieNamed(refresh.Result().Cookies(), adminsession.RefreshCookieName)
	if rotatedAccessCookie == nil || rotatedRefreshCookie == nil {
		t.Fatalf("refresh did not rotate both session cookies: %#v", refresh.Result().Cookies())
	}

	logout := httptest.NewRecorder()
	logoutRequest := httptest.NewRequest(http.MethodPost, "/api/admin/v1/auth/logout", strings.NewReader(`{}`))
	logoutRequest.Header.Set("Content-Type", "application/json")
	logoutRequest.AddCookie(rotatedRefreshCookie)
	router.ServeHTTP(logout, logoutRequest)
	if logout.Code != http.StatusOK {
		t.Fatalf("logout status = %d, body = %s", logout.Code, logout.Body.String())
	}
	clearedAccessCookie := cookieNamed(logout.Result().Cookies(), adminsession.AccessCookieName)
	clearedRefreshCookie := cookieNamed(logout.Result().Cookies(), adminsession.RefreshCookieName)
	if clearedAccessCookie == nil || clearedAccessCookie.MaxAge >= 0 || clearedRefreshCookie == nil || clearedRefreshCookie.MaxAge >= 0 {
		t.Fatalf("logout did not clear both session cookies: %#v", logout.Result().Cookies())
	}

	revoked := httptest.NewRecorder()
	revokedRequest := httptest.NewRequest(http.MethodGet, "/api/admin/v1/me", nil)
	revokedRequest.AddCookie(rotatedAccessCookie)
	router.ServeHTTP(revoked, revokedRequest)
	if revoked.Code != http.StatusUnauthorized {
		t.Fatalf("revoked access cookie status = %d, want %d", revoked.Code, http.StatusUnauthorized)
	}
}

func cookieNamed(cookies []*http.Cookie, name string) *http.Cookie {
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}
