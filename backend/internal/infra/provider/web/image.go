package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/websocket"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

const maxGeneratedImages = 10

var errLiteImageReady = errors.New("Lite 图片已完成")

type imagineModelConfig struct {
	Pro             bool
	NativeBatchSize int
	MaxReturnCount  int
}

type imagineImageValue struct {
	ID       string
	URL      string
	Blob     string
	Position int
	position bool
}

type imagineSlot struct {
	image     imagineImageValue
	completed bool
	moderated bool
	emitted   bool
}

type imagineCollector struct {
	slots         map[string]*imagineSlot
	terminalCount int
}

func resolveImagineModel(model, resolution string, count int) (imagineModelConfig, bool) {
	if model != "imagine" {
		return imagineModelConfig{}, false
	}
	batchSize := 4
	if count > 8 {
		batchSize = 12
	} else if count > 4 {
		batchSize = 8
	}
	return imagineModelConfig{Pro: resolution == "2k", NativeBatchSize: batchSize, MaxReturnCount: 10}, true
}

func invalidImageRequest(message string) (*provider.Response, error) {
	return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{
		"message": message, "type": "invalid_request_error",
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
		slot.image.URL = absoluteAssetURL(rawURL)
		slot.image.Blob, _ = message["blob"].(string)
		if position, ok := firstInt(message, "side_by_side_index", "order", "grid_index"); ok {
			slot.image.Position = position
			slot.image.position = true
		}
		return
	}
	status, _ := message["current_status"].(string)
	if position, ok := numberAsInt(message["order"]); ok && !slot.image.position {
		slot.image.Position = position
		slot.image.position = true
	}
	if status != "completed" {
		return
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
		if slot.completed && !slot.moderated && slot.image.URL == "" && slot.image.Blob == "" {
			return false
		}
	}
	return true
}

func (c *imagineCollector) Images() []imagineImageValue {
	values := make([]imagineImageValue, 0, len(c.slots))
	for _, slot := range c.slots {
		if slot.completed && !slot.moderated && (slot.image.URL != "" || slot.image.Blob != "") {
			values = append(values, slot.image)
		}
	}
	sort.SliceStable(values, func(i, j int) bool {
		if values[i].position != values[j].position {
			return values[i].position
		}
		if values[i].Position != values[j].Position {
			return values[i].Position < values[j].Position
		}
		return values[i].ID < values[j].ID
	})
	return values
}

func (c *imagineCollector) ReadyImages() []imagineImageValue {
	values := make([]imagineImageValue, 0, len(c.slots))
	for _, slot := range c.slots {
		if slot.completed && !slot.moderated && !slot.emitted && (slot.image.URL != "" || slot.image.Blob != "") {
			slot.emitted = true
			values = append(values, slot.image)
		}
	}
	sort.SliceStable(values, func(i, j int) bool {
		if values[i].position != values[j].position {
			return values[i].position
		}
		if values[i].Position != values[j].Position {
			return values[i].Position < values[j].Position
		}
		return values[i].ID < values[j].ID
	})
	return values
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
			return invalidImageRequest("grok-imagine-image 不支持 stream")
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
	resolution := strings.ToLower(strings.TrimSpace(request.Resolution))
	if resolution == "" {
		resolution = "1k"
	}
	if resolution != "1k" && resolution != "2k" {
		return invalidImageRequest("resolution 必须是 1k 或 2k")
	}
	modelConfig, ok := resolveImagineModel(protocolModel, resolution, count)
	if !ok {
		return invalidImageRequest("模型不支持图片生成")
	}
	if count > modelConfig.MaxReturnCount {
		return invalidImageRequest(fmt.Sprintf("resolution=%s 时 n 不能超过 %d", resolution, modelConfig.MaxReturnCount))
	}
	return a.generateWSImage(ctx, request, count, format, ratio, resolution, modelConfig)
}

