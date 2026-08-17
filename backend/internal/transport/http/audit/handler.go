package audit

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	auditapp "github.com/chenyme/grok2api/backend/internal/application/audit"
	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	service                 *auditapp.Service
	qualityGuardClientKeyID uint64
}

func NewHandler(service *auditapp.Service) *Handler { return &Handler{service: service} }

func NewQualityGuardHandler(service *auditapp.Service, clientKeyID uint64) *Handler {
	return &Handler{service: service, qualityGuardClientKeyID: clientKeyID}
}

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/request-audits", h.list)
	router.GET("/request-audits/summary", h.summary)
	router.GET("/request-audits/degrade-accounts", h.degradeAccounts)
	router.GET("/request-audits/:id", h.get)
}

// RegisterQualityGuard exposes only the audit cursor required by the sidecar.
func (h *Handler) RegisterQualityGuard(router *gin.RouterGroup) {
	router.GET("/request-audits", h.listQualityGuard)
}

type qualityGuardAuditResponse struct {
	ID              uint64  `json:"id,string"`
	RequestID       string  `json:"requestId"`
	QualityProbe    bool    `json:"qualityProbe"`
	Provider        string  `json:"provider"`
	EgressNodeID    *uint64 `json:"egressNodeId,string,omitempty"`
	EgressNodeName  string  `json:"egressNodeName,omitempty"`
	StatusCode      int     `json:"statusCode"`
	Streaming       bool    `json:"streaming"`
	OutputTokens    int64   `json:"outputTokens"`
	ReasoningTokens int64   `json:"reasoningTokens"`
	FirstTokenMS    *int64  `json:"firstTokenMs,omitempty"`
	DurationMS      int64   `json:"durationMs"`
	ErrorCode       string  `json:"errorCode,omitempty"`
}

func (h *Handler) listQualityGuard(c *gin.Context) {
	if h.qualityGuardClientKeyID == 0 {
		response.Error(c, http.StatusServiceUnavailable, "qualityGuardUnavailable", "质量守护配置暂不可用")
		return
	}
	pageSize, _ := strconv.Atoi(c.DefaultQuery("pageSize", "200"))
	_, pageSize = repository.NormalizePage(1, pageSize, repository.DefaultCursorPageSize)
	result, err := h.service.ListCursor(c.Request.Context(), c.Query("cursor"), pageSize, "", "24h", auditapp.ListFilter{})
	if errors.Is(err, auditapp.ErrInvalidCursor) {
		response.Error(c, http.StatusBadRequest, "invalidCursor", "审计游标无效")
		return
	}
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "auditListFailed", "读取审计记录失败")
		return
	}
	items := make([]qualityGuardAuditResponse, 0, len(result.Items))
	for _, value := range result.Items {
		items = append(items, qualityGuardAuditResponse{
			ID: value.ID, RequestID: value.RequestID, QualityProbe: value.ClientKeyID == h.qualityGuardClientKeyID,
			Provider: value.Provider, EgressNodeID: value.EgressNodeID, EgressNodeName: value.EgressNodeName,
			StatusCode: value.StatusCode, Streaming: value.Streaming, OutputTokens: value.OutputTokens,
			ReasoningTokens: value.ReasoningTokens, FirstTokenMS: value.FirstTokenMS,
			DurationMS: value.DurationMS, ErrorCode: value.ErrorCode,
		})
	}
	response.Success(c, http.StatusOK, gin.H{"items": items, "pageSize": pageSize, "nextCursor": result.NextCursor, "hasMore": result.HasMore})
}

