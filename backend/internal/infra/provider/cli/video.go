package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

const (
	buildVideoModel             = "grok-imagine-video-1.5"
	xaiVideoModel               = "grok-imagine-video-1.5-preview"
	buildVideoMaxImages         = 1
	buildVideoPollEvery         = 2 * time.Second
	buildVideoMaxBodySize       = 2 << 20
	buildVideoErrorSummaryLimit = 256
	buildVideoErrorMessageLimit = 160
)

type videoRequestProfile struct {
	model            string
	imageURLField    string
	userAgentProduct string
}

var (
	buildVideoRequestProfile = videoRequestProfile{model: buildVideoModel, imageURLField: "image_url"}
	// 官方 0.2.106 的直连 XAI 图片/视频工具使用 xai-grok-build/<version>，
	// 与 cli-chat-proxy 采样请求的 grok-shell/<version> UA 不同。
	xaiVideoRequestProfile = videoRequestProfile{model: xaiVideoModel, imageURLField: "url", userAgentProduct: "xai-grok-build"}
)

// mediaUploadSecretPattern 匹配上传路径及其中的 64-hex 一次性 token（含完整 URL 前缀）。
var mediaUploadSecretPattern = regexp.MustCompile(`(?i)(?:https?://[^\s"'\\<>]+)?/v1/media/uploads/[0-9a-f]{64}`)

// bareUploadTokenPattern 匹配无路径前缀的 64-hex 上传 token（上游可能仅回显 token 本身）。
var bareUploadTokenPattern = regexp.MustCompile(`(?i)\b[0-9a-f]{64}\b`)

// videoUpstreamError 是上游非 2xx 的可分类错误：保留 HTTP 状态，摘要经密钥脱敏且长度有界。
// 不得将原始响应体写入 Error()，避免 upload_url / token 泄漏到 media_jobs.error_message、日志或审计。
type videoUpstreamError struct {
	status  int
	summary string
}

func (e *videoUpstreamError) Error() string {
	if e == nil {
		return ""
	}
	return e.summary
}

func (e *videoUpstreamError) HTTPStatusCode() int {
	if e == nil {
		return 0
	}
	return e.status
}

// newVideoUpstreamError 从上游响应构造密钥安全、有界的错误摘要。
func newVideoUpstreamError(status int, body []byte) *videoUpstreamError {
	return &videoUpstreamError{status: status, summary: summarizeVideoUpstreamError(status, body)}
}

func summarizeVideoUpstreamError(status int, body []byte) string {
	code, message := extractVideoUpstreamErrorFields(body)
	parts := []string{fmt.Sprintf("Build 视频上游返回 %d", status)}
	if code != "" {
		parts = append(parts, code)
	}
	if message != "" {
		parts = append(parts, message)
	} else if code == "" {
		// 非结构化正文：仅作有界、脱敏片段，避免整包原文进入错误链。
		if snippet := redactVideoUploadSecrets(strings.TrimSpace(string(body))); snippet != "" {
			parts = append(parts, boundDiagnosticText(snippet, buildVideoErrorMessageLimit))
		}
	}
	return boundDiagnosticText(strings.Join(parts, ": "), buildVideoErrorSummaryLimit)
}

func extractVideoUpstreamErrorFields(body []byte) (code, message string) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 || body[0] != '{' {
		return "", ""
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return "", ""
	}
	if errObj, ok := root["error"].(map[string]any); ok {
		code = firstString(errObj, "code", "type", "error")
		message = firstString(errObj, "message", "error", "detail")
	}
	if code == "" {
		code = firstString(root, "code", "error_code", "type")
	}
	if message == "" {
		message = firstString(root, "message", "error_message", "detail")
	}
	if code != "" {
		code = boundDiagnosticText(redactVideoUploadSecrets(strings.TrimSpace(code)), 64)
	}
	if message != "" {
		message = safeVideoDiagnosticMessage(message)
	}
	return code, message
}

func redactVideoUploadSecrets(value string) string {
	if value == "" {
		return value
	}
	// 先替换完整 upload 路径，再替换残留的裸 64-hex token。
	value = mediaUploadSecretPattern.ReplaceAllString(value, "/v1/media/uploads/[REDACTED]")
	value = bareUploadTokenPattern.ReplaceAllString(value, "[REDACTED]")
	return value
}

// safeVideoDiagnosticMessage 对上游派生的 code/message 做密钥脱敏与长度有界处理。
// 用于非 2xx 摘要以及 2xx JSON 中的 error.message 等字段，避免 upload token 进入错误链。
func safeVideoDiagnosticMessage(message string) string {
	return boundDiagnosticText(redactVideoUploadSecrets(strings.TrimSpace(message)), buildVideoErrorMessageLimit)
}

