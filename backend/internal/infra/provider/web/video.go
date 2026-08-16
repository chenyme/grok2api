package web

import (
	"bufio"
	"context"
	"crypto/sha256"
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
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

type webMediaUpstreamError struct {
	status              int
	summary             string
	bodyBytes           int
	bodyTruncated       bool
	bodyPrefixSHA256    string
	bodyKind            string
	cloudflareChallenge bool
}

func (e *webMediaUpstreamError) Error() string {
	if e == nil {
		return ""
	}
	return e.summary
}

func (e *webMediaUpstreamError) HTTPStatusCode() int {
	if e == nil {
		return 0
	}
	return e.status
}

// isClearanceRefreshableMediaError distinguishes browser-session challenges
// from structured upstream policy responses such as content moderation. Empty
// and HTML 403 bodies are the forms returned by the media endpoints when the
// request is rejected before the application response is built.
func isClearanceRefreshableMediaError(e *webMediaUpstreamError) bool {
	if e == nil || e.status != http.StatusForbidden {
		return false
	}
	return e.cloudflareChallenge || e.bodyKind == "empty" || e.bodyKind == "html"
}

func (e *webMediaUpstreamError) providerResponse() *provider.Response {
	if e == nil {
		return nil
	}
	code := "upstream_forbidden"
	if e.status != http.StatusForbidden {
		code = "upstream_unavailable"
	}
	return jsonProviderResponse(e.status, map[string]any{"error": map[string]any{
		"message": e.summary,
		"type":    "upstream_error",
		"code":    code,
	}})
}

const (
	webMediaDiagnosticBodyLimit    = 64 << 10
	webMediaDiagnosticSummaryLimit = 256
	webMediaDiagnosticFieldLimit   = 160
)

var (
	webMediaAuthorizationPattern = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]+`)
	webMediaCookiePattern        = regexp.MustCompile(`(?i)\b(cookie|set-cookie)\b\s*[:=]\s*[^\r\n]+`)
	webMediaSecretPattern        = regexp.MustCompile(`(?i)(["']?(?:authorization|proxy-authorization|x-api-key|api[_-]?key|access[_-]?token|refresh[_-]?token|id[_-]?token|upload[_-]?url|cookie|sso|session[_-]?id)["']?\s*[:=]\s*["']?)[^"'\s,;}]+`)
	webMediaJWTPattern           = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{12,}\.[A-Za-z0-9_-]{12,}(?:\.[A-Za-z0-9_-]{12,})?\b`)
	webMediaEmailPattern         = regexp.MustCompile(`(?i)\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b`)
	webMediaURLPattern           = regexp.MustCompile(`https?://[^\s"'<>]+`)
	webMediaLongTokenPattern     = regexp.MustCompile(`[A-Za-z0-9+/=_-]{256,}`)
)

// newWebMediaUpstreamError keeps the HTTP status while exposing only a
// bounded, redacted summary through the error. Structured logs retain only
// body metadata and a prefix hash, never the upstream response body itself.
func newWebMediaUpstreamError(status int, body []byte, truncated bool) *webMediaUpstreamError {
	digest := sha256.Sum256(body)
	return &webMediaUpstreamError{
		status:              status,
		summary:             summarizeWebMediaUpstreamError(status, body, truncated),
		bodyBytes:           len(body),
		bodyTruncated:       truncated,
		bodyPrefixSHA256:    fmt.Sprintf("%x", digest),
		bodyKind:            classifyWebMediaDiagnosticBody(body),
		cloudflareChallenge: isCloudflareChallengeBody(body),
	}
}

func classifyWebMediaDiagnosticBody(body []byte) string {
	if !utf8.Valid(body) {
		return "binary"
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return "empty"
	}
	if json.Valid(body) {
		return "json"
	}
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "<!doctype html") || strings.HasPrefix(lower, "<html") {
		return "html"
	}
	for _, value := range trimmed {
		if value < 0x20 && value != '\t' && value != '\r' && value != '\n' {
			return "binary"
		}
	}
	return "text"
}

