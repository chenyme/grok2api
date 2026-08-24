package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"sort"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/websocket"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
)

const (
	maxGeneratedImages            = 10
	mediaOutputAttempts           = 3
	imageDownloadTimeout          = 60 * time.Second
	imagineSelfUploadSource       = "IMAGINE_SELF_UPLOAD_FILE_SOURCE"
	directFileUploadResponseLimit = 2 << 20
)

var errLiteImageReady = errors.New("Lite 图片已完成")

type imagineModelConfig struct {
	Pro            bool
	ExpectedCount  int
	MaxReturnCount int
}

type imagineImageValue struct {
	ID       string
	URL      string
	Blob     string
	Position int
	Width    int
	Height   int
	position bool
}

type imagineSlot struct {
	image          imagineImageValue
	preview        imagineImageValue
	final          bool
	previewReady   bool
	previewEmitted bool
	completed      bool
	moderated      bool
	emitted        bool
}

type imagineCollector struct {
	slots         map[string]*imagineSlot
	terminalCount int
}

func resolveImagineModel(model string, pro bool, count int) (imagineModelConfig, bool) {
	if model != "imagine" {
		return imagineModelConfig{}, false
	}
	return imagineModelConfig{Pro: pro, ExpectedCount: count, MaxReturnCount: 10}, true
}

func invalidImageRequest(message string) (*provider.Response, error) {
	return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{
		"message": message, "type": "invalid_request_error",
	}}), nil
}

func imageGenerationUserError(message, param, code string) (*provider.Response, error) {
	return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{
		"message": message, "type": "image_generation_user_error", "param": param, "code": code,
	}}), nil
}

func newImagineCollector() *imagineCollector {
	return &imagineCollector{slots: make(map[string]*imagineSlot)}
}

func (c *imagineCollector) Accept(message map[string]any) {
	typeName, _ := message["type"].(string)
	if typeName != "image" && typeName != "json" {
		return
	}
	rawURL, _ := message["url"].(string)
	imageID := firstString(message, "image_id", "job_id", "id")
	if imageID == "" && rawURL != "" {
		imageID = imageIDFromURL(rawURL)
	}
	if imageID == "" {
		return
	}
	slot := c.slots[imageID]
	if slot == nil {
		slot = &imagineSlot{image: imagineImageValue{ID: imageID}}
		c.slots[imageID] = slot
	}
	if typeName == "image" {
		if position, ok := firstInt(message, "side_by_side_index", "order", "grid_index"); ok {
			slot.image.Position = position
			slot.image.position = true
		}
		width, _ := numberAsInt(message["width"])
		height, _ := numberAsInt(message["height"])
		progress, hasProgress := numberAsInt(message["percentage_complete"])
		if hasProgress && progress < 100 {
			slot.preview = imagineImageValue{
				ID: imageID, URL: absoluteAssetURL(rawURL), Position: slot.image.Position,
				Width: width, Height: height, position: slot.image.position,
			}
			slot.preview.Blob, _ = message["blob"].(string)
			slot.previewReady = true
			return
		}
		slot.image.URL = absoluteAssetURL(rawURL)
		slot.image.Blob, _ = message["blob"].(string)
		slot.image.Width = width
		slot.image.Height = height
		slot.final = true
		return
	}
	status, _ := message["current_status"].(string)
	if position, ok := numberAsInt(message["order"]); ok && !slot.image.position {
		slot.image.Position = position
		slot.image.position = true
	}
	if width, ok := numberAsInt(message["width"]); ok && slot.image.Width == 0 {
		slot.image.Width = width
	}
	if height, ok := numberAsInt(message["height"]); ok && slot.image.Height == 0 {
		slot.image.Height = height
	}
	if status != "completed" {
		return
	}
	if rawURL != "" && !slot.final {
		slot.image.URL = absoluteAssetURL(rawURL)
		slot.image.Blob, _ = message["blob"].(string)
		slot.final = true
	}
	if !slot.completed {
		slot.completed = true
		c.terminalCount++
	}
	slot.moderated, _ = message["moderated"].(bool)
}

func (c *imagineCollector) Done(expected int) bool {
	if expected <= 0 || c.terminalCount < expected {
		return false
	}
	for _, slot := range c.slots {
		if slot.completed && !slot.moderated && (!slot.final || (slot.image.URL == "" && slot.image.Blob == "")) {
			return false
		}
	}
	return true
}

func (c *imagineCollector) Images() []imagineImageValue {
	values := make([]imagineImageValue, 0, len(c.slots))
	for _, slot := range c.slots {
		if slot.completed && !slot.moderated && slot.final && (slot.image.URL != "" || slot.image.Blob != "") {
			values = append(values, slot.image)
		}
	}
	sortImagineImages(values)
	return values
}

func (c *imagineCollector) ReadyImages() []imagineImageValue {
	values := make([]imagineImageValue, 0, len(c.slots))
	for _, slot := range c.slots {
		if slot.completed && !slot.moderated && slot.final && !slot.emitted && (slot.image.URL != "" || slot.image.Blob != "") {
			slot.emitted = true
			values = append(values, slot.image)
		}
	}
	sortImagineImages(values)
	return values
}

func (c *imagineCollector) ReadyPreviews() []imagineImageValue {
	values := make([]imagineImageValue, 0)
	for _, slot := range c.slots {
		if slot.previewReady && !slot.previewEmitted {
			slot.previewEmitted = true
			values = append(values, slot.preview)
		}
	}
	sortImagineImages(values)
	return values
}

func sortImagineImages(values []imagineImageValue) {
	sort.SliceStable(values, func(i, j int) bool {
		if values[i].position != values[j].position {
			return values[i].position
		}
		if values[i].Position != values[j].Position {
			return values[i].Position < values[j].Position
		}
		return values[i].ID < values[j].ID
	})
}

func (c *imagineCollector) UsableCount() int {
	count := 0
	for _, slot := range c.slots {
		if slot.completed && !slot.moderated && slot.final && (slot.image.URL != "" || slot.image.Blob != "") {
			count++
		}
	}
	return count
}

func firstString(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if result, _ := value[key].(string); result != "" {
			return result
		}
	}
	return ""
}

func firstInt(value map[string]any, keys ...string) (int, bool) {
	for _, key := range keys {
		if result, ok := numberAsInt(value[key]); ok {
			return result, true
		}
	}
	return 0, false
}

func numberAsInt(value any) (int, bool) {
	switch number := value.(type) {
	case float64:
		return int(number), true
	case int:
		return number, true
	case json.Number:
		parsed, err := number.Int64()
		return int(parsed), err == nil
	default:
		return 0, false
	}
}

func (a *Adapter) GenerateImage(ctx context.Context, request provider.ImageGenerationRequest) (*provider.Response, error) {
	count := request.Count
	if count <= 0 {
		count = 1
	}
	if request.Streaming && count != 1 {
		return imageGenerationUserError("Streaming is only supported with n=1.", "input", "unsupported_parameter")
	}
	if request.PartialImages < 0 || request.PartialImages > 3 {
		return invalidImageRequest("partial_images 必须在 0 到 3 之间")
	}
	if request.PartialImages > 0 && !request.Streaming {
		return invalidImageRequest("partial_images 仅可在 stream=true 时使用")
	}
	format := strings.ToLower(strings.TrimSpace(request.ResponseFormat))
	if format == "" {
		format = "url"
	}
	if format != "url" && format != "b64_json" {
		return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "response_format 必须是 url 或 b64_json", "type": "invalid_request_error"}}), nil
	}
	spec, modelKnown := Resolve(request.Model)
	if !modelKnown || spec.Capability != "image" {
		return invalidImageRequest("模型不支持图片生成")
	}
	protocolModel := spec.ProtocolModel
	if protocolModel == "" {
		protocolModel = spec.UpstreamModel
	}
	if protocolModel == "imagine-lite" {
		if request.Streaming {
			return invalidImageRequest("grok-imagine-image-lite 不支持 stream")
		}
		if count > maxGeneratedImages {
			return invalidImageRequest("n 不能超过 10")
		}
		return a.generateLiteImage(ctx, request, count, format)
	}
	ratio, err := resolveImageAspectRatio(request.AspectRatio, request.Size)
	if err != nil {
		return invalidImageRequest(err.Error())
	}
	modelConfig, ok := resolveImagineModel(protocolModel, spec.ImaginePro, count)
	if !ok {
		return invalidImageRequest("模型不支持图片生成")
	}
	if count > modelConfig.MaxReturnCount {
		return invalidImageRequest(fmt.Sprintf("n 不能超过 %d", modelConfig.MaxReturnCount))
	}
	return a.generateWSImage(ctx, request, count, format, ratio, modelConfig)
}

