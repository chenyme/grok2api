package web

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

type webMediaUpstreamError struct {
	status  int
	summary string
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
)

// newWebMediaUpstreamError keeps the HTTP status while exposing only a
// bounded, redacted summary to logs, persisted jobs, and API responses.
func newWebMediaUpstreamError(status int, body []byte, truncated bool) *webMediaUpstreamError {
	return &webMediaUpstreamError{status: status, summary: summarizeWebMediaUpstreamError(status, body, truncated)}
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
		return provider.VideoResult{}, err
	}
	lease, err := a.egress.AcquireCredential(ctx, domainegress.ScopeWeb, request.Credential)
	if err != nil {
		return provider.VideoResult{}, err
	}
	defer lease.Release()
	parentID := ""
	references := make([]string, 0, len(request.ReferenceURLs))
	for _, rawReference := range request.ReferenceURLs {
		reference, referenceErr := a.prepareVideoReference(ctx, cfg, lease, token, rawReference)
		if referenceErr != nil {
			return provider.VideoResult{}, referenceErr
		}
		references = append(references, reference)
	}
	if len(references) > 0 {
		parentID, err = a.createMediaPost(ctx, cfg, lease, token, "MEDIA_POST_TYPE_IMAGE", references[0], "")
	} else {
		parentID, err = a.createMediaPost(ctx, cfg, lease, token, "MEDIA_POST_TYPE_VIDEO", "", request.Prompt)
	}
	if err != nil {
		return provider.VideoResult{}, err
	}
	segments := videoSegments(request.Duration)
	if len(segments) == 0 {
		return provider.VideoResult{}, fmt.Errorf("duration 必须在 1 到 15 秒之间")
	}
	ratio := resolveAspectRatio(request.AspectRatio)
	resolution := request.Resolution
	if resolution == "" {
		resolution = "720p"
	}
	payload := videoCreatePayload(request.Prompt, parentID, ratio, resolution, segments[0], references)
	response, err := a.postJSON(ctx, cfg, lease, token, cfg.BaseURL+"/rest/app-chat/conversations/new", payload, time.Duration(cfg.VideoTimeoutSeconds)*time.Second)
	if err != nil {
		return provider.VideoResult{}, err
	}
	result, _, parseErr := parseVideoStream(response, request.Progress)
	_ = response.Body.Close()
	if parseErr != nil {
		return provider.VideoResult{}, parseErr
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
	uploaded, err := a.uploadFileLegacy(ctx, cfg, lease, token, image, cfg.BaseURL+"/imagine")
	if err != nil {
		return "", err
	}
	if uploaded.URI == "" {
		return "", fmt.Errorf("上传视频参考图片后未返回 fileUri")
	}
	return uploaded.URI, nil
}

type videoContentReadCloser struct {
	io.ReadCloser
	release func()
}

