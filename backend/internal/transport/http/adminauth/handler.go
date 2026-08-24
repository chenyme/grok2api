package adminauth

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	adminapp "github.com/chenyme/grok2api/backend/internal/application/adminauth"
	admindomain "github.com/chenyme/grok2api/backend/internal/domain/admin"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/chenyme/grok2api/backend/internal/transport/http/adminsession"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	service       *adminapp.Service
	secureCookies bool
}

func NewHandler(service *adminapp.Service, secureCookies bool) *Handler {
	return &Handler{service: service, secureCookies: secureCookies}
}

func (h *Handler) RegisterPublic(router *gin.RouterGroup) {
	router.POST("/auth/login", h.login)
	router.POST("/auth/refresh", h.refresh)
	router.POST("/auth/logout", h.logout)
}

func (h *Handler) RegisterAuthenticated(router *gin.RouterGroup) {
	router.GET("/me", h.me)
	router.PUT("/me/password", h.changePassword)
}

type loginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

type refreshRequest struct {
	RefreshToken string `json:"refreshToken"`
}

type changePasswordRequest struct {
	CurrentPassword string `json:"currentPassword" binding:"required"`
	NewPassword     string `json:"newPassword" binding:"required"`
}

type tokenResponse struct {
	AccessToken           string `json:"accessToken"`
	AccessTokenExpiresAt  string `json:"accessTokenExpiresAt"`
	RefreshTokenExpiresAt string `json:"refreshTokenExpiresAt"`
}

type adminResponse struct {
	ID       uint64 `json:"id,string"`
	Username string `json:"username"`
}

func (h *Handler) login(c *gin.Context) {
	var request loginRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	adminValue, tokens, err := h.service.Login(c.Request.Context(), request.Username, request.Password, remoteAddress(c.Request))
	if err != nil {
		if errors.Is(err, adminapp.ErrLoginRateLimited) {
			response.Error(c, http.StatusTooManyRequests, "loginRateLimited", "登录尝试过于频繁，请稍后重试")
			return
		}
		if errors.Is(err, adminapp.ErrRuntimeUnavailable) {
			response.Error(c, http.StatusServiceUnavailable, "authRuntimeUnavailable", "管理员认证服务暂不可用")
			return
		}
		response.Error(c, http.StatusUnauthorized, "invalidCredentials", "管理员账号或密码错误")
		return
	}
	h.setSessionCookies(c, tokens)
	response.Success(c, http.StatusOK, gin.H{"admin": newAdminResponse(adminValue), "tokens": newTokenResponse(tokens)})
}

func remoteAddress(request *http.Request) string {
	value := strings.TrimSpace(request.RemoteAddr)
	host, _, err := net.SplitHostPort(value)
	if err == nil && host != "" {
		return host
	}
	return value
}

func (h *Handler) refresh(c *gin.Context) {
	var request refreshRequest
	if err := c.ShouldBindJSON(&request); err != nil && !errors.Is(err, io.EOF) {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	if request.RefreshToken == "" {
		request.RefreshToken, _ = c.Cookie(adminsession.RefreshCookieName)
	}
	if request.RefreshToken == "" {
		response.Error(c, http.StatusUnauthorized, "invalidRefreshToken", "刷新会话无效")
		return
	}
	tokens, err := h.service.Refresh(c.Request.Context(), request.RefreshToken)
	if err != nil {
		if errors.Is(err, adminapp.ErrRuntimeUnavailable) {
			response.Error(c, http.StatusServiceUnavailable, "authRuntimeUnavailable", "管理员认证服务暂不可用")
			return
		}
		response.Error(c, http.StatusUnauthorized, "invalidRefreshToken", "刷新会话无效")
		return
	}
	h.setSessionCookies(c, tokens)
	response.Success(c, http.StatusOK, newTokenResponse(tokens))
}

func (h *Handler) logout(c *gin.Context) {
	var request refreshRequest
	if err := c.ShouldBindJSON(&request); err != nil && !errors.Is(err, io.EOF) {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	if request.RefreshToken == "" {
		request.RefreshToken, _ = c.Cookie(adminsession.RefreshCookieName)
	}
	if err := h.service.Logout(c.Request.Context(), request.RefreshToken); err != nil {
		response.Error(c, http.StatusServiceUnavailable, "authRuntimeUnavailable", "管理员认证服务暂不可用")
		return
	}
	h.clearSessionCookies(c)
	response.Success(c, http.StatusOK, gin.H{"loggedOut": true})
}

func (h *Handler) me(c *gin.Context) {
	value, ok := c.Get(middleware.AdminKey)
	adminValue, valid := value.(admindomain.Admin)
	if !ok || !valid {
		response.Error(c, http.StatusUnauthorized, "adminUnauthorized", "管理员登录已失效")
		return
	}
	response.Success(c, http.StatusOK, newAdminResponse(adminValue))
}

func (h *Handler) changePassword(c *gin.Context) {
	var request changePasswordRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	value, ok := c.Get(middleware.AdminKey)
	adminValue, valid := value.(admindomain.Admin)
	if !ok || !valid {
		response.Error(c, http.StatusUnauthorized, "adminUnauthorized", "管理员登录已失效")
		return
	}
	if err := h.service.ChangePassword(c.Request.Context(), adminValue.ID, request.CurrentPassword, request.NewPassword); err != nil {
		if errors.Is(err, adminapp.ErrInvalidCredentials) || errors.Is(err, adminapp.ErrInvalidPassword) {
			response.Error(c, http.StatusBadRequest, "passwordChangeFailed", err.Error())
			return
		}
		response.Error(c, http.StatusInternalServerError, "passwordChangeFailed", "修改管理员密码失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"passwordChanged": true})
}

func newAdminResponse(value admindomain.Admin) adminResponse {
	return adminResponse{ID: value.ID, Username: value.Username}
}

func newTokenResponse(value adminapp.Tokens) tokenResponse {
	return tokenResponse{AccessToken: value.AccessToken, AccessTokenExpiresAt: value.AccessTokenExpiresAt.Format(time.RFC3339), RefreshTokenExpiresAt: value.RefreshTokenExpiresAt.Format(time.RFC3339)}
}

func (h *Handler) setSessionCookies(c *gin.Context, value adminapp.Tokens) {
	secure := h.secureCookies || c.Request.TLS != nil
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(adminsession.AccessCookieName, value.AccessToken, cookieMaxAge(value.AccessTokenExpiresAt), adminsession.AccessCookiePath, "", secure, true)
	c.SetCookie(adminsession.RefreshCookieName, value.RefreshToken, cookieMaxAge(value.RefreshTokenExpiresAt), adminsession.RefreshCookiePath, "", secure, true)
}

func (h *Handler) clearSessionCookies(c *gin.Context) {
	secure := h.secureCookies || c.Request.TLS != nil
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(adminsession.AccessCookieName, "", -1, adminsession.AccessCookiePath, "", secure, true)
	c.SetCookie(adminsession.RefreshCookieName, "", -1, adminsession.RefreshCookiePath, "", secure, true)
}

func cookieMaxAge(expiresAt time.Time) int {
	maxAge := int(time.Until(expiresAt).Seconds())
	if maxAge < 0 {
		maxAge = 0
	}
	return maxAge
}
