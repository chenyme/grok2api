package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountsyncapp "github.com/chenyme/grok2api/backend/internal/application/accountsync"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type accountSynchronizer interface {
	Sync(ctx context.Context, accountIDs ...uint64) accountsyncapp.Result
	SyncStream(ctx context.Context, accountIDs <-chan uint64) accountsyncapp.Result
}

type accountSyncProgressor interface {
	SyncStreamObserved(ctx context.Context, accountIDs <-chan uint64, observer func(completed, total int)) accountsyncapp.Result
}

type accountModelSynchronizer interface {
	SyncModels(ctx context.Context, accountID uint64) error
}

const (
	maxAccountImportBytes         = 30 << 20
	maxAccountImportFiles         = 1000
	accountSyncQueueCapacity      = 20
	accountEventHeartbeatInterval = 15 * time.Second
	accountEventWriteTimeout      = 30 * time.Second
)

type Handler struct {
	service *accountapp.Service
	sync    accountSynchronizer
}

type accountSyncPipeline struct {
	ctx        context.Context
	cancel     context.CancelFunc
	ids        chan uint64
	done       chan accountsyncapp.Result
	progress   func(completed, total int)
	progressMu sync.Mutex
	queued     atomic.Int64
	completed  atomic.Int64
}

func NewHandler(service *accountapp.Service, sync accountSynchronizer) *Handler {
	return &Handler{service: service, sync: sync}
}

func (h *Handler) startSyncPipeline(parent context.Context, progress func(completed, total int)) *accountSyncPipeline {
	ctx, cancel := context.WithCancel(parent)
	pipeline := &accountSyncPipeline{ctx: ctx, cancel: cancel, progress: progress}
	if h.sync == nil {
		return pipeline
	}
	pipeline.ids = make(chan uint64, accountSyncQueueCapacity)
	pipeline.done = make(chan accountsyncapp.Result, 1)
	go func() {
		if observed, ok := h.sync.(accountSyncProgressor); ok && progress != nil {
			pipeline.done <- observed.SyncStreamObserved(ctx, pipeline.ids, func(completed, _ int) {
				pipeline.completed.Store(int64(completed))
				pipeline.reportProgress()
			})
			return
		}
		pipeline.done <- h.sync.SyncStream(ctx, pipeline.ids)
	}()
	return pipeline
}

func (p *accountSyncPipeline) Observe(accountID uint64) error {
	if p.ids == nil {
		return nil
	}
	p.queued.Add(1)
	select {
	case p.ids <- accountID:
		return nil
	case <-p.ctx.Done():
		p.queued.Add(-1)
		return p.ctx.Err()
	}
}

func (p *accountSyncPipeline) Finish(abort bool) accountsyncapp.Result {
	if abort {
		p.cancel()
	}
	if p.ids != nil {
		close(p.ids)
	}
	if !abort {
		// 转换阶段结束后不再增加同步任务，先报告一次固定分母，避免前端看到总数跳变。
		p.reportProgress()
	}
	result := accountsyncapp.Result{}
	if p.done != nil {
		result = <-p.done
	}
	if !abort && p.ids != nil {
		p.completed.Store(int64(result.Succeeded + result.Failed))
		p.reportProgress()
	}
	p.cancel()
	return result
}

// reportProgress 使用已进入同步流水线的任务数报告进度；转换结束后该分母保持固定。
func (p *accountSyncPipeline) reportProgress() {
	if p.ids == nil || p.progress == nil {
		return
	}
	p.progressMu.Lock()
	defer p.progressMu.Unlock()
	p.progress(int(p.completed.Load()), int(p.queued.Load()))
}

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/accounts", h.list)
	router.GET("/accounts/summary", h.summary)
	router.GET("/accounts/export", h.exportCredentials)
	router.GET("/accounts/:id", h.get)
	router.POST("/accounts/device/start", h.startDevice)
	router.POST("/accounts/device/:sessionId/poll", h.pollDevice)
	router.POST("/accounts/import", h.importAuth)
	router.POST("/accounts/web/import", h.importWebAuth)
	router.POST("/accounts/console/import", h.importConsoleAuth)
	router.POST("/accounts/web/convert-to-build", h.convertWebToBuild)
	router.POST("/accounts/web/sync-to-console", h.syncWebToConsole)
	router.POST("/accounts/web/refresh-quotas", h.refreshAllWebQuotas)
	router.POST("/accounts/console/refresh-quotas", h.refreshAllConsoleQuotas)
	router.POST("/accounts/refresh-billing", h.refreshAllBilling)
	router.POST("/accounts/refresh-tokens", h.refreshAllTokens)
	router.POST("/accounts/batch/refresh-billing", h.batchRefreshBilling)
	router.POST("/accounts/batch/refresh-quotas", h.batchRefreshQuotas)
	router.PATCH("/accounts/batch", h.batchUpdate)
	router.DELETE("/accounts", h.batchDelete)
	router.PATCH("/accounts/:id", h.update)
	router.DELETE("/accounts/:id", h.delete)
	router.POST("/accounts/:id/refresh-token", h.refreshToken)
	router.POST("/accounts/:id/refresh-billing", h.refreshBilling)
	router.POST("/accounts/:id/refresh-quota", h.refreshWebQuota)
}

type updateRequest struct {
	Name                   *string                       `json:"name"`
	Enabled                *bool                         `json:"enabled"`
	Priority               *int                          `json:"priority"`
	MaxConcurrent          *int                          `json:"maxConcurrent"`
	MinimumRemaining       *float64                      `json:"minimumRemaining"`
	CloudflareCookies      *string                       `json:"cloudflareCookies"`
	ClearCloudflareCookies bool                          `json:"clearCloudflareCookies"`
	BuildSuperEntitled     *bool                         `json:"buildSuperEntitled"`
	BuildRouteMode         *accountdomain.BuildRouteMode `json:"buildRouteMode"`
}

