package model

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type addRouteAccountsRequest struct {
	AccountIDs *[]string                        `json:"accountIds"`
	Expected   *addRouteAccountsExpectedRequest `json:"expected"`
}

type addRouteAccountsExpectedRequest struct {
	PublicID      string `json:"publicId"`
	Provider      string `json:"provider"`
	UpstreamModel string `json:"upstreamModel"`
	Capability    string `json:"capability"`
	Enabled       *bool  `json:"enabled"`
}

func (h *Handler) addRouteAccounts(c *gin.Context) {
	routeID, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || routeID == 0 {
		response.Error(c, http.StatusBadRequest, "invalidId", "ID 无效")
		return
	}
	var request addRouteAccountsRequest
	if decodeSingleJSON(c, &request) != nil || request.AccountIDs == nil || request.Expected == nil || request.Expected.Enabled == nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	accountIDs, err := parseIDs(*request.AccountIDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", err.Error())
		return
	}
	value, err := h.service.AddRouteAccounts(c.Request.Context(), routeID, modelapp.AddRouteAccountsInput{
		AccountIDs: accountIDs,
		Expected: modelapp.AddRouteAccountsExpected{
			PublicID: request.Expected.PublicID, Provider: account.Provider(request.Expected.Provider),
			UpstreamModel: request.Expected.UpstreamModel, Capability: modeldomain.Capability(request.Expected.Capability),
			Enabled: *request.Expected.Enabled,
		},
	})
	if err != nil {
		h.writeServiceError(c, "modelRouteAccountAddFailed", err)
		return
	}
	response.Success(c, http.StatusOK, newModelResponse(value))
}

func decodeSingleJSON(c *gin.Context, target any) error {
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