func (a *Adapter) generateLiteImage(ctx context.Context, request provider.ImageGenerationRequest, count int, format string) (*provider.Response, error) {
	spec, _ := Resolve(request.Model)
	urls := make([]string, 0, count)
	for len(urls) < count {
		value, err := a.generateLiteImageURL(ctx, request.Credential, spec, request.Prompt)
		if err != nil {
			var mediaErr *webMediaUpstreamError
			if errors.As(err, &mediaErr) && len(urls) == 0 {
				return mediaErr.providerResponse(), nil
			}
			var upstreamErr *liteUpstreamError
			if errors.As(err, &upstreamErr) && len(urls) == 0 {
				return upstreamErr.Response(), nil
			}
			if len(urls) > 0 {
				return jsonProviderResponse(http.StatusBadGateway, map[string]any{"error": map[string]any{
					"message": fmt.Sprintf("Lite 图片仅完成 %d/%d 张: %v", len(urls), count, err),
					"type":    "server_error", "code": "image_generation_incomplete",
				}}), nil
			}
			return nil, err
		}
		urls = append(urls, value)
	}
	response, err := a.imageResponse(ctx, request.Credential, urls, nil, count, format)
	if response != nil {
		response.QuotaUnits = count
	}
	return response, err
}

type liteUpstreamError struct {
	StatusCode int
	Status     string
	Body       []byte
}

func (e *liteUpstreamError) Error() string {
	return fmt.Sprintf("Lite 图片上游返回 %d", e.StatusCode)
}

func (e *liteUpstreamError) Response() *provider.Response {
	return &provider.Response{StatusCode: e.StatusCode, Status: e.Status, Header: jsonHeaders(), Body: io.NopCloser(bytes.NewReader(e.Body))}
}

func (a *Adapter) generateLiteImageURL(ctx context.Context, credential account.Credential, spec ModelSpec, prompt string) (string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		upstream, lease, _, statsigTarget, err := a.openChat(ctx, credential, "", spec, normalizedChatInput{Prompt: "Drawing: " + prompt}, gatewayOpenOptions{deferForbidden: true})
		if err != nil {
			return "", err
		}
		if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(upstream.Body, webMediaDiagnosticBodyLimit+1))
			_ = upstream.Body.Close()
			truncated := len(body) > webMediaDiagnosticBodyLimit
			if truncated {
				body = body[:webMediaDiagnosticBodyLimit]
			}
			if upstream.StatusCode == http.StatusForbidden {
				upstreamErr := newWebMediaUpstreamError(upstream.StatusCode, body, truncated)
				a.logWebMediaUpstreamRejection("image_lite_handshake", upstream, upstreamErr)
				if isClearanceRefreshableMediaError(upstreamErr) {
					// The failed WebSocket handshake invalidates the current browser
					// session. Statsig is independent and must not gate reacquiring
					// a fresh lease for the retry.
					lease.InvalidateClearance()
					_ = a.invalidateSignedStatsig(http.MethodPost, statsigTarget)
					if attempt == 0 {
						lease.Release()
						continue
					}
				}
				lease.Release()
				return "", upstreamErr
			}
			a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, upstream.StatusCode, nil)
			lease.Release()
			return "", &liteUpstreamError{StatusCode: upstream.StatusCode, Status: upstream.Status, Body: body}
		}
		firstImage := ""
		capture := &boundedCapture{limit: 8 << 20}
		parsed, consumeErr := consumeUpstream(io.TeeReader(upstream.Body, capture), func(kind, delta string) error {
			if kind != "image" || strings.TrimSpace(delta) == "" {
				return nil
			}
			firstImage = delta
			return errLiteImageReady
		})
		_ = upstream.Body.Close()
		if consumeErr != nil && !errors.Is(consumeErr, errLiteImageReady) {
			if errors.Is(consumeErr, errWebUsageLimit) {
				lease.Release()
				response := jsonProviderResponse(http.StatusTooManyRequests, map[string]any{"error": map[string]any{
					"message": "Grok Imagine 速率限制中，请稍后重试",
					"type":    "rate_limit_error",
					"code":    "usage_limit_reached",
				}})
				body, _ := io.ReadAll(response.Body)
				_ = response.Body.Close()
				return "", &liteUpstreamError{StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests", Body: body}
			}
			status := 0
			if errors.Is(consumeErr, errWebAntiBot) {
				status = http.StatusForbidden
				// A challenge can arrive inside an otherwise successful stream,
				// so the handshake path cannot invalidate it for us.
				lease.InvalidateClearance()
				if attempt == 0 {
					_ = a.invalidateSignedStatsig(http.MethodPost, statsigTarget)
					lease.Release()
					continue
				}
			}
			a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, status, consumeErr)
			lease.Release()
			if status == http.StatusForbidden {
				response := antiBotProviderResponse()
				body, _ := io.ReadAll(response.Body)
				_ = response.Body.Close()
				return "", &liteUpstreamError{StatusCode: status, Status: "403 Forbidden", Body: body}
			}
			return "", consumeErr
		}
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, http.StatusOK, nil)
		lease.Release()
		if firstImage != "" {
			return firstImage, nil
		}
		if len(parsed.Images) == 0 {
			parsed.Images = extractMarkdownImages(parsed.Text.String())
		}
		if len(parsed.Images) == 0 {
			parsed.Images = extractCapturedImageURLs(capture.Bytes())
		}
		if len(parsed.Images) == 0 {
			diagnostics := inspectLiteCapture(capture.Bytes())
			a.log().Warn("web_lite_image_not_found",
				"account_id", credential.ID,
				"captured_bytes", len(capture.Bytes()),
				"frames", diagnostics.Frames,
				"response_fields", diagnostics.ResponseFields,
				"message_tags", diagnostics.MessageTags,
				"image_chunks", diagnostics.ImageChunks,
				"image_urls", diagnostics.ImageURLs,
				"image_fields", diagnostics.ImageFields,
				"max_progress", diagnostics.MaxProgress,
				"soft_stop", diagnostics.SoftStop,
				"upstream_error_code", diagnostics.ErrorCode,
				"upstream_error", diagnostics.ErrorMessage,
			)
			return "", fmt.Errorf("Grok Web Lite 响应结束但未解析到最终图片")
		}
		// Lite 上游固定生成两张，但每次查询只计一次 Fast 额度；按旧协议取首张并为 n 重复查询。
		return parsed.Images[0], nil
	}
	return "", fmt.Errorf("Grok Web Lite 图片签名刷新失败")
}

