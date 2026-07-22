package egress

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type Handler struct{ service *egressapp.Service }

func NewHandler(service *egressapp.Service) *Handler { return &Handler{service: service} }

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/egress-nodes", h.list)
	router.POST("/egress-nodes", h.create)
	router.DELETE("/egress-nodes", h.deleteMany)
	router.POST("/egress-nodes/test", h.testNodes)
	router.POST("/egress-nodes/:id/test", h.testNode)
	router.POST("/egress-nodes/:id/accounts", h.assignAccounts)
	router.DELETE("/egress-nodes/accounts", h.unassignAccounts)
	router.PUT("/egress-nodes/:id", h.update)
	router.POST("/egress-nodes/:id/refresh-clearance", h.refreshClearance)
	router.DELETE("/egress-nodes/:id", h.delete)
	router.POST("/egress-imports", h.importText)
	router.GET("/egress-sources", h.listSources)
	router.POST("/egress-sources", h.createSource)
	router.POST("/egress-sources/:id/sync", h.syncSource)
	router.PUT("/egress-sources/:id", h.updateSource)
	router.DELETE("/egress-sources/:id", h.deleteSource)
	router.GET("/egress-operations", h.operationsConfig)
	router.PUT("/egress-operations", h.updateOperationsConfig)
	router.POST("/egress-operations/rebalance", h.rebalance)
}

func (h *Handler) refreshClearance(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	if err := h.service.RefreshClearance(c.Request.Context(), id); err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"refreshed": true})
}

type nodeRequest struct {
	Name              string  `json:"name"`
	Scope             string  `json:"scope"`
	Enabled           bool    `json:"enabled"`
	ProxyPool         *bool   `json:"proxyPool"`
	AccountCapacity   *int    `json:"accountCapacity"`
	ProxyURL          *string `json:"proxyURL"`
	ClearProxyURL     bool    `json:"clearProxyURL"`
	UserAgent         string  `json:"userAgent"`
	CloudflareCookies *string `json:"cloudflareCookies"`
	ClearCookies      bool    `json:"clearCookies"`
}

type nodeResponse struct {
	ID                   uint64     `json:"id,string"`
	Name                 string     `json:"name"`
	Scope                string     `json:"scope"`
	Enabled              bool       `json:"enabled"`
	ProxyConfigured      bool       `json:"proxyConfigured"`
	ProxyPool            bool       `json:"proxyPool"`
	SourceID             uint64     `json:"sourceId,omitempty,string"`
	AccountCapacity      int        `json:"accountCapacity"`
	UserAgent            string     `json:"userAgent"`
	CookieConfigured     bool       `json:"cookieConfigured"`
	AccountBoundProxy    bool       `json:"accountBoundProxy"`
	Health               float64    `json:"health"`
	FailureCount         int        `json:"failureCount"`
	CooldownUntil        *time.Time `json:"cooldownUntil,omitempty"`
	LastError            string     `json:"lastError,omitempty"`
	ProbeStatus          string     `json:"probeStatus"`
	LastProbedAt         *time.Time `json:"lastProbedAt,omitempty"`
	ProbeLatencyMS       int        `json:"probeLatencyMs"`
	ExitIP               string     `json:"exitIp,omitempty"`
	ProbeError           string     `json:"probeError,omitempty"`
	AssignedAccountCount int        `json:"assignedAccountCount"`
}

type accountAssignmentRequest struct {
	Provider string   `json:"provider" binding:"required"`
	IDs      []string `json:"ids" binding:"required"`
	Mode     string   `json:"mode"`
}

type batchNodeDeleteRequest struct {
	IDs []string `json:"ids" binding:"required"`
}

func (h *Handler) deleteMany(c *gin.Context) {
	var request batchNodeDeleteRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	ids, err := parseEgressNodeIDs(request.IDs)
	if err != nil || len(ids) > 5000 {
		response.Error(c, http.StatusBadRequest, "invalidId", "代理节点 ID 无效")
		return
	}
	deleted, err := h.service.DeleteMany(c.Request.Context(), ids)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": deleted})
}