type auditResponse struct {
	ID                      uint64                    `json:"id,string"`
	RequestID               string                    `json:"requestId"`
	ClientKeyID             uint64                    `json:"clientKeyId,string"`
	ClientKeyName           string                    `json:"clientKeyName,omitempty"`
	ModelRouteID            uint64                    `json:"modelRouteId,string"`
	ModelPublicID           string                    `json:"modelPublicId,omitempty"`
	ModelUpstreamModel      string                    `json:"modelUpstreamModel,omitempty"`
	Provider                string                    `json:"provider"`
	Operation               string                    `json:"operation"`
	UsageSource             string                    `json:"usageSource"`
	AccountID               *uint64                   `json:"accountId,string,omitempty"`
	AccountName             string                    `json:"accountName,omitempty"`
	EgressNodeID            *uint64                   `json:"egressNodeId,string,omitempty"`
	EgressNodeName          string                    `json:"egressNodeName,omitempty"`
	EgressScope             string                    `json:"egressScope,omitempty"`
	EgressMode              string                    `json:"egressMode,omitempty"`
	StatusCode              int                       `json:"statusCode"`
	Streaming               bool                      `json:"streaming"`
	MediaInputImages        int64                     `json:"mediaInputImages"`
	MediaOutputImages       int64                     `json:"mediaOutputImages"`
	MediaOutputSeconds      int64                     `json:"mediaOutputSeconds"`
	InputTokens             int64                     `json:"inputTokens"`
	CachedInputTokens       int64                     `json:"cachedInputTokens"`
	OutputTokens            int64                     `json:"outputTokens"`
	ReasoningTokens         int64                     `json:"reasoningTokens"`
	TotalTokens             int64                     `json:"totalTokens"`
	CostInUSDTicks          int64                     `json:"costInUsdTicks"`
	EstimatedCostInUSDTicks int64                     `json:"estimatedCostInUsdTicks"`
	PricingModel            string                    `json:"pricingModel,omitempty"`
	PricingVersion          string                    `json:"pricingVersion,omitempty"`
	Billing                 *billingBreakdownResponse `json:"billing,omitempty"`
	NumSourcesUsed          int64                     `json:"numSourcesUsed"`
	NumServerSideToolsUsed  int64                     `json:"numServerSideToolsUsed"`
	ContextInputTokens      int64                     `json:"contextInputTokens"`
	ContextOutputTokens     int64                     `json:"contextOutputTokens"`
	FirstTokenMS            *int64                    `json:"firstTokenMs,omitempty"`
	OutputTokensPerSecond   *float64                  `json:"outputTokensPerSecond,omitempty"`
	DurationMS              int64                     `json:"durationMs"`
	ErrorCode               string                    `json:"errorCode,omitempty"`
	AttemptCount            int                       `json:"attemptCount"`
	CreatedAt               time.Time                 `json:"createdAt"`
}

type billingBreakdownResponse struct {
	Source          string                     `json:"source"`
	Method          string                     `json:"method"`
	Model           string                     `json:"model,omitempty"`
	Version         string                     `json:"version,omitempty"`
	Tier            string                     `json:"tier,omitempty"`
	Components      []billingComponentResponse `json:"components"`
	TotalInUSDTicks int64                      `json:"totalInUsdTicks"`
}

type billingComponentResponse struct {
	Kind                string `json:"kind"`
	Unit                string `json:"unit"`
	Quantity            int64  `json:"quantity"`
	UnitPriceInUSDTicks int64  `json:"unitPriceInUsdTicks"`
	SubtotalInUSDTicks  int64  `json:"subtotalInUsdTicks"`
}

type auditAttemptResponse struct {
	ID                    uint64                    `json:"id,string"`
	Number                int                       `json:"number"`
	Source                string                    `json:"source"`
	Stage                 string                    `json:"stage"`
	AccountID             *uint64                   `json:"accountId,string,omitempty"`
	AccountName           string                    `json:"accountName,omitempty"`
	Method                string                    `json:"method,omitempty"`
	RequestPath           string                    `json:"requestPath,omitempty"`
	UpstreamURL           string                    `json:"upstreamUrl,omitempty"`
	StartedAt             time.Time                 `json:"startedAt"`
	DurationMS            int64                     `json:"durationMs"`
	UpstreamStatusCode    *int                      `json:"upstreamStatusCode,omitempty"`
	UpstreamStatus        string                    `json:"upstreamStatus,omitempty"`
	ResponseHeaders       map[string][]string       `json:"responseHeaders"`
	ResponseBody          string                    `json:"responseBody"`
	ResponseBodyEncoding  string                    `json:"responseBodyEncoding"`
	ResponseBodyTruncated bool                      `json:"responseBodyTruncated"`
	TransportError        string                    `json:"transportError,omitempty"`
	ErrorChain            []auditErrorFrameResponse `json:"errorChain"`
}

type auditErrorFrameResponse struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type auditDetailResponse struct {
	Audit    auditResponse          `json:"audit"`
	Attempts []auditAttemptResponse `json:"attempts"`
}

func (h *Handler) list(c *gin.Context) {
	if c.Query("pagination") == "cursor" {
		h.listCursor(c)
		return
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("pageSize", "20"))
	page, pageSize = repository.NormalizePage(page, pageSize, repository.DefaultPageSize)
	values, total, err := h.service.List(c.Request.Context(), page, pageSize)
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "auditListFailed", "读取审计记录失败")
		return
	}
	items := make([]auditResponse, 0, len(values))
	for _, value := range values {
		items = append(items, newAuditResponse(value))
	}
	response.Success(c, http.StatusOK, gin.H{"items": items, "page": page, "pageSize": pageSize, "total": total})
}

