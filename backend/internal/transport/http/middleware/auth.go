package middleware

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/application/adminauth"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/chenyme/grok2api/backend/internal/transport/http/adminsession"
	"github.com/gin-gonic/gin"
)

const (
	AdminKey  = "admin"
	ClientKey = "clientKey"
)

// AdminAuth 校验管理员 access JWT。
func AdminAuth(service *adminauth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, ok := adminAccessToken(c.Request)
		if !ok {
			response.Error(c, http.StatusUnauthorized, "adminUnauthorized", "管理员登录已失效")
			return
		}
		value, err := service.AuthenticateAccess(c.Request.Context(), raw)
		if err != nil {
			if errors.Is(err, adminauth.ErrRuntimeUnavailable) {
				response.Error(c, http.StatusServiceUnavailable, "authRuntimeUnavailable", "管理员认证服务暂不可用")
				return
			}
			response.Error(c, http.StatusUnauthorized, "adminUnauthorized", "管理员登录已失效")
			return
		}
		c.Set(AdminKey, value)
		c.Next()
	}
}

// adminAccessToken prefers the explicit Bearer credential used by the SPA and
// API clients. The scoped HttpOnly cookie is a browser fallback for deployments
// whose reverse proxy drops Authorization; unsafe cookie-authenticated requests
// must still originate from the same host.
func adminAccessToken(request *http.Request) (string, bool) {
	header := strings.TrimSpace(request.Header.Get("Authorization"))
	if header != "" {
		return bearerToken(header)
	}
	cookie, err := request.Cookie(adminsession.AccessCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" || !adminCookieRequestAllowed(request) {
		return "", false
	}
	return strings.TrimSpace(cookie.Value), true
}

func adminCookieRequestAllowed(request *http.Request) bool {
	if fetchSite := strings.ToLower(strings.TrimSpace(request.Header.Get("Sec-Fetch-Site"))); fetchSite != "" {
		return fetchSite == "same-origin"
	}
	switch request.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		// Older browsers may omit Fetch Metadata on same-origin reads. SameSite=Strict,
		// host-only cookies and the browser same-origin policy remain the fallback.
		return true
	}
	origin := strings.TrimSpace(request.Header.Get("Origin"))
	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return false
	}
	return strings.EqualFold(parsed.Host, request.Host)
}

// QualityGuardAuth accepts only the process-scoped token shared with the
// quality-guard sidecar. It is intentionally separate from administrator JWTs.
func QualityGuardAuth(expected string) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, ok := bearerToken(c.GetHeader("Authorization"))
		if !ok || len(raw) != len(expected) || subtle.ConstantTimeCompare([]byte(raw), []byte(expected)) != 1 {
			response.Error(c, http.StatusUnauthorized, "qualityGuardUnauthorized", "质量守护内部认证失败")
			return
		}
		c.Next()
	}
}

// ClientAuth 校验下游 API Key，并在请求结束时释放并发租约。
func ClientAuth(service *clientkeyapp.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, ok := bearerToken(c.GetHeader("Authorization"))
		if !ok {
			raw = strings.TrimSpace(c.GetHeader("X-API-Key"))
		}
		value, release, err := service.Authenticate(c.Request.Context(), raw)
		if err != nil {
			writeOpenAIError(c, clientErrorStatus(err), clientErrorCode(err), clientErrorMessage(err))
			return
		}
		defer release()
		c.Set(ClientKey, value)
		c.Next()
	}
}

func bearerToken(header string) (string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	token := parts[1]
	return token, token != ""
}

func clientErrorStatus(err error) int {
	switch {
	case errors.Is(err, clientkeyapp.ErrRuntimeUnavailable):
		return http.StatusServiceUnavailable
	case errors.Is(err, clientkeyapp.ErrRateLimited), errors.Is(err, clientkeyapp.ErrConcurrencyLimit), errors.Is(err, clientkeyapp.ErrBillingLimit):
		return http.StatusTooManyRequests
	default:
		return http.StatusUnauthorized
	}
}

func clientErrorCode(err error) string {
	switch {
	case errors.Is(err, clientkeyapp.ErrRuntimeUnavailable):
		return "runtime_store_unavailable"
	case errors.Is(err, clientkeyapp.ErrRateLimited):
		return "rate_limit_exceeded"
	case errors.Is(err, clientkeyapp.ErrConcurrencyLimit):
		return "concurrency_limit_exceeded"
	case errors.Is(err, clientkeyapp.ErrBillingLimit):
		return "billing_limit_exceeded"
	default:
		return "invalid_api_key"
	}
}

func clientErrorMessage(err error) string {
	if errors.Is(err, clientkeyapp.ErrRuntimeUnavailable) {
		return "网关运行态暂不可用，请稍后重试"
	}
	return err.Error()
}

func writeOpenAIError(c *gin.Context, status int, code, message string) {
	if c.Request.URL.Path == "/v1/messages" {
		errorType := "authentication_error"
		if status == http.StatusTooManyRequests {
			errorType = "rate_limit_error"
		} else if status >= 500 {
			errorType = "api_error"
		}
		c.AbortWithStatusJSON(status, gin.H{"type": "error", "error": gin.H{"type": errorType, "message": message}})
		return
	}
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"message": message, "type": "invalid_request_error", "code": code, "param": nil}})
}
