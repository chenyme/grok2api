package imagecapacity

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	imagecapacityapp "github.com/chenyme/grok2api/backend/internal/application/imagecapacity"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type attestationService interface {
	Attest(context.Context, imagecapacityapp.Request) (imagecapacityapp.Attestation, error)
}

type Handler struct{ service attestationService }

func NewHandler(service *imagecapacityapp.Service) *Handler { return &Handler{service: service} }

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/client-keys/:id/image-capacity-attestation", h.get)
}

func (h *Handler) get(c *gin.Context) {
	clientKeyID, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || clientKeyID == 0 {
		response.Error(c, http.StatusBadRequest, "invalidClientKeyId", "client key id is invalid")
		return
	}
	routeID, err := strconv.ParseUint(strings.TrimSpace(c.Query("routeId")), 10, 64)
	if err != nil || routeID == 0 {
		response.Error(c, http.StatusBadRequest, "invalidRouteId", "routeId is invalid")
		return
	}
	request := imagecapacityapp.Request{ClientKeyID: clientKeyID, RouteID: routeID, RunMarker: c.Query("runMarker")}
	if rawSince := strings.TrimSpace(c.Query("since")); rawSince != "" {
		since, parseErr := time.Parse(time.RFC3339, rawSince)
		if parseErr != nil {
			response.Error(c, http.StatusBadRequest, "invalidSince", "since must be an RFC3339 timestamp")
			return
		}
		since = since.UTC()
		request.Since = &since
	}
	value, err := h.service.Attest(c.Request.Context(), request)
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	response.Success(c, http.StatusOK, value)
}

func (h *Handler) writeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, imagecapacityapp.ErrInvalidInput):
		response.Error(c, http.StatusBadRequest, "invalidImageCapacityAttestation", "since and a 24-character lowercase-hex runMarker must form one valid coverage boundary")
	case errors.Is(err, imagecapacityapp.ErrClientKeyNotFound):
		response.Error(c, http.StatusNotFound, "clientKeyNotFound", "client key is unavailable")
	case errors.Is(err, imagecapacityapp.ErrRouteNotFound):
		response.Error(c, http.StatusNotFound, "imageRouteNotFound", "image route does not exist")
	case errors.Is(err, imagecapacityapp.ErrRouteNotAttestable):
		response.Error(c, http.StatusConflict, "imageRouteNotAttestable", "route must be an enabled, explicit Web image_pro route allowed by the client key")
	case errors.Is(err, imagecapacityapp.ErrBuildNotAttestable):
		response.Error(c, http.StatusServiceUnavailable, "buildAttestationUnavailable", "runtime build attestation is unavailable")
	default:
		response.Error(c, http.StatusServiceUnavailable, "imageCapacityAttestationUnavailable", "image capacity attestation is unavailable")
	}
}