type batchUpdateRequest struct {
	IDs              []string `json:"ids" binding:"required"`
	Provider         string   `json:"provider" binding:"required"`
	Enabled          *bool    `json:"enabled"`
	Priority         *int     `json:"priority"`
	MaxConcurrent    *int     `json:"maxConcurrent"`
	MinimumRemaining *float64 `json:"minimumRemaining"`
}

type batchDeleteRequest struct {
	IDs      []string `json:"ids" binding:"required"`
	Provider string   `json:"provider" binding:"required"`
}

type buildConversionRequest struct {
	IDs      []string                           `json:"ids"`
	All      bool                               `json:"all"`
	Strategy accountapp.BuildConversionStrategy `json:"strategy"`
}

type webConsoleSyncRequest struct {
	IDs      []string                          `json:"ids"`
	All      bool                              `json:"all"`
	Strategy accountapp.WebConsoleSyncStrategy `json:"strategy"`
}

type buildConversionResponse struct {
	Created    int `json:"created"`
	Linked     int `json:"linked"`
	Skipped    int `json:"skipped"`
	Failed     int `json:"failed"`
	Synced     int `json:"synced"`
	SyncFailed int `json:"syncFailed"`
}

type accountTaskProgressResponse struct {
	Completed int    `json:"completed"`
	Total     int    `json:"total"`
	Phase     string `json:"phase,omitempty"`
}

type accountBatchResponse struct {
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
}

type accountTokenRefreshResponse struct {
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
}

type accountImportResponse struct {
	Created    int `json:"created"`
	Updated    int `json:"updated"`
	Skipped    int `json:"skipped"`
	Synced     int `json:"synced"`
	SyncFailed int `json:"syncFailed"`
}

type accountResponse struct {
	ID                         uint64                `json:"id,string"`
	Provider                   string                `json:"provider"`
	AuthType                   string                `json:"authType"`
	WebTier                    string                `json:"webTier,omitempty"`
	WebTierSyncedAt            *time.Time            `json:"webTierSyncedAt,omitempty"`
	Name                       string                `json:"name"`
	Email                      string                `json:"email,omitempty"`
	UserID                     string                `json:"userId,omitempty"`
	TeamID                     string                `json:"teamId,omitempty"`
	Enabled                    bool                  `json:"enabled"`
	AuthStatus                 string                `json:"authStatus"`
	ExpiresAt                  *time.Time            `json:"expiresAt,omitempty"`
	Refreshable                bool                  `json:"refreshable"`
	RefreshDueAt               *time.Time            `json:"refreshDueAt,omitempty"`
	LastRefreshAt              *time.Time            `json:"lastRefreshAt,omitempty"`
	RefreshFailures            int                   `json:"refreshFailureCount"`
	LastRefreshError           string                `json:"lastRefreshErrorCode,omitempty"`
	Priority                   int                   `json:"priority"`
	MaxConcurrent              int                   `json:"maxConcurrent"`
	MinimumRemaining           float64               `json:"minimumRemaining"`
	FailureCount               int                   `json:"failureCount"`
	CooldownUntil              *time.Time            `json:"cooldownUntil,omitempty"`
	LastError                  string                `json:"lastError,omitempty"`
	LastUsedAt                 *time.Time            `json:"lastUsedAt,omitempty"`
	LinkedAccountID            uint64                `json:"linkedAccountId,omitempty,string"`
	LinkedName                 string                `json:"linkedAccountName,omitempty"`
	LinkedProvider             string                `json:"linkedProvider,omitempty"`
	CreatedAt                  time.Time             `json:"createdAt"`
	ObservedModel              string                `json:"observedModel,omitempty"`
	ObservedModelAt            *time.Time            `json:"observedModelAt,omitempty"`
	CloudflareCookieConfigured bool                  `json:"cloudflareCookieConfigured"`
	BuildSuperEntitled         bool                  `json:"buildSuperEntitled"`
	BuildRouteMode             string                `json:"buildRouteMode"`
	BuildBotFlagged            bool                  `json:"buildBotFlagged"`
	ModelSyncFailed            bool                  `json:"modelSyncFailed,omitempty"`
	Billing                    *billingResponse      `json:"billing,omitempty"`
	Quota                      quotaResponse         `json:"quota"`
	QuotaWindows               []quotaWindowResponse `json:"quotaWindows,omitempty"`
}

type quotaWindowResponse struct {
	Mode          string                   `json:"mode"`
	Remaining     int                      `json:"remaining"`
	Total         int                      `json:"total"`
	UsagePercent  float64                  `json:"usagePercent"`
	Breakdown     []quotaBreakdownResponse `json:"breakdown,omitempty"`
	WindowSeconds int                      `json:"windowSeconds"`
	ResetAt       *time.Time               `json:"resetAt,omitempty"`
	SyncedAt      *time.Time               `json:"syncedAt,omitempty"`
	Source        string                   `json:"source"`
}

type quotaBreakdownResponse struct {
	ProductCode  int     `json:"productCode"`
	UsagePercent float64 `json:"usagePercent"`
}

type billingResponse struct {
	PlanCode             string                   `json:"planCode,omitempty"`
	PlanName             string                   `json:"planName,omitempty"`
	MonthlyLimit         float64                  `json:"monthlyLimit"`
	Used                 float64                  `json:"used"`
	Remaining            float64                  `json:"remaining"`
	OnDemandCap          float64                  `json:"onDemandCap"`
	OnDemandUsed         float64                  `json:"onDemandUsed"`
	PrepaidBalance       float64                  `json:"prepaidBalance"`
	CreditUsagePercent   float64                  `json:"creditUsagePercent"`
	IsUnifiedBillingUser bool                     `json:"isUnifiedBillingUser"`
	OnDemandEnabled      *bool                    `json:"onDemandEnabled,omitempty"`
	TopUpMethod          string                   `json:"topUpMethod,omitempty"`
	UsagePeriodType      string                   `json:"usagePeriodType,omitempty"`
	UsagePeriodStart     string                   `json:"usagePeriodStart,omitempty"`
	UsagePeriodEnd       string                   `json:"usagePeriodEnd,omitempty"`
	BillingPeriodStart   string                   `json:"billingPeriodStart,omitempty"`
	BillingPeriodEnd     string                   `json:"billingPeriodEnd,omitempty"`
	History              []billingHistoryResponse `json:"history,omitempty"`
	SyncedAt             time.Time                `json:"syncedAt"`
}