func (a *Adapter) generateLiteImage(ctx context.Context, request provider.ImageGenerationRequest, count int, format string) (*provider.Response, error) {
	spec, _ := Resolve(request.Model)
	urls := make([]string, 0, count)
	for len(urls) < count {
		value, err := a.generateLiteImageURL(ctx, request.Credential, spec, request.Prompt)
		if err != nil {
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
		upstream, lease, _, statsigTarget, err := a.openChat(ctx, credential, "", spec, normalizedChatInput{Prompt: "Drawing: " + prompt})
		if err != nil {
			return "", err
		}
		if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(upstream.Body, 1<<20))
			_ = upstream.Body.Close()
			if upstream.StatusCode == http.StatusForbidden {
				if attempt == 0 && a.invalidateSignedStatsig(http.MethodPost, statsigTarget) {
					lease.Release()
					continue
				}
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
				if attempt == 0 && a.invalidateSignedStatsig(http.MethodPost, statsigTarget) {
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

func (a *Adapter) forwardLiteChatCompletion(ctx context.Context, request provider.ResponseResourceRequest, input openAIRequest, normalized normalizedChatInput, spec ModelSpec) (*provider.Response, error) {
	if len(normalized.Images) > 0 {
		return invalidImageRequest("grok-imagine-image 只支持文本生图；参考图片请使用 /v1/images/edits")
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
	responseID := newWebID("resp")
	streaming := input.Stream || request.Streaming
	if streaming {
		reader, writer := io.Pipe()
		streamCtx, cancel := context.WithCancel(ctx)
		go a.streamLiteChatImages(streamCtx, writer, request.Credential, spec, responseID, input.Model, normalized.Prompt, count, format)
		return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: streamHeaders(), Body: &cancelBody{ReadCloser: reader, cancel: cancel}, QuotaUnits: count}, nil
	}
	parsed := parsedChat{ResponseID: responseID, InputTokens: estimateTokens(normalized.Prompt)}
	for range count {
		rawURL, err := a.generateLiteImageURL(ctx, request.Credential, spec, normalized.Prompt)
		if err != nil {
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
			parsed.Text.WriteString("\n\n")
		}
		parsed.Text.WriteString(liteImageMarkdown(item))
	}
	payload := buildOpenAIResult("chat", responseID, input.Model, parsed, false)
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: jsonHeaders(), Body: io.NopCloser(bytes.NewReader(data)), QuotaUnits: count}, nil
}

func (a *Adapter) streamLiteChatImages(ctx context.Context, writer *io.PipeWriter, credential account.Credential, spec ModelSpec, responseID, model, prompt string, count int, format string) {
	parsed := parsedChat{ResponseID: responseID, InputTokens: estimateTokens(prompt)}
	writeStreamStart(writer, "chat", responseID, model, parsed.InputTokens)
	for range count {
		rawURL, err := a.generateLiteImageURL(ctx, credential, spec, prompt)
		if err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		item, err := a.imageDataItem(ctx, credential, imagineImageValue{URL: rawURL}, format)
		if err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		delta := liteImageMarkdown(item)
		if parsed.Text.Len() > 0 {
			delta = "\n\n" + delta
		}
		parsed.Text.WriteString(delta)
		if err := writeStreamDelta(writer, "chat", responseID, model, "text", delta); err != nil {
			_ = writer.CloseWithError(err)
			return
		}
	}
	payload := buildOpenAIResult("chat", responseID, model, parsed, false)
	writeStreamDone(writer, "chat", responseID, model, parsed, payload)
	_ = writer.Close()
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

func (a *Adapter) generateWSImage(ctx context.Context, request provider.ImageGenerationRequest, count int, format, ratio, resolution string, modelConfig imagineModelConfig) (*provider.Response, error) {
	cfg := a.config()
	token, err := a.cipher.Decrypt(request.Credential.EncryptedAccessToken)
	if err != nil {
		return nil, err
	}
	lease, err := a.egress.Acquire(ctx, domainegress.ScopeWeb, fmt.Sprintf("%d", request.Credential.ID))
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
	connection, response, err := lease.DialWebSocket(ctx, wsURL, headers, 30*time.Second)
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, status, err)
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
	if err := connection.WriteJSON(imagineResetMessage()); err != nil {
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, err)
		return nil, err
	}
	if err := connection.WriteJSON(imagineRequestMessage(newWebID("img"), request.Prompt, ratio, cfg.AllowNSFW, modelConfig.Pro, modelConfig.NativeBatchSize)); err != nil {
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, err)
		return nil, err
	}
	if request.Streaming {
		reader, writer := io.Pipe()
		streamCtx, cancel := context.WithCancel(ctx)
		leaseOwned = false
		connectionOwned = false
		streamID := newWebID("imggen")
		go a.streamImagineImages(streamCtx, writer, connection, lease, request.Credential, streamID, count, format, ratio, resolution, modelConfig)
		return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: streamHeaders(), Body: &cancelBody{ReadCloser: reader, cancel: cancel}, QuotaUnits: count}, nil
	}

	collector := newImagineCollector()
	for !collector.Done(modelConfig.NativeBatchSize) {
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

func (a *Adapter) EditImage(ctx context.Context, request provider.ImageEditRequest) (*provider.Response, error) {
	if len(request.ImageURLs) == 0 || len(request.ImageURLs) > 8 {
		return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "image 数量必须在 1 到 8 之间", "type": "invalid_request_error"}}), nil
	}
	count := request.Count
	if count <= 0 {
		count = 1
	}
	format := strings.ToLower(strings.TrimSpace(request.ResponseFormat))
	if format == "" {
		format = "url"
	}
	if format != "url" && format != "b64_json" {
		return invalidImageRequest("response_format 必须是 url 或 b64_json")
	}
	cfg := a.config()
	token, err := a.cipher.Decrypt(request.Credential.EncryptedAccessToken)
	if err != nil {
		return nil, err
	}
	lease, err := a.egress.Acquire(ctx, domainegress.ScopeWeb, fmt.Sprintf("%d", request.Credential.ID))
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	images := make([]provider.ImageInput, 0, len(request.ImageURLs))
	for _, rawURL := range request.ImageURLs {
		image, loadErr := a.loadChatImage(ctx, lease, rawURL, cfg.MaxInputImageBytes)
		if loadErr != nil {
			return invalidImageRequest(loadErr.Error())
		}
		images = append(images, image)
	}
	refs := make([]string, 0, len(images))
	parentID := ""
	for _, image := range images {
		uploaded, uploadErr := a.uploadImage(ctx, cfg, lease, token, image, cfg.BaseURL+"/imagine")
		if uploadErr != nil {
			return nil, uploadErr
		}
		if uploaded.URI == "" {
			return nil, fmt.Errorf("上传图片成功但上游未返回 fileUri")
		}
		refs = append(refs, uploaded.URI)
		postID, postErr := a.createMediaPost(ctx, cfg, lease, token, "MEDIA_POST_TYPE_IMAGE", uploaded.URI, "")
		if postErr != nil {
			return nil, postErr
		}
		if parentID == "" {
			parentID = postID
		}
	}
	payload := map[string]any{
		"temporary": true, "modelName": "imagine-image-edit", "message": request.Prompt,
		"enableImageGeneration": true, "returnImageBytes": false, "returnRawGrokInXaiRequest": false,
		"enableImageStreaming": true, "imageGenerationCount": max(2, count), "forceConcise": false,
		"enableSideBySide": true, "sendFinalMetadata": true, "isReasoning": false,
		"disableTextFollowUps": true, "disableMemory": false, "forceSideBySide": false,
		"responseMetadata": map[string]any{"modelConfigOverride": map[string]any{"modelMap": map[string]any{"imageEditModel": "imagine", "imageEditModelConfig": map[string]any{"imageReferences": refs, "parentPostId": parentID}}}},
	}
	response, err := a.postJSONWithReferer(ctx, cfg, lease, token, cfg.BaseURL+"/rest/app-chat/conversations/new", payload, time.Duration(cfg.ImageTimeoutSeconds)*time.Second, cfg.BaseURL+"/imagine/post/"+parentID)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		return &provider.Response{StatusCode: response.StatusCode, Status: response.Status, Header: jsonHeaders(), Body: io.NopCloser(bytes.NewReader(body))}, nil
	}
	capture := &boundedCapture{limit: 8 << 20}
	parsed, err := consumeUpstream(io.TeeReader(response.Body, capture), nil)
	if err != nil {
		return nil, err
	}
	urls := append([]string(nil), parsed.Images...)
	if len(urls) == 0 {
		urls = extractCapturedImageURLs(capture.Bytes())
	}
	if len(urls) == 0 {
		urls = extractMarkdownImages(parsed.Text.String())
	}
	if len(urls) == 0 {
		return nil, fmt.Errorf("图片编辑完成但没有返回图片")
	}
	result, err := a.imageResponse(ctx, request.Credential, urls, nil, count, format)
	if result != nil {
		result.QuotaUnits = count
	}
	return result, err
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