func isCloudflareChallengeBody(body []byte) bool {
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "just a moment") ||
		strings.Contains(lower, "challenge-platform") ||
		strings.Contains(lower, "__cf_chl") ||
		strings.Contains(lower, "cf-chl-")
}

func (a *Adapter) logWebMediaUpstreamRejection(stage string, response *http.Response, upstreamErr *webMediaUpstreamError) {
	if upstreamErr == nil {
		return
	}
	attributes := []any{
		"stage", stage,
		"status", upstreamErr.status,
		"body_bytes_captured", upstreamErr.bodyBytes,
		"body_truncated", upstreamErr.bodyTruncated,
		"body_prefix_sha256", upstreamErr.bodyPrefixSHA256,
		"body_kind", upstreamErr.bodyKind,
		"cloudflare_challenge", upstreamErr.cloudflareChallenge,
	}
	if response != nil {
		attributes = append(attributes,
			"content_type", safeWebMediaDiagnostic(response.Header.Get("Content-Type"), 128),
			"content_length", response.ContentLength,
			"content_encoding", safeWebMediaDiagnostic(response.Header.Get("Content-Encoding"), 64),
			"server", safeWebMediaDiagnostic(response.Header.Get("Server"), 128),
			"cf_ray", safeWebMediaDiagnostic(response.Header.Get("CF-Ray"), 128),
			"upstream_request_id", safeWebMediaDiagnostic(firstNonEmpty(response.Header.Get("X-Request-Id"), response.Header.Get("X-Xai-Request-Id")), 128),
		)
	}
	a.log().Warn("web_media_upstream_rejected", attributes...)
}

func summarizeWebMediaUpstreamError(status int, body []byte, truncated bool) string {
	code, message, structured := extractWebMediaUpstreamErrorFields(body)
	parts := []string{fmt.Sprintf("Grok Web 媒体上游返回 %d", status)}
	if code != "" {
		parts = append(parts, code)
	}
	if message != "" {
		parts = append(parts, message)
	} else if len(strings.TrimSpace(string(body))) == 0 {
		parts = append(parts, "<empty>")
	} else if truncated {
		parts = append(parts, "响应正文过长")
	} else if !structured {
		parts = append(parts, "响应正文不可解析")
	} else if code == "" {
		parts = append(parts, "未提供错误详情")
	}
	return boundWebMediaDiagnostic(strings.Join(parts, ": "), webMediaDiagnosticSummaryLimit)
}

func extractWebMediaUpstreamErrorFields(body []byte) (code, message string, structured bool) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return "", "", false
	}
	structured = true
	if errorObject, ok := root["error"].(map[string]any); ok {
		code = firstWebMediaDiagnosticCode(errorObject, "code", "type", "error")
		message = firstString(errorObject, "message", "error", "detail")
	} else if errorText, ok := root["error"].(string); ok {
		message = errorText
	}
	if code == "" {
		code = firstWebMediaDiagnosticCode(root, "code", "error_code", "type")
	}
	if message == "" {
		message = firstString(root, "message", "error_message", "detail")
	}
	return safeWebMediaDiagnostic(code, 64), safeWebMediaDiagnostic(message, webMediaDiagnosticFieldLimit), true
}

func firstWebMediaDiagnosticCode(value map[string]any, keys ...string) string {
	if code := firstString(value, keys...); code != "" {
		return code
	}
	if code, ok := firstInt(value, keys...); ok {
		return fmt.Sprintf("%d", code)
	}
	return ""
}

func safeWebMediaDiagnostic(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	value = webMediaCookiePattern.ReplaceAllString(value, "$1: [REDACTED]")
	value = webMediaAuthorizationPattern.ReplaceAllString(value, "$1 [REDACTED]")
	value = webMediaSecretPattern.ReplaceAllString(value, "$1[REDACTED]")
	value = webMediaJWTPattern.ReplaceAllString(value, "[REDACTED]")
	value = webMediaEmailPattern.ReplaceAllString(value, "[REDACTED_EMAIL]")
	value = webMediaURLPattern.ReplaceAllString(value, "[REDACTED_URL]")
	value = webMediaLongTokenPattern.ReplaceAllString(value, "[REDACTED_LONG_VALUE]")
	return boundWebMediaDiagnostic(value, limit)
}