func (h *Handler) listCursor(c *gin.Context) {
	pageSize, _ := strconv.Atoi(c.DefaultQuery("pageSize", "50"))
	_, pageSize = repository.NormalizePage(1, pageSize, repository.DefaultCursorPageSize)
	result, err := h.service.ListCursor(c.Request.Context(), c.Query("cursor"), pageSize, c.Query("search"), c.Query("period"), newListFilter(c))
	if errors.Is(err, auditapp.ErrInvalidCursor) {
		response.Error(c, http.StatusBadRequest, "invalidCursor", err.Error())
		return
	}
	if errors.Is(err, auditapp.ErrInvalidFilter) {
		response.Error(c, http.StatusBadRequest, "invalidFilter", err.Error())
		return
	}
	if errors.Is(err, auditapp.ErrInvalidPeriod) {
		response.Error(c, http.StatusBadRequest, "invalidAuditPeriod", "period 仅支持 24h、7d、30d、90d")
		return
	}
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "auditListFailed", "读取审计记录失败")
		return
	}
	items := make([]auditResponse, 0, len(result.Items))
	for _, value := range result.Items {
		items = append(items, newAuditResponse(value))
	}
	response.Success(c, http.StatusOK, gin.H{"items": items, "pageSize": pageSize, "nextCursor": result.NextCursor, "hasMore": result.HasMore})
}

func (h *Handler) get(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		response.Error(c, http.StatusBadRequest, "invalidId", "审计 ID 无效")
		return
	}
	value, err := h.service.Get(c.Request.Context(), id)
	if errors.Is(err, repository.ErrNotFound) {
		response.Error(c, http.StatusNotFound, "auditNotFound", "审计记录不存在")
		return
	}
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "auditDetailFailed", "读取审计详情失败")
		return
	}
	attempts := make([]auditAttemptResponse, 0, len(value.Attempts))
	for _, attempt := range value.Attempts {
		body := string(attempt.ResponseBody)
		encoding := "utf8"
		if !utf8.Valid(attempt.ResponseBody) {
			body = base64.StdEncoding.EncodeToString(attempt.ResponseBody)
			encoding = "base64"
		}
		errorChain := make([]auditErrorFrameResponse, 0, len(attempt.ErrorChain))
		for _, frame := range attempt.ErrorChain {
			errorChain = append(errorChain, auditErrorFrameResponse{Type: frame.Type, Message: frame.Message})
		}
		attempts = append(attempts, auditAttemptResponse{
			ID: attempt.ID, Number: attempt.Number, Source: string(attempt.Source), Stage: attempt.Stage,
			AccountID: attempt.AccountID, AccountName: attempt.AccountName, Method: attempt.Method, RequestPath: attempt.RequestPath,
			UpstreamURL: attempt.UpstreamURL, StartedAt: attempt.StartedAt, DurationMS: attempt.DurationMS,
			UpstreamStatusCode: attempt.UpstreamStatusCode, UpstreamStatus: attempt.UpstreamStatus,
			ResponseHeaders: attempt.ResponseHeaders, ResponseBody: body, ResponseBodyEncoding: encoding,
			ResponseBodyTruncated: attempt.ResponseBodyTruncated,
			TransportError:        attempt.TransportError, ErrorChain: errorChain,
		})
	}
	response.Success(c, http.StatusOK, auditDetailResponse{Audit: newAuditResponse(value), Attempts: attempts})
}

type summaryResponse struct {
	Period      string               `json:"period"`
	GeneratedAt time.Time            `json:"generatedAt"`
	Range       summaryRangeResponse `json:"range"`
	Usage       summaryUsageResponse `json:"usage"`
	Pricing     pricingResponse      `json:"pricing"`
}

type summaryRangeResponse struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

type summaryUsageResponse struct {
	Requests                int64   `json:"requests"`
	SuccessfulRequests      int64   `json:"successfulRequests"`
	FailedRequests          int64   `json:"failedRequests"`
	InputTokens             int64   `json:"inputTokens"`
	CachedInputTokens       int64   `json:"cachedInputTokens"`
	OutputTokens            int64   `json:"outputTokens"`
	ReasoningTokens         int64   `json:"reasoningTokens"`
	TotalTokens             int64   `json:"totalTokens"`
	AverageDurationMS       float64 `json:"averageDurationMs"`
	SuccessRate             float64 `json:"successRate"`
	EstimatedCostInUSDTicks int64   `json:"estimatedCostInUsdTicks"`
}

