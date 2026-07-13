package clientkey

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type Handler struct{ service *clientkeyapp.Service }

func NewHandler(service *clientkeyapp.Service) *Handler { return &Handler{service: service} }

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/client-keys", h.list)
	router.POST("/client-keys", h.create)
	router.PATCH("/client-keys/batch", h.batchUpdate)
	router.DELETE("/client-keys", h.batchDelete)
	router.GET("/client-keys/:id/secret", h.revealSecret)
	router.PATCH("/client-keys/:id", h.update)
	router.DELETE("/client-keys/:id", h.delete)
}

type createRequest struct {
	Name                 string   `json:"name" binding:"required"`
	Enabled              *bool    `json:"enabled"`
	ExpiresAt            string   `json:"expiresAt"`
	RPMLimit             int      `json:"rpmLimit"`
	MaxConcurrent        int      `json:"maxConcurrent"`
	BillingLimitUSDTicks int64    `json:"billingLimitUsdTicks"`
	AllowedModelIDs      []string `json:"allowedModelIds"`
}

type updateRequest struct {
	Name                 *string   `json:"name"`
	Enabled              *bool     `json:"enabled"`
	ExpiresAt            *string   `json:"expiresAt"`
	RPMLimit             *int      `json:"rpmLimit"`
	MaxConcurrent        *int      `json:"maxConcurrent"`
	BillingLimitUSDTicks *int64    `json:"billingLimitUsdTicks"`
	AllowedModelIDs      *[]string `json:"allowedModelIds"`
}

type batchUpdateRequest struct {
	IDs     []string `json:"ids" binding:"required"`
	Enabled bool     `json:"enabled"`
}

type batchDeleteRequest struct {
	IDs []string `json:"ids" binding:"required"`
}

type keyResponse struct {
	ID                   uint64     `json:"id,string"`
	Name                 string     `json:"name"`
	Prefix               string     `json:"prefix"`
	Enabled              bool       `json:"enabled"`
	ExpiresAt            *time.Time `json:"expiresAt,omitempty"`
	RPMLimit             int        `json:"rpmLimit"`
	MaxConcurrent        int        `json:"maxConcurrent"`
	BillingLimitUSDTicks int64      `json:"billingLimitUsdTicks"`
	BilledUsageUSDTicks  int64      `json:"billedUsageUsdTicks"`
	AllowedModelIDs      []string   `json:"allowedModelIds"`
	LastUsedAt           *time.Time `json:"lastUsedAt,omitempty"`
}

func (h *Handler) list(c *gin.Context) {
	page, pageSize := pagination(c)
	values, total, err := h.service.List(c.Request.Context(), page, pageSize, c.Query("search"), clientkeyapp.ListFilter{Status: c.Query("status"), ModelScope: c.Query("modelScope"), Sort: repository.SortQuery{Field: c.Query("sortBy"), Direction: repository.SortDirection(c.Query("sortOrder"))}})
	if errors.Is(err, clientkeyapp.ErrInvalidFilter) {
		response.Error(c, http.StatusBadRequest, "invalidFilter", err.Error())
		return
	}
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "clientKeyListFailed", "读取客户端 Key 失败")
		return
	}
	items := make([]keyResponse, 0, len(values))
	for _, value := range values {
		items = append(items, newKeyResponse(value))
	}
	response.Success(c, http.StatusOK, gin.H{"items": items, "page": page, "pageSize": pageSize, "total": total})
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
	updated, err := h.service.BatchSetEnabled(c.Request.Context(), ids, request.Enabled)
	if err != nil {
		h.writeServiceError(c, "clientKeyBatchUpdateFailed", err)
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
		h.writeServiceError(c, "clientKeyBatchDeleteFailed", err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": deleted})
}

func (h *Handler) create(c *gin.Context) {
	var request createRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	expiresAt, err := parseTime(request.ExpiresAt)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidExpiresAt", "expiresAt 必须是 RFC3339 时间")
		return
	}
	modelIDs, err := parseIDs(request.AllowedModelIDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidModelId", "allowedModelIds 包含无效 ID")
		return
	}
	enabled := true
	if request.Enabled != nil {
		enabled = *request.Enabled
	}
	created, err := h.service.Create(c.Request.Context(), clientkeyapp.CreateInput{Name: request.Name, Enabled: enabled, ExpiresAt: expiresAt, RPMLimit: request.RPMLimit, MaxConcurrent: request.MaxConcurrent, BillingLimitUSDTicks: request.BillingLimitUSDTicks, AllowedModels: modelIDs})
	if err != nil {
		h.writeServiceError(c, "clientKeyCreateFailed", err)
		return
	}
	response.Success(c, http.StatusCreated, gin.H{"key": newKeyResponse(created.Key), "secret": created.Secret})
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
	input := clientkeyapp.UpdateInput{Name: request.Name, Enabled: request.Enabled, RPMLimit: request.RPMLimit, MaxConcurrent: request.MaxConcurrent, BillingLimitUSDTicks: request.BillingLimitUSDTicks}
	if request.ExpiresAt != nil {
		if *request.ExpiresAt == "" {
			input.ClearExpiresAt = true
		} else {
			expiresAt, err := parseTime(*request.ExpiresAt)
			if err != nil {
				response.Error(c, http.StatusBadRequest, "invalidExpiresAt", "expiresAt 必须是 RFC3339 时间")
				return
			}
			input.ExpiresAt = expiresAt
		}
	}
	if request.AllowedModelIDs != nil {
		ids, err := parseIDs(*request.AllowedModelIDs)
		if err != nil {
			response.Error(c, http.StatusBadRequest, "invalidModelId", "allowedModelIds 包含无效 ID")
			return
		}
		input.AllowedModels = &ids
	}
	value, err := h.service.Update(c.Request.Context(), id, input)
	if err != nil {
		h.writeServiceError(c, "clientKeyUpdateFailed", err)
		return
	}
	response.Success(c, http.StatusOK, newKeyResponse(value))
}

