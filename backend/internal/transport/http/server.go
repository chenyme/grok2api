package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	_ "github.com/chenyme/grok2api/backend/docs"
	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountsyncapp "github.com/chenyme/grok2api/backend/internal/application/accountsync"
	adminauthapp "github.com/chenyme/grok2api/backend/internal/application/adminauth"
	auditapp "github.com/chenyme/grok2api/backend/internal/application/audit"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	dashboardapp "github.com/chenyme/grok2api/backend/internal/application/dashboard"
	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	settingsapp "github.com/chenyme/grok2api/backend/internal/application/settings"
	accounthttp "github.com/chenyme/grok2api/backend/internal/transport/http/account"
	adminauthhttp "github.com/chenyme/grok2api/backend/internal/transport/http/adminauth"
	audithttp "github.com/chenyme/grok2api/backend/internal/transport/http/audit"
	clientkeyhttp "github.com/chenyme/grok2api/backend/internal/transport/http/clientkey"
	dashboardhttp "github.com/chenyme/grok2api/backend/internal/transport/http/dashboard"
	egresshttp "github.com/chenyme/grok2api/backend/internal/transport/http/egress"
	"github.com/chenyme/grok2api/backend/internal/transport/http/inference"
	mediahttp "github.com/chenyme/grok2api/backend/internal/transport/http/media"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	modelhttp "github.com/chenyme/grok2api/backend/internal/transport/http/model"
	settingshttp "github.com/chenyme/grok2api/backend/internal/transport/http/settings"
	systemhttp "github.com/chenyme/grok2api/backend/internal/transport/http/system"
	"github.com/gin-gonic/gin"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
)

type Dependencies struct {
	Logger             *slog.Logger
	RequestTimeout     time.Duration
	MaxBodyBytes       int64
	SecureCookies      bool
	SwaggerEnabled     bool
	PublicAPIBaseURL   string
	FrontendStaticPath string
	// Readiness 返回可观测的分层就绪状态。Ready 仅为旧调用方保留。
	Readiness    func(context.Context) ReadinessSnapshot
	Ready        func(context.Context) bool
	TrafficReady func() bool
	AdminAuth    *adminauthapp.Service
	Accounts     *accountapp.Service
	AccountSync  *accountsyncapp.Service
	Models       *modelapp.Service
	ClientKeys   *clientkeyapp.Service
	Audits       *auditapp.Service
	Dashboard    *dashboardapp.Service
	Gateway      *gateway.Service
	Media        *mediaapp.Service
	Settings     *settingsapp.Service
	Egress       *egressapp.Service
}

type ReadinessComponent struct {
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// ReadinessCredentialReport 表示可公开的启动凭据恢复统计。
type ReadinessCredentialReport struct {
	SchedulesBackfilled int `json:"schedulesBackfilled"`
	CriticalFound       int `json:"criticalFound"`
	Refreshed           int `json:"refreshed"`
	Failed              int `json:"failed"`
}

// ReadinessStartupReport 表示可公开的启动恢复统计，不包含内部错误文本。
type ReadinessStartupReport struct {
	StartedAt                time.Time                 `json:"startedAt"`
	CompletedAt              *time.Time                `json:"completedAt,omitempty"`
	Credentials              ReadinessCredentialReport `json:"credentials"`
	CooldownsRestored        int                       `json:"cooldownsRestored"`
	QuotaRecoveriesRestored  int                       `json:"quotaRecoveriesRestored"`
	DueWebQuotasQueued       int                       `json:"dueWebQuotasQueued"`
	StatsigKeysWarmed        int                       `json:"statsigKeysWarmed"`
	StaleWebQuotasFound      int                       `json:"staleWebQuotasFound"`
	StaleWebQuotasSynced     int                       `json:"staleWebQuotasSynced"`
	StaleModelCatalogsFound  int                       `json:"staleModelCatalogsFound"`
	StaleModelCatalogsSynced int                       `json:"staleModelCatalogsSynced"`
	ErrorCount               int                       `json:"errorCount"`
}

// ReadinessSnapshot 表示公开就绪端点的稳定响应契约。
type ReadinessSnapshot struct {
	Ready      bool                          `json:"ready"`
	State      string                        `json:"state"`
	UpdatedAt  time.Time                     `json:"updatedAt"`
	Components map[string]ReadinessComponent `json:"components,omitempty"`
	Startup    *ReadinessStartupReport       `json:"startup,omitempty"`
}

// New 创建完整 HTTP 路由并明确区分公共、管理员和客户端鉴权边界。
func New(deps Dependencies) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	router := gin.New()
	router.Use(gin.Recovery(), middleware.RequestID(), middleware.SecurityHeaders(), middleware.MaxBodyBytes(deps.MaxBodyBytes), middleware.Timeout(deps.RequestTimeout), middleware.AccessLog(deps.Logger))
	router.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	router.GET("/readyz", func(c *gin.Context) {
		if deps.Readiness != nil {
			snapshot := deps.Readiness(c.Request.Context())
			status := http.StatusServiceUnavailable
			if snapshot.Ready {
				status = http.StatusOK
			}
			c.JSON(status, snapshot)
			return
		}
		if deps.Ready != nil && deps.Ready(c.Request.Context()) {
			c.JSON(http.StatusOK, gin.H{"ready": true, "state": "ready"})
			return
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"ready": false, "state": "not_ready"})
	})
	if deps.SwaggerEnabled {
		router.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))
	}
	mediahttp.NewHandler(deps.Media).RegisterPublic(router)

	adminRoot := router.Group("/api/admin/v1")
	authHandler := adminauthhttp.NewHandler(deps.AdminAuth, deps.SecureCookies)
	authHandler.RegisterPublic(adminRoot)
	adminProtected := adminRoot.Group("")
	adminProtected.Use(middleware.AdminAuth(deps.AdminAuth))
	authHandler.RegisterAuthenticated(adminProtected)
	accounthttp.NewHandler(deps.Accounts, deps.AccountSync).Register(adminProtected)
	modelhttp.NewHandler(deps.Models).Register(adminProtected)
	clientkeyhttp.NewHandler(deps.ClientKeys).Register(adminProtected)
	audithttp.NewHandler(deps.Audits).Register(adminProtected)
	dashboardhttp.NewHandler(deps.Dashboard).Register(adminProtected)
	settingshttp.NewHandler(deps.Settings).Register(adminProtected)
	egresshttp.NewHandler(deps.Egress).Register(adminProtected)
	systemhttp.NewHandler(deps.PublicAPIBaseURL).Register(adminProtected)

	v1 := router.Group("/v1")
	if deps.TrafficReady != nil {
		v1.Use(func(c *gin.Context) {
			if deps.TrafficReady() {
				c.Next()
				return
			}
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{
				"code": "service_reconciling", "message": "服务正在完成启动恢复，请稍后重试", "param": nil, "type": "server_error",
			}})
		})
	}
	v1.Use(middleware.ClientAuth(deps.ClientKeys))
	inference.NewHandler(deps.Gateway, deps.Models, deps.MaxBodyBytes).Register(v1)
	registerFrontend(router, deps.FrontendStaticPath)
	return router
}