func boundWebMediaDiagnostic(value string, limit int) string {
	if limit <= 0 || value == "" {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func (a *Adapter) GenerateVideo(ctx context.Context, request provider.VideoRequest) (provider.VideoResult, error) {
	cfg := a.config()
	token, err := a.cipher.Decrypt(request.Credential.EncryptedAccessToken)
	if err != nil {
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePrepare, 0, err)
	}
	lease, err := a.egress.AcquireCredential(ctx, domainegress.ScopeWeb, request.Credential)
	if err != nil {
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePrepare, 0, err)
	}
	defer lease.Release()
	parentID := ""
	rawReferences := make([]string, 0, 1+len(request.ReferenceURLs))
	if imageURL := strings.TrimSpace(request.ImageURL); imageURL != "" {
		rawReferences = append(rawReferences, imageURL)
	}
	for _, rawReference := range request.ReferenceURLs {
		if value := strings.TrimSpace(rawReference); value != "" {
			rawReferences = append(rawReferences, value)
		}
	}
	references := make([]string, 0, len(rawReferences))
	for _, rawReference := range rawReferences {
		reference, referenceErr := a.prepareVideoReference(ctx, cfg, lease, token, rawReference)
		if referenceErr != nil {
			return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoCreateFailureStage(referenceErr), 0, referenceErr)
		}
		references = append(references, reference)
	}
	if len(references) > 0 {
		parentID, err = a.createMediaPost(ctx, cfg, lease, token, "MEDIA_POST_TYPE_IMAGE", references[0], "", "video_reference_media_post")
	} else {
		parentID, err = a.createMediaPost(ctx, cfg, lease, token, "MEDIA_POST_TYPE_VIDEO", "", request.Prompt, "video_prompt_media_post")
	}
	if err != nil {
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoCreateFailureStage(err), 0, err)
	}
	segments := videoSegments(request.Duration)
	if len(segments) == 0 {
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePrepare, 0, fmt.Errorf("duration 必须在 1 到 15 秒之间"))
	}
	ratio := resolveAspectRatio(request.AspectRatio)
	resolution := request.Resolution
	if resolution == "" {
		resolution = "720p"
	}
	payload := videoCreatePayload(request.Prompt, parentID, ratio, resolution, segments[0], references)
	response, err := a.postJSON(ctx, cfg, lease, token, cfg.BaseURL+"/rest/app-chat/conversations/new", payload, time.Duration(cfg.VideoTimeoutSeconds)*time.Second)
	if err != nil {
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoCreateFailureStage(err), 0, err)
	}
	result, _, parseErr := parseVideoStream(response, request.Progress)
	_ = response.Body.Close()
	if parseErr != nil {
		if upstreamErr, ok := parseErr.(*webMediaUpstreamError); ok {
			a.logWebMediaUpstreamRejection("video_generation", response, upstreamErr)
		}
		stage := provider.VideoStagePoll
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			stage = provider.VideoCreateFailureStage(parseErr)
		}
		return provider.VideoResult{}, provider.WrapVideoStage(stage, 0, parseErr)
	}
	if result.URL == "" {
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePoll, 0, fmt.Errorf("视频生成完成但没有返回内容 URL"))
	}
	return result, nil
}

func (a *Adapter) prepareVideoReference(ctx context.Context, cfg Config, lease *egress.Lease, token, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("视频参考图片 URL 不能为空")
	}
	image, err := a.loadChatImage(ctx, lease, value, 20<<20)
	if err != nil {
		return "", err
	}
	uploaded, err := a.uploadFileV2Direct(ctx, cfg, lease, token, image, cfg.BaseURL+"/imagine", imagineSelfUploadSource, "video_reference_upload")
	if err != nil {
		return "", err
	}
	if uploaded.URI == "" {
		return "", fmt.Errorf("上传视频参考图片后未返回 fileUri")
	}
	return uploaded.URI, nil
}