func boundDiagnosticText(value string, limit int) string {
	if limit <= 0 || value == "" {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	for limit > 0 && !utf8.ValidString(value[:limit]) {
		limit--
	}
	return value[:limit]
}

// GenerateVideo 通过 Build OAuth 创建并轮询视频任务；对外仍使用 grok-imagine-video-1.5，
// 发往 XAI 时仅在出站协议层转换为官方 preview 模型与 image.url 结构。
// 最多 1 张首图；多于 1 张在调用上游前失败，不静默截断。
// 显式模式优先；auto 下仅已确认 Super 且 bot_flag_source=1 默认使用 XAI。
// 其他 auto Super 账号仅在当次 Build 创建返回 403 后探测 XAI。
func (a *Adapter) GenerateVideo(ctx context.Context, request provider.VideoRequest) (provider.VideoResult, error) {
	if len(request.ReferenceURLs) > buildVideoMaxImages {
		return provider.VideoResult{}, fmt.Errorf("Build grok-imagine-video-1.5 最多支持 1 张首图，当前为 %d 张", len(request.ReferenceURLs))
	}
	accessToken, err := a.cipher.Decrypt(request.Credential.EncryptedAccessToken)
	if err != nil {
		return provider.VideoResult{}, err
	}
	credential := request.Credential
	routeMode := normalizedBuildRouteMode(credential)
	xaiEligible := account.IsBuildSuper(credential, request.Billing)
	if routeMode == account.BuildRouteXAI || (routeMode == account.BuildRouteAuto && xaiEligible && a.CredentialMetadata(credential).BuildBotFlagged) {
		return a.generateVideoOnXAI(ctx, request, credential, accessToken, false)
	}
	// 非 bot-flagged 账号先走 Build 主地址，不读取历史回退标记。
	primaryBase := a.primaryBaseURL()
	payload, err := videoCreatePayload(request, "", buildVideoRequestProfile)
	if err != nil {
		return provider.VideoResult{}, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return provider.VideoResult{}, err
	}
	createResp, createErr := a.doVideoJSON(ctx, credential, accessToken, http.MethodPost, primaryBase, "/videos/generations", body, buildVideoRequestProfile, true)
	if createErr == nil {
		jobID, parseErr := parseVideoCreateResponse(createResp)
		if parseErr != nil {
			return provider.VideoResult{}, parseErr
		}
		if request.Progress != nil {
			request.Progress(1)
		}
		return a.pollVideoJob(ctx, credential, accessToken, primaryBase, jobID, "", buildVideoRequestProfile, request.Progress)
	}
	var upstream *videoUpstreamError
	if !asVideoUpstreamError(createErr, &upstream) || !isHTTPForbidden(upstream.status) {
		return provider.VideoResult{}, createErr
	}
	if routeMode == account.BuildRouteBuild || !xaiEligible {
		return provider.VideoResult{}, createErr
	}
	// 已确认 Super 的主地址 403：签发 upload_url 后探测 XAI；创建成功才标记降级。
	return a.generateVideoOnXAI(ctx, request, credential, accessToken, true)
}

func (a *Adapter) generateVideoOnXAI(ctx context.Context, request provider.VideoRequest, credential account.Credential, accessToken string, recordFallback bool) (provider.VideoResult, error) {
	issuer := a.uploadIssuerRef()
	if issuer == nil {
		return provider.VideoResult{}, fmt.Errorf("XAI 视频需要媒体上传接收服务")
	}
	jobKey := strings.TrimSpace(request.JobID)
	if jobKey == "" {
		jobKey = "video-pending"
	}
	uploadURL, assetID, err := issuer.IssueVideoUpload(ctx, jobKey)
	if err != nil {
		// 配置错误（例如 PublicAPIBaseURL 不可用）不得误标降级。
		return provider.VideoResult{}, err
	}
	payload, err := videoCreatePayload(request, uploadURL, xaiVideoRequestProfile)
	if err != nil {
		return provider.VideoResult{}, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return provider.VideoResult{}, err
	}
	base := a.fallbackBaseURL()
	createResp, err := a.doVideoJSON(ctx, credential, accessToken, http.MethodPost, base, "/videos/generations", body, xaiVideoRequestProfile, true)
	if err != nil {
		return provider.VideoResult{}, err
	}
	// 必须先解析到可用 job ID，再标记降级；畸形 2xx 不得激活或本地置位。
	jobID, err := parseVideoCreateResponse(createResp)
	if err != nil {
		return provider.VideoResult{}, err
	}
	if recordFallback {
		a.activateBuildAPIFallback(ctx, &credential)
	}
	if request.Progress != nil {
		request.Progress(1)
	}
	return a.pollVideoJob(ctx, credential, accessToken, base, jobID, assetID, xaiVideoRequestProfile, request.Progress)
}

// DownloadVideo 通过 Build egress 拉取已完成任务的公开 CDN URL。
// 资源域不需要 OAuth；不得解密或转发 token 与客户端身份头。
func (a *Adapter) DownloadVideo(ctx context.Context, credential account.Credential, rawURL string) (io.ReadCloser, string, int64, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || !trustedBuildVideoAssetHost(parsed.Hostname()) {
		return nil, "", 0, fmt.Errorf("视频内容 URL 不受信任")
	}
	requestCtx := infraegress.WithCredential(ctx, credential)
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, "", 0, err
	}
	// 仅保留 egress 路径上的匿名 GET；禁止 Authorization / Token-Auth / 会话身份头。
	req.Header.Set("Accept", "*/*")
	response, err := a.http.Do(req)
	if err != nil {
		return nil, "", 0, err
	}
	if err := normalizeGzipResponse(response); err != nil {
		return nil, "", 0, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_ = response.Body.Close()
		return nil, "", 0, fmt.Errorf("下载视频返回 %d", response.StatusCode)
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]))
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = "video/mp4"
	}
	if !strings.HasPrefix(contentType, "video/") {
		_ = response.Body.Close()
		return nil, "", 0, fmt.Errorf("上游视频 Content-Type 无效")
	}
	return response.Body, contentType, response.ContentLength, nil
}

