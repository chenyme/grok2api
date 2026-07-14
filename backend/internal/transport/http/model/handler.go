package model

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type Handler struct{ service *modelapp.Service }

func NewHandler(service *modelapp.Service) *Handler { return &Handler{service: service} }

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/models", h.list)
	router.GET("/models/accounts", h.listAccounts)
	router.POST("/models", h.create)
	router.POST("/models/sync", h.sync)
	router.PATCH("/models/batch", h.batchUpdate)
	router.DELETE("/models", h.batchDelete)
	router.PATCH("/models/:id", h.update)
	router.DELETE("/models/:id", h.delete)
}

type updateRequest struct {
	PublicID   *string   `json:"publicId"`
	Enabled    *bool     `json:"enabled"`
	AccountIDs *[]string `json:"accountIds"`
}

type createRequest struct {
	PublicID      string   `json:"publicId" binding:"required"`
	Provider      string   `json:"provider" binding:"required"`
	UpstreamModel string   `json:"upstreamModel" binding:"required"`
	Capability    string   `json:"capability" binding:"required"`
	Enabled       bool     `json:"enabled"`
	AccountIDs    []string `json:"accountIds"`
}

type batchUpdateRequest struct {
	IDs     []string `json:"ids" binding:"required"`
	Enabled bool     `json:"enabled"`
}

type batchDeleteRequest struct {
	IDs []string `json:"ids" binding:"required"`
}

type modelResponse struct {
	ID                uint64     `json:"id,string"`
	PublicID          string     `json:"publicId"`
	Provider          string     `json:"provider"`
	UpstreamModel     string     `json:"upstreamModel"`
	Capability        string     `json:"capability"`
	Enabled           bool       `json:"enabled"`
	Origin            string     `json:"origin"`
	AccountIDs        []string   `json:"accountIds"`
	BindingMode       bool       `json:"bindingMode"`
	SupportedAccounts int        `json:"supportedAccounts"`
	SyncedAccounts    int        `json:"syncedAccounts"`
	TotalAccounts     int        `json:"totalAccounts"`
	CapabilityKnown   bool       `json:"capabilityKnown"`
	Available         bool       `json:"available"`
	LastSyncedAt      *time.Time `json:"lastSyncedAt,omitempty"`
}

type accountOptionResponse struct {
	ID   uint64 `json:"id,string"`
	Name string `json:"name"`
}

func (h *Handler) list(c *gin.Context) {
	page, pageSize := pagination(c)
	values, total, err := h.service.List(c.Request.Context(), page, pageSize, c.Query("search"), modelapp.ListFilter{Provider: c.Query("provider"), Status: c.Query("status"), Sort: repository.SortQuery{Field: c.Query("sortBy"), Direction: repository.SortDirection(c.Query("sortOrder"))}})
	if errors.Is(err, modelapp.ErrInvalidFilter) {
		response.Error(c, http.StatusBadRequest, "invalidFilter", err.Error())
		return
	}
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "modelListFailed", "读取模型失败")
		return
	}
	items := make([]modelResponse, 0, len(values))
	for _, value := range values {
		items = append(items, newModelResponse(value))
	}
	response.Success(c, http.StatusOK, gin.H{"items": items, "page": page, "pageSize": pageSize, "total": total})
}

func (h *Handler) listAccounts(c *gin.Context) {
	values, err := h.service.ListBindableAccounts(c.Request.Context(), account.Provider(c.Query("provider")))
	if err != nil {
		h.writeServiceError(c, "modelAccountListFailed", err)
		return
	}
	items := make([]accountOptionResponse, 0, len(values))
	for _, value := range values {
		items = append(items, accountOptionResponse{ID: value.ID, Name: value.Name})
	}
	response.Success(c, http.StatusOK, gin.H{"items": items})
}

func (h *Handler) create(c *gin.Context) {
	var request createRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	accountIDs, err := parseIDs(request.AccountIDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", err.Error())
		return
	}
	value, err := h.service.Create(c.Request.Context(), modelapp.CreateInput{
		PublicID: request.PublicID, Provider: account.Provider(request.Provider), UpstreamModel: request.UpstreamModel,
		Capability: modeldomain.Capability(request.Capability), Enabled: request.Enabled, AccountIDs: accountIDs,
	})
	if err != nil {
		h.writeServiceError(c, "modelCreateFailed", err)
		return
	}
	response.Success(c, http.StatusCreated, newModelResponse(value))
}