func (a *Adapter) uploadImage(ctx context.Context, cfg Config, lease *egress.Lease, token string, image provider.ImageInput, referer string) (uploadedFile, error) {
	payload := map[string]any{"fileName": image.Filename, "fileMimeType": image.MIMEType, "content": base64.StdEncoding.EncodeToString(image.Data)}
	response, err := a.postJSONWithReferer(ctx, cfg, lease, token, cfg.BaseURL+"/rest/app-chat/upload-file", payload, time.Minute, referer)
	if err != nil {
		return uploadedFile{}, err
	}
	defer response.Body.Close()
	var value struct {
		FileMetadataID string `json:"fileMetadataId"`
		FileID         string `json:"fileId"`
		FileURI        string `json:"fileUri"`
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&value) != nil {
		return uploadedFile{}, fmt.Errorf("上传图片失败或上游响应无效")
	}
	if value.FileMetadataID == "" {
		value.FileMetadataID = value.FileID
	}
	fileURI := ""
	if value.FileURI != "" {
		fileURI = absoluteAssetURL(value.FileURI)
	}
	if value.FileMetadataID == "" && fileURI == "" {
		return uploadedFile{}, fmt.Errorf("上传图片成功但上游未返回文件标识")
	}
	return uploadedFile{ID: value.FileMetadataID, URI: fileURI}, nil
}