func (h *Handler) revealSecret(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	secret, err := h.service.RevealSecret(c.Request.Context(), id)
	if err != nil {
		h.writeServiceError(c, "clientKeySecretReadFailed", err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	response.Success(c, http.StatusOK, gin.H{"secret": secret})
}

func (h *Handler) delete(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	if err := h.service.Delete(c.Request.Context(), id); err != nil {
		h.writeServiceError(c, "clientKeyDeleteFailed", err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": true})
}

// writeServiceError 仅暴露明确的客户端 Key 业务错误，避免泄露持久化细节。
func (h *Handler) writeServiceError(c *gin.Context, code string, err error) {
	switch {
	case errors.Is(err, clientkeyapp.ErrInvalidInput):
		response.Error(c, http.StatusBadRequest, code, err.Error())
	case errors.Is(err, clientkeyapp.ErrNotFound):
		response.Error(c, http.StatusNotFound, "clientKeyNotFound", err.Error())
	case errors.Is(err, clientkeyapp.ErrConflict):
		response.Error(c, http.StatusConflict, "clientKeyConflict", err.Error())
	case errors.Is(err, clientkeyapp.ErrSecretUnavailable):
		response.Error(c, http.StatusConflict, "clientKeySecretUnavailable", err.Error())
	default:
		response.Error(c, http.StatusInternalServerError, code, "客户端 Key 操作失败")
	}
}

func newKeyResponse(value clientkeydomain.Key) keyResponse {
	ids := make([]string, 0, len(value.AllowedModels))
	for _, id := range value.AllowedModels {
		ids = append(ids, strconv.FormatUint(id, 10))
	}
	return keyResponse{ID: value.ID, Name: value.Name, Prefix: value.Prefix, Enabled: value.Enabled, ExpiresAt: value.ExpiresAt, RPMLimit: value.RPMLimit, MaxConcurrent: value.MaxConcurrent, BillingLimitUSDTicks: value.BillingLimitUSDTicks, BilledUsageUSDTicks: value.BilledUsageUSDTicks, AllowedModelIDs: ids, LastUsedAt: value.LastUsedAt}
}

func parseTime(value string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, err
	}
	parsed = parsed.UTC()
	return &parsed, nil
}

func parseIDs(values []string) ([]uint64, error) {
	result := make([]uint64, 0, len(values))
	for _, value := range values {
		id, err := strconv.ParseUint(value, 10, 64)
		if err != nil || id == 0 {
			return nil, fmt.Errorf("无效模型 ID: %s", value)
		}
		result = append(result, id)
	}
	return result, nil
}

func pathID(c *gin.Context) (uint64, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		response.Error(c, http.StatusBadRequest, "invalidId", "ID 无效")
		return 0, false
	}
	return id, true
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
