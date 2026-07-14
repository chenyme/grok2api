package system

import (
	"net/http"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type Handler struct{ publicAPIBaseURL string }

func NewHandler(publicAPIBaseURL string) *Handler {
	return &Handler{publicAPIBaseURL: strings.TrimRight(publicAPIBaseURL, "/")}
}

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/system", h.get)
}

func (h *Handler) get(c *gin.Context) {
	response.Success(c, http.StatusOK, gin.H{"publicApiBaseURL": h.publicAPIBaseURL})
}