type billingHistoryResponse struct {
	Year         int     `json:"year"`
	Month        int     `json:"month"`
	PeriodType   string  `json:"periodType,omitempty"`
	PeriodStart  string  `json:"periodStart,omitempty"`
	PeriodEnd    string  `json:"periodEnd,omitempty"`
	IncludedUsed float64 `json:"includedUsed"`
	OnDemandUsed float64 `json:"onDemandUsed"`
	TotalUsed    float64 `json:"totalUsed"`
}

type quotaResponse struct {
	Type            string     `json:"type"`
	Source          string     `json:"source"`
	Confidence      string     `json:"confidence"`
	Unit            string     `json:"unit,omitempty"`
	Used            float64    `json:"used"`
	Limit           float64    `json:"limit"`
	Remaining       float64    `json:"remaining"`
	UsagePercent    float64    `json:"usagePercent"`
	LimitKnown      bool       `json:"limitKnown"`
	WindowHours     int        `json:"windowHours,omitempty"`
	Observed        bool       `json:"observed"`
	Confirmed       bool       `json:"confirmed"`
	Status          string     `json:"status"`
	PeriodStart     string     `json:"periodStart,omitempty"`
	PeriodEnd       string     `json:"periodEnd,omitempty"`
	ExhaustedAt     *time.Time `json:"exhaustedAt,omitempty"`
	NextProbeAt     *time.Time `json:"nextProbeAt,omitempty"`
	LastConfirmedAt *time.Time `json:"lastConfirmedAt,omitempty"`
}

func (h *Handler) list(c *gin.Context) {
	page, pageSize := pagination(c)
	values, total, err := h.service.List(c.Request.Context(), page, pageSize, c.Query("search"), accountapp.ListFilter{Provider: c.Query("provider"), QuotaType: c.Query("type"), Status: c.Query("status"), Renewal: c.Query("renewal"), Risk: c.Query("risk"), Sort: repository.SortQuery{Field: c.Query("sortBy"), Direction: repository.SortDirection(c.Query("sortOrder"))}})
	if errors.Is(err, accountapp.ErrInvalidFilter) {
		response.Error(c, http.StatusBadRequest, "invalidFilter", err.Error())
		return
	}
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "accountListFailed", "读取账号失败")
		return
	}
	items := make([]accountResponse, 0, len(values))
	for _, value := range values {
		items = append(items, newAccountResponse(value))
	}
	response.Success(c, http.StatusOK, gin.H{"items": items, "page": page, "pageSize": pageSize, "total": total})
}

func (h *Handler) summary(c *gin.Context) {
	value, err := h.service.Summary(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "accountSummaryFailed", "读取账号统计失败")
		return
	}
	build := value.Providers[string(accountdomain.ProviderBuild)]
	web := value.Providers[string(accountdomain.ProviderWeb)]
	console := value.Providers[string(accountdomain.ProviderConsole)]
	response.Success(c, http.StatusOK, gin.H{
		"total": value.Total, "available": value.Available, "recovering": value.Recovering, "attention": value.Attention, "risk": value.Risk,
		"providers": gin.H{
			string(accountdomain.ProviderBuild):   gin.H{"total": build.Total, "available": build.Available},
			string(accountdomain.ProviderWeb):     gin.H{"total": web.Total, "available": web.Available},
			string(accountdomain.ProviderConsole): gin.H{"total": console.Total, "available": console.Available},
		},
		"recovery": gin.H{"cooldown": value.Recovery.Cooldown, "waitingReset": value.Recovery.WaitingReset, "probing": value.Recovery.Probing},
		"issues":   gin.H{"disabled": value.Issues.Disabled, "reauthRequired": value.Issues.ReauthRequired},
	})
}