func (h *Handler) assignAccounts(c *gin.Context) {
	nodeID, ok := pathID(c)
	if !ok {
		return
	}
	var request accountAssignmentRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	ids, err := parseAccountIDs(request.IDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", "账号 ID 无效")
		return
	}
	mode := accountdomain.EgressAssignmentMode(request.Mode)
	if mode == "" {
		mode = accountdomain.EgressAssignmentManual
	}
	result, err := h.service.AssignAccounts(c.Request.Context(), nodeID, accountdomain.Provider(request.Provider), ids, mode)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"assigned": result.Assigned})
}

func (h *Handler) unassignAccounts(c *gin.Context) {
	var request accountAssignmentRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	ids, err := parseAccountIDs(request.IDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", "账号 ID 无效")
		return
	}
	result, err := h.service.UnassignAccounts(c.Request.Context(), accountdomain.Provider(request.Provider), ids)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"assigned": result.Assigned})
}

func (value nodeRequest) input() egressapp.Input {
	return egressapp.Input{
		Name: value.Name, Scope: egressdomain.Scope(value.Scope), Enabled: value.Enabled, ProxyPool: value.ProxyPool,
		AccountCapacity: value.AccountCapacity,
		ProxyURL:        value.ProxyURL, ClearProxyURL: value.ClearProxyURL, UserAgent: value.UserAgent,
		CloudflareCookies: value.CloudflareCookies, ClearCookies: value.ClearCookies,
	}
}

func (h *Handler) list(c *gin.Context) {
	scope := egressdomain.Scope(c.Query("scope"))
	if scope != "" && scope != egressdomain.ScopeBuild && scope != egressdomain.ScopeWeb && scope != egressdomain.ScopeConsole && scope != egressdomain.ScopeWebAsset {
		response.Error(c, http.StatusBadRequest, "invalidEgressScope", "scope 必须是 grok_build、grok_web、grok_console 或 grok_web_asset")
		return
	}
	values, err := h.service.List(c.Request.Context(), scope, repository.SortQuery{Field: c.Query("sortBy"), Direction: repository.SortDirection(c.Query("sortOrder"))})
	if errors.Is(err, egressapp.ErrInvalidSort) {
		response.Error(c, http.StatusBadRequest, "invalidSort", err.Error())
		return
	}
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "egressNodeListFailed", "读取代理节点失败")
		return
	}
	items := make([]nodeResponse, 0, len(values))
	for _, value := range values {
		items = append(items, newNodeResponse(value))
	}
	response.Success(c, http.StatusOK, gin.H{"items": items, "defaultUserAgents": h.service.DefaultUserAgents()})
}

func (h *Handler) create(c *gin.Context) {
	var request nodeRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	value, err := h.service.Create(c.Request.Context(), request.input())
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusCreated, newNodeResponse(value))
}

func (h *Handler) update(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var request nodeRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	value, err := h.service.Update(c.Request.Context(), id, request.input())
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, newNodeResponse(value))
}

func newNodeResponse(value egressdomain.PublicNode) nodeResponse {
	return nodeResponse{
		ID: value.ID, Name: value.Name, Scope: string(value.Scope), Enabled: value.Enabled,
		ProxyConfigured: value.ProxyConfigured, ProxyPool: value.ProxyPool, UserAgent: value.UserAgent, CookieConfigured: value.CookieConfigured,
		AccountBoundProxy: value.AccountBoundProxy,
		SourceID:          value.SourceID, AccountCapacity: value.AccountCapacity,
		Health: value.Health, FailureCount: value.FailureCount, CooldownUntil: value.CooldownUntil, LastError: value.LastError,
		ProbeStatus: string(value.ProbeStatus), LastProbedAt: value.LastProbedAt, ProbeLatencyMS: value.ProbeLatencyMS, ExitIP: value.ExitIP, ProbeError: value.ProbeError,
		AssignedAccountCount: value.AssignedAccountCount,
	}
}