func (a *Adapter) createMediaPost(ctx context.Context, cfg Config, lease *egress.Lease, token, mediaType, mediaURL, prompt string) (string, error) {
	payload := map[string]any{"mediaType": mediaType}
	if mediaURL != "" {
		payload["mediaUrl"] = mediaURL
	}
	if prompt != "" {
		payload["prompt"] = prompt
	}
	response, err := a.postJSON(ctx, cfg, lease, token, cfg.BaseURL+"/rest/media/post/create", payload, time.Minute)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var value struct {
		Post struct {
			ID string `json:"id"`
		} `json:"post"`
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&value) != nil || value.Post.ID == "" {
		return "", fmt.Errorf("创建媒体 Post 失败")
	}
	return value.Post.ID, nil
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
		response, err := lease.Do(request)
		if err != nil {
			cancel()
			return nil, err
		}
		if response.StatusCode == http.StatusForbidden {
			if attempt == 0 && a.invalidateSignedStatsig(http.MethodPost, endpoint) {
				_ = response.Body.Close()
				cancel()
				continue
			}
			a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, http.StatusForbidden, nil)
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
		return nil, fmt.Errorf("图片媒体存储未配置")
	}
	raw, err := a.imageBytes(ctx, credential, image)
	if err != nil {
		return nil, err
	}
	asset, err := a.assets.SaveImage(ctx, raw)
	if err != nil {
		return nil, err
	}
	if format != "b64_json" {
		return map[string]any{"url": a.assets.PublicImageURL(asset.ID), "mime_type": asset.MIMEType, "revised_prompt": ""}, nil
	}
	return map[string]any{"b64_json": base64.StdEncoding.EncodeToString(raw), "mime_type": asset.MIMEType, "revised_prompt": ""}, nil
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

