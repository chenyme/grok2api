package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// FallbackMarker 记录 Build 请求因当次 403 成功回退到 XAI。
// 该标记只用于观测，不参与后续请求路由。
type FallbackMarker interface {
	MarkBuildAPIFallback(ctx context.Context, accountID uint64, enabled bool) error
}

// VideoUploadIssuer 为 XAI ZDR 视频签发一次性 PUT 接收地址并等待本地资产就绪。
type VideoUploadIssuer interface {
	// IssueVideoUpload 返回可被 xAI HTTPS PUT 的 URL 与绑定的本地 assetID。
	// 不得在错误信息中回显完整 URL 或票据明文。
	IssueVideoUpload(ctx context.Context, jobID string) (uploadURL, assetID string, err error)
	// WaitVideoUpload 在上游任务完成后等待本地 PUT 资产就绪。
	WaitVideoUpload(ctx context.Context, assetID string) (contentType string, err error)
}

func (a *Adapter) SetFallbackMarker(marker FallbackMarker) {
	a.cfgMu.Lock()
	a.fallbackMarker = marker
	a.cfgMu.Unlock()
}

func (a *Adapter) SetVideoUploadIssuer(issuer VideoUploadIssuer) {
	a.cfgMu.Lock()
	a.uploadIssuer = issuer
	a.cfgMu.Unlock()
}

func (a *Adapter) fallbackMarkerRef() FallbackMarker {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.fallbackMarker
}

func (a *Adapter) uploadIssuerRef() VideoUploadIssuer {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.uploadIssuer
}

func (a *Adapter) primaryBaseURL() string {
	return strings.TrimRight(strings.TrimSpace(a.config().BaseURL), "/")
}

func (a *Adapter) fallbackBaseURL() string {
	return strings.TrimRight(config.NormalizeBuildFallbackBaseURL(a.config().FallbackBaseURL), "/")
}

// isXAIInferenceFallbackCapable 判断该 Build API 操作是否可走 XAI 推理回退。
//
//	支持：POST /responses、POST /responses/compact、视频 create/poll
//	不支持：GET /models（始终主地址）、GET/DELETE /responses/{id}、GET /billing、未知路径
//
// OAuth 认证端点始终使用独立认证 host，不受此函数影响。
func isXAIInferenceFallbackCapable(method, path string) bool {
	method = strings.ToUpper(strings.TrimSpace(method))
	path = normalizeBuildAPIPath(path)
	switch {
	case method == http.MethodPost && path == "/responses":
		// Responses create 与 Chat/Messages 兼容转发均走 POST /responses。
		return true
	case method == http.MethodPost && path == "/responses/compact":
		return true
	case method == http.MethodPost && path == "/videos/generations":
		return true
	case method == http.MethodGet && strings.HasPrefix(path, "/videos/") && path != "/videos" && path != "/videos/generations":
		// 视频任务轮询：GET /videos/{id}
		return true
	default:
		// /models、Billing、stored-resource GET/DELETE、未知路径：仅主地址。
		return false
	}
}

func normalizeBuildAPIPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	if len(path) > 1 {
		path = strings.TrimRight(path, "/")
	}
	return path
}

func normalizedBuildRouteMode(credential account.Credential) account.BuildRouteMode {
	if credential.Provider == account.ProviderBuild && credential.BuildRouteMode.IsValid() {
		return credential.BuildRouteMode
	}
	return account.BuildRouteAuto
}

// inferenceBaseForOperation 先应用管理员的显式模式，再校验 auto 的 Super 资格与 bot flag。
// Free 与未确认等级的账号在 auto 下始终使用 Build；历史 fallback 标记不参与选择。
func (a *Adapter) inferenceBaseForOperation(credential account.Credential, billing *account.Billing, method, path string) string {
	if !isXAIInferenceFallbackCapable(method, path) {
		return a.primaryBaseURL()
	}
	switch normalizedBuildRouteMode(credential) {
	case account.BuildRouteBuild:
		return a.primaryBaseURL()
	case account.BuildRouteXAI:
		return a.fallbackBaseURL()
	}
	if !account.IsBuildSuper(credential, billing) {
		return a.primaryBaseURL()
	}
	if a.CredentialMetadata(credential).BuildBotFlagged {
		return a.fallbackBaseURL()
	}
	return a.primaryBaseURL()
}