func (c *videoContentReadCloser) Close() error {
	err := c.ReadCloser.Close()
	if c.release != nil {
		c.release()
		c.release = nil
	}
	return err
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
		lease.Release()
		return nil, "", 0, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_ = response.Body.Close()
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
	return &videoContentReadCloser{ReadCloser: response.Body, release: lease.Release}, contentType, response.ContentLength, nil
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
	diagnostics := videoStreamDiagnostics{}
	handle := func(root map[string]any) (bool, error) {
		diagnostics.frames++
		if errorValue, exists := root["error"]; exists {
			if streamErr := webMediaStreamErrorValue(errorValue); streamErr != nil {
				return false, streamErr
			}
		}
		if responseValue := nestedMap(root, "result", "response"); responseValue != nil {
			if errorValue, exists := responseValue["error"]; exists {
				if streamErr := webMediaStreamErrorValue(errorValue); streamErr != nil {
					return false, streamErr
				}
			}
		}
		stream := nestedMap(root, "result", "response", "streamingVideoGenerationResponse")
		if stream != nil {
			diagnostics.streamFrames++
			if value, ok := numberAsInt(stream["progress"]); ok {
				diagnostics.progress = max(diagnostics.progress, value)
				if progress != nil {
					progress(value)
				}
			}
			if value := firstString(stream, "videoId", "video_id"); value != "" {
				diagnostics.videoID = value
			}
			if value := firstString(stream, "assetId", "asset_id"); value != "" {
				diagnostics.assetID = value
			}
			if value := firstString(stream, "videoPostId", "video_post_id"); value != "" {
				postID = value
			} else if value := firstString(stream, "videoId", "video_id"); value != "" {
				postID = value
			}
			diagnostics.postID = postID
			if value := firstString(stream, "status", "currentStatus", "current_status"); value != "" {
				diagnostics.status = value
			}
			moderated, _ := stream["moderated"].(bool)
			if moderated {
				diagnostics.moderated = true
				return false, nil
			}
			candidate := firstString(stream, "videoUrl", "contentUrl", "contentURL", "assetUrl", "assetURL", "fileUri", "fileURL")
			if candidate != "" {
				diagnostics.urlCandidates++
			}
			if !diagnostics.moderated && setVideoResultURL(&result, candidate) {
				return true, nil
			}
			if candidate != "" {
				diagnostics.rejectedURLs++
			}
		}
		if modelResponse := nestedMap(root, "result", "response", "modelResponse"); modelResponse != nil {
			if value := firstString(modelResponse, "message", "text", "token"); value != "" {
				diagnostics.message = safeWebMediaDiagnostic(value, webMediaDiagnosticFieldLimit)
			}
		}
		for _, attachment := range videoFileAttachments(root) {
			diagnostics.urlCandidates++
			if !diagnostics.moderated && setVideoResultURL(&result, attachment) {
				return true, nil
			}
			diagnostics.rejectedURLs++
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
	if result.URL == "" {
		return provider.VideoResult{}, postID, diagnostics.missingContentError()
	}
	return result, postID, nil
}

type videoStreamDiagnostics struct {
	frames        int
	streamFrames  int
	progress      int
	urlCandidates int
	rejectedURLs  int
	moderated     bool
	videoID       string
	assetID       string
	postID        string
	status        string
	message       string
}

func (d videoStreamDiagnostics) missingContentError() error {
	reason := "视频生成流结束但没有返回内容 URL"
	if d.moderated {
		reason = "视频被上游内容审核拦截"
	} else if d.rejectedURLs > 0 {
		reason = "视频上游返回了不受支持的内容 URL"
	}
	parts := []string{
		fmt.Sprintf("progress=%d", d.progress),
		fmt.Sprintf("frames=%d", d.frames),
		fmt.Sprintf("stream_frames=%d", d.streamFrames),
		fmt.Sprintf("url_candidates=%d", d.urlCandidates),
	}
	if d.moderated {
		parts = append(parts, "moderated=true")
	}
	if d.status != "" {
		parts = append(parts, "status="+safeWebMediaDiagnostic(d.status, 64))
	}
	if d.videoID != "" {
		parts = append(parts, "video_id="+safeWebMediaDiagnostic(d.videoID, 80))
	}
	if d.assetID != "" {
		parts = append(parts, "asset_id="+safeWebMediaDiagnostic(d.assetID, 80))
	}
	if d.postID != "" {
		parts = append(parts, "post_id="+safeWebMediaDiagnostic(d.postID, 80))
	}
	if d.message != "" {
		parts = append(parts, "message="+d.message)
	}
	return fmt.Errorf("%s (%s)", reason, strings.Join(parts, ", "))
}

func webMediaStreamError(value map[string]any) error {
	code := safeWebMediaDiagnostic(firstWebMediaDiagnosticCode(value, "code", "error_code", "systemErrCode"), 64)
	errorType := safeWebMediaDiagnostic(firstString(value, "type", "error_type"), 64)
	status := safeWebMediaDiagnostic(firstWebMediaDiagnosticCode(value, "status", "status_code"), 64)
	internalError := webMediaDiagnosticScalar(value["internalError"], 64)
	severity := webMediaDiagnosticScalar(value["severity"], 64)
	reason := safeWebMediaDiagnostic(firstString(value, "reason"), webMediaDiagnosticFieldLimit)
	message := webMediaStreamMessage(value)
	details := webMediaStreamDetails(value["details"])
	parts := make([]string, 0, 9)
	for _, item := range []struct {
		name  string
		value string
	}{
		{name: "code", value: code},
		{name: "type", value: errorType},
		{name: "status", value: status},
		{name: "internal_error", value: internalError},
		{name: "severity", value: severity},
		{name: "reason", value: reason},
		{name: "message", value: message},
		{name: "details", value: details},
	} {
		if item.value != "" {
			parts = append(parts, item.name+"="+item.value)
		}
	}
	if reason == "" && message == "" && details == "" {
		parts = append(parts, "fields=["+strings.Join(webMediaDiagnosticKeys(value), ",")+"]")
	}
	if len(parts) == 0 {
		parts = append(parts, "未提供错误详情")
	}
	return fmt.Errorf("视频上游错误: %s", boundWebMediaDiagnostic(strings.Join(parts, ", "), webMediaDiagnosticSummaryLimit))
}

func webMediaStreamErrorValue(value any) error {
	switch typed := value.(type) {
	case map[string]any:
		if !webMediaStreamErrorActive(typed) {
			return nil
		}
		return webMediaStreamError(typed)
	case string:
		message := safeWebMediaDiagnostic(typed, webMediaDiagnosticFieldLimit)
		if message != "" {
			return fmt.Errorf("视频上游错误: message=%s", message)
		}
	}
	return nil
}

func webMediaStreamErrorActive(value map[string]any) bool {
	for key, item := range value {
		// Some Grok video frames include an empty error envelope whose severity
		// is populated even though every actual error value is empty.
		if key == "severity" {
			continue
		}
		if webMediaStreamValueMeaningful(item) {
			return true
		}
	}
	return false
}

func webMediaStreamValueMeaningful(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case string:
		value := strings.ToLower(strings.TrimSpace(typed))
		return value != "" && value != "0" && value != "false" && value != "null" && value != "none"
	case bool:
		return typed
	case float64:
		return typed != 0
	case float32:
		return typed != 0
	case int:
		return typed != 0
	case int64:
		return typed != 0
	case json.Number:
		return typed.String() != "" && typed.String() != "0"
	case map[string]any:
		return webMediaStreamErrorActive(typed)
	case []any:
		for _, item := range typed {
			if webMediaStreamValueMeaningful(item) {
				return true
			}
		}
		return false
	default:
		return true
	}
}

func webMediaStreamMessage(value map[string]any) string {
	for _, key := range []string{"message", "error", "detail", "description", "error_description"} {
		if message := webMediaStreamDetails(value[key]); message != "" {
			return message
		}
	}
	return ""
}

func webMediaDiagnosticScalar(value any, limit int) string {
	switch typed := value.(type) {
	case string:
		return safeWebMediaDiagnostic(typed, limit)
	case bool, float64, float32, int, int64, json.Number:
		return safeWebMediaDiagnostic(fmt.Sprint(typed), limit)
	default:
		return ""
	}
}

func webMediaStreamDetails(value any) string {
	switch typed := value.(type) {
	case string:
		return safeWebMediaDiagnostic(typed, webMediaDiagnosticFieldLimit)
	case map[string]any:
		code := safeWebMediaDiagnostic(firstWebMediaDiagnosticCode(typed, "code", "error_code", "systemErrCode"), 64)
		message := safeWebMediaDiagnostic(firstString(typed, "message", "text", "error", "detail", "reason", "description"), webMediaDiagnosticFieldLimit)
		if code != "" && message != "" {
			return boundWebMediaDiagnostic(code+": "+message, webMediaDiagnosticFieldLimit)
		}
		if message != "" {
			return message
		}
		if code != "" {
			return code
		}
		return "fields=[" + strings.Join(webMediaDiagnosticKeys(typed), ",") + "]"
	case []any:
		parts := make([]string, 0, min(3, len(typed)))
		for _, item := range typed {
			if len(parts) >= 3 {
				break
			}
			if detail := webMediaStreamDetails(item); detail != "" {
				parts = append(parts, detail)
			}
		}
		return boundWebMediaDiagnostic(strings.Join(parts, "; "), webMediaDiagnosticFieldLimit)
	case bool, float64, float32, int, int64, json.Number:
		return webMediaDiagnosticScalar(typed, webMediaDiagnosticFieldLimit)
	default:
		return ""
	}
}

func webMediaDiagnosticKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		key = safeWebMediaDiagnostic(key, 32)
		if key != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) > 8 {
		keys = keys[:8]
	}
	return keys
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