func (h *Handler) batchUpdate(c *gin.Context) {
	var request batchUpdateRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	ids := make([]uint64, 0, len(request.IDs))
	for _, value := range request.IDs {
		id, err := strconv.ParseUint(value, 10, 64)
		if err != nil || id == 0 {
			response.Error(c, http.StatusBadRequest, "invalidId", fmt.Sprintf("无效模型 ID: %s", value))
			return
		}
		ids = append(ids, id)
	}
	updated, err := h.service.BatchSetEnabled(c.Request.Context(), ids, request.Enabled)
	if err != nil {
		h.writeServiceError(c, "modelBatchUpdateFailed", err)
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
	deleted, err := h.service.BatchDelete(c.Request.Context(), ids)
	if err != nil {
		h.writeServiceError(c, "modelBatchDeleteFailed", err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": deleted})
}

func (h *Handler) sync(c *gin.Context) {
	count, err := h.service.Sync(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusBadGateway, "modelSyncFailed", "同步上游模型失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"synced": count})
}

func (h *Handler) update(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		response.Error(c, http.StatusBadRequest, "invalidId", "ID 无效")
		return
	}
	var request updateRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	var accountIDs *[]uint64
	if request.AccountIDs != nil {
		parsed, parseErr := parseIDs(*request.AccountIDs)
		if parseErr != nil {
			response.Error(c, http.StatusBadRequest, "invalidId", parseErr.Error())
			return
		}
		accountIDs = &parsed
	}
	value, err := h.service.Update(c.Request.Context(), id, modelapp.UpdateInput{PublicID: request.PublicID, Enabled: request.Enabled, AccountIDs: accountIDs})
	if err != nil {
		h.writeServiceError(c, "modelUpdateFailed", err)
		return
	}
	response.Success(c, http.StatusOK, newModelResponse(value))
}

func (h *Handler) delete(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		response.Error(c, http.StatusBadRequest, "invalidId", "ID 无效")
		return
	}
	if err := h.service.Delete(c.Request.Context(), id); err != nil {
		h.writeServiceError(c, "modelDeleteFailed", err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": true})
}

// writeServiceError 仅暴露明确的模型业务错误，避免泄露持久化细节。
func (h *Handler) writeServiceError(c *gin.Context, code string, err error) {
	switch {
	case errors.Is(err, modelapp.ErrInvalidInput):
		response.Error(c, http.StatusBadRequest, code, err.Error())
	case errors.Is(err, modelapp.ErrNotFound):
		response.Error(c, http.StatusNotFound, "modelNotFound", err.Error())
	case errors.Is(err, modelapp.ErrConflict):
		response.Error(c, http.StatusConflict, "modelConflict", err.Error())
	default:
		response.Error(c, http.StatusInternalServerError, code, "模型操作失败")
	}
}

func newModelResponse(value modeldomain.Route) modelResponse {
	manualBinding := len(value.BoundAccountIDs) > 0
	capabilityKnown := manualBinding || value.SyncedAccounts > 0
	available := value.TotalAccounts > 0 && (value.SupportedAccounts > 0 || (!manualBinding && value.SyncedAccounts < value.TotalAccounts))
	accountIDs := make([]string, 0, len(value.BoundAccountIDs))
	for _, id := range value.BoundAccountIDs {
		accountIDs = append(accountIDs, strconv.FormatUint(id, 10))
	}
	return modelResponse{
		ID: value.ID, PublicID: modeldomain.ExternalPublicID(value.Provider, value.PublicID), Provider: string(value.Provider), UpstreamModel: modeldomain.DisplayUpstreamModel(value.Provider, value.UpstreamModel), Capability: string(value.Capability),
		Enabled: value.Enabled, Origin: string(value.Origin), AccountIDs: accountIDs, BindingMode: manualBinding, SupportedAccounts: value.SupportedAccounts,
		SyncedAccounts: value.SyncedAccounts, TotalAccounts: value.TotalAccounts, CapabilityKnown: capabilityKnown,
		Available: available, LastSyncedAt: value.LastSyncedAt,
	}
}

func parseIDs(values []string) ([]uint64, error) {
	ids := make([]uint64, 0, len(values))
	for _, value := range values {
		id, err := strconv.ParseUint(value, 10, 64)
		if err != nil || id == 0 {
			return nil, fmt.Errorf("无效 ID: %s", value)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func pagination(c *gin.Context) (int, int) {
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
	return page, pageSize
}