// DownloadVideo retrieves a completed Grok asset through its source SSO
// session. Direct asset URLs are not public and must not be exposed as a
// substitute for this authenticated transfer.
func (a *Adapter) DownloadVideo(ctx context.Context, credential account.Credential, rawURL string) (io.ReadCloser, string, int64, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme != "https" || !trustedImageAssetHost(parsed.Hostname()) || parsed.User != nil {
		return nil, "", 0, fmt.Errorf("视频内容 URL 不受信任")
	}
	token, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return nil, "", 0, err
	}
	// 视频生成与成品下载必须复用同一账号身份；否则 Resin 会为 WebAsset
	// 重新分配租约，账号级 Cloudflare clearance 也不会进入下载请求。
	lease, err := a.egress.AcquireCredential(ctx, domainegress.ScopeWebAsset, credential)
	if err != nil {
		return nil, "", 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		lease.Release()
		return nil, "", 0, err
	}
	request.Header = buildHeaders(token, lease, "")
	request.Header.Del("Content-Type")
	response, err := lease.Do(request)
	if err != nil {
		a.egress.FeedbackForScope(context.WithoutCancel(ctx), domainegress.ScopeWebAsset, lease.NodeID, 0, err)
		lease.Release()
		return nil, "", 0, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_ = response.Body.Close()
		a.egress.FeedbackForScope(context.WithoutCancel(ctx), domainegress.ScopeWebAsset, lease.NodeID, response.StatusCode, nil)
		lease.Release()
		return nil, "", 0, fmt.Errorf("下载视频返回 %d", response.StatusCode)
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]))
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = "video/mp4"
	}
	if !strings.HasPrefix(contentType, "video/") {
		_ = response.Body.Close()
		lease.Release()
		return nil, "", 0, fmt.Errorf("上游视频 Content-Type 无效")
	}
	onFinished := func(readErr error, complete bool) {
		if readErr != nil {
			a.egress.FeedbackForScope(context.WithoutCancel(ctx), domainegress.ScopeWebAsset, lease.NodeID, 0, readErr)
		} else if complete {
			a.egress.FeedbackForScope(context.WithoutCancel(ctx), domainegress.ScopeWebAsset, lease.NodeID, response.StatusCode, nil)
		}
		lease.Release()
	}
	return provider.NewCompletionReadCloser(response.Body, onFinished), contentType, response.ContentLength, nil
}

func parseVideoStream(response *http.Response, progress func(int)) (provider.VideoResult, string, error) {
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, webMediaDiagnosticBodyLimit+1))
		if response.StatusCode == http.StatusUnauthorized {
			return provider.VideoResult{}, "", provider.ErrUnauthorized
		}
		truncated := len(body) > webMediaDiagnosticBodyLimit
		if truncated {
			body = body[:webMediaDiagnosticBodyLimit]
		}
		return provider.VideoResult{}, "", newWebMediaUpstreamError(response.StatusCode, body, truncated)
	}
	var result provider.VideoResult
	var postID string
	handle := func(root map[string]any) (bool, error) {
		if errorValue, ok := root["error"].(map[string]any); ok {
			return false, webMediaStreamError(errorValue)
		}
		if errorValue := nestedMap(root, "result", "response", "error"); errorValue != nil {
			return false, webMediaStreamError(errorValue)
		}
		stream := nestedMap(root, "result", "response", "streamingVideoGenerationResponse")
		if stream != nil {
			if value, ok := numberAsInt(stream["progress"]); ok && progress != nil {
				progress(value)
			}
			if value, _ := stream["videoPostId"].(string); value != "" {
				postID = value
			} else if value, _ := stream["videoId"].(string); value != "" {
				postID = value
			}
			moderated, _ := stream["moderated"].(bool)
			if moderated {
				return false, nil
			}
			if setVideoResultURL(&result, firstString(stream, "videoUrl", "contentUrl", "contentURL", "assetUrl", "assetURL", "fileUri", "fileURL")) {
				return true, nil
			}
		}
		for _, attachment := range videoFileAttachments(root) {
			if setVideoResultURL(&result, attachment) {
				return true, nil
			}
		}
		return false, nil
	}

	reader := bufio.NewReader(response.Body)
	prefix, _ := reader.Peek(64)
	trimmedPrefix := strings.TrimSpace(string(prefix))
	var err error
	if strings.HasPrefix(trimmedPrefix, "data:") || strings.HasPrefix(trimmedPrefix, "event:") {
		err = consumeVideoSSE(reader, handle)
	} else {
		err = consumeVideoJSON(reader, handle)
	}
	if err != nil {
		return provider.VideoResult{}, "", err
	}
	return result, postID, nil
}

