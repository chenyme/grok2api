package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

const (
	consoleMediaBodyLimit       = 2 << 20
	consoleImageBodyLimit       = 32 << 20
	consoleImageDownloadTimeout = 45 * time.Second
	consoleMediaOutputAttempts  = 3
	consoleVideoPollEvery       = 2 * time.Second
	consoleMaxEditImages        = 3
	// Upstream enforces the two video image inputs separately (measured against
	// console.x.ai on both grok-imagine-video and grok-imagine-video-1.5):
	//   image            = first frame, image-to-video, at most 1
	//   reference_images = style/content references, reference-to-video, at most 7
	//     8 references answer 400 "Too many reference images: 8. Maximum allowed is 7."
	// The two are mutually exclusive below and their limits are never summed, so a
	// combined ceiling would accept payloads that upstream then rejects.
	consoleMaxVideoFirstFrames = 1
)

type consoleMediaUpstreamError struct {
	status        int
	summary       string
	retryAfter    time.Duration
	requestScoped bool
}

var (
	_ provider.HTTPStatusError    = (*consoleMediaUpstreamError)(nil)
	_ provider.RetryAfterError    = (*consoleMediaUpstreamError)(nil)
	_ provider.RequestScopedError = (*consoleMediaUpstreamError)(nil)
	_ provider.PublicMessageError = (*consoleMediaUpstreamError)(nil)
)

func (e *consoleMediaUpstreamError) Error() string {
	if e == nil {
		return ""
	}
	return e.summary
}

func (e *consoleMediaUpstreamError) HTTPStatusCode() int {
	if e == nil {
		return 0
	}
	return e.status
}

func (e *consoleMediaUpstreamError) RetryAfterDuration() time.Duration {
	if e == nil {
		return 0
	}
	return e.retryAfter
}

func (e *consoleMediaUpstreamError) RequestScopedFailure() bool {
	return e != nil && e.requestScoped
}

func (e *consoleMediaUpstreamError) PublicErrorMessage() string {
	if e == nil {
		return ""
	}
	return e.summary
}

func (a *Adapter) GenerateImage(ctx context.Context, request provider.ImageGenerationRequest) (*provider.Response, error) {
	if !ResolveMedia(request.Model, modeldomain.CapabilityImage) {
		return invalidConsoleMediaRequest("模型不支持 Console 图片生成"), nil
	}
	if request.Streaming || request.PartialImages != 0 {
		return invalidConsoleMediaRequest("Grok Console 标准图片接口不支持 stream 或 partial_images"), nil
	}
	count := request.Count
	if count <= 0 {
		count = 1
	}
	if count > 10 {
		return invalidConsoleMediaRequest("n 必须在 1 到 10 之间"), nil
	}
	format, err := normalizeConsoleImageFormat(request.ResponseFormat)
	if err != nil {
		return invalidConsoleMediaRequest(err.Error()), nil
	}
	ratio, err := resolveConsoleImageAspectRatio(request.AspectRatio, request.Size)
	if err != nil {
		return invalidConsoleMediaRequest(err.Error()), nil
	}
	resolution, err := normalizeConsoleImageResolution(request.Resolution)
	if err != nil {
		return invalidConsoleMediaRequest(err.Error()), nil
	}
	quality, err := normalizeConsoleImageQuality(request.Model, request.Quality)
	if err != nil {
		return invalidConsoleMediaRequest(err.Error()), nil
	}
	payload := map[string]any{"model": request.Model, "prompt": request.Prompt, "n": count, "response_format": format}
	if ratio != "" {
		payload["aspect_ratio"] = ratio
	}
	if resolution != "" {
		payload["resolution"] = resolution
	}
	if quality != "" {
		payload["quality"] = quality
	}
	return a.forwardConsoleMedia(ctx, request.Credential, "/images/generations", payload, format, count)
}