func (a *Adapter) streamImagineImages(ctx context.Context, writer *io.PipeWriter, connection *websocket.Conn, lease *egress.Lease, credential account.Credential, streamID string, count int, format, ratio, resolution string, modelConfig imagineModelConfig) {
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
	createdAt := time.Now().Unix()
	if err := writeSSE(writer, "image_generation.started", map[string]any{
		"type": "image_generation.started", "id": streamID, "object": "image_generation",
		"created": createdAt, "model": "grok-imagine-image-quality", "status": "in_progress",
		"n": count, "aspect_ratio": ratio, "resolution": strings.ToLower(strings.TrimSpace(resolution)),
	}); err != nil {
		_ = writer.CloseWithError(err)
		return
	}
	collector := newImagineCollector()
	emitted := 0
	for emitted < count {
		messageType, data, readErr := connection.ReadMessage()
		if readErr != nil {
			if ctx.Err() != nil {
				_ = writer.CloseWithError(ctx.Err())
				return
			}
			a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, readErr)
			writeImagineStreamFailure(writer, streamID, "upstream_stream_error", "图片生成流意外中断")
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
			writeImagineStreamFailure(writer, streamID, "upstream_error", "上游图片生成失败")
			_ = writer.CloseWithError(upstreamErr)
			return
		}
		collector.Accept(message)
		for _, image := range collector.ReadyImages() {
			if emitted >= count {
				break
			}
			item, err := a.imageDataItem(ctx, credential, image, format)
			if err != nil {
				writeImagineStreamFailure(writer, streamID, "image_output_error", "图片结果处理失败")
				_ = writer.CloseWithError(err)
				return
			}
			if err := writeSSE(writer, "image_generation.image.completed", map[string]any{
				"type": "image_generation.image.completed", "id": streamID,
				"index": emitted, "image": item,
			}); err != nil {
				_ = writer.CloseWithError(err)
				return
			}
			emitted++
		}
		if collector.Done(modelConfig.NativeBatchSize) && emitted < count {
			incompleteErr := fmt.Errorf("上游仅返回 %d/%d 张可用图片", emitted, count)
			writeImagineStreamFailure(writer, streamID, "image_generation_incomplete", incompleteErr.Error())
			_ = writer.CloseWithError(incompleteErr)
			return
		}
	}
	a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, http.StatusOK, nil)
	if err := writeSSE(writer, "image_generation.completed", map[string]any{
		"type": "image_generation.completed", "id": streamID, "object": "image_generation",
		"created": createdAt, "model": "grok-imagine-image-quality", "status": "completed", "n": emitted,
	}); err != nil {
		_ = writer.CloseWithError(err)
		return
	}
	if _, err := io.WriteString(writer, "data: [DONE]\n\n"); err != nil {
		_ = writer.CloseWithError(err)
		return
	}
	_ = writer.Close()
}

func writeImagineStreamFailure(writer io.Writer, streamID, code, message string) {
	_ = writeSSE(writer, "image_generation.failed", map[string]any{
		"type": "image_generation.failed", "id": streamID, "status": "failed",
		"error": map[string]any{"code": code, "message": message},
	})
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
	lease, err := a.egress.Acquire(ctx, domainegress.ScopeWebAsset, fmt.Sprintf("%d", credential.ID))
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header = buildHeaders(token, lease, "")
	request.Header.Del("Content-Type")
	response, err := lease.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("下载图片返回 %d", response.StatusCode)
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]))
	if contentType != "" && !strings.HasPrefix(contentType, "image/") {
		return nil, fmt.Errorf("上游图片 Content-Type 无效")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (32<<20)+1))
	if err != nil || len(raw) > 32<<20 {
		return nil, fmt.Errorf("图片下载失败或超过 32 MiB")
	}
	return raw, nil
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
	value.Scheme = "wss"
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