func videoCreatePayload(request provider.VideoRequest, uploadURL string, profile videoRequestProfile) (map[string]any, error) {
	payload := map[string]any{
		"model": profile.model,
	}
	if prompt := strings.TrimSpace(request.Prompt); prompt != "" {
		payload["prompt"] = prompt
	}
	if request.Duration > 0 {
		payload["duration"] = request.Duration
	}
	if ratio := strings.TrimSpace(request.AspectRatio); ratio != "" {
		payload["aspect_ratio"] = ratio
	}
	if resolution := strings.TrimSpace(request.Resolution); resolution != "" {
		payload["resolution"] = resolution
	}
	if len(request.ReferenceURLs) == 1 {
		imageURL := strings.TrimSpace(request.ReferenceURLs[0])
		if imageURL == "" {
			return nil, fmt.Errorf("视频首图 URL 不能为空")
		}
		payload["image"] = map[string]any{profile.imageURLField: imageURL}
	}
	if _, hasPrompt := payload["prompt"]; !hasPrompt {
		if _, hasImage := payload["image"]; !hasImage {
			return nil, fmt.Errorf("文本生视频必须提供 prompt；图片生视频可以省略 prompt")
		}
	}
	if uploadURL = strings.TrimSpace(uploadURL); uploadURL != "" {
		payload["output"] = map[string]any{"upload_url": uploadURL}
	}
	return payload, nil
}

