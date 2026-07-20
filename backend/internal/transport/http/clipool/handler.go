package clipool

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	pool *gateway.CliPool
}

func NewHandler(pool *gateway.CliPool) *Handler {
	return &Handler{pool: pool}
}

func (h *Handler) Register(router *gin.RouterGroup) {
	router.POST("/cli-pool/lease", h.lease)
	router.POST("/cli-pool/report", h.report)
	router.POST("/cli-pool/release", h.release)
}

type leaseRequest struct {
	Excluded []uint64 `json:"excluded"`
}

type reportRequest struct {
	AccountID  uint64 `json:"accountId" binding:"required"`
	Reason     string `json:"reason" binding:"required"`
	HTTPStatus int    `json:"httpStatus"`
	LeaseID    string `json:"leaseId"`
}

type releaseRequest struct {
	LeaseID string `json:"leaseId" binding:"required"`
}

func (h *Handler) lease(c *gin.Context) {
	if h.pool == nil {
		response.Error(c, http.StatusServiceUnavailable, "cliPoolUnavailable", "CLI 号池未启用")
		return
	}
	var request leaseRequest
	if c.Request.ContentLength > 0 && c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	result, err := h.pool.Lease(c.Request.Context(), request.Excluded)
	if err != nil {
		if gateway.IsSelectionUnavailable(err) {
			var unavailable *gateway.SelectionUnavailableError
			_ = errors.As(err, &unavailable)
			retryAfter := 0
			if unavailable != nil && unavailable.RetryAfter > 0 {
				retryAfter = int(unavailable.RetryAfter.Round(time.Second) / time.Second)
				if retryAfter < 1 {
					retryAfter = 1
				}
				c.Header("Retry-After", strconv.Itoa(retryAfter))
			}
			response.Error(c, http.StatusTooManyRequests, "upstream_quota_exhausted", err.Error())
			return
		}
		response.Error(c, http.StatusInternalServerError, "cliPoolLeaseFailed", "租号失败: "+err.Error())
		return
	}
	response.Success(c, http.StatusOK, result)
}

func (h *Handler) report(c *gin.Context) {
	if h.pool == nil {
		response.Error(c, http.StatusServiceUnavailable, "cliPoolUnavailable", "CLI 号池未启用")
		return
	}
	var request reportRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	reason := gateway.ReportReason(request.Reason)
	switch reason {
	case gateway.ReportReasonOK, gateway.ReportReasonQuotaExhausted, gateway.ReportReasonFreeUsageExhausted, gateway.ReportReasonAuthRejected:
	default:
		response.Error(c, http.StatusBadRequest, "invalidRequest", "未知报障原因")
		return
	}
	if err := h.pool.Report(c.Request.Context(), request.AccountID, reason, request.HTTPStatus, request.LeaseID); err != nil {
		response.Error(c, http.StatusInternalServerError, "cliPoolReportFailed", "报障失败: "+err.Error())
		return
	}
	response.Success(c, http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) release(c *gin.Context) {
	if h.pool == nil {
		response.Error(c, http.StatusServiceUnavailable, "cliPoolUnavailable", "CLI 号池未启用")
		return
	}
	var request releaseRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	if err := h.pool.Release(request.LeaseID); err != nil {
		response.Error(c, http.StatusBadRequest, "cliPoolReleaseFailed", err.Error())
		return
	}
	response.Success(c, http.StatusOK, gin.H{"ok": true})
}