func (a *Adapter) forwardImageChatCompletion(ctx context.Context, request provider.ResponseResourceRequest, input openAIRequest, normalized normalizedChatInput, spec ModelSpec) (*provider.Response, error) {
	if len(normalized.Attachments) > 0 {
		return invalidImageRequest("文生图模型只接受当前用户消息中的纯文本；图生图请使用 grok-imagine-image-edit 和 /v1/images/edits")
	}
	count := 1
	format := "url"
	if input.ImageConfig != nil {
		if input.ImageConfig.Count != nil {
			count = *input.ImageConfig.Count
		}
		if strings.TrimSpace(input.ImageConfig.ResponseFormat) != "" {
			format = strings.ToLower(strings.TrimSpace(input.ImageConfig.ResponseFormat))
		}
	}
	if count < 1 || count > maxGeneratedImages {
		return invalidImageRequest("image_config.n 必须在 1 到 10 之间")
	}
	if format != "url" && format != "b64_json" {
		return invalidImageRequest("image_config.response_format 必须是 url 或 b64_json")
	}
	if spec.ProtocolModel != "imagine-lite" {
		return a.forwardQualityImageChatCompletion(ctx, request, input, normalized, count, format)
	}
	responseID := newWebID("resp")
	parsed := parsedChat{ResponseID: responseID, InputTokens: estimateTokens(normalized.Prompt)}
	for range count {
		rawURL, err := a.generateLiteImageURL(ctx, request.Credential, spec, normalized.Prompt)
		if err != nil {
			var mediaErr *webMediaUpstreamError
			if errors.As(err, &mediaErr) && parsed.Text.Len() == 0 {
				return mediaErr.providerResponse(), nil
			}
			var upstreamErr *liteUpstreamError
			if errors.As(err, &upstreamErr) && parsed.Text.Len() == 0 {
				return upstreamErr.Response(), nil
			}
			return nil, err
		}
		item, err := a.imageDataItem(ctx, request.Credential, imagineImageValue{URL: rawURL}, format)
		if err != nil {
			return nil, err
		}
		if parsed.Text.Len() > 0 {
			parsed.appendText("\n\n")
		}
		parsed.appendText(liteImageMarkdown(item))
	}
	if input.Stream || request.Streaming {
		stream, err := buildImageCompatibilityStream(request.Operation, responseID, input.Model, &parsed)
		if err != nil {
			return nil, err
		}
		return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: streamHeaders(), Body: io.NopCloser(bytes.NewReader(stream)), QuotaUnits: count}, nil
	}
	payload := buildOpenAIResult(request.Operation, responseID, input.Model, parsed, false)
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: jsonHeaders(), Body: io.NopCloser(bytes.NewReader(data)), QuotaUnits: count}, nil
}

func (a *Adapter) forwardQualityImageChatCompletion(ctx context.Context, request provider.ResponseResourceRequest, input openAIRequest, normalized normalizedChatInput, count int, format string) (*provider.Response, error) {
	aspectRatio := ""
	resolution := ""
	if input.ImageConfig != nil {
		aspectRatio = input.ImageConfig.AspectRatio
		resolution = input.ImageConfig.Resolution
	}
	generated, err := a.GenerateImage(ctx, provider.ImageGenerationRequest{
		Credential: request.Credential, Model: request.Model, Prompt: normalized.Prompt,
		Count: count, AspectRatio: aspectRatio, Resolution: resolution, ResponseFormat: format,
	})
	if err != nil {
		return nil, err
	}
	if generated.StatusCode < http.StatusOK || generated.StatusCode >= http.StatusMultipleChoices {
		return generated, nil
	}
	defer generated.Body.Close()
	var payload struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(generated.Body).Decode(&payload); err != nil {
		return nil, provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, fmt.Errorf("图片生成兼容响应解析失败: %w", err))
	}
	parsed := parsedChat{ResponseID: newWebID("resp"), InputTokens: estimateTokens(normalized.Prompt)}
	for _, item := range payload.Data {
		markdown := liteImageMarkdown(item)
		if markdown == "" {
			continue
		}
		if parsed.Text.Len() > 0 {
			parsed.appendText("\n\n")
		}
		parsed.appendText(markdown)
	}
	if parsed.Text.Len() == 0 {
		return nil, fmt.Errorf("图片生成兼容响应中没有图片")
	}
	if input.Stream || request.Streaming {
		stream, err := buildImageCompatibilityStream(request.Operation, parsed.ResponseID, input.Model, &parsed)
		if err != nil {
			return nil, err
		}
		return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: streamHeaders(), Body: io.NopCloser(bytes.NewReader(stream)), QuotaUnits: generated.QuotaUnits}, nil
	}
	result := buildOpenAIResult(request.Operation, parsed.ResponseID, input.Model, parsed, false)
	data, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: jsonHeaders(), Body: io.NopCloser(bytes.NewReader(data)), QuotaUnits: generated.QuotaUnits}, nil
}

func buildImageCompatibilityStream(operation, responseID, model string, parsed *parsedChat) ([]byte, error) {
	var stream bytes.Buffer
	writeStreamStart(&stream, operation, responseID, model, parsed.InputTokens)
	if operation == conversation.OperationResponses {
		responsesStream := newWebResponsesStream(&stream, responseID)
		if err := responsesStream.Delta("text", parsed.Text.String()); err != nil {
			return nil, err
		}
		if err := responsesStream.Finish(parsed); err != nil {
			return nil, err
		}
	} else if err := writeStreamDelta(&stream, operation, responseID, model, "text", parsed.Text.String()); err != nil {
		return nil, err
	}
	payload := buildOpenAIResult(operation, responseID, model, *parsed, false)
	writeStreamDone(&stream, operation, responseID, model, *parsed, payload)
	return stream.Bytes(), nil
}

func liteImageMarkdown(item map[string]any) string {
	if value, _ := item["url"].(string); value != "" {
		return "![image](" + value + ")"
	}
	if value, _ := item["b64_json"].(string); value != "" {
		mimeType, _ := item["mime_type"].(string)
		if mimeType == "" {
			mimeType = "image/jpeg"
		}
		return "![image](data:" + mimeType + ";base64," + value + ")"
	}
	return ""
}

func (a *Adapter) generateWSImage(ctx context.Context, request provider.ImageGenerationRequest, count int, format, ratio string, modelConfig imagineModelConfig) (*provider.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		response, err := a.generateWSImageAttempt(ctx, request, count, format, ratio, modelConfig)
		if err == nil {
			return response, nil
		}
		var upstreamErr *webMediaUpstreamError
		if !errors.As(err, &upstreamErr) || !isClearanceRefreshableMediaError(upstreamErr) || attempt > 0 {
			if errors.As(err, &upstreamErr) {
				return upstreamErr.providerResponse(), nil
			}
			return nil, err
		}
		a.log().Warn("web_image_clearance_retry", "operation", "imagine", "status", upstreamErr.status, "body_kind", upstreamErr.bodyKind)
	}
	return nil, fmt.Errorf("Imagine WebSocket Clearance 重试耗尽")
}

