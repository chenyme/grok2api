package egress

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
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
	router.PUT("/egress-nodes/:id", h.update)
	router.DELETE("/egress-nodes/:id", h.delete)
}

type nodeRequest struct {
	Name              string  `json:"name"`
	Scope             string  `json:"scope"`
	Enabled           bool    `json:"enabled"`
	ProxyURL          *string `json:"proxyURL"`
	ClearProxyURL     bool    `json:"clearProxyURL"`
	UserAgent         string  `json:"userAgent"`
	CloudflareCookies *string `json:"cloudflareCookies"`
	ClearCookies      bool    `json:"clearCookies"`
}

type nodeResponse struct {
	ID               uint64     `json:"id,string"`
	Name             string     `json:"name"`
	Scope            string     `json:"scope"`
	Enabled          bool       `json:"enabled"`
	ProxyConfigured  bool       `json:"proxyConfigured"`
	UserAgent        string     `json:"userAgent"`
	CookieConfigured bool       `json:"cookieConfigured"`
	Health           float64    `json:"health"`
	FailureCount     int        `json:"failureCount"`
	CooldownUntil    *time.Time `json:"cooldownUntil,omitempty"`
	LastError        string     `json:"lastError,omitempty"`
}

func (value nodeRequest) input() egressapp.Input {
	return egressapp.Input{
		Name: value.Name, Scope: egressdomain.Scope(value.Scope), Enabled: value.Enabled,
		ProxyURL: value.ProxyURL, ClearProxyURL: value.ClearProxyURL, UserAgent: value.UserAgent,
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
		ProxyConfigured: value.ProxyConfigured, UserAgent: value.UserAgent, CookieConfigured: value.CookieConfigured,
		Health: value.Health, FailureCount: value.FailureCount, CooldownUntil: value.CooldownUntil, LastError: value.LastError,
	}
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

func (h *Handler) writeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, egressapp.ErrInvalidInput):
		response.Error(c, http.StatusBadRequest, "invalidEgressNode", err.Error())
	case errors.Is(err, egressapp.ErrNotFound):
		response.Error(c, http.StatusNotFound, "egressNodeNotFound", err.Error())
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