func (h *Handler) batchUpdate(c *gin.Context) {
	var request batchUpdateRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	ids, err := parseIDs(request.IDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", err.Error())
		return
	}
	if !h.validateProviderIDs(c, ids, request.Provider) {
		return
	}
	updated, err := h.service.BatchUpdate(c.Request.Context(), ids, accountapp.UpdateInput{Enabled: request.Enabled, Priority: request.Priority, MaxConcurrent: request.MaxConcurrent, MinimumRemaining: request.MinimumRemaining})
	if err != nil {
		h.writeServiceError(c, "accountBatchUpdateFailed", err, http.StatusInternalServerError, "批量更新账号失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"updated": updated})
}

func (h *Handler) batchDelete(c *gin.Context) {
	var request batchDeleteRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	ids, err := parseIDs(request.IDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", err.Error())
		return
	}
	if !h.validateProviderIDs(c, ids, request.Provider) {
		return
	}
	deleted, err := h.service.BatchDelete(c.Request.Context(), ids)
	if err != nil {
		h.writeServiceError(c, "accountBatchDeleteFailed", err, http.StatusInternalServerError, "批量删除账号失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": deleted})
}

func (h *Handler) batchRefreshBilling(c *gin.Context) {
	var request batchDeleteRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	ids, err := parseIDs(request.IDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", err.Error())
		return
	}
	if request.Provider != string(accountdomain.ProviderBuild) {
		response.Error(c, http.StatusBadRequest, "invalidProvider", "仅 Grok Build 账号支持 Billing 同步")
		return
	}
	if !h.validateProviderIDs(c, ids, request.Provider) {
		return
	}
	succeeded, failed, err := h.service.BatchRefreshBilling(c.Request.Context(), ids)
	if err != nil {
		h.writeServiceError(c, "billingBatchRefreshFailed", err, http.StatusBadGateway, "批量同步 Billing 失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"succeeded": succeeded, "failed": failed})
}

func (h *Handler) batchRefreshQuotas(c *gin.Context) {
	var request batchDeleteRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	ids, err := parseIDs(request.IDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", err.Error())
		return
	}
	providerValue := accountdomain.Provider(request.Provider)
	if !providerValue.IsValid() {
		response.Error(c, http.StatusBadRequest, "invalidProvider", "账号来源无效")
		return
	}
	if !h.validateProviderIDs(c, ids, request.Provider) {
		return
	}
	var succeeded, failed int
	if providerValue == accountdomain.ProviderBuild {
		succeeded, failed, err = h.service.BatchRefreshBilling(c.Request.Context(), ids)
	} else {
		succeeded, failed, err = h.service.BatchRefreshQuota(c.Request.Context(), ids)
	}
	if err != nil {
		h.writeServiceError(c, "quotaBatchRefreshFailed", err, http.StatusBadGateway, "批量同步账号额度失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"succeeded": succeeded, "failed": failed})
}

func (h *Handler) get(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	value, err := h.service.Get(c.Request.Context(), id)
	if err != nil {
		h.writeServiceError(c, "accountGetFailed", err, http.StatusInternalServerError, "读取账号失败")
		return
	}
	response.Success(c, http.StatusOK, newAccountResponse(value))
}

func (h *Handler) startDevice(c *gin.Context) {
	value, err := h.service.StartDeviceLogin(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusBadGateway, "deviceLoginStartFailed", "启动 Device OAuth 失败")
		return
	}
	response.Success(c, http.StatusCreated, gin.H{"sessionId": value.SessionID, "userCode": value.UserCode, "verificationUri": value.VerificationURI, "verificationUriComplete": value.VerificationURIComplete, "intervalSeconds": int(value.Interval.Seconds()), "expiresAt": value.ExpiresAt})
}

func (h *Handler) pollDevice(c *gin.Context) {
	value, err := h.service.PollDeviceLogin(c.Request.Context(), c.Param("sessionId"))
	if errors.Is(err, accountapp.ErrDevicePending) {
		response.Success(c, http.StatusAccepted, gin.H{"status": "pending"})
		return
	}
	if errors.Is(err, accountapp.ErrDeviceSlowDown) {
		response.Error(c, http.StatusTooManyRequests, "devicePollTooFast", "轮询过快，请稍后重试")
		return
	}
	if errors.Is(err, accountapp.ErrDeviceDenied) {
		response.Error(c, http.StatusGone, "deviceLoginExpired", "Device OAuth 已拒绝或过期")
		return
	}
	if err != nil {
		response.Error(c, http.StatusBadGateway, "deviceLoginFailed", "Device OAuth 登录失败")
		return
	}
	syncResult := h.syncInitial(c.Request.Context(), value.Credential.ID)
	if refreshed, refreshErr := h.service.Get(c.Request.Context(), value.Credential.ID); refreshErr == nil {
		value = refreshed
	}
	status := "succeeded"
	if syncResult.Failed > 0 {
		status = "syncFailed"
	}
	response.Success(c, http.StatusOK, gin.H{"status": status, "account": newAccountResponse(value), "synced": syncResult.Succeeded, "syncFailed": syncResult.Failed})
}

func (h *Handler) importAuth(c *gin.Context) {
	h.importFile(c, accountdomain.ProviderBuild)
}

func (h *Handler) importWebAuth(c *gin.Context) {
	h.importFile(c, accountdomain.ProviderWeb)
}

func (h *Handler) importConsoleAuth(c *gin.Context) {
	h.importFile(c, accountdomain.ProviderConsole)
}

func (h *Handler) convertWebToBuild(c *gin.Context) {
	var request buildConversionRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "转换请求无效")
		return
	}
	if request.All && len(request.IDs) > 0 {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "全部转换与指定账号不能同时提交")
		return
	}
	if request.Strategy == "" {
		request.Strategy = accountapp.BuildConversionMissing
	}
	if request.Strategy != accountapp.BuildConversionAll && request.Strategy != accountapp.BuildConversionMissing {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "转换策略无效")
		return
	}
	var ids []uint64
	if !request.All {
		var err error
		ids, err = parseIDs(request.IDs)
		if err != nil {
			response.Error(c, http.StatusBadRequest, "invalidId", err.Error())
			return
		}
		if !h.validateProviderIDs(c, ids, string(accountdomain.ProviderWeb)) {
			return
		}
	}
	h.streamWebToBuildConversion(c, request.All, ids, request.Strategy)
}

func (h *Handler) syncWebToConsole(c *gin.Context) {
	var request webConsoleSyncRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "同步请求无效")
		return
	}
	if request.All && len(request.IDs) > 0 {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "全部同步与指定账号不能同时提交")
		return
	}
	if request.Strategy == "" {
		request.Strategy = accountapp.WebConsoleSyncAll
	}
	if request.Strategy != accountapp.WebConsoleSyncAll && request.Strategy != accountapp.WebConsoleSyncMissing {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "同步策略无效")
		return
	}
	var ids []uint64
	if !request.All {
		var err error
		ids, err = parseIDs(request.IDs)
		if err != nil {
			response.Error(c, http.StatusBadRequest, "invalidId", err.Error())
			return
		}
		if !h.validateProviderIDs(c, ids, string(accountdomain.ProviderWeb)) {
			return
		}
	}
	h.streamWebToConsoleSync(c, request.All, ids, request.Strategy)
}