func (a *Adapter) generateWSImageAttempt(ctx context.Context, request provider.ImageGenerationRequest, count int, format, ratio string, modelConfig imagineModelConfig) (*provider.Response, error) {
	cfg := a.config()
	token, err := a.cipher.Decrypt(request.Credential.EncryptedAccessToken)
	if err != nil {
		return nil, err
	}
	lease, err := a.egress.AcquireCredential(ctx, domainegress.ScopeWeb, request.Credential)
	if err != nil {
		return nil, err
	}
	leaseOwned := true
	defer func() {
		if leaseOwned {
			lease.Release()
		}
	}()
	wsURL, err := imagineURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	headers := fhttp.Header{}
	headers.Set("Origin", cfg.BaseURL)
	headers.Set("User-Agent", lease.UserAgent)
	headers.Set("Cookie", egress.BuildSSOCookie(token, lease.CFCookies))
	headers.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Pragma", "no-cache")
	connection, response, err := lease.DialWebSocketDeferredForbidden(ctx, wsURL, headers, 30*time.Second)
	if err != nil {
		if response != nil {
			var body []byte
			if response.Body != nil {
				body, _ = io.ReadAll(io.LimitReader(response.Body, webMediaDiagnosticBodyLimit+1))
				_ = response.Body.Close()
			}
			truncated := len(body) > webMediaDiagnosticBodyLimit
			if truncated {
				body = body[:webMediaDiagnosticBodyLimit]
			}
			upstreamErr := newWebMediaUpstreamError(response.StatusCode, body, truncated)
			if isClearanceRefreshableMediaError(upstreamErr) {
				lease.InvalidateClearance()
			}
			a.logWebMediaUpstreamRejection("image_imagine_handshake", &http.Response{
				StatusCode: response.StatusCode,
				Header:     http.Header(response.Header).Clone(),
			}, upstreamErr)
			if response.StatusCode != http.StatusForbidden {
				a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, response.StatusCode, err)
			}
			return nil, upstreamErr
		}
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, err)
		return nil, fmt.Errorf("连接 Imagine WebSocket: %w", err)
	}
	connectionOwned := true
	defer func() {
		if connectionOwned {
			_ = connection.Close()
		}
	}()
	connection.SetReadLimit(64 << 20)
	deadline := time.Now().Add(time.Duration(cfg.ImageTimeoutSeconds) * time.Second)
	_ = connection.SetReadDeadline(deadline)
	_ = connection.SetWriteDeadline(deadline)
	if err := connection.WriteJSON(imagineResetMessage()); err != nil {
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, err)
		return nil, err
	}
	if err := connection.WriteJSON(imagineRequestMessage(newWebID("img"), request.Prompt, ratio, cfg.AllowNSFW, modelConfig.Pro, modelConfig.ExpectedCount)); err != nil {
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, err)
		return nil, err
	}
	if request.Streaming {
		reader, writer := io.Pipe()
		streamCtx, cancel := context.WithCancel(ctx)
		leaseOwned = false
		connectionOwned = false
		go a.streamImagineImages(streamCtx, writer, connection, lease, request.Credential, count, request.PartialImages, modelConfig)
		return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: streamHeaders(), Body: &cancelBody{ReadCloser: reader, cancel: cancel}, QuotaUnits: count}, nil
	}

	collector := newImagineCollector()
	for collector.UsableCount() < count && !collector.Done(modelConfig.ExpectedCount) {
		messageType, data, readErr := connection.ReadMessage()
		if readErr != nil {
			a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, readErr)
			return nil, fmt.Errorf("读取 Imagine WebSocket: %w", readErr)
		}
		if messageType != websocket.TextMessage {
			continue
		}
		var message map[string]any
		if json.Unmarshal(data, &message) != nil {
			continue
		}
		if message["type"] == "error" {
			upstreamErr := fmt.Errorf("Imagine WebSocket 返回错误")
			a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, upstreamErr)
			return nil, upstreamErr
		}
		collector.Accept(message)
	}
	a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, http.StatusOK, nil)
	images := collector.Images()
	if len(images) == 0 {
		return nil, fmt.Errorf("Imagine WebSocket 完成但没有可用图片")
	}
	if len(images) < count {
		return jsonProviderResponse(http.StatusBadGateway, map[string]any{"error": map[string]any{
			"message": fmt.Sprintf("上游仅返回 %d/%d 张可用图片", len(images), count),
			"type":    "server_error", "code": "image_generation_incomplete",
		}}), nil
	}
	urls := make([]string, 0, len(images))
	blobs := make([]string, 0, len(images))
	for _, image := range images {
		urls = append(urls, image.URL)
		blobs = append(blobs, image.Blob)
	}
	result, err := a.imageResponse(ctx, request.Credential, urls, blobs, count, format)
	if result != nil {
		result.QuotaUnits = count
	}
	return result, err
}

// EditImage retries the complete browser media flow once after a challenge
// response. Reacquiring the lease is required because the failed lease keeps
// the immutable browser-session cookies that were rejected upstream.
func (a *Adapter) EditImage(ctx context.Context, request provider.ImageEditRequest) (*provider.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		response, err := a.editImageAttempt(ctx, request)
		if err == nil {
			return response, nil
		}
		var upstreamErr *webMediaUpstreamError
		if !errors.As(err, &upstreamErr) || !isClearanceRefreshableMediaError(upstreamErr) || attempt > 0 {
			if errors.As(err, &upstreamErr) {
				return upstreamErr.providerResponse(), nil
			}
			return nil, err
		}
		a.log().Warn("web_image_clearance_retry", "operation", "edit", "status", upstreamErr.status, "body_kind", upstreamErr.bodyKind)
	}
	return nil, fmt.Errorf("图片编辑 Clearance 重试耗尽")
}

func (a *Adapter) editImageAttempt(ctx context.Context, request provider.ImageEditRequest) (*provider.Response, error) {
	if strings.TrimSpace(request.Quality) != "" {
		return invalidImageRequest("Grok Web 图片模型不支持 quality")
	}
	if len(request.ImageURLs) == 0 || len(request.ImageURLs) > 8 {
		return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "image 数量必须在 1 到 8 之间", "type": "invalid_request_error"}}), nil
	}
	count := request.Count
	if count <= 0 {
		count = 1
	}
	if count != 1 {
		return invalidImageRequest("Grok Web 图片编辑当前仅支持 n=1")
	}
	if request.PartialImages < 0 || request.PartialImages > 3 {
		return invalidImageRequest("partial_images 必须在 0 到 3 之间")
	}
	if request.PartialImages > 0 && !request.Streaming {
		return invalidImageRequest("partial_images 仅可在 stream=true 时使用")
	}
	resolution := strings.ToLower(strings.TrimSpace(request.Resolution))
	if resolution == "" {
		resolution = "1k"
	}
	if resolution != "1k" {
		return invalidImageRequest("Grok Web 图片编辑当前仅支持 resolution=1k")
	}
	format := strings.ToLower(strings.TrimSpace(request.ResponseFormat))
	if format == "" {
		format = "url"
	}
	if format != "url" && format != "b64_json" {
		return invalidImageRequest("response_format 必须是 url 或 b64_json")
	}
	ratio, err := resolveImageEditAspectRatio(request.AspectRatio, request.Size)
	if err != nil {
		return invalidImageRequest(err.Error())
	}
	cfg := a.config()
	token, err := a.cipher.Decrypt(request.Credential.EncryptedAccessToken)
	if err != nil {
		return nil, err
	}
	lease, err := a.egress.AcquireCredential(ctx, domainegress.ScopeWeb, request.Credential)
	if err != nil {
		return nil, err
	}
	leaseOwned := true
	defer func() {
		if leaseOwned {
			lease.Release()
		}
	}()
	images := make([]provider.ImageInput, 0, len(request.ImageURLs))
	for _, rawURL := range request.ImageURLs {
		image, loadErr := a.loadChatImage(ctx, lease, rawURL, cfg.MaxInputImageBytes)
		if loadErr != nil {
			return invalidImageRequest(loadErr.Error())
		}
		images = append(images, image)
	}
	assets := make([]string, 0, len(images))
	for _, image := range images {
		uploaded, uploadErr := a.uploadFileV2Direct(ctx, cfg, lease, token, image, cfg.BaseURL+"/imagine", imagineSelfUploadSource, "image_edit_upload")
		if uploadErr != nil {
			return nil, uploadErr
		}
		if uploaded.MetadataID == "" {
			return nil, fmt.Errorf("上传图片成功但上游未返回 fileMetadataId")
		}
		assets = append(assets, uploaded.MetadataID)
	}
	payload := buildImageEditPayload(request.Prompt, assets, ratio)
	response, err := a.postJSONWithReferer(ctx, cfg, lease, token, cfg.BaseURL+"/rest/app-chat/conversations/new", payload, time.Duration(cfg.ImageTimeoutSeconds)*time.Second, cfg.BaseURL+"/imagine")
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, webMediaDiagnosticBodyLimit+1))
		_ = response.Body.Close()
		truncated := len(body) > webMediaDiagnosticBodyLimit
		if truncated {
			body = body[:webMediaDiagnosticBodyLimit]
		}
		upstreamErr := newWebMediaUpstreamError(response.StatusCode, body, truncated)
		a.logWebMediaUpstreamRejection("image_edit_generate", response, upstreamErr)
		return nil, upstreamErr
	}
	if request.Streaming {
		reader, writer := io.Pipe()
		streamCtx, cancel := context.WithCancel(ctx)
		leaseOwned = false
		go a.streamImageEdit(streamCtx, writer, response.Body, lease, request.Credential, request.PartialImages, request.Size, ratio)
		return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: streamHeaders(), Body: &cancelBody{ReadCloser: reader, cancel: cancel}, QuotaUnits: 1}, nil
	}
	defer response.Body.Close()
	capture := &boundedCapture{limit: 8 << 20}
	parsed, consumeErr := consumeUpstream(io.TeeReader(response.Body, capture), nil)
	if consumeErr != nil {
		return nil, consumeErr
	}
	urls := imageEditResultURLs(&parsed, capture.Bytes())
	if len(urls) == 0 {
		return jsonProviderResponse(http.StatusBadGateway, map[string]any{"error": map[string]any{
			"message": "上游未返回可用的编辑图片",
			"type":    "server_error", "code": "image_edit_incomplete",
		}}), nil
	}
	result, err := a.imageResponse(ctx, request.Credential, urls, nil, 1, format)
	if result != nil {
		result.QuotaUnits = 1
	}
	return result, err
}