func (a *Adapter) EditImage(ctx context.Context, request provider.ImageEditRequest) (*provider.Response, error) {
	if !ResolveMedia(request.Model, modeldomain.CapabilityImageEdit) {
		return invalidConsoleMediaRequest("模型不支持 Console 图片编辑"), nil
	}
	if request.Streaming || request.PartialImages != 0 {
		return invalidConsoleMediaRequest("Grok Console 标准图片接口不支持 stream 或 partial_images"), nil
	}
	if len(request.ImageURLs) == 0 || len(request.ImageURLs) > consoleMaxEditImages {
		return invalidConsoleMediaRequest("Console 图片编辑必须提供 1 到 3 张图片"), nil
	}
	count := request.Count
	if count <= 0 {
		count = 1
	}
	if count > 10 {
		return invalidConsoleMediaRequest("n 必须在 1 到 10 之间"), nil
	}
	format, err := normalizeConsoleImageFormat(request.ResponseFormat)
	if err != nil {
		return invalidConsoleMediaRequest(err.Error()), nil
	}
	ratio, err := resolveConsoleImageAspectRatio(request.AspectRatio, request.Size)
	if err != nil {
		return invalidConsoleMediaRequest(err.Error()), nil
	}
	resolution, err := normalizeConsoleImageResolution(request.Resolution)
	if err != nil {
		return invalidConsoleMediaRequest(err.Error()), nil
	}
	quality, err := normalizeConsoleImageQuality(request.Model, request.Quality)
	if err != nil {
		return invalidConsoleMediaRequest(err.Error()), nil
	}
	images := make([]map[string]any, 0, len(request.ImageURLs))
	for _, rawURL := range request.ImageURLs {
		value := strings.TrimSpace(rawURL)
		if !validConsoleMediaInputURL(value, "image") {
			return invalidConsoleMediaRequest("每张编辑图片都必须是 HTTPS URL 或 image data URL"), nil
		}
		images = append(images, map[string]any{"type": "image_url", "url": value})
	}
	payload := map[string]any{"model": request.Model, "prompt": request.Prompt, "n": count, "response_format": format}
	if len(images) == 1 {
		payload["image"] = images[0]
	} else {
		payload["images"] = images
	}
	if ratio != "" {
		payload["aspect_ratio"] = ratio
	}
	if resolution != "" {
		payload["resolution"] = resolution
	}
	if quality != "" {
		payload["quality"] = quality
	}
	return a.forwardConsoleMedia(ctx, request.Credential, "/images/edits", payload, format, count)
}

func (a *Adapter) forwardConsoleMedia(ctx context.Context, credential account.Credential, path string, payload any, format string, quotaUnits int) (*provider.Response, error) {
	token, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	cfg := a.config()
	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutSeconds)*time.Second)
	lease, err := a.egress.AcquireCredential(requestCtx, egressdomain.ScopeConsole, credential)
	if err != nil {
		cancel()
		return nil, err
	}
	response, err := a.doDPoPRequest(requestCtx, credential, token, lease, http.MethodPost, consoleV1Endpoint(cfg.BaseURL, path), body, "application/json")
	if err != nil {
		a.egress.FeedbackForScope(context.WithoutCancel(ctx), egressdomain.ScopeConsole, lease.NodeID, 0, err)
		lease.Release()
		cancel()
		return nil, err
	}
	var rateLimit *provider.RateLimitMetadata
	if response.StatusCode == http.StatusTooManyRequests {
		_, rateLimit, err = normalizeRateLimitResponse(response)
		if err != nil {
			lease.Release()
			cancel()
			return nil, err
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, truncated, readErr := provider.ReadDiagnosticBody(response.Body)
		_ = response.Body.Close()
		dpopRequired := response.StatusCode == http.StatusForbidden && provider.IsDPoPProofRequiredBody(data)
		if response.StatusCode == http.StatusForbidden && shouldInvalidateConsoleClearance(data) {
			lease.InvalidateClearance()
		}
		if !dpopRequired {
			a.egress.FeedbackForScope(context.WithoutCancel(ctx), egressdomain.ScopeConsole, lease.NodeID, response.StatusCode, nil)
		}
		lease.Release()
		cancel()
		if readErr != nil {
			return nil, readErr
		}
		response.Body = io.NopCloser(bytes.NewReader(data))
		response.ContentLength = int64(len(data))
		response.Header.Set("Content-Length", strconv.Itoa(len(data)))
		if truncated {
			response.Header.Set("X-Grok2API-Body-Truncated", "1")
		}
		result := responseResult(response, response.Body)
		result.Diagnostic = &provider.DiagnosticResponse{StatusCode: response.StatusCode, Status: response.Status, Header: response.Header.Clone(), Body: data, BodyTruncated: truncated}
		result.RateLimit = rateLimit
		return result, nil
	}
	if format == "url" {
		data, readErr := io.ReadAll(io.LimitReader(response.Body, consoleMediaBodyLimit+1))
		_ = response.Body.Close()
		a.egress.FeedbackForScope(context.WithoutCancel(ctx), egressdomain.ScopeConsole, lease.NodeID, response.StatusCode, readErr)
		lease.Release()
		cancel()
		if readErr == nil && len(data) > consoleMediaBodyLimit {
			readErr = errors.New("Console 图片上游响应超过 2 MiB")
		}
		if readErr == nil {
			data, readErr = a.localizeConsoleImageResponse(ctx, credential, cfg.BaseURL, data)
		}
		if readErr != nil {
			if !provider.IsMediaPostProcessingError(readErr) {
				readErr = provider.NewMediaPostProcessingError(provider.MediaPostProcessingDownload, readErr)
			}
			return nil, readErr
		}
		response.Body = io.NopCloser(bytes.NewReader(data))
		response.ContentLength = int64(len(data))
		response.Header.Set("Content-Length", strconv.Itoa(len(data)))
		response.Header.Set("Content-Type", "application/json")
		result := responseResult(response, response.Body)
		result.QuotaUnits = max(1, quotaUnits)
		result.RateLimit = rateLimit
		return result, nil
	}
	release := func() {
		a.egress.FeedbackForScope(context.WithoutCancel(ctx), egressdomain.ScopeConsole, lease.NodeID, response.StatusCode, nil)
		lease.Release()
		cancel()
	}
	result := responseResult(response, &releaseBody{ReadCloser: response.Body, release: release})
	result.QuotaUnits = max(1, quotaUnits)
	result.RateLimit = rateLimit
	return result, nil
}