func (h *Handler) runWebToConsoleSync(ctx context.Context, all bool, ids []uint64, strategy accountapp.WebConsoleSyncStrategy, progress accountapp.BatchProgressObserver, syncProgress func(completed, total int)) (accountapp.ImportResult, accountsyncapp.Result, error) {
	pipeline := h.startSyncPipeline(ctx, syncProgress)
	var (
		result accountapp.ImportResult
		err    error
	)
	if all {
		result, err = h.service.SyncAllWebAccountsToConsoleWithStrategy(pipeline.ctx, strategy, pipeline.Observe, progress)
	} else {
		result, err = h.service.SyncWebAccountsToConsoleWithStrategy(pipeline.ctx, ids, strategy, pipeline.Observe, progress)
	}
	syncResult := pipeline.Finish(err != nil)
	return result, syncResult, err
}

func (h *Handler) streamWebToConsoleSync(c *gin.Context, all bool, ids []uint64, strategy accountapp.WebConsoleSyncStrategy) {
	stream := newAccountEventStream(c)
	defer stream.Close()
	var total atomic.Int64
	result, syncResult, err := h.runWebToConsoleSync(c.Request.Context(), all, ids, strategy, stream.PhaseProgressObserver("importing", &total), stream.SyncProgressObserver())
	if err != nil {
		stream.WriteError("accountConsoleSyncFailed", "Grok Web 账号同步到 Console 失败")
		return
	}
	_ = stream.Write("complete", accountImportResponse{Created: result.Created, Updated: result.Updated, Skipped: result.Skipped, Synced: syncResult.Succeeded, SyncFailed: syncResult.Failed})
}

func (h *Handler) runWebToBuildConversion(ctx context.Context, all bool, ids []uint64, strategy accountapp.BuildConversionStrategy, progress accountapp.BatchProgressObserver, syncProgress func(completed, total int)) (accountapp.BuildConversionResult, accountsyncapp.Result, error) {
	pipeline := h.startSyncPipeline(ctx, syncProgress)
	var (
		result accountapp.BuildConversionResult
		err    error
	)
	if all {
		result, err = h.service.ConvertAllWebAccountsToBuildWithStrategy(pipeline.ctx, strategy, pipeline.Observe, progress)
	} else {
		result, err = h.service.ConvertWebAccountsToBuildWithStrategy(pipeline.ctx, ids, strategy, pipeline.Observe, progress)
	}
	syncResult := pipeline.Finish(err != nil)
	return result, syncResult, err
}

func (h *Handler) streamWebToBuildConversion(c *gin.Context, all bool, ids []uint64, strategy accountapp.BuildConversionStrategy) {
	stream := newAccountEventStream(c)
	defer stream.Close()
	var total atomic.Int64
	result, syncResult, err := h.runWebToBuildConversion(c.Request.Context(), all, ids, strategy, stream.PhaseProgressObserver("converting", &total), stream.SyncProgressObserver())
	if err != nil {
		stream.WriteError("accountConversionFailed", "Grok Web 账号转换失败")
		return
	}
	_ = stream.Write("complete", newBuildConversionResponse(result, syncResult))
}

func newBuildConversionResponse(result accountapp.BuildConversionResult, syncResult accountsyncapp.Result) buildConversionResponse {
	return buildConversionResponse{
		Created: result.Created, Linked: result.Linked, Skipped: result.Skipped, Failed: result.Failed,
		Synced: syncResult.Succeeded, SyncFailed: syncResult.Failed,
	}
}

func prepareAccountEventStream(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-cache, no-transform")
	c.Header("X-Accel-Buffering", "no")
}

type accountEventStream struct {
	context   *gin.Context
	mu        sync.Mutex
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func newAccountEventStream(c *gin.Context) *accountEventStream {
	prepareAccountEventStream(c)
	stream := &accountEventStream{context: c, stop: make(chan struct{}), done: make(chan struct{})}
	_ = stream.writeComment("connected")
	go stream.heartbeat()
	return stream
}

func (s *accountEventStream) ProgressObserver() accountapp.BatchProgressObserver {
	return s.PhaseProgressObserver("", nil)
}

func (s *accountEventStream) PhaseProgressObserver(phase string, totalValue *atomic.Int64) accountapp.BatchProgressObserver {
	return func(completed, total int) error {
		if totalValue != nil {
			totalValue.Store(int64(total))
		}
		return s.Write("progress", accountTaskProgressResponse{Completed: completed, Total: total, Phase: phase})
	}
}

func (s *accountEventStream) SyncProgressObserver() func(completed, total int) {
	return func(completed, total int) {
		_ = s.Write("progress", accountTaskProgressResponse{Completed: completed, Total: total, Phase: "syncing"})
	}
}

func (s *accountEventStream) WriteError(code, message string) {
	_ = s.Write("error", gin.H{"code": code, "message": message})
}

func (s *accountEventStream) Write(event string, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := setAccountWriteDeadline(s.context.Writer); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.context.Writer, "event: %s\ndata: %s\n\n", event, payload); err != nil {
		return err
	}
	s.context.Writer.Flush()
	return s.context.Request.Context().Err()
}

func (s *accountEventStream) writeComment(comment string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := setAccountWriteDeadline(s.context.Writer); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.context.Writer, ": %s\n\n", comment); err != nil {
		return err
	}
	s.context.Writer.Flush()
	return s.context.Request.Context().Err()
}

func (s *accountEventStream) heartbeat() {
	defer close(s.done)
	ticker := time.NewTicker(accountEventHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-s.context.Request.Context().Done():
			return
		case <-ticker.C:
			if err := s.writeComment("heartbeat"); err != nil {
				return
			}
		}
	}
}

func (s *accountEventStream) Close() {
	s.closeOnce.Do(func() { close(s.stop) })
	<-s.done
}