func buildImageEditPayload(prompt string, assets []string, aspectRatio string) map[string]any {
	imageToImage := map[string]any{
		"prompt":      prompt,
		"inputAssets": assets,
	}
	if aspectRatio != "" {
		imageToImage["aspectRatio"] = aspectRatio
	}
	return map[string]any{
		"modelName": "imagine-image-edit", "message": prompt,
		"enableImageStreaming": true, "enableSideBySide": true, "sendFinalMetadata": true,
		"mediaGenInput": map[string]any{"imageToImage": imageToImage},
	}
}

func resolveImageEditAspectRatio(aspectRatio, size string) (string, error) {
	if strings.TrimSpace(aspectRatio) == "" && strings.TrimSpace(size) == "" {
		return "", nil
	}
	return resolveImageAspectRatio(aspectRatio, size)
}

type imageEditStreamFrame struct {
	URL       string
	Progress  int
	Moderated bool
}

func parseImageEditStreamFrame(data []byte) (imageEditStreamFrame, bool) {
	var root map[string]any
	if json.Unmarshal(data, &root) != nil {
		return imageEditStreamFrame{}, false
	}
	result, _ := root["result"].(map[string]any)
	response, _ := result["response"].(map[string]any)
	imageResponse, _ := response["streamingImageGenerationResponse"].(map[string]any)
	if imageResponse == nil {
		return imageEditStreamFrame{}, false
	}
	rawURL := firstString(imageResponse, "imageUrl", "url")
	if rawURL == "" {
		return imageEditStreamFrame{}, false
	}
	progress, hasProgress := numberAsInt(imageResponse["progress"])
	if !hasProgress {
		if final, _ := imageResponse["isFinal"].(bool); !final {
			return imageEditStreamFrame{}, false
		}
		progress = 100
	}
	moderated, _ := imageResponse["moderated"].(bool)
	return imageEditStreamFrame{URL: absoluteAssetURL(rawURL), Progress: progress, Moderated: moderated}, true
}

func (a *Adapter) streamImageEdit(
	ctx context.Context,
	writer *io.PipeWriter,
	source io.ReadCloser,
	lease *egress.Lease,
	credential account.Credential,
	partialImages int,
	size string,
	aspectRatio string,
) {
	defer lease.Release()
	defer source.Close()
	createdAt := time.Now().Unix()
	parsed := parsedChat{}
	capture := &boundedCapture{limit: 8 << 20}
	seenPartials := make(map[string]struct{}, partialImages)
	partialIndex := 0
	consumeErr := consumeJSONObjects(io.TeeReader(source, capture), 8<<20, func(data []byte) error {
		if _, _, err := parseUpstreamFrame(data, &parsed); err != nil {
			return err
		}
		frame, ok := parseImageEditStreamFrame(data)
		if !ok || frame.Moderated || frame.Progress >= 100 || partialIndex >= partialImages {
			return nil
		}
		if _, exists := seenPartials[frame.URL]; exists {
			return nil
		}
		raw, err := a.imageBytes(ctx, credential, imagineImageValue{URL: frame.URL})
		if err != nil {
			// partial_images 是尽力而为；预览下载失败不应阻断最终编辑结果。
			return nil
		}
		if err := writeSSE(writer, "image_edit.partial_image", openAIImageEditStreamEvent(
			"image_edit.partial_image", raw, createdAt, imageEditEventSize(size, aspectRatio), partialIndex,
		)); err != nil {
			return err
		}
		seenPartials[frame.URL] = struct{}{}
		partialIndex++
		return nil
	})
	if consumeErr != nil {
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, consumeErr)
		_ = writer.CloseWithError(consumeErr)
		return
	}
	urls := imageEditResultURLs(&parsed, capture.Bytes())
	if len(urls) == 0 {
		err := fmt.Errorf("上游未返回可用的编辑图片")
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, err)
		_ = writer.CloseWithError(err)
		return
	}
	raw, err := a.imageBytes(ctx, credential, imagineImageValue{URL: urls[0]})
	if err != nil {
		_ = writer.CloseWithError(provider.NewMediaPostProcessingError(provider.MediaPostProcessingDownload, err))
		return
	}
	if err := a.saveStreamImage(ctx, raw); err != nil {
		_ = writer.CloseWithError(err)
		return
	}
	if err := writeSSE(writer, "image_edit.completed", openAIImageEditStreamEvent(
		"image_edit.completed", raw, createdAt, imageEditEventSize(size, aspectRatio), 0,
	)); err != nil {
		_ = writer.CloseWithError(err)
		return
	}
	a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, http.StatusOK, nil)
	_ = writer.Close()
}

func openAIImageEditStreamEvent(eventType string, raw []byte, createdAt int64, size string, partialIndex int) map[string]any {
	value := map[string]any{
		"type": eventType, "b64_json": base64.StdEncoding.EncodeToString(raw),
		"created_at": createdAt, "size": size, "quality": "auto",
		"background": "auto", "output_format": imageOutputFormat(raw),
	}
	if eventType == "image_edit.partial_image" {
		value["partial_image_index"] = partialIndex
	} else {
		value["usage"] = map[string]any{
			"total_tokens": 0, "input_tokens": 0, "output_tokens": 0,
			"input_tokens_details": map[string]any{"text_tokens": 0, "image_tokens": 0},
		}
	}
	return value
}

func imageEditEventSize(size, aspectRatio string) string {
	switch value := strings.ToLower(strings.TrimSpace(size)); value {
	case "1024x1024", "1024x1536", "1536x1024", "auto":
		return value
	}
	switch strings.ToLower(strings.TrimSpace(aspectRatio)) {
	case "1:1":
		return "1024x1024"
	case "2:3":
		return "1024x1536"
	case "3:2":
		return "1536x1024"
	default:
		return "auto"
	}
}

func imageEditResultURLs(parsed *parsedChat, captured []byte) []string {
	values := append([]string(nil), parsed.Images...)
	if len(values) == 0 {
		values = extractCapturedImageURLs(captured)
	}
	if len(values) == 0 {
		values = extractMarkdownImages(parsed.Text.String())
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = absoluteAssetURL(value)
		if _, moderated := parsed.moderatedImages[value]; moderated || containsString(result, value) {
			continue
		}
		result = append(result, value)
	}
	return result
}

type boundedCapture struct {
	data  []byte
	limit int
}

func (w *boundedCapture) Write(value []byte) (int, error) {
	remaining := w.limit - len(w.data)
	if remaining > 0 {
		w.data = append(w.data, value[:min(remaining, len(value))]...)
	}
	return len(value), nil
}

func (w *boundedCapture) Bytes() []byte { return w.data }

func extractCapturedImageURLs(data []byte) []string {
	results := make([]string, 0, 2)
	_ = consumeJSONObjects(bytes.NewReader(data), 8<<20, func(frame []byte) error {
		var value any
		if json.Unmarshal(frame, &value) == nil {
			collectCapturedImageURLs(value, &results)
		}
		return nil
	})
	return results
}

type liteCaptureDiagnostics struct {
	Frames         int
	ResponseFields []string
	MessageTags    []string
	ImageChunks    int
	ImageURLs      int
	ImageFields    []string
	MaxProgress    int
	SoftStop       bool
	ErrorCode      string
	ErrorMessage   string
}