func (a *Adapter) pollVideoJob(ctx context.Context, credential account.Credential, accessToken, base, jobID, assetID string, profile videoRequestProfile, progress func(int)) (provider.VideoResult, error) {
	ticker := time.NewTicker(buildVideoPollEvery)
	defer ticker.Stop()
	for {
		statusBody, err := a.doVideoJSON(ctx, credential, accessToken, http.MethodGet, base, "/videos/"+url.PathEscape(jobID), nil, profile, false)
		if err != nil {
			return provider.VideoResult{}, err
		}
		result, done, pollErr := parseVideoStatusResponse(statusBody, progress, assetID != "")
		if pollErr != nil {
			return provider.VideoResult{}, pollErr
		}
		if done {
			if assetID != "" {
				issuer := a.uploadIssuerRef()
				if issuer == nil {
					return provider.VideoResult{}, fmt.Errorf("XAI 视频需要媒体上传接收服务")
				}
				contentType, waitErr := issuer.WaitVideoUpload(ctx, assetID)
				if waitErr != nil {
					// 上游 done 但本地未收到上传：若 status 带 CDN URL 仍可回退远程读取。
					if result.URL != "" {
						return result, nil
					}
					return provider.VideoResult{}, waitErr
				}
				if contentType == "" {
					contentType = "video/mp4"
				}
				return provider.VideoResult{ContentType: contentType, AssetID: assetID}, nil
			}
			return result, nil
		}
		select {
		case <-ctx.Done():
			return provider.VideoResult{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (a *Adapter) doVideoJSON(ctx context.Context, credential account.Credential, accessToken, method, base, path string, body []byte, profile videoRequestProfile, withTrace bool) ([]byte, error) {
	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	requestCtx := infraegress.WithCredential(ctx, credential)
	req, err := http.NewRequestWithContext(requestCtx, method, a.urlWithBase(base, path), bodyReader)
	if err != nil {
		return nil, err
	}
	if err := a.applyHeaders(req, credential, accessToken, profile.model, "", withTrace); err != nil {
		return nil, err
	}
	if profile.userAgentProduct != "" {
		version := strings.TrimSpace(a.config().ClientVersion)
		if version != "" {
			req.Header.Set("User-Agent", profile.userAgentProduct+"/"+version)
		}
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	if err := normalizeGzipResponse(resp); err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, buildVideoMaxBodySize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > buildVideoMaxBodySize {
		return nil, fmt.Errorf("Build 视频上游响应超过 %d 字节", buildVideoMaxBodySize)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, provider.ErrUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newVideoUpstreamError(resp.StatusCode, data)
	}
	return data, nil
}

func parseVideoCreateResponse(body []byte) (string, error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return "", fmt.Errorf("解析 Build 视频创建响应: %w", err)
	}
	if message := firstNestedString(root, "error", "message"); message != "" {
		return "", fmt.Errorf("Build 视频创建失败: %s", safeVideoDiagnosticMessage(message))
	}
	if id := firstString(root, "request_id", "id"); id != "" {
		return id, nil
	}
	if data, ok := root["data"].(map[string]any); ok {
		if id := firstString(data, "request_id", "id"); id != "" {
			return id, nil
		}
	}
	return "", fmt.Errorf("Build 视频创建响应缺少 request_id")
}

func parseVideoStatusResponse(body []byte, progress func(int), allowMissingURL bool) (provider.VideoResult, bool, error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return provider.VideoResult{}, false, fmt.Errorf("解析 Build 视频状态响应: %w", err)
	}
	if message := firstNestedString(root, "error", "message"); message != "" {
		return provider.VideoResult{}, false, fmt.Errorf("Build 视频生成失败: %s", safeVideoDiagnosticMessage(message))
	}
	statusSource := root
	if data, ok := root["data"].(map[string]any); ok {
		statusSource = data
	}
	if value, ok := numberAsInt(firstValue(statusSource, "progress")); ok && progress != nil {
		progress(value)
	}
	status := strings.ToLower(strings.TrimSpace(firstString(statusSource, "status", "state")))
	switch status {
	case "failed", "error", "cancelled", "canceled":
		message := firstNestedString(statusSource, "error", "message")
		if message == "" {
			message = firstString(statusSource, "error_message", "message")
		}
		if message == "" {
			message = status
		}
		return provider.VideoResult{}, false, fmt.Errorf("Build 视频生成失败: %s", safeVideoDiagnosticMessage(message))
	case "completed", "succeeded", "success", "ready", "done":
		videoURL := extractBuildVideoURL(root)
		if videoURL == "" && !allowMissingURL {
			return provider.VideoResult{}, false, fmt.Errorf("视频生成完成但没有返回内容 URL")
		}
		return provider.VideoResult{URL: videoURL, ContentType: "video/mp4"}, true, nil
	default:
		if videoURL := extractBuildVideoURL(root); videoURL != "" {
			return provider.VideoResult{URL: videoURL, ContentType: "video/mp4"}, true, nil
		}
		return provider.VideoResult{}, false, nil
	}
}

func asVideoUpstreamError(err error, target **videoUpstreamError) bool {
	if err == nil {
		return false
	}
	if v, ok := err.(*videoUpstreamError); ok {
		*target = v
		return true
	}
	return false
}

func extractBuildVideoURL(root map[string]any) string {
	candidates := []string{
		firstString(root, "video_url", "url"),
		firstNestedString(root, "video", "url"),
		firstNestedString(root, "output", "url"),
		firstNestedString(root, "result", "url"),
	}
	if data, ok := root["data"].(map[string]any); ok {
		candidates = append(candidates,
			firstString(data, "video_url", "url"),
			firstNestedString(data, "video", "url"),
			firstNestedString(data, "output", "url"),
		)
		if items, ok := data["data"].([]any); ok {
			for _, item := range items {
				if m, ok := item.(map[string]any); ok {
					if u := firstString(m, "url", "video_url"); u != "" {
						candidates = append(candidates, u)
					}
				}
			}
		}
	}
	if items, ok := root["data"].([]any); ok {
		for _, item := range items {
			if m, ok := item.(map[string]any); ok {
				if u := firstString(m, "url", "video_url"); u != "" {
					candidates = append(candidates, u)
				}
			}
		}
	}
	for _, value := range candidates {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func trustedBuildVideoAssetHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	trusted := []string{
		"x.ai", "grok.com", "assets.grok.com", "cdn.x.ai", "videos.x.ai",
	}
	for _, suffix := range trusted {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

func firstString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if text := strings.TrimSpace(stringValue(values[key])); text != "" {
			return text
		}
	}
	return ""
}

func firstNestedString(values map[string]any, objectKey, field string) string {
	nested, _ := values[objectKey].(map[string]any)
	if nested == nil {
		return ""
	}
	return strings.TrimSpace(stringValue(nested[field]))
}

func numberAsInt(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case json.Number:
		n, err := typed.Int64()
		return int(n), err == nil
	default:
		return 0, false
	}
}