func webMediaStreamError(value map[string]any) error {
	message := safeWebMediaDiagnostic(firstString(value, "message", "error", "detail"), webMediaDiagnosticFieldLimit)
	if message == "" {
		message = "未提供错误详情"
	}
	return fmt.Errorf("视频上游错误: %s", message)
}

func videoFileAttachments(root map[string]any) []string {
	modelResponse := nestedMap(root, "result", "response", "modelResponse")
	if modelResponse == nil {
		return nil
	}
	values, _ := modelResponse["fileAttachments"].([]any)
	attachments := make([]string, 0, len(values))
	for _, value := range values {
		if attachment, _ := value.(string); attachment != "" {
			attachments = append(attachments, attachment)
		}
	}
	return attachments
}

func setVideoResultURL(result *provider.VideoResult, value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	lower := strings.ToLower(value)
	if !strings.HasSuffix(strings.SplitN(lower, "?", 2)[0], ".mp4") && !strings.Contains(lower, "/content") {
		return false
	}
	result.URL = absoluteAssetURL(value)
	result.ContentType = "video/mp4"
	return true
}

func consumeVideoSSE(reader io.Reader, handle func(map[string]any) (bool, error)) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "data:") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
		if line == "" || line == "[DONE]" || !strings.HasPrefix(line, "{") {
			continue
		}
		var root map[string]any
		if json.Unmarshal([]byte(line), &root) != nil {
			continue
		}
		complete, err := handle(root)
		if err != nil {
			return err
		}
		if complete {
			return nil
		}
	}
	return scanner.Err()
}

func consumeVideoJSON(reader io.Reader, handle func(map[string]any) (bool, error)) error {
	decoder := json.NewDecoder(io.LimitReader(reader, 64<<20))
	for {
		var root map[string]any
		if err := decoder.Decode(&root); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("解析视频上游流: %w", err)
		}
		complete, err := handle(root)
		if err != nil {
			return err
		}
		if complete {
			return nil
		}
	}
}

func nestedMap(value map[string]any, keys ...string) map[string]any {
	current := value
	for _, key := range keys {
		next, ok := current[key].(map[string]any)
		if !ok {
			return nil
		}
		current = next
	}
	return current
}

func videoSegments(seconds int) []int {
	if seconds < 1 || seconds > 15 {
		return nil
	}
	return []int{seconds}
}

func videoCreatePayload(prompt, parentID, ratio, resolution string, seconds int, references []string) map[string]any {
	config := map[string]any{"parentPostId": parentID, "aspectRatio": ratio, "videoLength": seconds, "resolutionName": resolution}
	if len(references) > 0 {
		config["isVideoEdit"] = false
		config["isReferenceToVideo"] = true
		config["imageReferences"] = references
	}
	return map[string]any{
		"temporary": true, "modelName": "imagine-video-gen", "message": prompt + " --mode=custom", "enableSideBySide": true,
		"responseMetadata": map[string]any{"experiments": []any{}, "modelConfigOverride": map[string]any{"modelMap": map[string]any{"videoGenModelConfig": config}}},
	}
}