func (a *Adapter) localizeConsoleImageResponse(ctx context.Context, credential account.Credential, baseURL string, data []byte) ([]byte, error) {
	if a.assets == nil {
		return nil, provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, errors.New("图片媒体存储未配置"))
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, provider.NewMediaPostProcessingError(provider.MediaPostProcessingDownload, fmt.Errorf("解析 Console 图片响应: %w", err))
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(envelope["data"], &items); err != nil || len(items) == 0 || len(items) > 10 {
		return nil, provider.NewMediaPostProcessingError(provider.MediaPostProcessingDownload, errors.New("Console 图片响应缺少有效 data"))
	}

	for index, item := range items {
		var rawURL string
		if err := json.Unmarshal(item["url"], &rawURL); err != nil || strings.TrimSpace(rawURL) == "" {
			return nil, provider.NewMediaPostProcessingError(provider.MediaPostProcessingDownload, fmt.Errorf("Console 图片响应第 %d 项缺少 URL", index+1))
		}
		raw, err := a.downloadConsoleImage(ctx, credential, baseURL, rawURL)
		if err != nil {
			return nil, provider.NewMediaPostProcessingError(provider.MediaPostProcessingDownload, err)
		}
		asset, err := a.saveConsoleImage(ctx, raw)
		if err != nil {
			return nil, provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, err)
		}
		items[index]["url"], _ = json.Marshal(a.assets.PublicImageURL(asset.ID))
		items[index]["mime_type"], _ = json.Marshal(asset.MIMEType)
	}
	encodedItems, err := json.Marshal(items)
	if err != nil {
		return nil, provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, err)
	}
	envelope["data"] = encodedItems
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return nil, provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, err)
	}
	return encoded, nil
}

func (a *Adapter) downloadConsoleImage(ctx context.Context, credential account.Credential, baseURL, rawURL string) ([]byte, error) {
	parsed, err := trustedConsoleImageURL(rawURL, baseURL)
	if err != nil {
		return nil, err
	}
	downloadCtx, cancel := context.WithTimeout(ctx, consoleImageDownloadTimeout)
	defer cancel()
	var lastErr error
	for attempt := 0; attempt < consoleMediaOutputAttempts; attempt++ {
		raw, retryable, attemptErr := a.downloadConsoleImageAttempt(downloadCtx, credential, baseURL, parsed.String())
		if attemptErr == nil {
			return raw, nil
		}
		lastErr = attemptErr
		if !retryable || downloadCtx.Err() != nil || attempt+1 >= consoleMediaOutputAttempts {
			break
		}
		if err := waitConsoleMediaRetry(downloadCtx, attempt); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

func (a *Adapter) downloadConsoleImageAttempt(ctx context.Context, credential account.Credential, baseURL, rawURL string) ([]byte, bool, error) {
	lease, err := a.egress.AcquireCredential(ctx, egressdomain.ScopeConsoleAsset, credential)
	if err != nil {
		return nil, true, err
	}
	defer lease.Release()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, false, err
	}
	request.Header.Set("Accept", "image/*,*/*;q=0.8")
	userAgent := strings.TrimSpace(lease.UserAgent)
	if userAgent == "" {
		userAgent = infraegress.DefaultUserAgent
	}
	request.Header.Set("User-Agent", userAgent)
	response, err := lease.DoDeferredForbidden(request)
	if err != nil {
		a.egress.FeedbackForScope(context.WithoutCancel(ctx), egressdomain.ScopeConsoleAsset, lease.NodeID, 0, err)
		return nil, ctx.Err() == nil, fmt.Errorf("下载 Console 图片: %w", err)
	}
	defer response.Body.Close()
	// 标准 net/http 响应会保留最终请求 URL；兼容不填充 Request 的自定义出口客户端时，
	// 初始 URL 已在发起请求前完成校验。若存在最终 URL，则额外检查重定向目标。
	if response.Request != nil && response.Request.URL != nil {
		if _, err := trustedConsoleImageURL(response.Request.URL.String(), baseURL); err != nil {
			return nil, false, errors.New("Console 图片下载重定向到不受信任的地址")
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		retryable := response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooEarly || response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
		a.egress.FeedbackForScope(context.WithoutCancel(ctx), egressdomain.ScopeConsoleAsset, lease.NodeID, response.StatusCode, nil)
		return nil, retryable, fmt.Errorf("下载 Console 图片返回 %d", response.StatusCode)
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]))
	if contentType != "" && contentType != "application/octet-stream" && !strings.HasPrefix(contentType, "image/") {
		return nil, false, errors.New("Console 图片 Content-Type 无效")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, consoleImageBodyLimit+1))
	if err != nil {
		return nil, ctx.Err() == nil, fmt.Errorf("读取 Console 图片: %w", err)
	}
	if len(raw) == 0 || len(raw) > consoleImageBodyLimit {
		return nil, false, errors.New("Console 图片为空或超过 32 MiB")
	}
	a.egress.FeedbackForScope(context.WithoutCancel(ctx), egressdomain.ScopeConsoleAsset, lease.NodeID, response.StatusCode, nil)
	return raw, false, nil
}