func parseAccountIDs(values []string) ([]uint64, error) {
	result := make([]uint64, 0, len(values))
	seen := make(map[uint64]struct{}, len(values))
	for _, value := range values {
		id, err := strconv.ParseUint(value, 10, 64)
		if err != nil || id == 0 {
			return nil, errors.New("invalid id")
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	if len(result) == 0 {
		return nil, errors.New("no ids")
	}
	return result, nil
}

func parseEgressNodeIDs(values []string) ([]uint64, error) {
	result := make([]uint64, 0, len(values))
	seen := make(map[uint64]struct{}, len(values))
	for _, value := range values {
		id, err := strconv.ParseUint(value, 10, 64)
		if err != nil || id == 0 {
			return nil, errors.New("invalid id")
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	if len(result) == 0 {
		return nil, errors.New("no ids")
	}
	return result, nil
}

func (h *Handler) delete(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	if err := h.service.Delete(c.Request.Context(), id); err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": true})
}

type sourceRequest struct {
	Name                   string  `json:"name"`
	Scope                  string  `json:"scope"`
	Enabled                bool    `json:"enabled"`
	URL                    *string `json:"url"`
	ClearURL               bool    `json:"clearUrl"`
	RefreshIntervalSeconds *int    `json:"refreshIntervalSeconds"`
	DefaultAccountCapacity *int    `json:"defaultAccountCapacity"`
}

type sourceResponse struct {
	ID                     uint64     `json:"id,string"`
	Name                   string     `json:"name"`
	Scope                  string     `json:"scope"`
	Enabled                bool       `json:"enabled"`
	URLConfigured          bool       `json:"urlConfigured"`
	RefreshIntervalSeconds int        `json:"refreshIntervalSeconds"`
	DefaultAccountCapacity int        `json:"defaultAccountCapacity"`
	LastSyncedAt           *time.Time `json:"lastSyncedAt,omitempty"`
	NextSyncAt             *time.Time `json:"nextSyncAt,omitempty"`
	LastSyncImported       int        `json:"lastSyncImported"`
	LastSyncError          string     `json:"lastSyncError,omitempty"`
}

type importRequest struct {
	Name            string `json:"name"`
	Scope           string `json:"scope"`
	AccountCapacity int    `json:"accountCapacity"`
	Content         string `json:"content"`
}

type probeBatchRequest struct {
	IDs []string `json:"ids"`
}

type operationsConfigRequest struct {
	ProbeIntervalSeconds      int                                  `json:"probeIntervalSeconds"`
	AutoAssignEnabled         bool                                 `json:"autoAssignEnabled"`
	AutoBalanceEnabled        bool                                 `json:"autoBalanceEnabled"`
	AssignmentIntervalSeconds int                                  `json:"assignmentIntervalSeconds"`
	Fallbacks                 map[string]operationsFallbackRequest `json:"fallbacks"`
}

type operationsFallbackRequest struct {
	Mode   string `json:"mode"`
	NodeID string `json:"nodeId"`
}

type operationsConfigResponse struct {
	ProbeIntervalSeconds      int                                   `json:"probeIntervalSeconds"`
	AutoAssignEnabled         bool                                  `json:"autoAssignEnabled"`
	AutoBalanceEnabled        bool                                  `json:"autoBalanceEnabled"`
	AssignmentIntervalSeconds int                                   `json:"assignmentIntervalSeconds"`
	Fallbacks                 map[string]operationsFallbackResponse `json:"fallbacks"`
	UpdatedAt                 time.Time                             `json:"updatedAt"`
}

type operationsFallbackResponse struct {
	Mode   string `json:"mode"`
	NodeID string `json:"nodeId,omitempty"`
}

func (value operationsConfigRequest) input() (egressapp.OperationsConfigInput, error) {
	result := egressapp.OperationsConfigInput{
		ProbeIntervalSeconds: value.ProbeIntervalSeconds, AutoAssignEnabled: value.AutoAssignEnabled,
		AutoBalanceEnabled: value.AutoBalanceEnabled, AssignmentIntervalSeconds: value.AssignmentIntervalSeconds,
	}
	if value.Fallbacks == nil {
		return result, nil
	}
	result.Fallbacks = make(map[egressdomain.Scope]egressapp.FallbackConfigInput, len(value.Fallbacks))
	for rawScope, fallback := range value.Fallbacks {
		nodeID := uint64(0)
		if strings.TrimSpace(fallback.NodeID) != "" {
			parsed, err := strconv.ParseUint(fallback.NodeID, 10, 64)
			if err != nil || parsed == 0 {
				return egressapp.OperationsConfigInput{}, fmt.Errorf("%w: 固定回退节点 ID 无效", egressapp.ErrInvalidInput)
			}
			nodeID = parsed
		}
		result.Fallbacks[egressdomain.Scope(rawScope)] = egressapp.FallbackConfigInput{
			Mode: egressdomain.FallbackMode(strings.TrimSpace(fallback.Mode)), NodeID: nodeID,
		}
	}
	return result, nil
}

func (value sourceRequest) input() egressapp.SubscriptionSourceInput {
	return egressapp.SubscriptionSourceInput{
		Name: value.Name, Scope: egressdomain.Scope(value.Scope), Enabled: value.Enabled, URL: value.URL, ClearURL: value.ClearURL,
		RefreshIntervalSeconds: value.RefreshIntervalSeconds, DefaultAccountCapacity: value.DefaultAccountCapacity,
	}
}

func newSourceResponse(value egressdomain.PublicSubscriptionSource) sourceResponse {
	return sourceResponse{
		ID: value.ID, Name: value.Name, Scope: string(value.Scope), Enabled: value.Enabled, URLConfigured: value.URLConfigured,
		RefreshIntervalSeconds: value.RefreshIntervalSeconds, DefaultAccountCapacity: value.DefaultAccountCapacity,
		LastSyncedAt: value.LastSyncedAt, NextSyncAt: value.NextSyncAt, LastSyncImported: value.LastSyncImported, LastSyncError: value.LastSyncError,
	}
}

func newOperationsConfigResponse(value egressdomain.OperationsConfig) operationsConfigResponse {
	fallbacks := make(map[string]operationsFallbackResponse, 4)
	for _, scope := range []egressdomain.Scope{egressdomain.ScopeBuild, egressdomain.ScopeWeb, egressdomain.ScopeConsole, egressdomain.ScopeWebAsset} {
		fallback := value.FallbackFor(scope)
		item := operationsFallbackResponse{Mode: string(fallback.Mode)}
		if fallback.NodeID != 0 {
			item.NodeID = strconv.FormatUint(fallback.NodeID, 10)
		}
		fallbacks[string(scope)] = item
	}
	return operationsConfigResponse{
		ProbeIntervalSeconds: value.ProbeIntervalSeconds, AutoAssignEnabled: value.AutoAssignEnabled,
		AutoBalanceEnabled: value.AutoBalanceEnabled, AssignmentIntervalSeconds: value.AssignmentIntervalSeconds,
		Fallbacks: fallbacks, UpdatedAt: value.UpdatedAt,
	}
}

func (h *Handler) listSources(c *gin.Context) {
	values, err := h.service.ListSources(c.Request.Context())
	if err != nil {
		h.writeError(c, err)
		return
	}
	items := make([]sourceResponse, 0, len(values))
	for _, value := range values {
		items = append(items, newSourceResponse(value))
	}
	response.Success(c, http.StatusOK, gin.H{"items": items})
}

func (h *Handler) createSource(c *gin.Context) {
	var request sourceRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	value, err := h.service.CreateSource(c.Request.Context(), request.input())
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusCreated, newSourceResponse(value))
}

func (h *Handler) updateSource(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var request sourceRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	value, err := h.service.UpdateSource(c.Request.Context(), id, request.input())
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, newSourceResponse(value))
}

func (h *Handler) deleteSource(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	if err := h.service.DeleteSource(c.Request.Context(), id); err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": true})
}

func (h *Handler) syncSource(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	value, err := h.service.SyncSource(c.Request.Context(), id)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"imported": value.Imported, "skipped": value.Skipped})
}

