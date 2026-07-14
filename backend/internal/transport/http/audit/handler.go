package audit

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	auditapp "github.com/chenyme/grok2api/backend/internal/application/audit"
	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type Handler struct{ service *auditapp.Service }

func NewHandler(service *auditapp.Service) *Handler { return &Handler{service: service} }

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/request-audits", h.list)
	router.GET("/request-audits/summary", h.summary)
}

type auditResponse struct {
	ID                      uint64    `json:"id,string"`
	RequestID               string    `json:"requestId"`
	ClientKeyID             uint64    `json:"clientKeyId,string"`
	ClientKeyName           string    `json:"clientKeyName,omitempty"`
	ModelRouteID            uint64    `json:"modelRouteId,string"`
	ModelPublicID           string    `json:"modelPublicId,omitempty"`
	ModelUpstreamModel      string    `json:"modelUpstreamModel,omitempty"`
	Provider                string    `json:"provider"`
	Operation               string    `json:"operation"`
	UsageSource             string    `json:"usageSource"`
	AccountID               *uint64   `json:"accountId,string,omitempty"`
	AccountName             string    `json:"accountName,omitempty"`
	StatusCode              int       `json:"statusCode"`
	Streaming               bool      `json:"streaming"`
	MediaInputImages        int64     `json:"mediaInputImages"`
	MediaOutputImages       int64     `json:"mediaOutputImages"`
	MediaOutputSeconds      int64     `json:"mediaOutputSeconds"`
	InputTokens             int64     `json:"inputTokens"`
	CachedInputTokens       int64     `json:"cachedInputTokens"`
	OutputTokens            int64     `json:"outputTokens"`
	ReasoningTokens         int64     `json:"reasoningTokens"`
	TotalTokens             int64     `json:"totalTokens"`
	CostInUSDTicks          int64     `json:"costInUsdTicks"`
	EstimatedCostInUSDTicks int64     `json:"estimatedCostInUsdTicks"`
	PricingModel            string    `json:"pricingModel,omitempty"`
	PricingVersion          string    `json:"pricingVersion,omitempty"`
	NumSourcesUsed          int64     `json:"numSourcesUsed"`
	NumServerSideToolsUsed  int64     `json:"numServerSideToolsUsed"`
	ContextInputTokens      int64     `json:"contextInputTokens"`
	ContextOutputTokens     int64     `json:"contextOutputTokens"`
	DurationMS              int64     `json:"durationMs"`
	ErrorCode               string    `json:"errorCode,omitempty"`
	CreatedAt               time.Time `json:"createdAt"`
}

func (h *Handler) list(c *gin.Context) {
	if c.Query("pagination") == "cursor" {
		h.listCursor(c)
		return
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("pageSize", "20"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
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
		AccountID: value.AccountID, AccountName: value.AccountName, StatusCode: value.StatusCode, Streaming: value.Streaming,
		MediaInputImages: value.MediaInputImages, MediaOutputImages: value.MediaOutputImages, MediaOutputSeconds: value.MediaOutputSeconds,
		InputTokens: value.InputTokens, CachedInputTokens: value.CachedInputTokens, OutputTokens: value.OutputTokens,
		ReasoningTokens: value.ReasoningTokens, TotalTokens: value.TotalTokens, CostInUSDTicks: value.CostInUSDTicks,
		EstimatedCostInUSDTicks: value.EstimatedCostInUSDTicks, PricingModel: value.PricingModel, PricingVersion: value.PricingVersion,
		NumSourcesUsed: value.NumSourcesUsed, NumServerSideToolsUsed: value.NumServerSideToolsUsed,
		ContextInputTokens: value.ContextInputTokens, ContextOutputTokens: value.ContextOutputTokens, DurationMS: value.DurationMS,
		ErrorCode: value.ErrorCode, CreatedAt: value.CreatedAt,
	}
}