func inspectLiteCapture(data []byte) liteCaptureDiagnostics {
	result := liteCaptureDiagnostics{}
	fields := make(map[string]struct{})
	tags := make(map[string]struct{})
	imageFields := make(map[string]struct{})
	_ = consumeJSONObjects(bytes.NewReader(data), 8<<20, func(frame []byte) error {
		result.Frames++
		var root map[string]any
		if json.Unmarshal(frame, &root) != nil {
			return nil
		}
		value, _ := root["result"].(map[string]any)
		response, _ := value["response"].(map[string]any)
		for key := range response {
			fields[key] = struct{}{}
		}
		if tag, _ := response["messageTag"].(string); tag != "" {
			tags[tag] = struct{}{}
		}
		if stopped, _ := response["isSoftStop"].(bool); stopped {
			result.SoftStop = true
		}
		if responseError, ok := response["error"].(map[string]any); ok {
			result.ErrorCode = fmt.Sprint(responseError["code"])
			result.ErrorMessage = firstString(responseError, "message", "error")
			if len(result.ErrorMessage) > 200 {
				result.ErrorMessage = result.ErrorMessage[:200]
			}
		}
		inspectLiteCaptureValue(response, &result, imageFields)
		return nil
	})
	result.ResponseFields = sortedSetValues(fields)
	result.MessageTags = sortedSetValues(tags)
	result.ImageFields = sortedSetValues(imageFields)
	return result
}

func inspectLiteCaptureValue(value any, result *liteCaptureDiagnostics, imageFields map[string]struct{}) {
	switch current := value.(type) {
	case map[string]any:
		for key, nested := range current {
			if key == "jsonData" {
				if encoded, _ := nested.(string); encoded != "" {
					var decoded any
					if json.Unmarshal([]byte(encoded), &decoded) == nil {
						inspectLiteCaptureValue(decoded, result, imageFields)
					}
				}
			}
			if key == "image_chunk" || key == "imageChunk" {
				if chunk, ok := nested.(map[string]any); ok {
					result.ImageChunks++
					for field := range chunk {
						imageFields[field] = struct{}{}
					}
					if firstString(chunk, "imageUrl", "image_url", "url") != "" {
						result.ImageURLs++
					}
					if progress, ok := numberAsInt(chunk["progress"]); ok && progress > result.MaxProgress {
						result.MaxProgress = progress
					}
				}
			}
			inspectLiteCaptureValue(nested, result, imageFields)
		}
	case []any:
		for _, nested := range current {
			inspectLiteCaptureValue(nested, result, imageFields)
		}
	}
}

func sortedSetValues(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func collectCapturedImageURLs(value any, results *[]string) {
	switch current := value.(type) {
	case map[string]any:
		if rawURL := imageURLFromCardData(current); rawURL != "" {
			appendCapturedImageURL(results, rawURL)
		}
		moderated, _ := current["moderated"].(bool)
		progress, hasProgress := numberAsInt(current["progress"])
		if !moderated && hasProgress && progress >= 100 {
			appendCapturedImageURL(results, firstString(current, "imageUrl", "image_url", "url"))
		}
		for _, nested := range current {
			collectCapturedImageURLs(nested, results)
		}
	case []any:
		for _, nested := range current {
			collectCapturedImageURLs(nested, results)
		}
	case string:
		trimmed := strings.TrimSpace(current)
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			var nested any
			if json.Unmarshal([]byte(trimmed), &nested) == nil {
				collectCapturedImageURLs(nested, results)
				return
			}
		}
		appendCapturedImageURL(results, trimmed)
	}
}

func appendCapturedImageURL(results *[]string, value string) {
	value = strings.TrimSpace(value)
	if !strings.Contains(value, "/generated/") || strings.Contains(value, "-part-") || strings.ContainsAny(value, "{}[]\"") {
		return
	}
	if !strings.HasPrefix(value, "https://") && !strings.HasPrefix(value, "users/") && !strings.HasPrefix(value, "/users/") {
		return
	}
	value = absoluteAssetURL(value)
	if !containsString(*results, value) {
		*results = append(*results, value)
	}
}

func (a *Adapter) uploadFileV2Direct(ctx context.Context, cfg Config, lease *egress.Lease, token string, file provider.ImageInput, referer, fileSource, stage string) (uploadedFile, error) {
	body, contentType, err := buildDirectFileUploadBody(file, fileSource)
	if err != nil {
		return uploadedFile{}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, cfg.BaseURL+"/http/upload-file-v2/direct", bytes.NewReader(body))
	if err != nil {
		return uploadedFile{}, err
	}
	request.Header = buildHeaders(token, lease, contentType)
	request.Header.Del("x-xai-request-id")
	applyAppHeaders(request.Header, cfg.BaseURL, referer)
	response, err := lease.DoDeferredForbidden(request)
	if err != nil {
		return uploadedFile{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, webMediaDiagnosticBodyLimit+1))
		if readErr != nil {
			return uploadedFile{}, fmt.Errorf("读取 V2 上传文件错误响应: %w", readErr)
		}
		truncated := len(responseBody) > webMediaDiagnosticBodyLimit
		if truncated {
			responseBody = responseBody[:webMediaDiagnosticBodyLimit]
		}
		upstreamErr := newWebMediaUpstreamError(response.StatusCode, responseBody, truncated)
		if isClearanceRefreshableMediaError(upstreamErr) {
			lease.InvalidateClearance()
		}
		a.logWebMediaUpstreamRejection(stage, response, upstreamErr)
		return uploadedFile{}, upstreamErr
	}
	uploaded, err := decodeDirectFileUploadResponse(io.LimitReader(response.Body, directFileUploadResponseLimit))
	if err != nil {
		return uploadedFile{}, err
	}
	return uploaded, nil
}

func buildDirectFileUploadBody(file provider.ImageInput, fileSource string) ([]byte, string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, browserMultipartFilename(file.Filename)))
	header.Set("Content-Type", file.MIMEType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(file.Data); err != nil {
		return nil, "", err
	}
	if fileSource != "" {
		if err := writer.WriteField("file_source", fileSource); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return body.Bytes(), writer.FormDataContentType(), nil
}

func browserMultipartFilename(value string) string {
	value = strings.Map(func(character rune) rune {
		switch {
		case character == '\r' || character == '\n':
			return -1
		case character < 0x20 || character == 0x7f:
			return '_'
		default:
			return character
		}
	}, value)
	if strings.TrimSpace(value) == "" {
		value = "upload.bin"
	}
	return strings.NewReplacer("\\", "\\\\", `"`, `\"`).Replace(value)
}

func decodeDirectFileUploadResponse(source io.Reader) (uploadedFile, error) {
	var value struct {
		UploadID      string          `json:"uploadId"`
		TerminalError json.RawMessage `json:"terminalError"`
		FileMetadata  struct {
			ID      string `json:"fileMetadataId"`
			FileID  string `json:"fileId"`
			FileURI string `json:"fileUri"`
		} `json:"fileMetadata"`
	}
	if err := json.NewDecoder(source).Decode(&value); err != nil {
		return uploadedFile{}, fmt.Errorf("V2 上传文件响应无效: %w", err)
	}
	if directFileUploadTerminalError(value.TerminalError) {
		return uploadedFile{}, errors.New("V2 上传文件被上游拒绝")
	}
	metadataID := strings.TrimSpace(value.FileMetadata.ID)
	fileID := metadataID
	if fileID == "" {
		fileID = strings.TrimSpace(value.FileMetadata.FileID)
	}
	if fileID == "" {
		// Some successful uploads complete asynchronously and only expose the
		// upload task ID. Gateway accepts it as the file reference; prefer the
		// browser's fileMetadataId whenever it is already available.
		fileID = strings.TrimSpace(value.UploadID)
	}
	fileURI := ""
	if value.FileMetadata.FileURI != "" {
		fileURI = absoluteAssetURL(value.FileMetadata.FileURI)
	}
	if fileID == "" && fileURI == "" {
		return uploadedFile{}, fmt.Errorf("V2 上传文件成功但上游未返回完整文件标识")
	}
	return uploadedFile{ID: fileID, MetadataID: metadataID, URI: fileURI}, nil
}

func directFileUploadTerminalError(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("false")) || bytes.Equal(trimmed, []byte("0")) {
		return false
	}
	var value any
	if json.Unmarshal(trimmed, &value) != nil {
		return true
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed) != ""
	case map[string]any:
		return len(typed) != 0
	case []any:
		return len(typed) != 0
	case bool:
		return typed
	case float64:
		return typed != 0
	default:
		return value != nil
	}
}