func (a *Adapter) saveConsoleImage(ctx context.Context, raw []byte) (asset mediadomain.Asset, err error) {
	for attempt := 0; attempt < consoleMediaOutputAttempts; attempt++ {
		asset, err = a.assets.SaveImage(ctx, raw)
		if err == nil || ctx.Err() != nil || attempt+1 >= consoleMediaOutputAttempts {
			return asset, err
		}
		if err = waitConsoleMediaRetry(ctx, attempt); err != nil {
			return mediadomain.Asset{}, err
		}
	}
	return asset, err
}

func waitConsoleMediaRetry(ctx context.Context, attempt int) error {
	delays := [...]time.Duration{200 * time.Millisecond, 750 * time.Millisecond}
	timer := time.NewTimer(delays[min(attempt, len(delays)-1)])
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func trustedConsoleImageURL(rawURL, baseURL string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.User != nil || parsed.Hostname() == "" {
		return nil, errors.New("Console 图片内容 URL 不受信任")
	}
	if parsed.Scheme == "https" && trustedConsoleImageHost(parsed.Hostname()) {
		return parsed, nil
	}
	base, baseErr := url.Parse(strings.TrimSpace(baseURL))
	if baseErr == nil && base.User == nil && strings.EqualFold(parsed.Scheme, base.Scheme) && strings.EqualFold(parsed.Host, base.Host) && (parsed.Scheme == "http" || parsed.Scheme == "https") {
		return parsed, nil
	}
	return nil, errors.New("Console 图片内容 URL 不受信任")
}

func trustedConsoleImageHost(host string) bool {
	return strings.EqualFold(host, "assets.grok.com") || strings.EqualFold(host, "imagine-public.x.ai") || strings.EqualFold(host, "imgen.x.ai")
}

func (a *Adapter) GenerateVideo(ctx context.Context, request provider.VideoRequest) (provider.VideoResult, error) {
	modelName := strings.TrimSpace(request.Model)
	if modelName == "" {
		modelName = "grok-imagine-video"
	}
	if !ResolveMedia(modelName, modeldomain.CapabilityVideo) {
		return provider.VideoResult{}, fmt.Errorf("Console 视频模型未注册: %s", modelName)
	}
	operation := request.Operation
	if operation == "" {
		operation = provider.VideoOperationGenerate
	}
	switch operation {
	case provider.VideoOperationGenerate, provider.VideoOperationEdit, provider.VideoOperationExtend:
	default:
		return provider.VideoResult{}, fmt.Errorf("不支持的视频操作: %s", operation)
	}
	// Official video edit/extension docs and model catalog only expose VIDEO input on
	// grok-imagine-video. 1.5 is generation-oriented (TEXT/IMAGE/AUDIO) and is rejected here.
	if operation != provider.VideoOperationGenerate && modelName != "grok-imagine-video" {
		return provider.VideoResult{}, fmt.Errorf("%s 仅支持 grok-imagine-video", operation)
	}
	firstFrames, referenceImages := consoleVideoImageCounts(request)
	if firstFrames > consoleMaxVideoFirstFrames {
		return provider.VideoResult{}, fmt.Errorf("Console %s 最多支持 %d 张首帧图，当前为 %d 张", modelName, consoleMaxVideoFirstFrames, firstFrames)
	}
	if referenceImages > provider.ConsoleVideoMaxReferenceImages {
		return provider.VideoResult{}, fmt.Errorf("Console %s 最多支持 %d 张参考图，当前为 %d 张", modelName, provider.ConsoleVideoMaxReferenceImages, referenceImages)
	}
	if operation == provider.VideoOperationGenerate {
		if request.Duration < 1 || request.Duration > 15 {
			return provider.VideoResult{}, errors.New("duration 必须在 1 到 15 秒之间")
		}
		// reference-to-video on the base model caps at 10s upstream; measured:
		//   grok-imagine-video + reference_images + duration=15
		//   -> 400 "Duration 15s exceeds the maximum allowed for reference-to-video, which is 10s."
		// image-to-video (the image field) and grok-imagine-video-1.5 both keep 15s,
		// so the cap keys on reference_images plus the base model, not on duration alone.
		if modelName == "grok-imagine-video" && referenceImages > 0 && request.Duration > provider.ConsoleVideoMaxReferenceDurationSeconds {
			return provider.VideoResult{}, fmt.Errorf("%s 的参考图生视频最长 %d 秒，当前为 %d 秒", modelName, provider.ConsoleVideoMaxReferenceDurationSeconds, request.Duration)
		}
	} else if operation == provider.VideoOperationExtend {
		// Official /v1/videos/extensions defaults to 6s and accepts 2-10s.
		if request.Duration == 0 {
			request.Duration = 6
		}
		if request.Duration < 2 || request.Duration > 10 {
			return provider.VideoResult{}, errors.New("视频延长 duration 必须在 2 到 10 秒之间")
		}
	} else if request.Duration != 0 {
		return provider.VideoResult{}, errors.New("视频编辑不支持 duration")
	}
	if request.Resolution == "1080p" {
		if modelName != "grok-imagine-video-1.5" {
			return provider.VideoResult{}, fmt.Errorf("%s 不支持 1080p", modelName)
		}
		if len(request.ReferenceURLs) > 0 {
			return provider.VideoResult{}, errors.New("reference_images 模式最高支持 720p")
		}
	} else if request.Resolution != "" && request.Resolution != "480p" && request.Resolution != "720p" {
		return provider.VideoResult{}, fmt.Errorf("%s 仅支持 480p、720p 或 1080p", modelName)
	}
	payload := map[string]any{"model": modelName}
	if operation == provider.VideoOperationGenerate || operation == provider.VideoOperationExtend {
		payload["duration"] = request.Duration
	}
	if prompt := strings.TrimSpace(request.Prompt); prompt != "" {
		payload["prompt"] = prompt
	}
	if operation == provider.VideoOperationGenerate {
		if ratio := strings.TrimSpace(request.AspectRatio); ratio != "" {
			payload["aspect_ratio"] = ratio
		}
		if resolution := strings.TrimSpace(request.Resolution); resolution != "" {
			payload["resolution"] = resolution
		}
		if imageURL := strings.TrimSpace(request.ImageURL); imageURL != "" {
			if !validConsoleMediaInputURL(imageURL, "image") {
				return provider.VideoResult{}, errors.New("视频首图必须是 HTTPS URL 或 image data URL")
			}
			payload["image"] = map[string]any{"url": imageURL}
		}
		if strings.TrimSpace(request.ImageURL) != "" && (len(request.ReferenceURLs) > 0 || len(request.ReferenceAudios) > 0) {
			return provider.VideoResult{}, errors.New("image 不能与 reference_images/reference_audios 同时使用")
		}
		if len(request.ReferenceURLs) > 0 {
			references := make([]map[string]any, 0, len(request.ReferenceURLs))
			for _, rawURL := range request.ReferenceURLs {
				value := strings.TrimSpace(rawURL)
				if !validConsoleMediaInputURL(value, "image") {
					return provider.VideoResult{}, errors.New("每张视频参考图都必须是 HTTPS URL 或 image data URL")
				}
				references = append(references, map[string]any{"url": value})
			}
			// A single reference must stay in reference_images; do not coerce to image.
			payload["reference_images"] = references
		}
		if len(request.ReferenceAudios) > 0 {
			if len(request.ReferenceAudios) > 3 {
				return provider.VideoResult{}, errors.New("reference_audios 最多 3 个")
			}
			audios := make([]map[string]any, 0, len(request.ReferenceAudios))
			for _, raw := range request.ReferenceAudios {
				voiceID := strings.TrimSpace(raw)
				if voiceID == "" {
					return provider.VideoResult{}, errors.New("reference_audios.voice_id 不能为空")
				}
				audios = append(audios, map[string]any{"voice_id": voiceID})
			}
			payload["reference_audios"] = audios
		}
		hasReferenceMode := len(request.ReferenceURLs) > 0 || len(request.ReferenceAudios) > 0
		if hasReferenceMode {
			if strings.TrimSpace(request.Prompt) == "" {
				return provider.VideoResult{}, errors.New("参考图/参考音频视频必须提供 prompt")
			}
			if strings.EqualFold(strings.TrimSpace(request.Resolution), "1080p") {
				return provider.VideoResult{}, errors.New("参考图视频 resolution 最高 720p")
			}
		}
		if _, hasPrompt := payload["prompt"]; !hasPrompt {
			if _, hasImage := payload["image"]; !hasImage {
				if !hasReferenceMode {
					return provider.VideoResult{}, errors.New("文本生视频必须提供 prompt；图片生视频可以省略 prompt")
				}
			}
		}
	} else {
		prompt := strings.TrimSpace(request.Prompt)
		if prompt == "" {
			return provider.VideoResult{}, errors.New("视频编辑/延长必须提供 prompt")
		}
		videoURL := strings.TrimSpace(request.VideoURL)
		if !validConsoleMediaInputURL(videoURL, "video") {
			return provider.VideoResult{}, errors.New("输入视频必须是 HTTPS URL 或 video data URL")
		}
		payload["prompt"] = prompt
		payload["video"] = map[string]any{"url": videoURL}
		if strings.TrimSpace(request.ImageURL) != "" || len(request.ReferenceURLs) > 0 || len(request.ReferenceAudios) > 0 {
			return provider.VideoResult{}, errors.New("视频编辑/延长不支持 image、reference_images 或 reference_audios")
		}
		if strings.TrimSpace(request.AspectRatio) != "" || strings.TrimSpace(request.Resolution) != "" {
			return provider.VideoResult{}, errors.New("视频编辑/延长不支持 aspect_ratio 或 resolution")
		}
	}

	token, err := a.cipher.Decrypt(request.Credential.EncryptedAccessToken)
	if err != nil {
		return provider.VideoResult{}, err
	}
	lease, err := a.egress.AcquireCredential(ctx, egressdomain.ScopeConsole, request.Credential)
	if err != nil {
		return provider.VideoResult{}, err
	}
	defer lease.Release()
	body, err := json.Marshal(payload)
	if err != nil {
		return provider.VideoResult{}, err
	}
	baseURL := a.config().BaseURL
	createPath := "/videos/generations"
	switch operation {
	case provider.VideoOperationEdit:
		createPath = "/videos/edits"
	case provider.VideoOperationExtend:
		createPath = "/videos/extensions"
	}
	created, err := a.doConsoleVideoJSON(ctx, request.Credential, token, lease, http.MethodPost, consoleV1Endpoint(baseURL, createPath), body)
	if err != nil {
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoCreateFailureStage(err), 0, err)
	}
	requestID, err := parseConsoleVideoCreate(created)
	if err != nil {
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStageSubmitted, 0, err)
	}
	if request.Progress != nil {
		request.Progress(1)
	}
	ticker := time.NewTicker(consoleVideoPollEvery)
	defer ticker.Stop()
	for {
		statusBody, pollErr := a.doConsoleVideoJSON(ctx, request.Credential, token, lease, http.MethodGet, consoleV1Endpoint(baseURL, "/videos/"+url.PathEscape(requestID)), nil)
		if pollErr != nil {
			return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePoll, 0, pollErr)
		}
		result, done, parseErr := parseConsoleVideoStatus(statusBody, request.Progress)
		if parseErr != nil {
			return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePoll, 0, parseErr)
		}
		if done {
			return result, nil
		}
		select {
		case <-ctx.Done():
			return provider.VideoResult{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (a *Adapter) doConsoleVideoJSON(ctx context.Context, credential account.Credential, token string, lease *infraegress.Lease, method, endpoint string, body []byte) ([]byte, error) {
	requestCtx := ctx
	cancel := func() {}
	if timeout := a.config().TimeoutSeconds; timeout > 0 {
		requestCtx, cancel = context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	}
	defer cancel()
	response, err := a.doDPoPRequest(requestCtx, credential, token, lease, method, endpoint, body, "application/json")
	if err != nil {
		a.egress.FeedbackForScope(context.WithoutCancel(ctx), egressdomain.ScopeConsole, lease.NodeID, 0, err)
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, consoleMediaBodyLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > consoleMediaBodyLimit {
		return nil, errors.New("Console 视频上游响应超过 2 MiB")
	}
	if response.StatusCode == http.StatusUnauthorized {
		return nil, provider.ErrUnauthorized
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		dpopRequired := response.StatusCode == http.StatusForbidden && provider.IsDPoPProofRequiredBody(data)
		if response.StatusCode == http.StatusForbidden && shouldInvalidateConsoleClearance(data) {
			lease.InvalidateClearance()
		}
		if !dpopRequired {
			a.egress.FeedbackForScope(context.WithoutCancel(ctx), egressdomain.ScopeConsole, lease.NodeID, response.StatusCode, nil)
		}
		return nil, newConsoleMediaUpstreamError(response.StatusCode, data, parseConsoleRetryAfterHeader(response.Header.Get("Retry-After"), time.Now().UTC()))
	}
	a.egress.FeedbackForScope(context.WithoutCancel(ctx), egressdomain.ScopeConsole, lease.NodeID, response.StatusCode, nil)
	return data, nil
}

func (a *Adapter) DownloadVideo(ctx context.Context, credential account.Credential, rawURL string) (io.ReadCloser, string, int64, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || !trustedConsoleVideoHost(parsed.Hostname()) {
		return nil, "", 0, errors.New("Console 视频内容 URL 不受信任")
	}
	lease, err := a.egress.AcquireCredential(ctx, egressdomain.ScopeConsoleAsset, credential)
	if err != nil {
		return nil, "", 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		lease.Release()
		return nil, "", 0, err
	}
	request.Header.Set("Accept", "video/*,*/*;q=0.8")
	userAgent := strings.TrimSpace(lease.UserAgent)
	if userAgent == "" {
		userAgent = infraegress.DefaultUserAgent
	}
	request.Header.Set("User-Agent", userAgent)
	response, err := lease.DoDeferredForbidden(request)
	if err != nil {
		a.egress.FeedbackForScope(context.WithoutCancel(ctx), egressdomain.ScopeConsoleAsset, lease.NodeID, 0, err)
		lease.Release()
		return nil, "", 0, err
	}
	if response.Request != nil && response.Request.URL != nil {
		finalURL := response.Request.URL
		if finalURL.Scheme != "https" || finalURL.User != nil || !trustedConsoleVideoHost(finalURL.Hostname()) {
			_ = response.Body.Close()
			lease.Release()
			return nil, "", 0, errors.New("Console 视频下载重定向到不受信任的地址")
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_ = response.Body.Close()
		a.egress.FeedbackForScope(context.WithoutCancel(ctx), egressdomain.ScopeConsoleAsset, lease.NodeID, response.StatusCode, nil)
		lease.Release()
		return nil, "", 0, fmt.Errorf("下载 Console 视频返回 %d", response.StatusCode)
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]))
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = "video/mp4"
	}
	if !strings.HasPrefix(contentType, "video/") {
		_ = response.Body.Close()
		lease.Release()
		return nil, "", 0, errors.New("Console 视频 Content-Type 无效")
	}
	onFinished := func(readErr error, complete bool) {
		if readErr != nil {
			a.egress.FeedbackForScope(context.WithoutCancel(ctx), egressdomain.ScopeConsoleAsset, lease.NodeID, 0, readErr)
		} else if complete {
			a.egress.FeedbackForScope(context.WithoutCancel(ctx), egressdomain.ScopeConsoleAsset, lease.NodeID, response.StatusCode, nil)
		}
		lease.Release()
	}
	return provider.NewCompletionReadCloser(response.Body, onFinished), contentType, response.ContentLength, nil
}

func invalidConsoleMediaRequest(message string) *provider.Response {
	return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{
		"message": message, "type": "invalid_request_error",
	}})
}