// shouldProbeXAIInferenceFallback 只由当次 Build CLI 的严格 403 触发。
// bot-flagged 账号已直接使用 XAI，不走该探测分支。
func shouldProbeXAIInferenceFallback(credential account.Credential, billing *account.Billing, method, path string, primaryStatus int) bool {
	return account.IsBuildSuper(credential, billing) && normalizedBuildRouteMode(credential) == account.BuildRouteAuto && isHTTPForbidden(primaryStatus) && isXAIInferenceFallbackCapable(method, path)
}

func (a *Adapter) urlWithBase(base, path string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
}

// activateBuildAPIFallback 在 XAI 推理回退成功后幂等记录账号；标记失败不撤销当前成功结果。
// 仅应在可回退操作（responses create|compact / video）成功后调用，不得由 /models、Billing 或 stored-resource 触发。
func (a *Adapter) activateBuildAPIFallback(ctx context.Context, credential *account.Credential) {
	if credential == nil || credential.ID == 0 || credential.BuildAPIFallback {
		return
	}
	credential.BuildAPIFallback = true
	marker := a.fallbackMarkerRef()
	if marker == nil {
		slog.Error("build_api_fallback_mark_skipped", "account_id", credential.ID, "reason", "marker_unavailable")
		return
	}
	if err := marker.MarkBuildAPIFallback(ctx, credential.ID, true); err != nil {
		// 不含 token；仅记录账号与错误类型，便于后续幂等重写。
		slog.Error("build_api_fallback_mark_failed", "account_id", credential.ID, "error", err.Error())
	}
}

func isHTTPForbidden(status int) bool {
	return status == http.StatusForbidden
}

func isDefinitiveAccountBlockBody(body []byte) bool {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	code := fallbackStringField(payload, "code")
	message := firstNonEmpty(fallbackStringField(payload, "error"), fallbackStringField(payload, "message"))
	if nested, ok := payload["error"].(map[string]any); ok {
		code = firstNonEmpty(fallbackStringField(nested, "code"), code)
		message = firstNonEmpty(fallbackStringField(nested, "message"), message)
	}
	code = strings.ToLower(strings.TrimSpace(code))
	message = strings.ToLower(strings.Trim(strings.TrimSpace(message), " .!\t\r\n"))
	return strings.Contains(code, "blocked-user") || message == "user is blocked"
}

func fallbackStringField(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return value
}

func bufferedFailureDiagnostic(response *http.Response, body []byte, truncated bool) *provider.DiagnosticResponse {
	if response == nil {
		return &provider.DiagnosticResponse{StatusCode: http.StatusForbidden, Status: "403 Forbidden", Header: make(http.Header), Body: append([]byte(nil), body...), BodyTruncated: truncated}
	}
	return &provider.DiagnosticResponse{
		StatusCode: response.StatusCode, Status: response.Status, Header: response.Header.Clone(),
		Body: append([]byte(nil), body...), BodyTruncated: truncated,
	}
}

func isHTTPSuccess(status int) bool {
	return status >= 200 && status < 300
}

// cloneBufferedResponse 用已读取的正文重建可再次消费的 HTTP 响应，保留状态与头。
func cloneBufferedResponse(source *http.Response, body []byte, truncated bool) *http.Response {
	if source == nil {
		return &http.Response{
			StatusCode:    http.StatusForbidden,
			Status:        "403 Forbidden",
			Header:        make(http.Header),
			Body:          io.NopCloser(bytes.NewReader(body)),
			ContentLength: int64(len(body)),
		}
	}
	header := source.Header.Clone()
	if header == nil {
		header = make(http.Header)
	}
	if truncated {
		header.Set("X-Grok2API-Body-Truncated", "1")
	}
	header.Set("Content-Length", strconv.Itoa(len(body)))
	return &http.Response{
		StatusCode:       source.StatusCode,
		Status:           source.Status,
		Proto:            source.Proto,
		ProtoMajor:       source.ProtoMajor,
		ProtoMinor:       source.ProtoMinor,
		Header:           header,
		Body:             io.NopCloser(bytes.NewReader(body)),
		ContentLength:    int64(len(body)),
		TransferEncoding: append([]string(nil), source.TransferEncoding...),
		Uncompressed:     source.Uncompressed,
		Trailer:          source.Trailer.Clone(),
		Request:          source.Request,
		TLS:              source.TLS,
	}
}
