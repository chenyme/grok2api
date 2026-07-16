package system

import (
	"net/http"
	"strings"

	updatecheckapp "github.com/chenyme/grok2api/backend/internal/application/updatecheck"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	publicAPIBaseURL func() string
	updates          *updatecheckapp.Service
}

func NewHandler(publicAPIBaseURL func() string, updates *updatecheckapp.Service) *Handler {
	if publicAPIBaseURL == nil {
		publicAPIBaseURL = func() string { return "" }
	}
	if updates == nil {
		updates = updatecheckapp.NewService("dev", nil)
	}
	return &Handler{publicAPIBaseURL: publicAPIBaseURL, updates: updates}
}

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/system", h.get)
	router.GET("/system/version", h.version)
	router.POST("/system/update/check", h.checkUpdate)
}

func (h *Handler) version(c *gin.Context) {
	response.Success(c, http.StatusOK, h.updates.Snapshot())
}

func (h *Handler) checkUpdate(c *gin.Context) {
	response.Success(c, http.StatusOK, h.updates.Check(c.Request.Context()))
}

func (h *Handler) get(c *gin.Context) {
	response.Success(c, http.StatusOK, gin.H{"publicApiBaseURL": strings.TrimRight(strings.TrimSpace(h.publicAPIBaseURL()), "/")})
}