func normalizeConsoleImageFormat(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "url", nil
	}
	if value != "url" && value != "b64_json" {
		return "", errors.New("response_format 必须是 url 或 b64_json")
	}
	return value, nil
}

func normalizeConsoleImageResolution(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "", nil
	}
	if value != "1k" && value != "2k" {
		return "", errors.New("resolution 必须是 1k 或 2k")
	}
	return value, nil
}

func normalizeConsoleImageQuality(model, value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "", nil
	}
	if strings.TrimSpace(model) != "grok-imagine-image-2.0" {
		return "", errors.New("quality 仅支持 grok-imagine-image-2.0")
	}
	if value != "low" && value != "medium" {
		return "", errors.New("quality 必须是 low 或 medium")
	}
	return value, nil
}

func resolveConsoleImageAspectRatio(aspectRatio, size string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(aspectRatio))
	if value == "" {
		value = strings.ToLower(strings.TrimSpace(size))
	}
	if value == "" || value == "auto" {
		return "", nil
	}
	values := map[string]string{
		"1:1": "1:1", "16:9": "16:9", "9:16": "9:16", "4:3": "4:3", "3:4": "3:4", "3:2": "3:2", "2:3": "2:3", "2:1": "2:1", "1:2": "1:2",
		"1024x1024": "1:1", "1280x720": "16:9", "720x1280": "9:16", "1792x1024": "3:2", "1536x1024": "3:2", "1024x1792": "2:3", "1024x1536": "2:3",
	}
	if resolved := values[value]; resolved != "" {
		return resolved, nil
	}
	return "", errors.New("aspect_ratio 或 size 不受支持")
}

