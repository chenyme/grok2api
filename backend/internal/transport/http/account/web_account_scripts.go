package account

import (
	"net/http"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

const maxWebAccountScriptRequestIDs = 1000

type webAccountScriptsRequest struct {
	IDs     []string                       `json:"ids"`
	All     bool                           `json:"all"`
	Actions webAccountScriptActionsRequest `json:"actions"`
}

type webAccountScriptActionsRequest struct {
	AcceptTerms  bool `json:"acceptTerms"`
	SetBirthDate bool `json:"setBirthDate"`
	EnableNSFW   bool `json:"enableNSFW"`
}

func (r webAccountScriptActionsRequest) options() accountapp.WebAccountScriptOptions {
	return accountapp.WebAccountScriptOptions{
		AcceptTerms:  r.AcceptTerms,
		SetBirthDate: r.SetBirthDate,
		EnableNSFW:   r.EnableNSFW,
	}
}

func (r webAccountScriptActionsRequest) empty() bool {
	return !r.AcceptTerms && !r.SetBirthDate && !r.EnableNSFW
}

func (h *Handler) runWebAccountScripts(c *gin.Context) {
	var request webAccountScriptsRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "账号脚本请求无效")
		return
	}
	if request.All && len(request.IDs) > 0 {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "全部账号与指定账号不能同时提交")
		return
	}
	if !request.All && len(request.IDs) == 0 {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "至少选择一个账号")
		return
	}
	if len(request.IDs) > maxWebAccountScriptRequestIDs {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "单次最多处理 1000 个账号")
		return
	}
	if request.Actions.empty() {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "至少选择一个账号脚本")
		return
	}

	var ids []uint64
	if !request.All {
		var err error
		ids, err = parseIDs(request.IDs)
		if err != nil {
			response.Error(c, http.StatusBadRequest, "invalidId", err.Error())
			return
		}
		ids = uniqueWebAccountScriptIDs(ids)
		if !h.validateProviderIDs(c, ids, string(accountdomain.ProviderWeb)) {
			return
		}
	}

	stream := newAccountEventStream(c)
	defer stream.Close()
	var (
		succeeded int
		failed    int
		err       error
	)
	if request.All {
		succeeded, failed, err = h.service.RunAllWebAccountScriptsWithProgress(c.Request.Context(), request.Actions.options(), stream.ProgressObserver())
	} else {
		succeeded, failed, err = h.service.RunWebAccountScriptsWithProgress(c.Request.Context(), ids, request.Actions.options(), stream.ProgressObserver())
	}
	if err != nil {
		stream.WriteError("webAccountScriptFailed", "执行 Grok Web 账号脚本失败")
		return
	}
	_ = stream.Write("complete", accountBatchResponse{Succeeded: succeeded, Failed: failed})
}

func uniqueWebAccountScriptIDs(ids []uint64) []uint64 {
	seen := make(map[uint64]struct{}, len(ids))
	result := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result
}