type pricingResponse struct {
	Source           string `json:"source"`
	AsOf             string `json:"asOf"`
	PricedRequests   int64  `json:"pricedRequests"`
	UnpricedRequests int64  `json:"unpricedRequests"`
	PricedTokens     int64  `json:"pricedTokens"`
	UnpricedTokens   int64  `json:"unpricedTokens"`
}

func (h *Handler) summary(c *gin.Context) {
	load := h.service.Summary
	if c.Query("refresh") == "1" {
		load = h.service.SummaryFresh
	}
	result, err := load(c.Request.Context(), c.Query("search"), c.Query("period"), newListFilter(c))
	if errors.Is(err, auditapp.ErrInvalidFilter) {
		response.Error(c, http.StatusBadRequest, "invalidFilter", err.Error())
		return
	}
	if errors.Is(err, auditapp.ErrInvalidPeriod) {
		response.Error(c, http.StatusBadRequest, "invalidAuditPeriod", "period 仅支持 24h、7d、30d、90d")
		return
	}
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "auditSummaryFailed", "读取审计统计失败")
		return
	}
	response.Success(c, http.StatusOK, summaryResponse{
		Period: string(result.Period), GeneratedAt: result.GeneratedAt, Range: summaryRangeResponse{Start: result.Start, End: result.End},
		Usage: summaryUsageResponse{
			Requests: result.Usage.Requests, SuccessfulRequests: result.Usage.SuccessfulRequests, FailedRequests: result.Usage.FailedRequests,
			InputTokens: result.Usage.InputTokens, CachedInputTokens: result.Usage.CachedInputTokens, OutputTokens: result.Usage.OutputTokens,
			ReasoningTokens: result.Usage.ReasoningTokens, TotalTokens: result.Usage.TotalTokens, AverageDurationMS: result.Usage.AverageDurationMS,
			SuccessRate: result.Usage.SuccessRate, EstimatedCostInUSDTicks: result.Usage.EstimatedCostInUSDTicks,
		},
		Pricing: pricingResponse{
			Source: auditdomain.OfficialPricingSource, AsOf: auditdomain.OfficialPricingAsOf,
			PricedRequests: result.Usage.PricedRequests, UnpricedRequests: result.Usage.UnpricedRequests,
			PricedTokens: result.Usage.PricedTokens, UnpricedTokens: result.Usage.UnpricedTokens,
		},
	})
}

func (h *Handler) degradeAccounts(c *gin.Context) {
	softTPS, _ := strconv.ParseFloat(c.Query("softTPS"), 64)
	hardTPS, _ := strconv.ParseFloat(c.Query("hardTPS"), 64)
	minGenMS, _ := strconv.ParseInt(c.Query("minGenMs"), 10, 64)
	failClosed, _ := strconv.ParseBool(c.Query("failClosed"))
	minHits, _ := strconv.Atoi(c.Query("minHits"))
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("pageSize"))
	result, err := h.service.DegradeSummary(
		c.Request.Context(), c.DefaultQuery("window", "24h"),
		auditapp.DegradeThresholds{SoftTPS: softTPS, HardTPS: hardTPS, MinGenMS: minGenMS, FailClosed: failClosed},
		auditapp.DegradeAccountFilter{
			Search: c.Query("search"), Status: c.Query("status"), Class: c.Query("class"), MinHits: minHits,
			Page: page, PageSize: pageSize,
		},
	)
	if errors.Is(err, auditapp.ErrInvalidPeriod) {
		response.Error(c, http.StatusBadRequest, "invalidAuditPeriod", "window 仅支持 1h、6h、24h、7d")
		return
	}
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "auditDegradeFailed", "读取降智账号失败")
		return
	}
	accounts := make([]degradeAccountResponse, 0, len(result.Accounts))
	for _, account := range result.Accounts {
		accounts = append(accounts, degradeAccountResponse{
			ID: strconv.FormatUint(account.ID, 10), Name: account.Name, Email: account.Email, Hits: account.Hits,
			MaxTPS: account.MaxTPS, Classes: account.Classes, Nodes: account.Nodes, Last: account.Last,
			Enabled: account.Enabled, Found: account.Found, BFS: account.BFS,
		})
	}
	events := make([]degradeEventResponse, 0, len(result.Events))
	for _, event := range result.Events {
		item := degradeEventResponse{
			ID: strconv.FormatUint(event.ID, 10), RequestID: event.RequestID, AccountName: event.AccountName,
			NodeName: event.NodeName, OutputTokens: event.OutputTokens, TPS: event.TPS, Class: event.Class,
			CreatedAt: event.CreatedAt, Model: event.Model,
		}
		if event.AccountID != nil {
			id := strconv.FormatUint(*event.AccountID, 10)
			item.AccountID = &id
		}
		events = append(events, item)
	}
	response.Success(c, http.StatusOK, degradeSummaryResponse{
		Window: result.Window, GeneratedAt: result.GeneratedAt,
		Thresholds: degradeThresholdsResponse{SoftTPS: result.Thresholds.SoftTPS, HardTPS: result.Thresholds.HardTPS, MinGenMS: result.Thresholds.MinGenMS, MinOut: result.Thresholds.MinOut},
		Totals: degradeTotalsResponse{
			Hits: result.Totals.Hits, Accounts: result.Totals.Accounts, StillEnabled: result.Totals.StillEnabled,
			Disabled: result.Totals.Disabled, Deleted: result.Totals.Deleted, Hard: result.Totals.Hard,
			Soft: result.Totals.Soft, Burst: result.Totals.Burst, MaxTPS: result.Totals.MaxTPS,
		},
		Series: result.Series, Nodes: result.Nodes, Accounts: accounts,
		AccountPage: degradeAccountPageResponse{
			Page: result.AccountPage.Page, PageSize: result.AccountPage.PageSize,
			Total: result.AccountPage.Total, HasMore: result.AccountPage.HasMore,
		},
		Events: events,
	})
}