func setAccountWriteDeadline(writer http.ResponseWriter) error {
	err := http.NewResponseController(writer).SetWriteDeadline(time.Now().Add(accountEventWriteTimeout))
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

// writeAccountEvent keeps the event encoder independently testable without starting a heartbeat.
func writeAccountEvent(c *gin.Context, event string, value any) error {
	return (&accountEventStream{context: c}).Write(event, value)
}

func (h *Handler) importFile(c *gin.Context, providerValue accountdomain.Provider) {
	fileDescription := "账号凭据 JSON"
	if providerValue == accountdomain.ProviderWeb {
		fileDescription = "Grok Web JSON 或 SSO 文本"
	} else if providerValue == accountdomain.ProviderConsole {
		fileDescription = "Grok Console JSON 或 SSO 文本"
	}
	documents, ok := readAccountImportDocuments(c, fileDescription)
	if !ok {
		return
	}
	stream := newAccountEventStream(c)
	defer stream.Close()
	var total atomic.Int64
	pipeline := h.startSyncPipeline(c.Request.Context(), stream.SyncProgressObserver())
	var result accountapp.ImportResult
	var err error
	if providerValue == accountdomain.ProviderWeb {
		result, err = h.service.ImportWebCredentialDocumentsWithProgress(pipeline.ctx, documents, pipeline.Observe, stream.PhaseProgressObserver("importing", &total))
	} else if providerValue == accountdomain.ProviderConsole {
		result, err = h.service.ImportConsoleCredentialDocumentsWithProgress(pipeline.ctx, documents, pipeline.Observe, stream.PhaseProgressObserver("importing", &total))
	} else {
		result, err = h.service.ImportCredentialDocumentsWithProgress(pipeline.ctx, documents, pipeline.Observe, stream.PhaseProgressObserver("importing", &total))
	}
	syncResult := pipeline.Finish(err != nil)
	if err != nil {
		stream.WriteError("authImportFailed", "导入账号失败")
		return
	}
	_ = stream.Write("complete", accountImportResponse{Created: result.Created, Updated: result.Updated, Synced: syncResult.Succeeded, SyncFailed: syncResult.Failed})
}

func readAccountImportDocuments(c *gin.Context, fileDescription string) ([][]byte, bool) {
	form, err := c.MultipartForm()
	if err != nil {
		var sizeError *http.MaxBytesError
		if errors.As(err, &sizeError) {
			response.Error(c, http.StatusRequestEntityTooLarge, "accountImportFileTooLarge", "账号凭据文件总大小不能超过 30 MiB")
			return nil, false
		}
		response.Error(c, http.StatusBadRequest, "invalidAuthFile", "请选择有效的"+fileDescription)
		return nil, false
	}
	defer form.RemoveAll()
	files := append(form.File["files"], form.File["file"]...)
	if len(files) == 0 {
		response.Error(c, http.StatusBadRequest, "invalidAuthFile", "请选择有效的"+fileDescription)
		return nil, false
	}
	if len(files) > maxAccountImportFiles {
		response.Error(c, http.StatusBadRequest, "invalidAuthFile", "单次最多选择 1000 个账号文件")
		return nil, false
	}
	documents := make([][]byte, 0, len(files))
	totalBytes := int64(0)
	for _, file := range files {
		if file.Size < 0 || totalBytes+file.Size > maxAccountImportBytes {
			response.Error(c, http.StatusRequestEntityTooLarge, "accountImportFileTooLarge", "账号凭据文件总大小不能超过 30 MiB")
			return nil, false
		}
		opened, openErr := file.Open()
		if openErr != nil {
			response.Error(c, http.StatusBadRequest, "invalidAuthFile", "无法读取"+fileDescription)
			return nil, false
		}
		data, readErr := io.ReadAll(io.LimitReader(opened, maxAccountImportBytes-totalBytes+1))
		_ = opened.Close()
		if readErr != nil {
			response.Error(c, http.StatusBadRequest, "invalidAuthFile", "无法读取"+fileDescription)
			return nil, false
		}
		totalBytes += int64(len(data))
		if totalBytes > maxAccountImportBytes {
			response.Error(c, http.StatusRequestEntityTooLarge, "accountImportFileTooLarge", "账号凭据文件总大小不能超过 30 MiB")
			return nil, false
		}
		documents = append(documents, data)
	}
	return documents, true
}

func (h *Handler) refreshWebQuota(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	if _, err := h.service.RefreshQuota(c.Request.Context(), id); err != nil {
		h.writeServiceError(c, "quotaRefreshFailed", err, http.StatusBadGateway, "同步 Provider 额度失败")
		return
	}
	value, err := h.service.Get(c.Request.Context(), id)
	if err != nil {
		h.writeServiceError(c, "accountGetFailed", err, http.StatusInternalServerError, "读取账号失败")
		return
	}
	response.Success(c, http.StatusOK, newAccountResponse(value))
}

func (h *Handler) exportCredentials(c *gin.Context) {
	result, err := h.service.ExportCredentials(c.Request.Context())
	if err != nil {
		h.writeServiceError(c, "accountExportFailed", err, http.StatusInternalServerError, "导出账号失败")
		return
	}
	filename := "grok2api-accounts-" + time.Now().UTC().Format("20060102T150405Z") + ".json"
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("X-Exported-Accounts", strconv.Itoa(result.Count))
	c.Data(http.StatusOK, "application/json; charset=utf-8", result.Data)
}

func (h *Handler) syncInitial(ctx context.Context, accountIDs ...uint64) accountsyncapp.Result {
	if h.sync == nil {
		return accountsyncapp.Result{}
	}
	return h.sync.Sync(ctx, accountIDs...)
}

func (h *Handler) update(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var request updateRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	value, err := h.service.Update(c.Request.Context(), id, accountapp.UpdateInput{
		Name: request.Name, Enabled: request.Enabled, Priority: request.Priority,
		MaxConcurrent: request.MaxConcurrent, MinimumRemaining: request.MinimumRemaining,
		CloudflareCookies: request.CloudflareCookies, ClearCloudflareCookies: request.ClearCloudflareCookies,
		BuildSuperEntitled: request.BuildSuperEntitled, BuildRouteMode: request.BuildRouteMode,
	})
	if err != nil {
		h.writeServiceError(c, "accountUpdateFailed", err, http.StatusInternalServerError, "更新账号失败")
		return
	}
	result := newAccountResponse(value)
	if request.BuildSuperEntitled != nil {
		if synchronizer, ok := h.sync.(accountModelSynchronizer); ok {
			result.ModelSyncFailed = synchronizer.SyncModels(c.Request.Context(), id) != nil
		}
	}
	response.Success(c, http.StatusOK, result)
}

func (h *Handler) delete(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	if err := h.service.Delete(c.Request.Context(), id); err != nil {
		h.writeServiceError(c, "accountDeleteFailed", err, http.StatusInternalServerError, "删除账号失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": true})
}

// writeServiceError 仅暴露明确的账号业务错误，未知内部错误使用稳定文案。
func (h *Handler) writeServiceError(c *gin.Context, code string, err error, fallbackStatus int, fallbackMessage string) {
	switch {
	case errors.Is(err, accountapp.ErrImportLimit):
		response.Error(c, http.StatusBadRequest, "accountImportLimitExceeded", err.Error())
	case errors.Is(err, accountapp.ErrExportLimit):
		response.Error(c, http.StatusBadRequest, "accountExportLimitExceeded", err.Error())
	case errors.Is(err, accountapp.ErrInvalidInput), errors.Is(err, accountapp.ErrInvalidImport):
		response.Error(c, http.StatusBadRequest, code, err.Error())
	case errors.Is(err, accountapp.ErrNotFound):
		response.Error(c, http.StatusNotFound, "accountNotFound", err.Error())
	case errors.Is(err, accountapp.ErrUnsupported):
		response.Error(c, http.StatusConflict, "accountOperationUnsupported", err.Error())
	case errors.Is(err, accountapp.ErrConversionBusy):
		response.Error(c, http.StatusConflict, "accountConversionBusy", err.Error())
	default:
		response.Error(c, fallbackStatus, code, fallbackMessage)
	}
}

func (h *Handler) refreshToken(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	value, err := h.service.RefreshToken(c.Request.Context(), id)
	if err != nil {
		h.writeServiceError(c, "tokenRefreshFailed", err, http.StatusBadGateway, "刷新账号凭据失败")
		return
	}
	response.Success(c, http.StatusOK, newAccountResponse(value))
}

func (h *Handler) refreshBilling(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	value, err := h.service.RefreshBilling(c.Request.Context(), id)
	if err != nil {
		h.writeServiceError(c, "billingRefreshFailed", err, http.StatusBadGateway, "刷新账号额度失败")
		return
	}
	response.Success(c, http.StatusOK, newBillingResponse(value))
}

func (h *Handler) refreshAllBilling(c *gin.Context) {
	stream := newAccountEventStream(c)
	defer stream.Close()
	succeeded, failed, err := h.service.SyncAllBillingWithProgress(c.Request.Context(), stream.ProgressObserver())
	if err != nil {
		stream.WriteError("billingRefreshFailed", "刷新账号额度失败")
		return
	}
	_ = stream.Write("complete", accountBatchResponse{Succeeded: succeeded, Failed: failed})
}

func (h *Handler) refreshAllTokens(c *gin.Context) {
	stream := newAccountEventStream(c)
	defer stream.Close()
	succeeded, failed, skipped, err := h.service.RefreshAllTokensWithProgress(c.Request.Context(), stream.ProgressObserver())
	if err != nil {
		stream.WriteError("tokenRefreshFailed", "续期账号凭据失败")
		return
	}
	_ = stream.Write("complete", accountTokenRefreshResponse{Succeeded: succeeded, Failed: failed, Skipped: skipped})
}

func (h *Handler) refreshAllWebQuotas(c *gin.Context) {
	stream := newAccountEventStream(c)
	defer stream.Close()
	succeeded, failed, err := h.service.SyncAllWebQuotasWithProgress(c.Request.Context(), stream.ProgressObserver())
	if err != nil {
		stream.WriteError("quotaRefreshFailed", "同步 Grok Web 账号额度失败")
		return
	}
	_ = stream.Write("complete", accountBatchResponse{Succeeded: succeeded, Failed: failed})
}

func (h *Handler) refreshAllConsoleQuotas(c *gin.Context) {
	stream := newAccountEventStream(c)
	defer stream.Close()
	succeeded, failed, err := h.service.SyncAllConsoleQuotasWithProgress(c.Request.Context(), stream.ProgressObserver())
	if err != nil {
		stream.WriteError("quotaRefreshFailed", "同步 Grok Console 账号额度失败")
		return
	}
	_ = stream.Write("complete", accountBatchResponse{Succeeded: succeeded, Failed: failed})
}

func newAccountResponse(value accountapp.View) accountResponse {
	c := value.Credential
	buildRouteMode := c.BuildRouteMode
	if c.Provider != accountdomain.ProviderBuild || !buildRouteMode.IsValid() {
		buildRouteMode = accountdomain.BuildRouteAuto
	}
	result := accountResponse{
		ID: c.ID, Provider: string(c.Provider), AuthType: string(c.AuthType), WebTier: string(c.WebTier),
		WebTierSyncedAt: c.WebTierSyncedAt, Name: c.Name, Email: c.Email, UserID: c.UserID, TeamID: c.TeamID,
		Enabled: c.Enabled, AuthStatus: string(c.AuthStatus), Refreshable: c.EncryptedRefreshToken != "",
		RefreshDueAt: c.RefreshDueAt, LastRefreshAt: c.LastRefreshAt,
		RefreshFailures: c.RefreshFailureCount, LastRefreshError: c.LastRefreshErrorCode,
		Priority: c.Priority, MaxConcurrent: c.MaxConcurrent, MinimumRemaining: c.MinimumRemaining,
		FailureCount: c.FailureCount, CooldownUntil: c.CooldownUntil, LastError: c.LastError,
		LastUsedAt: c.LastUsedAt, LinkedAccountID: c.LinkedAccountID, LinkedName: c.LinkedAccountName, LinkedProvider: string(c.LinkedProvider),
		CreatedAt: c.CreatedAt, ObservedModel: c.ObservedModel, ObservedModelAt: c.ObservedModelAt,
		CloudflareCookieConfigured: c.EncryptedCloudflareCookie != "",
		BuildSuperEntitled:         c.BuildSuperEntitled && c.Provider == accountdomain.ProviderBuild,
		BuildRouteMode:             string(buildRouteMode),
		BuildBotFlagged:            value.BuildBotFlagged && c.Provider == accountdomain.ProviderBuild,
		Quota:                      newQuotaResponse(value.Quota), QuotaWindows: make([]quotaWindowResponse, 0, len(value.QuotaWindows)),
	}
	for _, window := range value.QuotaWindows {
		breakdown := make([]quotaBreakdownResponse, 0, len(window.Breakdown))
		for _, item := range window.Breakdown {
			breakdown = append(breakdown, quotaBreakdownResponse{ProductCode: item.ProductCode, UsagePercent: item.UsagePercent})
		}
		result.QuotaWindows = append(result.QuotaWindows, quotaWindowResponse{
			Mode: window.Mode, Remaining: window.Remaining, Total: window.Total,
			UsagePercent: window.UsagePercent, Breakdown: breakdown,
			WindowSeconds: window.WindowSeconds, ResetAt: window.ResetAt, SyncedAt: window.SyncedAt,
			Source: string(window.Source),
		})
	}
	if !c.ExpiresAt.IsZero() {
		expiresAt := c.ExpiresAt
		result.ExpiresAt = &expiresAt
	}
	if value.Billing != nil {
		billing := newBillingResponse(*value.Billing)
		result.Billing = &billing
	}
	return result
}

func newQuotaResponse(value accountapp.QuotaView) quotaResponse {
	return quotaResponse{Type: string(value.Type), Source: value.Source, Confidence: value.Confidence, Unit: value.Unit, Used: value.Used, Limit: value.Limit, Remaining: value.Remaining, UsagePercent: value.UsagePercent, LimitKnown: value.LimitKnown, WindowHours: value.WindowHours, Observed: value.Observed, Confirmed: value.Confirmed, Status: string(value.Status), PeriodStart: value.PeriodStart, PeriodEnd: value.PeriodEnd, ExhaustedAt: value.ExhaustedAt, NextProbeAt: value.NextProbeAt, LastConfirmedAt: value.LastConfirmedAt}
}

func newBillingResponse(value accountdomain.Billing) billingResponse {
	history := make([]billingHistoryResponse, 0, len(value.History))
	for _, entry := range value.History {
		history = append(history, billingHistoryResponse{
			Year: entry.Year, Month: entry.Month,
			PeriodType: entry.PeriodType, PeriodStart: entry.PeriodStart, PeriodEnd: entry.PeriodEnd,
			IncludedUsed: entry.IncludedUsed, OnDemandUsed: entry.OnDemandUsed, TotalUsed: entry.TotalUsed,
		})
	}
	return billingResponse{PlanCode: value.PlanCode, PlanName: value.PlanName, MonthlyLimit: value.MonthlyLimit, Used: value.Used, Remaining: value.Remaining(), OnDemandCap: value.OnDemandCap, OnDemandUsed: value.OnDemandUsed, PrepaidBalance: value.PrepaidBalance, CreditUsagePercent: value.CreditUsagePercent, IsUnifiedBillingUser: value.IsUnifiedBillingUser, OnDemandEnabled: value.OnDemandEnabled, TopUpMethod: value.TopUpMethod, UsagePeriodType: value.UsagePeriodType, UsagePeriodStart: value.UsagePeriodStart, UsagePeriodEnd: value.UsagePeriodEnd, BillingPeriodStart: value.BillingPeriodStart, BillingPeriodEnd: value.BillingPeriodEnd, History: history, SyncedAt: value.SyncedAt}
}

func pagination(c *gin.Context) (int, int) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("pageSize", "20"))
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 20
	}
	if size > 100 {
		size = 100
	}
	return page, size
}

func pathID(c *gin.Context) (uint64, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		response.Error(c, http.StatusBadRequest, "invalidId", "ID 无效")
		return 0, false
	}
	return id, true
}

func parseIDs(values []string) ([]uint64, error) {
	ids := make([]uint64, 0, len(values))
	for _, value := range values {
		id, err := strconv.ParseUint(value, 10, 64)
		if err != nil || id == 0 {
			return nil, errors.New("ID 无效")
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (h *Handler) validateProviderIDs(c *gin.Context, ids []uint64, providerValue string) bool {
	if providerValue != string(accountdomain.ProviderBuild) && providerValue != string(accountdomain.ProviderWeb) && providerValue != string(accountdomain.ProviderConsole) {
		response.Error(c, http.StatusBadRequest, "invalidProvider", "账号来源无效")
		return false
	}
	for _, id := range ids {
		value, err := h.service.Get(c.Request.Context(), id)
		if err != nil || string(value.Credential.Provider) != providerValue {
			response.Error(c, http.StatusConflict, "accountPoolMismatch", "批量操作包含不属于当前号池的账号")
			return false
		}
	}
	return true
}
