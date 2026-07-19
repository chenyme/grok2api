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

type Handler struct{ service *auditapp.Service }

func NewHandler(service *auditapp.Service) *Handler { return &Handler{service: service} }

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/request-audits", h.list)
	router.GET("/request-audits/summary", h.summary)
	router.GET("/request-audits/:id", h.get)
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
	EgressNodeID            *uint64   `json:"egressNodeId,string,omitempty"`
	EgressNodeName          string    `json:"egressNodeName,omitempty"`
	EgressScope             string    `json:"egressScope,omitempty"`
	EgressMode              string    `json:"egressMode,omitempty"`
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
	AttemptCount            int       `json:"attemptCount"`
	CreatedAt               time.Time `json:"createdAt"`
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
		NumSourcesUsed: value.NumSourcesUsed, NumServerSideToolsUsed: value.NumServerSideToolsUsed,
		ContextInputTokens: value.ContextInputTokens, ContextOutputTokens: value.ContextOutputTokens, DurationMS: value.DurationMS,
		ErrorCode: value.ErrorCode, AttemptCount: value.AttemptCount, CreatedAt: value.CreatedAt,
	}
}