type degradeSummaryResponse struct {
	Window      string                     `json:"window"`
	GeneratedAt time.Time                  `json:"generatedAt"`
	Thresholds  degradeThresholdsResponse  `json:"thresholds"`
	Totals      degradeTotalsResponse      `json:"totals"`
	Series      []auditapp.DegradeBucket   `json:"series"`
	Nodes       []auditapp.DegradeNode     `json:"nodes"`
	Accounts    []degradeAccountResponse   `json:"accounts"`
	AccountPage degradeAccountPageResponse `json:"accountPage"`
	Events      []degradeEventResponse     `json:"events"`
}

type degradeAccountPageResponse struct {
	Page     int   `json:"page"`
	PageSize int   `json:"pageSize"`
	Total    int64 `json:"total"`
	HasMore  bool  `json:"hasMore"`
}

type degradeThresholdsResponse struct {
	SoftTPS  float64 `json:"softTPS"`
	HardTPS  float64 `json:"hardTPS"`
	MinGenMS int64   `json:"minGenMs"`
	MinOut   int64   `json:"minOutputTokens"`
}

type degradeTotalsResponse struct {
	Hits         int64   `json:"hits"`
	Accounts     int64   `json:"accounts"`
	StillEnabled int64   `json:"stillEnabled"`
	Disabled     int64   `json:"disabled"`
	Deleted      int64   `json:"deleted"`
	Hard         int64   `json:"hard"`
	Soft         int64   `json:"soft"`
	Burst        int64   `json:"burst"`
	MaxTPS       float64 `json:"maxTPS"`
}

type degradeAccountResponse struct {
	ID      string           `json:"id"`
	Name    string           `json:"name"`
	Email   string           `json:"email"`
	Hits    int64            `json:"hits"`
	MaxTPS  float64          `json:"maxTPS"`
	Classes map[string]int64 `json:"classes"`
	Nodes   []string         `json:"nodes"`
	Last    time.Time        `json:"last"`
	Enabled bool             `json:"enabled"`
	Found   bool             `json:"found"`
	BFS     int              `json:"bfs"`
}

type degradeEventResponse struct {
	ID           string    `json:"id"`
	RequestID    string    `json:"requestId"`
	AccountID    *string   `json:"accountId,omitempty"`
	AccountName  string    `json:"accountName"`
	NodeName     string    `json:"nodeName"`
	OutputTokens int64     `json:"outputTokens"`
	TPS          float64   `json:"tps"`
	Class        string    `json:"class"`
	CreatedAt    time.Time `json:"createdAt"`
	Model        string    `json:"model"`
}

func newListFilter(c *gin.Context) auditapp.ListFilter {
	return auditapp.ListFilter{
		Model: c.Query("model"), Status: c.Query("status"), Mode: c.Query("mode"),
		Key: c.Query("key"), Account: c.Query("account"),
		Sort: repository.SortQuery{Field: c.Query("sortBy"), Direction: repository.SortDirection(c.Query("sortOrder"))},
	}
}

