package cli

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

// FallbackMarker 同时提供 Build XAI 回退资格判定与回退标记持久化。
// XAI 仅接受 Super 账号；资格查询失败时 Adapter 必须 fail closed。
type FallbackMarker interface {
	CanUseBuildAPIFallback(ctx context.Context, accountID uint64) (bool, error)
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
// 生产探针（账号 954 / api.x.ai/v1）已验证：
//
//	支持：GET /models、POST /responses、POST /responses/compact、视频 create/poll
//	不支持：GET/DELETE /responses/{id}、GET /billing（404）；Billing 主地址 403 不得改写为 XAI 404
//
// OAuth 认证端点始终使用独立认证 host，不受此函数影响。
func isXAIInferenceFallbackCapable(method, path string) bool {
	method = strings.ToUpper(strings.TrimSpace(method))
	path = normalizeBuildAPIPath(path)
	switch {
	case method == http.MethodGet && path == "/models":
		return true
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
		// Billing、stored-resource GET/DELETE、未知路径：仅主地址。
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

// apiBaseForOperation 按操作与实时 Billing 资格选择 Build API 根地址。
// 旧数据即使误标 BuildAPIFallback，Free/Unknown 账号也始终走主地址。
func (a *Adapter) apiBaseForOperation(ctx context.Context, credential account.Credential, method, path string) string {
	if credential.BuildAPIFallback && isXAIInferenceFallbackCapable(method, path) && a.canUseBuildAPIFallback(ctx, credential.ID) {
		return a.fallbackBaseURL()
	}
	return a.primaryBaseURL()
}

// shouldProbeXAIInferenceFallback 仅允许已确认 Super 的未标记账号在主地址 403 后探测 XAI。
func (a *Adapter) shouldProbeXAIInferenceFallback(ctx context.Context, credential account.Credential, method, path string, primaryStatus int) bool {
	return !credential.BuildAPIFallback && isHTTPForbidden(primaryStatus) && isXAIInferenceFallbackCapable(method, path) && a.canUseBuildAPIFallback(ctx, credential.ID)
}

func (a *Adapter) canUseBuildAPIFallback(ctx context.Context, accountID uint64) bool {
	allowed, err := a.buildXAIEntitled(ctx, accountID)
	if err != nil {
		slog.Warn("build_api_fallback_policy_failed", "account_id", accountID, "error", err.Error())
		return false
	}
	return allowed
}

// buildXAIEntitled 返回账号是否具备 XAI 能力。调用方自行决定策略查询失败时
// 是对推理 fail closed，还是让模型同步失败以保留上一份完整能力快照。
func (a *Adapter) buildXAIEntitled(ctx context.Context, accountID uint64) (bool, error) {
	marker := a.fallbackMarkerRef()
	if marker == nil || accountID == 0 {
		return false, nil
	}
	allowed, err := marker.CanUseBuildAPIFallback(ctx, accountID)
	if err != nil {
		return false, err
	}
	return allowed, nil
}

func (a *Adapter) urlWithBase(base, path string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
}

// activateBuildAPIFallback 在 XAI 推理回退成功后幂等标记账号；标记失败不撤销当前成功结果。
// 仅应在可回退操作（models / responses create|compact / video）成功后调用，不得由 Billing 或 stored-resource 触发。
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