func (h *Handler) importText(c *gin.Context) {
	var request importRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	value, err := h.service.ImportText(c.Request.Context(), egressapp.ImportInput{
		Name: request.Name, Scope: egressdomain.Scope(request.Scope), AccountCapacity: request.AccountCapacity, Content: request.Content,
	})
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusCreated, gin.H{"imported": value.Imported, "skipped": value.Skipped})
}

func (h *Handler) testNode(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	value, err := h.service.TestNode(c.Request.Context(), id)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"status": value.Status, "testedAt": value.TestedAt, "latencyMs": value.LatencyMS, "exitIp": value.ExitIP, "error": value.Error})
}

func (h *Handler) testNodes(c *gin.Context) {
	var request probeBatchRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	ids, err := parseOptionalAccountIDs(request.IDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", "账号 ID 无效")
		return
	}
	value, err := h.service.TestNodes(c.Request.Context(), ids)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"requested": value.Requested, "healthy": value.Healthy, "unhealthy": value.Unhealthy})
}

func (h *Handler) operationsConfig(c *gin.Context) {
	value, err := h.service.OperationsConfig(c.Request.Context())
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, newOperationsConfigResponse(value))
}

func (h *Handler) updateOperationsConfig(c *gin.Context) {
	var request operationsConfigRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	input, err := request.input()
	if err != nil {
		h.writeError(c, err)
		return
	}
	value, err := h.service.UpdateOperationsConfig(c.Request.Context(), input)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, newOperationsConfigResponse(value))
}