func newAuditResponse(value auditdomain.Record) auditResponse {
	return auditResponse{
		ID: value.ID, RequestID: value.RequestID, ClientKeyID: value.ClientKeyID, ClientKeyName: value.ClientKeyName,
		ModelRouteID: value.ModelRouteID, ModelPublicID: value.ModelPublicID, ModelUpstreamModel: value.ModelUpstreamModel,
		Provider: value.Provider, Operation: string(value.Operation), UsageSource: string(value.UsageSource),
		AccountID: value.AccountID, AccountName: value.AccountName,
		EgressNodeID: value.EgressNodeID, EgressNodeName: value.EgressNodeName, EgressScope: value.EgressScope, EgressMode: string(value.EgressMode),
		StatusCode: value.StatusCode, Streaming: value.Streaming,
		MediaInputImages: value.MediaInputImages, MediaOutputImages: value.MediaOutputImages, MediaOutputSeconds: value.MediaOutputSeconds,
		InputTokens: value.InputTokens, CachedInputTokens: value.CachedInputTokens, OutputTokens: value.OutputTokens,
		ReasoningTokens: value.ReasoningTokens, TotalTokens: value.TotalTokens, CostInUSDTicks: value.CostInUSDTicks,
		EstimatedCostInUSDTicks: value.EstimatedCostInUSDTicks, PricingModel: value.PricingModel, PricingVersion: value.PricingVersion,
		Billing:        newBillingBreakdown(value),
		NumSourcesUsed: value.NumSourcesUsed, NumServerSideToolsUsed: value.NumServerSideToolsUsed,
		ContextInputTokens: value.ContextInputTokens, ContextOutputTokens: value.ContextOutputTokens,
		FirstTokenMS: value.FirstTokenMS, OutputTokensPerSecond: auditOutputTokensPerSecond(value), DurationMS: value.DurationMS,
		ErrorCode: value.ErrorCode, AttemptCount: value.AttemptCount, CreatedAt: value.CreatedAt,
	}
}

func newBillingBreakdown(value auditdomain.Record) *billingBreakdownResponse {
	if value.CostInUSDTicks > 0 {
		return &billingBreakdownResponse{
			Source: "upstream", Method: "upstream_reported", Components: []billingComponentResponse{}, TotalInUSDTicks: value.CostInUSDTicks,
		}
	}
	if value.PricingModel == "" {
		return nil
	}
	breakdown := &billingBreakdownResponse{
		Source: "official", Method: "stored_estimate", Model: value.PricingModel, Version: value.PricingVersion,
		Components: []billingComponentResponse{}, TotalInUSDTicks: value.EstimatedCostInUSDTicks,
	}
	if value.PricingVersion != auditdomain.OfficialPricingAsOf {
		return breakdown
	}
	pricing, ok := auditdomain.ReconstructOfficialCost(
		value.PricingModel,
		value.InputTokens,
		value.CachedInputTokens,
		value.OutputTokens,
		value.ContextInputTokens,
		value.MediaInputImages,
		value.MediaOutputImages,
		value.MediaOutputSeconds,
	)
	if !ok || pricing.CostInUSDTicks != value.EstimatedCostInUSDTicks {
		return breakdown
	}
	breakdown.Method = "official_rates"
	breakdown.Model = pricing.Model
	breakdown.Tier = string(pricing.Tier)
	breakdown.Components = make([]billingComponentResponse, 0, len(pricing.Components))
	for _, component := range pricing.Components {
		breakdown.Components = append(breakdown.Components, billingComponentResponse{
			Kind: string(component.Kind), Unit: string(component.Unit), Quantity: component.Quantity,
			UnitPriceInUSDTicks: component.UnitPriceInUSDTicks, SubtotalInUSDTicks: component.CostInUSDTicks,
		})
	}
	return breakdown
}

func auditOutputTokensPerSecond(value auditdomain.Record) *float64 {
	if !value.Streaming || value.StatusCode < 200 || value.StatusCode >= 300 || value.ErrorCode != "" || value.FirstTokenMS == nil || value.OutputTokens <= 0 || value.DurationMS <= *value.FirstTokenMS {
		return nil
	}
	throughput := auditdomain.OutputTokensPerSecond(value.OutputTokens, value.ReasoningTokens, *value.FirstTokenMS, value.DurationMS)
	if throughput <= 0 {
		return nil
	}
	return &throughput
}