func (a *Adapter) postJSON(ctx context.Context, cfg Config, lease *egress.Lease, token, endpoint string, payload any, timeout time.Duration) (*http.Response, error) {
	return a.postJSONWithReferer(ctx, cfg, lease, token, endpoint, payload, timeout, cfg.BaseURL+"/imagine")
}

func (a *Adapter) postJSONWithReferer(ctx context.Context, cfg Config, lease *egress.Lease, token, endpoint string, payload any, timeout time.Duration, referer string) (*http.Response, error) {
	data, _ := json.Marshal(payload)
	for attempt := 0; attempt < 2; attempt++ {
		requestCtx, cancel := context.WithTimeout(ctx, timeout)
		request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(data))
		if err != nil {
			cancel()
			return nil, err
		}
		request.Header = buildHeaders(token, lease, "application/json")
		applyAppHeaders(request.Header, cfg.BaseURL, referer)
		a.applySignedStatsig(requestCtx, request, token, lease)
		response, err := lease.DoDeferredForbidden(request)
		if err != nil {
			cancel()
			return nil, err
		}
		if response.StatusCode == http.StatusForbidden {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, webMediaDiagnosticBodyLimit+1))
			_ = response.Body.Close()
			cancel()
			if readErr != nil {
				return nil, fmt.Errorf("读取 Grok Web 403 响应: %w", readErr)
			}
			truncated := len(body) > webMediaDiagnosticBodyLimit
			if truncated {
				body = body[:webMediaDiagnosticBodyLimit]
			}
			upstreamErr := newWebMediaUpstreamError(response.StatusCode, body, truncated)
			response.Body = io.NopCloser(bytes.NewReader(body))
			response.ContentLength = int64(len(body))
			if isClearanceRefreshableMediaError(upstreamErr) {
				lease.InvalidateClearance()
				_ = a.invalidateSignedStatsig(http.MethodPost, endpoint)
				return response, nil
			}
			// Code 7 is the application-layer equivalent of reloading the Grok
			// page: refresh only the path-bound Statsig signature and replay the
			// explicitly rejected POST once. It is not a Cloudflare challenge, so
			// the current Clearance lease remains valid.
			if isStatsigRefreshableMediaError(upstreamErr, body) {
				if attempt == 0 && a.invalidateSignedStatsig(http.MethodPost, endpoint) {
					continue
				}
				return response, nil
			}
			// Remaining structured JSON responses are application policy decisions.
			// They must not invalidate Clearance, affect egress health, or be replayed.
			if upstreamErr.bodyKind == "json" || attempt > 0 || !a.invalidateSignedStatsig(http.MethodPost, endpoint) {
				return response, nil
			}
			continue
		}
		response.Body = &cancelBody{ReadCloser: response.Body, cancel: cancel}
		return response, nil
	}
	return nil, fmt.Errorf("Grok Web Statsig 刷新失败")
}

func (a *Adapter) imageResponse(ctx context.Context, credential account.Credential, urls, blobs []string, count int, format string) (*provider.Response, error) {
	data := make([]any, 0, min(count, len(urls)))
	for index := 0; index < count && index < len(urls); index++ {
		blob := ""
		if index < len(blobs) {
			blob = blobs[index]
		}
		item, err := a.imageDataItem(ctx, credential, imagineImageValue{URL: urls[index], Blob: blob}, format)
		if err != nil {
			return nil, err
		}
		data = append(data, item)
	}
	return jsonProviderResponse(http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": data}), nil
}

func (a *Adapter) imageDataItem(ctx context.Context, credential account.Credential, image imagineImageValue, format string) (map[string]any, error) {
	if a.assets == nil {
		return nil, provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, fmt.Errorf("图片媒体存储未配置"))
	}
	raw, err := a.imageBytes(ctx, credential, image)
	if err != nil {
		return nil, provider.NewMediaPostProcessingError(provider.MediaPostProcessingDownload, err)
	}
	asset, err := a.saveImageWithRetry(ctx, raw)
	if err != nil {
		return nil, provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, err)
	}
	if format != "b64_json" {
		return map[string]any{"url": a.assets.PublicImageURL(asset.ID), "mime_type": asset.MIMEType, "revised_prompt": ""}, nil
	}
	return map[string]any{"b64_json": base64.StdEncoding.EncodeToString(raw), "mime_type": asset.MIMEType, "revised_prompt": ""}, nil
}

// saveImageWithRetry 只重试当前生成结果的本地持久化，不重新请求上游生成。
func (a *Adapter) saveImageWithRetry(ctx context.Context, raw []byte) (mediadomain.Asset, error) {
	var lastErr error
	for attempt := 0; attempt < mediaOutputAttempts; attempt++ {
		asset, err := a.assets.SaveImage(ctx, raw)
		if err == nil {
			return asset, nil
		}
		lastErr = err
		if ctx.Err() != nil || attempt+1 >= mediaOutputAttempts {
			break
		}
		if err := waitMediaOutputRetry(ctx, attempt); err != nil {
			return mediadomain.Asset{}, err
		}
	}
	return mediadomain.Asset{}, lastErr
}

func (a *Adapter) imageBytes(ctx context.Context, credential account.Credential, image imagineImageValue) ([]byte, error) {
	if strings.TrimSpace(image.Blob) != "" {
		raw, err := decodeImageBlob(image.Blob)
		if err == nil {
			return raw, nil
		}
		if strings.TrimSpace(image.URL) == "" {
			return nil, err
		}
	}
	return a.downloadImage(ctx, credential, image.URL)
}

func (a *Adapter) streamImagineImages(ctx context.Context, writer *io.PipeWriter, connection *websocket.Conn, lease *egress.Lease, credential account.Credential, count, partialImages int, modelConfig imagineModelConfig) {
	defer lease.Release()
	defer connection.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = connection.Close()
		case <-done:
		}
	}()
	collector := newImagineCollector()
	emitted := 0
	partialIndex := 0
	for emitted < count {
		messageType, data, readErr := connection.ReadMessage()
		if readErr != nil {
			if ctx.Err() != nil {
				_ = writer.CloseWithError(ctx.Err())
				return
			}
			a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, readErr)
			_ = writer.CloseWithError(readErr)
			return
		}
		if messageType != websocket.TextMessage {
			continue
		}
		var message map[string]any
		if json.Unmarshal(data, &message) != nil {
			continue
		}
		if message["type"] == "error" {
			upstreamErr := fmt.Errorf("Imagine WebSocket 返回错误")
			a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, upstreamErr)
			_ = writer.CloseWithError(upstreamErr)
			return
		}
		collector.Accept(message)
		if partialImages > 0 {
			for _, image := range collector.ReadyPreviews() {
				if partialIndex >= partialImages {
					continue
				}
				raw, err := a.imageBytes(ctx, credential, image)
				if err != nil {
					if ctx.Err() != nil {
						_ = writer.CloseWithError(ctx.Err())
						return
					}
					continue
				}
				if err := writeSSE(writer, "image_generation.partial_image", openAIImageStreamEvent("image_generation.partial_image", image, raw, partialIndex)); err != nil {
					_ = writer.CloseWithError(err)
					return
				}
				partialIndex++
			}
		}
		for _, image := range collector.ReadyImages() {
			if emitted >= count {
				break
			}
			raw, err := a.imageBytes(ctx, credential, image)
			if err != nil {
				_ = writer.CloseWithError(err)
				return
			}
			if err := a.saveStreamImage(ctx, raw); err != nil {
				_ = writer.CloseWithError(err)
				return
			}
			if err := writeSSE(writer, "image_generation.completed", openAIImageStreamEvent("image_generation.completed", image, raw, 0)); err != nil {
				_ = writer.CloseWithError(err)
				return
			}
			emitted++
		}
		if collector.Done(modelConfig.ExpectedCount) && emitted < count {
			incompleteErr := fmt.Errorf("上游仅返回 %d/%d 张可用图片", emitted, count)
			_ = writer.CloseWithError(incompleteErr)
			return
		}
	}
	a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, http.StatusOK, nil)
	_ = writer.Close()
}