func validConsoleMediaInputURL(value, mediaType string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	if strings.HasPrefix(lower, "data:"+mediaType+"/") {
		return strings.Contains(lower, ";base64,")
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil
}

func parseConsoleVideoCreate(body []byte) (string, error) {
	var payload struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("解析 Console 视频创建响应: %w", err)
	}
	if strings.TrimSpace(payload.RequestID) == "" {
		return "", errors.New("Console 视频创建响应缺少 request_id")
	}
	return payload.RequestID, nil
}

func parseConsoleVideoStatus(body []byte, progress func(int)) (provider.VideoResult, bool, error) {
	var payload struct {
		Status   string `json:"status"`
		Progress int    `json:"progress"`
		Video    struct {
			URL string `json:"url"`
		} `json:"video"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return provider.VideoResult{}, false, fmt.Errorf("解析 Console 视频状态响应: %w", err)
	}
	if progress != nil && payload.Progress > 0 {
		progress(min(99, payload.Progress))
	}
	switch status := strings.ToLower(strings.TrimSpace(payload.Status)); status {
	case "done", "completed", "succeeded", "success", "ready":
		if strings.TrimSpace(payload.Video.URL) == "" {
			return provider.VideoResult{}, false, errors.New("Console 视频生成完成但没有返回内容 URL")
		}
		return provider.VideoResult{URL: strings.TrimSpace(payload.Video.URL), ContentType: "video/mp4"}, true, nil
	case "failed", "expired", "cancelled", "canceled", "error":
		message := safeConsoleMediaErrorValue(payload.Error)
		if message == "" {
			message = strings.ToLower(strings.TrimSpace(payload.Status))
		}
		return provider.VideoResult{}, false, fmt.Errorf("Console 视频生成失败: %s", message)
	case "pending", "processing", "in_progress", "queued":
		return provider.VideoResult{}, false, nil
	default:
		return provider.VideoResult{}, false, fmt.Errorf("Console 视频状态无效: %q", safeConsoleMediaText(status))
	}
}

func newConsoleMediaUpstreamError(status int, body []byte, retryAfter time.Duration) error {
	message := ""
	var payload map[string]any
	if json.Unmarshal(body, &payload) == nil {
		message = safeConsoleMediaErrorValue(payload["error"])
		if message == "" {
			message = safeConsoleMediaErrorValue(payload["message"])
		}
		if message == "" {
			message = safeConsoleMediaErrorValue(payload["detail"])
		}
	}
	summary := fmt.Sprintf("Console 媒体上游返回 %d", status)
	if message != "" {
		summary += ": " + message
	}
	if retryAfter <= 0 {
		retryAfter = consoleRetryAfter(body)
	}
	return &consoleMediaUpstreamError{
		status: status, summary: summary, retryAfter: retryAfter,
		requestScoped: status == http.StatusForbidden && provider.IsDPoPProofRequiredBody(body),
	}
}

func safeConsoleMediaErrorValue(value any) string {
	if object, ok := value.(map[string]any); ok {
		for _, key := range []string{"message", "msg", "code", "type", "detail", "error_description"} {
			raw, exists := object[key]
			if !exists || raw == nil {
				continue
			}
			if text := safeConsoleMediaErrorValue(raw); text != "" {
				return text
			}
		}
	}
	if values, ok := value.([]any); ok {
		for _, item := range values {
			if text := safeConsoleMediaErrorValue(item); text != "" {
				return text
			}
		}
	}
	if text, ok := value.(string); ok {
		return safeConsoleMediaText(text)
	}
	switch value.(type) {
	case json.Number, float64, float32, int, int64, int32, uint, uint64, uint32, bool:
		return safeConsoleMediaText(fmt.Sprint(value))
	}
	return ""
}

func safeConsoleMediaText(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	lower := strings.ToLower(value)
	for _, sensitive := range []string{
		"authorization", "cookie", "bearer ",
		"access_token", "access-token", "refresh_token", "refresh-token",
		"api_key", "api-key", "sso-rw", "cf_clearance",
	} {
		if strings.Contains(lower, sensitive) {
			return "上游拒绝请求"
		}
	}
	const limit = 160
	if len(value) <= limit {
		return value
	}
	end := limit
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end]
}

func trustedConsoleVideoHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	return host == "vidgen.x.ai" || strings.HasSuffix(host, ".vidgen.x.ai")
}

func consoleVideoImageCounts(request provider.VideoRequest) (firstFrames int, referenceImages int) {
	if strings.TrimSpace(request.ImageURL) != "" {
		firstFrames++
	}
	for _, raw := range request.ReferenceURLs {
		if strings.TrimSpace(raw) != "" {
			referenceImages++
		}
	}
	return firstFrames, referenceImages
}
