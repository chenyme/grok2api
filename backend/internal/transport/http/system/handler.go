package system

import (
	"net/http"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	publicAPIBaseURL func() string
}

func NewHandler(publicAPIBaseURL func() string) *Handler {
	if publicAPIBaseURL == nil {
		publicAPIBaseURL = func() string { return "" }
	}
	return &Handler{publicAPIBaseURL: publicAPIBaseURL}
}

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/system", h.get)
}

func (h *Handler) get(c *gin.Context) {
	response.Success(c, http.StatusOK, gin.H{"publicApiBaseURL": strings.TrimRight(strings.TrimSpace(h.publicAPIBaseURL()), "/")})
}