func openAIImageStreamEvent(eventType string, image imagineImageValue, raw []byte, partialIndex int) map[string]any {
	width, height := image.Width, image.Height
	size := "auto"
	if width > 0 && height > 0 {
		size = fmt.Sprintf("%dx%d", width, height)
	}
	value := map[string]any{
		"type": eventType, "b64_json": base64.StdEncoding.EncodeToString(raw),
		"created_at": time.Now().Unix(), "size": size, "quality": "auto",
		"background": "auto", "output_format": imageOutputFormat(raw),
	}
	if eventType == "image_generation.partial_image" {
		value["partial_image_index"] = partialIndex
	}
	return value
}

func imageOutputFormat(raw []byte) string {
	mimeType := http.DetectContentType(raw)
	switch mimeType {
	case "image/png":
		return "png"
	case "image/webp":
		return "webp"
	default:
		return "jpeg"
	}
}

func (a *Adapter) saveStreamImage(ctx context.Context, raw []byte) error {
	if a.assets == nil {
		return provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, fmt.Errorf("图片媒体存储未配置"))
	}
	if _, err := a.saveImageWithRetry(ctx, raw); err != nil {
		return provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, err)
	}
	return nil
}

func (a *Adapter) downloadImage(ctx context.Context, credential account.Credential, rawURL string) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || !trustedImageAssetHost(parsed.Hostname()) || parsed.User != nil {
		return nil, fmt.Errorf("图片内容 URL 不受信任")
	}
	token, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return nil, err
	}
	downloadCtx, cancel := context.WithTimeout(ctx, imageDownloadTimeout)
	defer cancel()
	var lastErr error
	for attempt := 0; attempt < mediaOutputAttempts; attempt++ {
		raw, retryable, attemptErr := a.downloadImageAttempt(downloadCtx, credential, token, parsed.String())
		if attemptErr == nil {
			return raw, nil
		}
		lastErr = attemptErr
		if !retryable || downloadCtx.Err() != nil || attempt+1 >= mediaOutputAttempts {
			break
		}
		if err := waitMediaOutputRetry(downloadCtx, attempt); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// downloadImageAttempt 每次沿用同一账号，只允许出口管理器重新选择资源节点。
func (a *Adapter) downloadImageAttempt(ctx context.Context, credential account.Credential, token, rawURL string) ([]byte, bool, error) {
	lease, err := a.egress.AcquireCredential(ctx, domainegress.ScopeWebAsset, credential)
	if err != nil {
		return nil, true, err
	}
	defer lease.Release()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, false, err
	}
	request.Header = buildHeaders(token, lease, "")
	request.Header.Del("Content-Type")
	response, err := lease.Do(request)
	if err != nil {
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, err)
		return nil, ctx.Err() == nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, response.StatusCode, nil)
		retryable := response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooEarly || response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
		return nil, retryable, fmt.Errorf("下载图片返回 %d", response.StatusCode)
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]))
	if contentType != "" && !strings.HasPrefix(contentType, "image/") {
		return nil, false, fmt.Errorf("上游图片 Content-Type 无效")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (32<<20)+1))
	if err != nil {
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, err)
		return nil, ctx.Err() == nil, fmt.Errorf("读取图片内容: %w", err)
	}
	if len(raw) > 32<<20 {
		return nil, false, fmt.Errorf("图片下载超过 32 MiB")
	}
	a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, response.StatusCode, nil)
	return raw, false, nil
}

func waitMediaOutputRetry(ctx context.Context, attempt int) error {
	delays := [...]time.Duration{200 * time.Millisecond, 750 * time.Millisecond}
	delay := delays[min(attempt, len(delays)-1)]
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func decodeImageBlob(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), "data:") {
		comma := strings.IndexByte(value, ',')
		if comma < 0 || !strings.Contains(strings.ToLower(value[:comma]), ";base64") {
			return nil, fmt.Errorf("图片 blob data URI 无效")
		}
		value = value[comma+1:]
	}
	if value == "" || base64.StdEncoding.DecodedLen(len(value)) > 32<<20 {
		return nil, fmt.Errorf("图片 blob 为空或超过 32 MiB")
	}
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(value)
	}
	if err != nil || len(raw) == 0 || len(raw) > 32<<20 {
		return nil, fmt.Errorf("图片 blob Base64 无效")
	}
	return raw, nil
}

func trustedImageAssetHost(host string) bool {
	return strings.EqualFold(host, "assets.grok.com") || strings.EqualFold(host, "imagine-public.x.ai") || strings.EqualFold(host, "imgen.x.ai")
}

func imagineURL(baseURL string) (string, error) {
	value, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	switch value.Scheme {
	case "https":
		value.Scheme = "wss"
	case "http":
		value.Scheme = "ws"
	default:
		return "", fmt.Errorf("Grok Web Base URL 协议无效")
	}
	value.Path = "/ws/imagine/listen"
	value.RawQuery = ""
	return value.String(), nil
}

func imagineResetMessage() map[string]any {
	return map[string]any{"type": "conversation.item.create", "timestamp": time.Now().UnixMilli(), "item": map[string]any{"type": "message", "content": []any{map[string]any{"type": "reset"}}}}
}

func imagineRequestMessage(id, prompt, ratio string, nsfw, pro bool, generations int) map[string]any {
	return map[string]any{"type": "conversation.item.create", "timestamp": time.Now().UnixMilli(), "item": map[string]any{"type": "message", "content": []any{map[string]any{"requestId": id, "text": prompt, "type": "input_text", "properties": map[string]any{"section_count": 0, "is_kids_mode": false, "enable_nsfw": nsfw, "skip_upsampler": false, "enable_side_by_side": true, "is_initial": false, "aspect_ratio": ratio, "enable_pro": pro, "num_generations": generations}}}}}
}

func resolveImageAspectRatio(aspectRatio, size string) (string, error) {
	values := map[string]string{
		"auto": "auto", "1:1": "1:1", "16:9": "16:9", "9:16": "9:16", "4:3": "4:3", "3:4": "3:4",
		"3:2": "3:2", "2:3": "2:3", "2:1": "2:1", "1:2": "1:2", "19.5:9": "19.5:9", "9:19.5": "9:19.5", "20:9": "20:9", "9:20": "9:20",
		"1280x720": "16:9", "720x1280": "9:16", "1792x1024": "3:2", "1536x1024": "3:2", "1024x1792": "2:3", "1024x1536": "2:3", "1024x1024": "1:1",
	}
	value := strings.ToLower(strings.TrimSpace(aspectRatio))
	if value == "" {
		value = strings.ToLower(strings.TrimSpace(size))
	}
	if value == "" {
		return "auto", nil
	}
	if resolved := values[value]; resolved != "" {
		return resolved, nil
	}
	return "", fmt.Errorf("aspect_ratio 不受支持")
}

func resolveAspectRatio(size string) string {
	if strings.TrimSpace(size) == "" {
		return "1:1"
	}
	value, err := resolveImageAspectRatio("", size)
	if err != nil {
		return "1:1"
	}
	return value
}

func imageIDFromURL(value string) string {
	parts := strings.Split(strings.Trim(value, "/"), "/")
	if len(parts) == 0 {
		return value
	}
	name := parts[len(parts)-1]
	if index := strings.IndexByte(name, '.'); index > 0 {
		return name[:index]
	}
	return name
}

func absoluteAssetURL(value string) string {
	if strings.HasPrefix(value, "https://") {
		return value
	}
	return "https://assets.grok.com/" + strings.TrimPrefix(value, "/")
}

func extractMarkdownImages(value string) []string {
	results := make([]string, 0, 2)
	for {
		start := strings.Index(value, "![image](")
		if start < 0 {
			break
		}
		value = value[start+len("![image]("):]
		end := strings.IndexByte(value, ')')
		if end < 0 {
			break
		}
		results = append(results, value[:end])
		value = value[end+1:]
	}
	return results
}