func (h *Handler) rebalance(c *gin.Context) {
	config, err := h.service.OperationsConfig(c.Request.Context())
	if err != nil {
		h.writeError(c, err)
		return
	}
	value, err := h.service.RebalanceAccounts(c.Request.Context(), true, true, time.Duration(config.ProbeIntervalSeconds)*time.Second)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"assigned": value.Assigned, "rebalanced": value.Rebalanced, "unplaced": value.Unplaced})
}

func parseOptionalAccountIDs(values []string) ([]uint64, error) {
	if len(values) == 0 {
		return nil, nil
	}
	return parseAccountIDs(values)
}

func (h *Handler) writeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, egressapp.ErrInvalidInput):
		response.Error(c, http.StatusBadRequest, "invalidEgressNode", err.Error())
	case errors.Is(err, egressapp.ErrNotFound):
		response.Error(c, http.StatusNotFound, "egressNodeNotFound", err.Error())
	case errors.Is(err, repository.ErrConflict):
		response.Error(c, http.StatusConflict, "egressConflict", "名称已存在")
	case errors.Is(err, egressapp.ErrOperationsUnavailable):
		response.Error(c, http.StatusServiceUnavailable, "egressOperationsUnavailable", "代理运营功能暂不可用")
	case errors.Is(err, egressapp.ErrSubscriptionSync):
		response.Error(c, http.StatusBadGateway, "egressSubscriptionSyncFailed", "代理订阅同步失败")
	case errors.Is(err, egressapp.ErrClearanceUnavailable):
		response.Error(c, http.StatusConflict, "clearanceRefreshUnavailable", err.Error())
	case strings.Contains(err.Error(), "FlareSolverr") || strings.Contains(err.Error(), "Clearance"):
		response.Error(c, http.StatusBadGateway, "clearanceRefreshFailed", err.Error())
	default:
		response.Error(c, http.StatusInternalServerError, "egressNodeOperationFailed", "代理节点操作失败")
	}
}

func pathID(c *gin.Context) (uint64, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		response.Error(c, http.StatusBadRequest, "invalidId", "ID 无效")
		return 0, false
	}
	return id, true
}
