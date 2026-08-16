package inference

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/pkg/mediafile"
	"github.com/chenyme/grok2api/backend/internal/pkg/neterror"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	gateway          *gateway.Service
	models           *modelapp.Service
	maxBodyBytes     int64
	publicAPIBaseURL string
	publicBaseURL    func() string
}

const (
	responseCopyBufferBytes         = 32 << 10
	maxJSONMetadataInspectionBytes  = 8 << 20
	maxStreamEventInspectionBytes   = 8 << 20
	maxStreamFailureDiagnosticBytes = 64 << 10
	maxCredentialErrorInspectBytes  = 64 << 10
	maxJSONResponseTransferBytes    = 128 << 20
	maxStreamResponseTransferBytes  = 256 << 20
	maxMediaResponseTransferBytes   = int64(2) << 30
	responseWriteTimeout            = 30 * time.Second
)

var (
	errResponseTransferLimit    = errors.New("响应超过代理安全上限")
	errUpstreamStreamIncomplete = errors.New("上游流在终止事件前结束")
	errUpstreamStreamFailed     = errors.New("上游流返回失败终止事件")
	errUpstreamStreamRead       = errors.New("读取上游流失败")
)

type streamProtocol uint8

const (
	streamProtocolResponses streamProtocol = iota
	streamProtocolChat
	streamProtocolAnthropic
	streamProtocolImage
)

const mediaTransferErrorTrailer = "X-Grok2API-Transfer-Error"

func NewHandler(gatewayService *gateway.Service, models *modelapp.Service, maxBodyBytes int64, publicAPIBaseURL ...string) *Handler {
	baseURL := ""
	if len(publicAPIBaseURL) > 0 {
		baseURL = strings.TrimRight(strings.TrimSpace(publicAPIBaseURL[0]), "/")
	}
	return &Handler{gateway: gatewayService, models: models, maxBodyBytes: maxBodyBytes, publicAPIBaseURL: baseURL}
}

// SetPublicAPIBaseURLResolver makes video content URLs follow hot-updated runtime settings.
// Set it before Register; request handling only reads the resolver.
func (h *Handler) SetPublicAPIBaseURLResolver(resolve func() string) *Handler {
	h.publicBaseURL = resolve
	return h
}

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/models", h.listModels)
	router.POST("/responses", h.createResponse)
	router.POST("/chat/completions", h.createChatCompletion)
	router.POST("/messages", h.createMessage)
	router.POST("/images/generations", h.generateImage)
	router.POST("/images/edits", h.editImage)
	router.POST("/videos/generations", h.generateVideo)
	router.POST("/videos/edits", h.editVideo)
	router.POST("/videos/extensions", h.extendVideo)
	router.GET("/videos/:requestId", h.getVideo)
	router.GET("/videos/:requestId/content", h.getVideoContent)
	router.POST("/tts", h.synthesizeSpeech)
	router.GET("/tts/voices", h.listTTSVoices)
	router.GET("/tts/voices/:voiceId", h.getTTSVoice)
	router.POST("/stt", h.transcribeSpeech)
	router.GET("/stt", h.proxySTTWebSocket)
	// OpenAI-compatible audio aliases for common client SDKs.
	router.POST("/audio/speech", h.synthesizeOpenAISpeech)
	router.POST("/audio/tasks", h.synthesizeOpenAIAudioTask)
	router.POST("/audio/transcriptions", h.transcribeOpenAIAudio)
	router.GET("/realtime", h.proxyRealtimeWebSocket)
	router.POST("/responses/compact", h.compactResponse)
	router.GET("/responses/:responseId", h.getResponse)
	router.DELETE("/responses/:responseId", h.deleteResponse)
}

type responsesRequest struct {
	Model              string `json:"model"`
	Stream             bool   `json:"stream"`
	PromptCacheKey     string `json:"prompt_cache_key"`
	PreviousResponseID string `json:"previous_response_id"`
}

type chatCompletionRequest struct {
	Model          string `json:"model"`
	Stream         bool   `json:"stream"`
	PromptCacheKey string `json:"prompt_cache_key"`
}

type messagesRequest struct {
	Model          string          `json:"model"`
	MaxTokens      *int            `json:"max_tokens"`
	Messages       json.RawMessage `json:"messages"`
	Stream         bool            `json:"stream"`
	PromptCacheKey string          `json:"prompt_cache_key"`
}

type imageGenerationRequest struct {
	Model          string          `json:"model"`
	Prompt         string          `json:"prompt"`
	Count          *int            `json:"n"`
	PartialImages  *int            `json:"partial_images"`
	Size           string          `json:"size"`
	AspectRatio    string          `json:"aspect_ratio"`
	Resolution     string          `json:"resolution"`
	Quality        string          `json:"quality"`
	ResponseFormat string          `json:"response_format"`
	StorageOptions json.RawMessage `json:"storage_options"`
	Stream         bool            `json:"stream"`
}

type imageEditJSONImage struct {
	URL    string `json:"url"`
	FileID string `json:"file_id"`
}

type imageEditJSONRequest struct {
	Model          string               `json:"model"`
	Prompt         string               `json:"prompt"`
	Image          *imageEditJSONImage  `json:"image"`
	Images         []imageEditJSONImage `json:"images"`
	Count          *int                 `json:"n"`
	Size           string               `json:"size"`
	AspectRatio    string               `json:"aspect_ratio"`
	Resolution     string               `json:"resolution"`
	Quality        string               `json:"quality"`
	ResponseFormat string               `json:"response_format"`
	StorageOptions json.RawMessage      `json:"storage_options"`
	Stream         bool                 `json:"stream"`
	PartialImages  *int                 `json:"partial_images"`
}

type videoGenerationImage struct {
	URL    string `json:"url"`
	FileID string `json:"file_id"`
}

type videoGenerationAudio struct {
	VoiceID string `json:"voice_id"`
}

type videoGenerationRequest struct {
	Model           string                 `json:"model"`
	Prompt          string                 `json:"prompt"`
	User            *string                `json:"user"`
	Duration        json.RawMessage        `json:"duration"`
	AspectRatio     string                 `json:"aspect_ratio"`
	Resolution      string                 `json:"resolution"`
	Image           *videoGenerationImage  `json:"image"`
	ReferenceImages []videoGenerationImage `json:"reference_images"`
	ReferenceAudios []videoGenerationAudio `json:"reference_audios"`
	Video           *videoGenerationImage  `json:"video"`
	Output          json.RawMessage        `json:"output"`
	StorageOptions  json.RawMessage        `json:"storage_options"`
}

type modelListItem struct {
	ID         string                 `json:"id"`
	Object     string                 `json:"object"`
	Created    int64                  `json:"created"`
	OwnedBy    string                 `json:"owned_by"`
	Provider   account.Provider       `json:"-"`
	Capability modeldomain.Capability `json:"-"`
}

func (h *Handler) listModels(c *gin.Context) {
	allowAliases := false
	var clientKey clientkeydomain.Key
	hasClientKey := false
	if clientValue, exists := c.Get(middleware.ClientKey); exists {
		if value, ok := clientValue.(clientkeydomain.Key); ok {
			clientKey = value
			hasClientKey = true
			allowAliases = clientKey.AllowModelAliases
		}
	}
	var values []modeldomain.Route
	var err error
	if hasClientKey {
		values, err = h.models.ListEnabledForClientKey(c.Request.Context(), clientKey)
	} else {
		values, err = h.models.ListEnabled(c.Request.Context())
	}
	if err != nil {
		writeOpenAIError(c, http.StatusInternalServerError, "model_list_failed", "读取模型列表失败")
		return
	}
	if hasClientKey {
		values = filterModelRoutesForClientKey(values, clientKey)
	}
	items := newModelListItems(values)
	if allowAliases {
		items = appendReasoningModelAliases(items)
	}
	if clientVersion := strings.TrimSpace(c.Query("client_version")); clientVersion != "" {
		writeCodexModelCatalog(c, newCodexModelCatalog(items))
		return
	}
	c.JSON(http.StatusOK, gin.H{"object": "list", "data": items})
}

func filterModelRoutesForClientKey(values []modeldomain.Route, key clientkeydomain.Key) []modeldomain.Route {
	filtered := make([]modeldomain.Route, 0, len(values))
	scope := key.AccountScope()
	for _, value := range values {
		if scope.AllowsProvider(value.Provider) && key.AllowsModel(value.ID) {
			filtered = append(filtered, value)
		}
	}
	return filtered
}

// newModelListItems deduplicates by downstream public name and hides Provider prefixes used only for internal routing.
func newModelListItems(values []modeldomain.Route) []modelListItem {
	data := make([]modelListItem, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		publicID := modeldomain.ExternalPublicID(value.Provider, value.PublicID)
		if seen[publicID] {
			continue
		}
		seen[publicID] = true
		data = append(data, modelListItem{ID: publicID, Object: "model", Created: value.CreatedAt.Unix(), OwnedBy: "grok2api", Provider: value.Provider, Capability: value.Capability})
	}
	return data
}

// appendReasoningModelAliases expands base models into effort-suffixed aliases using only
// levels each model actually supports (never a blanket none/low/medium/high/xhigh/max template).
func appendReasoningModelAliases(items []modelListItem) []modelListItem {
	if len(items) == 0 {
		return items
	}
	seen := make(map[string]bool, len(items)*2)
	result := make([]modelListItem, 0, len(items)*2)
	for _, item := range items {
		seen[item.ID] = true
		result = append(result, item)
	}
	for _, item := range items {
		for _, aliasID := range modeldomain.ReasoningAliasPublicIDsForProvider(item.Provider, item.ID) {
			if seen[aliasID] {
				continue
			}
			seen[aliasID] = true
			result = append(result, modelListItem{
				ID: aliasID, Object: "model", Created: item.Created, OwnedBy: item.OwnedBy,
				Provider: item.Provider, Capability: item.Capability,
			})
		}
	}
	return result
}

func (h *Handler) createResponse(c *gin.Context) {
	h.handleCreate(c, false)
}

func (h *Handler) compactResponse(c *gin.Context) {
	h.handleCreate(c, true)
}

func (h *Handler) createChatCompletion(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxBodyBytes)
	if !isJSONRequest(c) {
		writeOpenAIError(c, http.StatusUnsupportedMediaType, "invalid_request", "Chat Completions only supports application/json")
		return
	}
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		writeOpenAIError(c, http.StatusRequestEntityTooLarge, "request_too_large", "请求体超过限制")
		return
	}
	var request chatCompletionRequest
	if json.Unmarshal(body, &request) != nil || strings.TrimSpace(request.Model) == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "Chat Completions 请求缺少有效 model")
		return
	}
	clientValue, exists := c.Get(middleware.ClientKey)
	clientKey, ok := clientValue.(clientkeydomain.Key)
	if !exists || !ok {
		writeOpenAIError(c, http.StatusUnauthorized, "invalid_api_key", "客户端 API Key 无效")
		return
	}
	requestID, _ := c.Get(middleware.RequestIDKey)
	requestIDValue, _ := requestID.(string)
	result, err := h.gateway.CreateChatCompletion(c.Request.Context(), gateway.Input{
		RequestID: requestIDValue, ClientKey: clientKey, PublicModel: request.Model,
		Body: body, Streaming: request.Stream, PromptCacheKey: request.PromptCacheKey,
		PromptCacheSeed:           extractPromptCacheSeed(c.Request.Header, body),
		AllowClientToolCacheRoute: allowBuildClientToolCacheRoute(c.Request.Header),
		GrokTurnIndex:             c.GetHeader("x-grok-turn-idx"),
	})
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeResult(c, result, request.Stream, streamProtocolChat)
}

func (h *Handler) createMessage(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxBodyBytes)
	if !isJSONRequest(c) {
		writeAnthropicError(c, http.StatusUnsupportedMediaType, "invalid_request_error", "Messages only supports application/json")
		return
	}
	if strings.TrimSpace(c.GetHeader("anthropic-version")) == "" {
		writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "anthropic-version header is required")
		return
	}
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		writeAnthropicError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body exceeds the configured limit")
		return
	}
	var request messagesRequest
	if json.Unmarshal(body, &request) != nil || strings.TrimSpace(request.Model) == "" || request.MaxTokens == nil || *request.MaxTokens <= 0 || len(bytes.TrimSpace(request.Messages)) == 0 {
		writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "model, max_tokens, and messages are required")
		return
	}
	clientValue, exists := c.Get(middleware.ClientKey)
	clientKey, ok := clientValue.(clientkeydomain.Key)
	if !exists || !ok {
		writeAnthropicError(c, http.StatusUnauthorized, "authentication_error", "invalid API key")
		return
	}
	requestID, _ := c.Get(middleware.RequestIDKey)
	requestIDValue, _ := requestID.(string)
	result, err := h.gateway.CreateMessage(c.Request.Context(), gateway.Input{
		RequestID: requestIDValue, ClientKey: clientKey, PublicModel: request.Model,
		Body: body, Streaming: request.Stream, PromptCacheKey: request.PromptCacheKey,
		PromptCacheSeed:           extractPromptCacheSeed(c.Request.Header, body),
		AllowClientToolCacheRoute: allowBuildClientToolCacheRoute(c.Request.Header),
		GrokTurnIndex:             c.GetHeader("x-grok-turn-idx"),
	})
	if err != nil {
		writeGatewayAnthropicError(c, err)
		return
	}
	h.writeAnthropicResult(c, result, request.Stream)
}

func (h *Handler) generateImage(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxBodyBytes)
	if !isJSONRequest(c) {
		writeOpenAIError(c, http.StatusUnsupportedMediaType, "invalid_request", "图片生成仅支持 application/json")
		return
	}
	var request imageGenerationRequest
	if decodeSingleJSON(c.Request.Body, &request, false) != nil || strings.TrimSpace(request.Model) == "" || strings.TrimSpace(request.Prompt) == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "图片请求缺少有效 model 或 prompt")
		return
	}
	if value := bytes.TrimSpace(request.StorageOptions); len(value) > 0 && !bytes.Equal(value, []byte("null")) {
		writeOpenAIError(c, http.StatusBadRequest, "unsupported_parameter", "当前兼容层暂不支持 storage_options")
		return
	}
	count := 1
	if request.Count != nil {
		if *request.Count < 1 || *request.Count > 10 {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "n 必须在 1 到 10 之间")
			return
		}
		count = *request.Count
	}
	if request.Stream && count != 1 {
		writeImageGenerationUserError(c, "unsupported_parameter", "input", "Streaming is only supported with n=1.")
		return
	}
	partialImages := 0
	if request.PartialImages != nil {
		if *request.PartialImages < 0 || *request.PartialImages > 3 {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "partial_images 必须在 0 到 3 之间")
			return
		}
		partialImages = *request.PartialImages
		if partialImages > 0 && !request.Stream {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "partial_images 仅可在 stream=true 时使用")
			return
		}
	}
	quality := strings.ToLower(strings.TrimSpace(request.Quality))
	if quality != "" && quality != "low" && quality != "medium" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "quality 必须是 low 或 medium")
		return
	}
	clientKey, requestID, ok := requestIdentity(c)
	if !ok {
		return
	}
	result, err := h.gateway.GenerateImage(c.Request.Context(), gateway.ImageGenerationInput{
		RequestID: requestID, ClientKey: clientKey, PublicModel: request.Model, Prompt: request.Prompt,
		Count: count, Size: request.Size, AspectRatio: request.AspectRatio,
		Resolution: request.Resolution, Quality: quality, ResponseFormat: request.ResponseFormat,
		Streaming: request.Stream, PartialImages: partialImages,
	})
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeResult(c, result, request.Stream, streamProtocolImage)
}

func (h *Handler) writeMediaResult(c *gin.Context, result *gateway.Result) {
	errorCode := ""
	defer result.Body.Close()
	defer func() { result.Finalize(gateway.Usage{}, "", errorCode) }()
	if isUpstreamCredentialStatus(result.StatusCode) {
		errorCode = "upstream_unavailable"
		clientCode := readCredentialErrorCode(result.StatusCode, result.Body)
		writeOpenAIError(c, http.StatusServiceUnavailable, clientCode, credentialErrorMessage(clientCode))
		return
	}
	if result.StatusCode < http.StatusOK || (result.StatusCode >= http.StatusMultipleChoices && result.StatusCode < http.StatusBadRequest) {
		errorCode = "invalid_upstream_status"
		writeOpenAIError(c, http.StatusBadGateway, "invalid_upstream_response", "上游媒体服务返回了不安全的重定向响应")
		return
	}
	contentType, safeContentType := normalizeMediaResponseContentType(result.Header.Get("Content-Type"))
	if !safeContentType {
		errorCode = "unsafe_media_content_type"
		writeOpenAIError(c, http.StatusBadGateway, "invalid_media_type", "上游媒体服务返回了不受支持的内容类型")
		return
	}
	contentLength, contentLengthErr := strconv.ParseInt(result.Header.Get("Content-Length"), 10, 64)
	if contentLengthErr == nil && contentLength > maxMediaResponseTransferBytes {
		errorCode = "response_too_large"
		writeOpenAIError(c, http.StatusBadGateway, "media_too_large", "上游媒体超过 2 GiB 安全上限")
		return
	}
	setSafeMediaResponseHeaders(c, result.Header)
	if contentLengthErr == nil && contentLength >= 0 {
		c.Header("Content-Length", strconv.FormatInt(contentLength, 10))
	} else {
		c.Header("Trailer", mediaTransferErrorTrailer)
	}
	if err := writeMediaBody(c, result.Body, contentType, result.StatusCode, maxMediaResponseTransferBytes); err != nil {
		if errors.Is(err, errResponseTransferLimit) {
			errorCode = "response_too_large"
		} else {
			errorCode = "stream_interrupted"
		}
		if contentLengthErr != nil {
			c.Header(mediaTransferErrorTrailer, errorCode)
		}
	}
}

func normalizeMediaResponseContentType(value string) (string, bool) {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(value))
	if err != nil {
		return "", false
	}
	mediaType = strings.ToLower(mediaType)
	switch mediaType {
	case "application/json":
		return "application/json; charset=utf-8", true
	case "text/plain":
		return "text/plain; charset=utf-8", true
	case "application/ogg":
		return mediaType, true
	}
	if strings.HasPrefix(mediaType, "audio/") {
		switch mediaType {
		case "audio/aac", "audio/flac", "audio/l16", "audio/mpeg", "audio/mp3", "audio/ogg", "audio/opus", "audio/pcm", "audio/wav", "audio/webm", "audio/x-flac", "audio/x-wav":
			return mediaType, true
		}
	}
	return "", false
}

func setSafeMediaResponseHeaders(c *gin.Context, upstream http.Header) {
	c.Header("Cache-Control", "private, no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Security-Policy", "default-src 'none'; sandbox")
	c.Header("Referrer-Policy", "no-referrer")
	for _, name := range []string{"Retry-After", "X-Request-Id"} {
		if value := strings.TrimSpace(upstream.Get(name)); value != "" {
			c.Header(name, value)
		}
	}
}

func setResponseWriteDeadline(writer http.ResponseWriter) error {
	err := http.NewResponseController(writer).SetWriteDeadline(time.Now().Add(responseWriteTimeout))
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

// writeMediaBody binds the validated non-HTML content type before emitting the
// response body, so no caller can stream media bytes without their MIME context.
func writeMediaBody(c *gin.Context, source io.Reader, contentType string, statusCode int, limit int64) error {
	c.Header("Content-Type", contentType)
	c.Status(statusCode)
	buffer := make([]byte, 64<<10)
	var transferred int64
	for {
		n, readErr := source.Read(buffer)
		if n > 0 {
			remaining := limit - transferred
			if remaining <= 0 {
				return errResponseTransferLimit
			}
			writeSize := n
			if int64(writeSize) > remaining {
				writeSize = int(remaining)
			}
			if err := setResponseWriteDeadline(c.Writer); err != nil {
				return err
			}
			written, writeErr := c.Writer.Write(buffer[:writeSize])
			transferred += int64(written)
			if writeErr != nil {
				return writeErr
			}
			if written != writeSize {
				return io.ErrShortWrite
			}
			if writeSize != n {
				return errResponseTransferLimit
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

func (h *Handler) editImage(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxBodyBytes)
	if !isJSONRequest(c) {
		writeOpenAIError(c, http.StatusUnsupportedMediaType, "invalid_request", "图片编辑仅支持 application/json")
		return
	}
	var request imageEditJSONRequest
	if err := decodeSingleJSON(c.Request.Body, &request, false); err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "图片编辑 JSON 请求无效")
		return
	}
	if value := bytes.TrimSpace(request.StorageOptions); len(value) > 0 && !bytes.Equal(value, []byte("null")) {
		writeOpenAIError(c, http.StatusBadRequest, "unsupported_parameter", "当前兼容层暂不支持 storage_options")
		return
	}
	model := strings.TrimSpace(request.Model)
	prompt := strings.TrimSpace(request.Prompt)
	count := 1
	if request.Count != nil {
		count = *request.Count
	}
	inputs := append([]imageEditJSONImage(nil), request.Images...)
	if request.Image != nil {
		inputs = append([]imageEditJSONImage{*request.Image}, inputs...)
	}
	if len(inputs) == 0 || len(inputs) > 8 {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "image 或 images 数量必须在 1 到 8 之间")
		return
	}
	imageURLs := make([]string, 0, len(inputs))
	for _, input := range inputs {
		if strings.TrimSpace(input.FileID) != "" {
			writeOpenAIError(c, http.StatusBadRequest, "unsupported_parameter", "当前暂不支持 image.file_id，请使用 image.url")
			return
		}
		if value := strings.TrimSpace(input.URL); value != "" {
			imageURLs = append(imageURLs, value)
		}
	}
	if len(imageURLs) != len(inputs) {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "每个 image 都必须提供有效 url")
		return
	}
	if model == "" || prompt == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "图片编辑缺少有效 model 或 prompt")
		return
	}
	if count < 1 || count > 10 {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "n 必须在 1 到 10 之间")
		return
	}
	partialImages := 0
	if request.PartialImages != nil {
		if *request.PartialImages < 0 || *request.PartialImages > 3 {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "partial_images 必须在 0 到 3 之间")
			return
		}
		partialImages = *request.PartialImages
		if partialImages > 0 && !request.Stream {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "partial_images 仅可在 stream=true 时使用")
			return
		}
	}
	aspectRatio := strings.ToLower(strings.TrimSpace(request.AspectRatio))
	size := strings.ToLower(strings.TrimSpace(request.Size))
	if aspectRatio != "" && !validImageAspectRatio(aspectRatio) {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "aspect_ratio 不受支持")
		return
	}
	if size != "" && !validImageEditSize(size) {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "size 必须是 auto、1024x1024、1024x1536 或 1536x1024")
		return
	}
	resolution := strings.ToLower(strings.TrimSpace(request.Resolution))
	if resolution == "" {
		resolution = "1k"
	}
	if resolution != "1k" && resolution != "2k" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "resolution 必须是 1k 或 2k")
		return
	}
	quality := strings.ToLower(strings.TrimSpace(request.Quality))
	if quality != "" && quality != "low" && quality != "medium" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "quality 必须是 low 或 medium")
		return
	}
	clientKey, requestID, ok := requestIdentity(c)
	if !ok {
		return
	}
	result, err := h.gateway.EditImage(c.Request.Context(), gateway.ImageEditInput{
		RequestID: requestID, ClientKey: clientKey, PublicModel: model, Prompt: prompt,
		ImageURLs: imageURLs, Count: count, Size: size, AspectRatio: aspectRatio,
		Resolution: resolution, Quality: quality, ResponseFormat: request.ResponseFormat,
		Streaming: request.Stream, PartialImages: partialImages,
	})
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeResult(c, result, request.Stream, streamProtocolImage)
}

func requestIdentity(c *gin.Context) (clientkeydomain.Key, string, bool) {
	clientValue, exists := c.Get(middleware.ClientKey)
	clientKey, ok := clientValue.(clientkeydomain.Key)
	if !exists || !ok {
		writeOpenAIError(c, http.StatusUnauthorized, "invalid_api_key", "客户端 API Key 无效")
		return clientkeydomain.Key{}, "", false
	}
	requestID, _ := c.Get(middleware.RequestIDKey)
	requestIDValue, _ := requestID.(string)
	return clientKey, requestIDValue, true
}

func (h *Handler) generateVideo(c *gin.Context) {
	h.handleVideoCreate(c, gatewayVideoOperationGenerate, "视频生成")
}

func (h *Handler) editVideo(c *gin.Context) {
	h.handleVideoCreate(c, gatewayVideoOperationEdit, "视频编辑")
}

func (h *Handler) extendVideo(c *gin.Context) {
	h.handleVideoCreate(c, gatewayVideoOperationExtend, "视频延长")
}

const (
	gatewayVideoOperationGenerate = "generate"
	gatewayVideoOperationEdit     = "edit"
	gatewayVideoOperationExtend   = "extend"
)

func (h *Handler) handleVideoCreate(c *gin.Context, operation, label string) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxBodyBytes)
	if !isJSONRequest(c) {
		writeOpenAIError(c, http.StatusUnsupportedMediaType, "invalid_request", label+"仅支持 application/json")
		return
	}
	var request videoGenerationRequest
	if err := decodeSingleJSON(c.Request.Body, &request, true); err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", label+" JSON 请求无效: "+err.Error())
		return
	}
	if hasJSONValue(request.Output) {
		writeOpenAIError(c, http.StatusBadRequest, "unsupported_parameter", "当前兼容层暂不支持 output.upload_url")
		return
	}
	if hasJSONValue(request.StorageOptions) {
		writeOpenAIError(c, http.StatusBadRequest, "unsupported_parameter", "当前兼容层暂不支持 storage_options")
		return
	}
	model := strings.TrimSpace(request.Model)
	prompt := strings.TrimSpace(request.Prompt)
	if model == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", label+"缺少有效 model")
		return
	}
	parseVideoImage := func(input videoGenerationImage, field string) (string, bool) {
		urlValue := strings.TrimSpace(input.URL)
		fileID := strings.TrimSpace(input.FileID)
		if (urlValue == "") == (fileID == "") {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", field+" 必须且只能提供 url 或 file_id")
			return "", false
		}
		if fileID != "" {
			if !mediadomain.IsInputAssetID(fileID) {
				writeOpenAIError(c, http.StatusBadRequest, "invalid_request", field+".file_id 无效")
				return "", false
			}
			return gateway.VideoInputFileReference(fileID), true
		}
		return urlValue, true
	}

	duration := 0
	aspectRatio := ""
	resolution := ""
	imageURL := ""
	referenceURLs := []string{}
	referenceAudios := []string{}
	videoURL := ""

	if operation == gatewayVideoOperationGenerate {
		var err error
		duration, err = parseVideoDuration(request.Duration)
		if err != nil {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		aspectRatio = strings.TrimSpace(request.AspectRatio)
		if aspectRatio == "" {
			aspectRatio = "16:9"
		}
		if !validVideoAspectRatio(aspectRatio) {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "aspect_ratio 必须是 1:1、16:9、9:16、4:3、3:4、3:2 或 2:3")
			return
		}
		resolution = strings.ToLower(strings.TrimSpace(request.Resolution))
		if resolution == "" {
			resolution = "720p"
		}
		if resolution != "480p" && resolution != "720p" && resolution != "1080p" {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "resolution 必须是 480p、720p 或 1080p")
			return
		}
		if request.Image != nil {
			value, ok := parseVideoImage(*request.Image, "image")
			if !ok {
				return
			}
			imageURL = value
		}
		referenceURLs = make([]string, 0, len(request.ReferenceImages))
		for _, input := range request.ReferenceImages {
			value, ok := parseVideoImage(input, "reference_images")
			if !ok {
				return
			}
			referenceURLs = append(referenceURLs, value)
		}
		referenceAudios = make([]string, 0, len(request.ReferenceAudios))
		for i, input := range request.ReferenceAudios {
			voiceID := strings.TrimSpace(input.VoiceID)
			if voiceID == "" {
				writeOpenAIError(c, http.StatusBadRequest, "invalid_request", fmt.Sprintf("reference_audios[%d].voice_id 不能为空", i))
				return
			}
			referenceAudios = append(referenceAudios, voiceID)
		}
		if len(referenceAudios) > 3 {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "reference_audios 最多 3 个")
			return
		}
		if imageURL != "" && (len(referenceURLs) > 0 || len(referenceAudios) > 0) {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "image 不能与 reference_images/reference_audios 同时使用")
			return
		}
		if len(referenceURLs) > mediadomain.MaxInputImages {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", fmt.Sprintf("reference_images 不能超过 %d 张", mediadomain.MaxInputImages))
			return
		}
		hasReferenceMode := len(referenceURLs) > 0 || len(referenceAudios) > 0
		if hasReferenceMode {
			if prompt == "" {
				writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "参考图/参考音频视频必须提供 prompt")
				return
			}
			if resolution == "1080p" {
				writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "参考图视频 resolution 最高 720p")
				return
			}
		}
		if prompt == "" && imageURL == "" && !hasReferenceMode {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "文本生视频必须提供 prompt；图片生视频可以省略 prompt")
			return
		}
		if request.Video != nil {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "视频生成不支持 video 输入")
			return
		}
	} else {
		if prompt == "" {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", label+"必须提供 prompt")
			return
		}
		if request.Video == nil {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", label+"必须提供 video")
			return
		}
		value, ok := parseVideoImage(*request.Video, "video")
		if !ok {
			return
		}
		videoURL = value
		if request.Image != nil || len(request.ReferenceImages) > 0 || len(request.ReferenceAudios) > 0 {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", label+"不支持 image、reference_images 或 reference_audios")
			return
		}
		if strings.TrimSpace(request.AspectRatio) != "" || strings.TrimSpace(request.Resolution) != "" {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", label+"不支持 aspect_ratio 或 resolution")
			return
		}
		if operation == gatewayVideoOperationEdit {
			if hasJSONValue(request.Duration) {
				writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "视频编辑不支持 duration")
				return
			}
		} else {
			// extend: duration optional, default 6, range 2-10
			if hasJSONValue(request.Duration) {
				var err error
				duration, err = parseVideoDuration(request.Duration)
				if err != nil {
					writeOpenAIError(c, http.StatusBadRequest, "invalid_request", err.Error())
					return
				}
			} else {
				duration = 6
			}
			if duration < 2 || duration > 10 {
				writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "视频延长 duration 必须在 2 到 10 秒之间")
				return
			}
		}
	}

	clientKey, requestID, ok := requestIdentity(c)
	if !ok {
		return
	}
	var op provider.VideoOperation
	switch operation {
	case gatewayVideoOperationEdit:
		op = provider.VideoOperationEdit
	case gatewayVideoOperationExtend:
		op = provider.VideoOperationExtend
	default:
		op = provider.VideoOperationGenerate
	}
	job, err := h.gateway.CreateVideo(c.Request.Context(), gateway.VideoInput{
		RequestID: requestID, ClientKey: clientKey, PublicModel: model,
		Operation: op,
		Prompt:    prompt, Duration: duration, AspectRatio: aspectRatio, Resolution: resolution,
		ImageURL: imageURL, ReferenceURLs: referenceURLs, ReferenceAudios: referenceAudios, VideoURL: videoURL,
	})
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"request_id": job.ID})
}

func (h *Handler) getVideo(c *gin.Context) {
	clientKey, _, ok := requestIdentity(c)
	if !ok {
		return
	}
	job, err := h.gateway.GetVideo(c.Request.Context(), strings.TrimSpace(c.Param("requestId")), clientKey)
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	c.JSON(http.StatusOK, videoGenerationResponse(job, h.videoPlaybackURL(job)))
}

func (h *Handler) publicURL(path string) string {
	baseURL := h.publicAPIBaseURL
	if h.publicBaseURL != nil {
		baseURL = strings.TrimRight(strings.TrimSpace(h.publicBaseURL()), "/")
	}
	if baseURL == "" {
		return path
	}
	return baseURL + path
}

func (h *Handler) videoContentURL(jobID string) string {
	return h.publicURL("/v1/videos/" + url.PathEscape(jobID) + "/content")
}

// videoPlaybackURL prefers the stored asset served by the public media route, so the
// returned link opens directly in browsers and players. /v1/videos/{id}/content needs
// the client API key, which makes the URL unusable outside an authenticated client.
// Images already return their public media URL; this keeps video consistent. Jobs
// without a stored asset keep the protected content endpoint.
func (h *Handler) videoPlaybackURL(job mediadomain.Job) string {
	if assetID := strings.TrimSpace(job.ResultAssetID); assetID != "" {
		return h.publicURL("/v1/media/videos/" + url.PathEscape(assetID))
	}
	return h.videoContentURL(job.ID)
}

func (h *Handler) getVideoContent(c *gin.Context) {
	clientKey, _, ok := requestIdentity(c)
	if !ok {
		return
	}
	body, contentType, size, err := h.gateway.OpenVideoContent(c.Request.Context(), strings.TrimSpace(c.Param("requestId")), clientKey)
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	defer func() { _ = body.Close() }()
	writeVideoContent(c, body, contentType, size, strings.TrimSpace(c.Param("requestId")))
}

func writeVideoContent(c *gin.Context, body io.Reader, contentType string, size int64, downloadName string) {
	if size > maxMediaResponseTransferBytes {
		writeOpenAIError(c, http.StatusBadGateway, "media_too_large", "上游媒体超过 2 GiB 安全上限")
		return
	}
	contentType, ok := normalizeVideoResponseContentType(contentType)
	if !ok {
		writeOpenAIError(c, http.StatusBadGateway, "invalid_media_type", "上游视频服务返回了不受支持的内容类型")
		return
	}
	// Clients that save the response need an extension to get a playable file.
	c.Header("Content-Disposition", mediafile.VideoContentDisposition(downloadName, contentType))
	c.Header("Cache-Control", "private, no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Security-Policy", "default-src 'none'; sandbox")
	c.Header("Referrer-Policy", "no-referrer")
	if size >= 0 {
		c.Header("Content-Length", strconv.FormatInt(size, 10))
	} else {
		c.Header("Trailer", mediaTransferErrorTrailer)
	}
	if err := writeMediaBody(c, body, contentType, http.StatusOK, maxMediaResponseTransferBytes); err != nil && size < 0 {
		errorCode := "stream_interrupted"
		if errors.Is(err, errResponseTransferLimit) {
			errorCode = "response_too_large"
		}
		c.Header(mediaTransferErrorTrailer, errorCode)
	}
}

func normalizeVideoResponseContentType(value string) (string, bool) {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(value))
	if err != nil {
		return "", false
	}
	switch strings.ToLower(mediaType) {
	case "video/mp4", "video/quicktime", "video/webm":
		return strings.ToLower(mediaType), true
	default:
		return "", false
	}
}

func parseVideoDuration(durationRaw json.RawMessage) (int, error) {
	duration, hasDuration, err := parseOptionalVideoInteger(durationRaw)
	if err != nil {
		return 0, fmt.Errorf("duration 必须是整数或整数字符串")
	}
	value := 8
	if hasDuration {
		value = duration
	}
	if value < 1 || value > 15 {
		return 0, fmt.Errorf("duration 必须在 1 到 15 秒之间")
	}
	return value, nil
}

func parseOptionalVideoInteger(raw json.RawMessage) (int, bool, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, false, nil
	}
	var number int
	if json.Unmarshal(raw, &number) != nil {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return 0, true, errors.New("必须是整数或整数字符串")
		}
		parsed, err := strconv.Atoi(strings.TrimSpace(text))
		if err != nil {
			return 0, true, errors.New("必须是整数或整数字符串")
		}
		number = parsed
	}
	return number, true, nil
}

func hasJSONValue(value json.RawMessage) bool {
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

func validVideoAspectRatio(value string) bool {
	switch value {
	case "1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3":
		return true
	default:
		return false
	}
}

func validImageAspectRatio(value string) bool {
	switch value {
	case "auto", "1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "2:1", "1:2", "19.5:9", "9:19.5", "20:9", "9:20":
		return true
	default:
		return false
	}
}

func validImageEditSize(value string) bool {
	switch value {
	case "auto", "1024x1024", "1024x1536", "1536x1024":
		return true
	default:
		return false
	}
}

func videoGenerationResponse(job mediadomain.Job, contentURLs ...string) gin.H {
	switch job.Status {
	case mediadomain.StatusCompleted:
		videoURL := job.UpstreamURL
		if len(contentURLs) > 0 && contentURLs[0] != "" {
			videoURL = contentURLs[0]
		}
		video := gin.H{"url": videoURL, "respect_moderation": true}
		operation := job.Operation
		if operation == "" {
			operation = mediadomain.VideoOperationGenerate
		}
		if operation == mediadomain.VideoOperationGenerate && job.Seconds > 0 {
			video["duration"] = job.Seconds
		}
		return gin.H{
			"status": "done", "model": job.Model, "progress": 100,
			"video": video,
		}
	case mediadomain.StatusFailed:
		return gin.H{
			"status": "failed",
			"error":  gin.H{"code": officialVideoErrorCode(job.ErrorCode), "message": job.ErrorMessage},
		}
	default:
		return gin.H{"status": "pending", "model": job.Model, "progress": min(99, max(0, job.Progress))}
	}
}

func officialVideoErrorCode(value string) string {
	switch value {
	case "account_unavailable", "provider_unavailable":
		return "service_unavailable"
	case "model_not_found":
		return "invalid_argument"
	default:
		return "internal_error"
	}
}

func (h *Handler) handleCreate(c *gin.Context, compact bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxBodyBytes)
	if !isJSONRequest(c) {
		writeOpenAIError(c, http.StatusUnsupportedMediaType, "invalid_request", "Responses only supports application/json")
		return
	}
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		writeOpenAIError(c, http.StatusRequestEntityTooLarge, "request_too_large", "请求体超过限制")
		return
	}
	var request responsesRequest
	if err := json.Unmarshal(body, &request); err != nil || strings.TrimSpace(request.Model) == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "Responses 请求缺少有效 model")
		return
	}
	if compact {
		body, err = forceJSONBoolean(body, "stream", false)
		if err != nil {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "Compact 请求格式无效")
			return
		}
		request.Stream = false
	}
	clientValue, exists := c.Get(middleware.ClientKey)
	clientKey, ok := clientValue.(clientkeydomain.Key)
	if !exists || !ok {
		writeOpenAIError(c, http.StatusUnauthorized, "invalid_api_key", "客户端 API Key 无效")
		return
	}
	requestID, _ := c.Get(middleware.RequestIDKey)
	requestIDValue, _ := requestID.(string)
	input := gateway.Input{
		RequestID: requestIDValue, ClientKey: clientKey, PublicModel: request.Model,
		Body: body, Streaming: request.Stream, PromptCacheKey: request.PromptCacheKey,
		PromptCacheSeed: extractPromptCacheSeed(c.Request.Header, body), PreviousResponseID: request.PreviousResponseID,
		AllowClientToolCacheRoute: allowBuildClientToolCacheRoute(c.Request.Header),
		GrokTurnIndex:             c.GetHeader("x-grok-turn-idx"),
	}
	var result *gateway.Result
	if compact {
		result, err = h.gateway.CompactResponse(c.Request.Context(), input)
	} else {
		result, err = h.gateway.CreateResponse(c.Request.Context(), input)
	}
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeResult(c, result, request.Stream && !compact, streamProtocolResponses)
}

func isJSONRequest(c *gin.Context) bool {
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	return err == nil && strings.EqualFold(mediaType, "application/json")
}

func decodeSingleJSON(reader io.Reader, target any, disallowUnknown bool) error {
	decoder := json.NewDecoder(reader)
	if disallowUnknown {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("请求体只能包含一个 JSON 对象")
		}
		return err
	}
	return nil
}

func (h *Handler) getResponse(c *gin.Context) {
	h.handleOwnedResource(c, false)
}

func (h *Handler) deleteResponse(c *gin.Context) {
	h.handleOwnedResource(c, true)
}

func (h *Handler) handleOwnedResource(c *gin.Context, deleteResource bool) {
	clientValue, exists := c.Get(middleware.ClientKey)
	clientKey, ok := clientValue.(clientkeydomain.Key)
	if !exists || !ok {
		writeOpenAIError(c, http.StatusUnauthorized, "invalid_api_key", "客户端 API Key 无效")
		return
	}
	input := gateway.ResourceInput{ClientKey: clientKey, ResponseID: strings.TrimSpace(c.Param("responseId")), RawQuery: c.Request.URL.RawQuery}
	if input.ResponseID == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "response_id 不能为空")
		return
	}
	var result *gateway.Result
	var err error
	if deleteResource {
		result, err = h.gateway.DeleteResponse(c.Request.Context(), input)
	} else {
		result, err = h.gateway.GetResponse(c.Request.Context(), input)
	}
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeResult(c, result, false, streamProtocolResponses)
}

func (h *Handler) writeResult(c *gin.Context, result *gateway.Result, stream bool, protocol streamProtocol) {
	h.writeProtocolResult(c, result, stream, false, protocol)
}

func (h *Handler) writeAnthropicResult(c *gin.Context, result *gateway.Result, stream bool) {
	h.writeProtocolResult(c, result, stream, true, streamProtocolAnthropic)
}

func (h *Handler) writeProtocolResult(c *gin.Context, result *gateway.Result, stream, anthropic bool, protocol streamProtocol) {
	usage := gateway.Usage{}
	responseID := ""
	errorCode := ""
	defer result.Body.Close()
	defer func() { result.Finalize(usage, responseID, errorCode) }()
	if isUpstreamCredentialStatus(result.StatusCode) {
		errorCode = "upstream_unavailable"
		clientCode := readCredentialErrorCode(result.StatusCode, result.Body)
		if anthropic {
			writeAnthropicError(c, http.StatusServiceUnavailable, "overloaded_error", credentialErrorMessage(clientCode), clientCode)
		} else {
			writeOpenAIError(c, http.StatusServiceUnavailable, clientCode, credentialErrorMessage(clientCode))
		}
		return
	}
	transferLimit := int64(maxJSONResponseTransferBytes)
	if stream {
		transferLimit = maxStreamResponseTransferBytes
	}
	if contentLength, parseErr := strconv.ParseInt(result.Header.Get("Content-Length"), 10, 64); parseErr == nil && contentLength > transferLimit {
		errorCode = "response_too_large"
		writeOpenAIError(c, http.StatusBadGateway, "response_too_large", "上游响应超过代理安全上限")
		return
	}
	copyHeaders(c.Writer.Header(), result.Header)
	c.Status(result.StatusCode)
	if result.StatusCode >= 400 {
		errorCode = "upstream_error"
	}
	var err error
	if stream {
		metadata, copyErr := copyStream(c.Writer, result.Body, protocol, result.MarkFirstToken)
		usage, responseID, err = metadata.Usage, metadata.ResponseID, copyErr
		if metadata.StreamFailure != nil && result.RecordStreamFailure != nil {
			result.RecordStreamFailure(*metadata.StreamFailure)
		}
	} else {
		metadata, copyErr := copyJSON(c.Writer, result.Body, protocol)
		usage, responseID, err = metadata.Usage, metadata.ResponseID, copyErr
	}
	if err != nil {
		switch {
		case errors.Is(err, errResponseTransferLimit):
			errorCode = "response_too_large"
		case errors.Is(err, errUpstreamStreamFailed):
			errorCode = "upstream_stream_error"
		case errors.Is(err, errUpstreamStreamIncomplete):
			errorCode = "upstream_stream_incomplete"
		case errors.Is(err, neterror.ErrUpstreamStreamIdleTimeout):
			errorCode = "upstream_stream_idle_timeout"
		case errors.Is(err, errUpstreamStreamRead):
			errorCode = "upstream_stream_interrupted"
		default:
			errorCode = "stream_interrupted"
		}
	}
}

type responseMetadata struct {
	Usage                    gateway.Usage
	cacheCreationInputTokens int64
	ResponseID               string
	Model                    string
	StreamFailure            *gateway.StreamFailureDiagnostic
}

func copyStream(writer gin.ResponseWriter, source io.Reader, protocol streamProtocol, onFirstToken func()) (responseMetadata, error) {
	inspector := &responseInspector{protocol: protocol, onFirstToken: onFirstToken}
	buffer := make([]byte, responseCopyBufferBytes)
	transferred := 0
	for {
		n, readErr := source.Read(buffer)
		if n > 0 {
			if transferred+n > maxStreamResponseTransferBytes {
				return inspector.Metadata(), fmt.Errorf("%w: 流式响应超过 %d MiB", errResponseTransferLimit, maxStreamResponseTransferBytes>>20)
			}
			chunk := buffer[:n]
			inspector.Inspect(chunk)
			if err := setResponseWriteDeadline(writer); err != nil {
				return inspector.Metadata(), err
			}
			if _, err := writer.Write(chunk); err != nil {
				return inspector.Metadata(), err
			}
			writer.Flush()
			inspector.markFirstTokenForwarded()
			transferred += n
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				inspector.Finish()
				return inspector.Metadata(), inspector.TerminalError()
			}
			if inspector.terminalSuccess {
				return inspector.Metadata(), nil
			}
			return inspector.Metadata(), fmt.Errorf("%w: %w", errUpstreamStreamRead, readErr)
		}
	}
}

func copyJSON(writer gin.ResponseWriter, source io.Reader, protocol streamProtocol) (responseMetadata, error) {
	buffer := make([]byte, responseCopyBufferBytes)
	metadataBody := make([]byte, 0, responseCopyBufferBytes)
	metadataComplete := true
	transferred := 0
	for {
		n, readErr := source.Read(buffer)
		if n > 0 {
			if transferred+n > maxJSONResponseTransferBytes {
				return responseMetadata{}, fmt.Errorf("%w: 非流式响应超过 %d MiB", errResponseTransferLimit, maxJSONResponseTransferBytes>>20)
			}
			chunk := buffer[:n]
			if err := setResponseWriteDeadline(writer); err != nil {
				return responseMetadata{}, err
			}
			if _, err := writer.Write(chunk); err != nil {
				return responseMetadata{}, err
			}
			transferred += n
			if metadataComplete {
				if len(metadataBody)+len(chunk) <= maxJSONMetadataInspectionBytes {
					metadataBody = append(metadataBody, chunk...)
				} else {
					metadataBody = nil
					metadataComplete = false
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if metadataComplete {
					return normalizeMetadataUsage(extractMetadata(metadataBody), protocol), nil
				}
				return responseMetadata{}, nil
			}
			return responseMetadata{}, readErr
		}
	}
}

type responseInspector struct {
	protocol        streamProtocol
	pending         []byte
	metadata        responseMetadata
	onFirstToken    func()
	firstTokenSeen  bool
	firstTokenReady bool
	terminalSuccess bool
	terminalFailure bool
}

func (i *responseInspector) Inspect(chunk []byte) {
	i.pending = append(i.pending, chunk...)
	for {
		index := bytes.IndexByte(i.pending, '\n')
		if index < 0 {
			if len(i.pending) > maxStreamEventInspectionBytes {
				i.pending = nil
			}
			return
		}
		line := bytes.TrimSpace(i.pending[:index])
		i.pending = i.pending[index+1:]
		if bytes.HasPrefix(line, []byte("data:")) {
			value := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			i.observeFirstToken(value)
			i.observeTerminal(value)
			if !bytes.Equal(value, []byte("[DONE]")) {
				metadata := extractMetadata(value)
				if hasUsageMetadata(metadata.Usage) {
					if metadata.Usage.ResponseModel == "" {
						metadata.Usage.ResponseModel = i.metadata.Model
					}
					i.metadata.Usage = mergeGatewayUsage(i.metadata.Usage, metadata.Usage)
				}
				if metadata.ResponseID != "" {
					i.metadata.ResponseID = metadata.ResponseID
				}
				if metadata.Model != "" {
					i.metadata.Model = metadata.Model
					i.metadata.Usage.ResponseModel = metadata.Model
				}
				if metadata.cacheCreationInputTokens > 0 {
					i.metadata.cacheCreationInputTokens = metadata.cacheCreationInputTokens
				}
			}
		}
	}
}

func (i *responseInspector) observeFirstToken(data []byte) {
	if i.firstTokenSeen || i.firstTokenReady || i.onFirstToken == nil || len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return
	}
	if !containsGeneratedDelta(data, i.protocol) {
		return
	}
	i.firstTokenReady = true
}

func (i *responseInspector) markFirstTokenForwarded() {
	if i.firstTokenSeen || !i.firstTokenReady || i.onFirstToken == nil {
		return
	}
	i.firstTokenReady = false
	i.firstTokenSeen = true
	i.onFirstToken()
	i.onFirstToken = nil
}

func containsGeneratedDelta(data []byte, protocol streamProtocol) bool {
	switch protocol {
	case streamProtocolResponses:
		var event struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		}
		if json.Unmarshal(data, &event) != nil || event.Delta == "" {
			return false
		}
		switch event.Type {
		case "response.output_text.delta", "response.reasoning_summary_text.delta", "response.reasoning_text.delta", "response.refusal.delta", "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
			return true
		}
	case streamProtocolChat:
		var event struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					Reasoning        string `json:"reasoning"`
					ReasoningContent string `json:"reasoning_content"`
					Refusal          string `json:"refusal"`
					ToolCalls        []struct {
						Function struct {
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal(data, &event) != nil {
			return false
		}
		for _, choice := range event.Choices {
			delta := choice.Delta
			if delta.Content != "" || delta.Reasoning != "" || delta.ReasoningContent != "" || delta.Refusal != "" {
				return true
			}
			for _, call := range delta.ToolCalls {
				if call.Function.Arguments != "" {
					return true
				}
			}
		}
	case streamProtocolAnthropic:
		var event struct {
			Type  string `json:"type"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal(data, &event) != nil || event.Type != "content_block_delta" {
			return false
		}
		switch event.Delta.Type {
		case "text_delta":
			return event.Delta.Text != ""
		case "thinking_delta":
			return event.Delta.Thinking != ""
		case "input_json_delta":
			return event.Delta.PartialJSON != ""
		}
	}
	return false
}

func (i *responseInspector) Metadata() responseMetadata {
	return normalizeMetadataUsage(i.metadata, i.protocol)
}

func normalizeMetadataUsage(metadata responseMetadata, protocol streamProtocol) responseMetadata {
	if protocol != streamProtocolAnthropic {
		return metadata
	}
	inputTokens := saturatingUsageSum(metadata.Usage.InputTokens, metadata.Usage.CachedInputTokens, metadata.cacheCreationInputTokens)
	metadata.Usage.InputTokens = inputTokens
	metadata.Usage.TotalTokens = saturatingUsageSum(inputTokens, metadata.Usage.OutputTokens)
	return metadata
}

func saturatingUsageSum(values ...int64) int64 {
	var total int64
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if value > math.MaxInt64-total {
			return math.MaxInt64
		}
		total += value
	}
	return total
}

func (i *responseInspector) TerminalError() error {
	if i.terminalFailure {
		return errUpstreamStreamFailed
	}
	if !i.terminalSuccess {
		return errUpstreamStreamIncomplete
	}
	return nil
}

func (i *responseInspector) observeTerminal(data []byte) {
	if bytes.Equal(data, []byte("[DONE]")) {
		if i.protocol == streamProtocolChat {
			i.terminalSuccess = true
		}
		return
	}
	var payload struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return
	}
	switch i.protocol {
	case streamProtocolResponses:
		switch payload.Type {
		case "response.completed":
			i.terminalSuccess = true
		case "response.failed", "response.incomplete", "response.error", "error":
			i.markTerminalFailure(data)
		}
	case streamProtocolChat:
		if payload.Type == "error" {
			i.markTerminalFailure(data)
		}
	case streamProtocolAnthropic:
		switch payload.Type {
		case "message_stop":
			i.terminalSuccess = true
		case "error":
			i.markTerminalFailure(data)
		}
	case streamProtocolImage:
		switch payload.Type {
		case "image_generation.completed":
			i.terminalSuccess = true
		case "image_generation.failed", "error":
			i.markTerminalFailure(data)
		}
	}
}

func (i *responseInspector) markTerminalFailure(data []byte) {
	i.terminalFailure = true
	if i.metadata.StreamFailure != nil {
		return
	}
	diagnostic := projectStreamFailureDiagnostic(data)
	if len(diagnostic.Body) > 0 {
		i.metadata.StreamFailure = &diagnostic
	}
}

func projectStreamFailureDiagnostic(data []byte) gateway.StreamFailureDiagnostic {
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil {
		return gateway.StreamFailureDiagnostic{}
	}
	projected := make(map[string]json.RawMessage)
	copySafeDiagnosticFields(projected, root, "type", "status", "code", "message", "param")
	if raw := projectSafeErrorValue(root["error"]); len(raw) > 0 {
		projected["error"] = raw
	}
	if responseRaw := root["response"]; len(responseRaw) > 0 {
		var response map[string]json.RawMessage
		if json.Unmarshal(responseRaw, &response) == nil {
			safeResponse := make(map[string]json.RawMessage)
			copySafeDiagnosticFields(safeResponse, response, "id", "status", "code", "message")
			if raw := projectSafeErrorValue(response["error"]); len(raw) > 0 {
				safeResponse["error"] = raw
			}
			if raw := projectSafeErrorValue(response["incomplete_details"]); len(raw) > 0 {
				safeResponse["incomplete_details"] = raw
			}
			if len(safeResponse) > 0 {
				if encoded, err := json.Marshal(safeResponse); err == nil {
					projected["response"] = encoded
				}
			}
		}
	}
	if len(projected) == 0 {
		return gateway.StreamFailureDiagnostic{}
	}
	encoded, err := json.Marshal(projected)
	if err != nil {
		return gateway.StreamFailureDiagnostic{}
	}
	diagnostic := gateway.StreamFailureDiagnostic{Body: encoded}
	if len(diagnostic.Body) > maxStreamFailureDiagnosticBytes {
		bounded := diagnostic.Body[:maxStreamFailureDiagnosticBytes]
		for len(bounded) > 0 && !utf8.Valid(bounded) {
			bounded = bounded[:len(bounded)-1]
		}
		diagnostic.Body = append([]byte(nil), bounded...)
		diagnostic.BodyTruncated = true
	} else {
		diagnostic.Body = append([]byte(nil), diagnostic.Body...)
	}
	return diagnostic
}

func copySafeDiagnosticFields(destination, source map[string]json.RawMessage, fields ...string) {
	for _, field := range fields {
		if raw := projectSafeScalar(source[field]); len(raw) > 0 {
			destination[field] = raw
		}
	}
}

func projectSafeErrorValue(raw json.RawMessage) json.RawMessage {
	if scalar := projectSafeScalar(raw); len(scalar) > 0 {
		return scalar
	}
	var value map[string]json.RawMessage
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	projected := make(map[string]json.RawMessage)
	copySafeDiagnosticFields(projected, value, "type", "status", "code", "message", "param", "reason")
	if len(projected) == 0 {
		return nil
	}
	encoded, err := json.Marshal(projected)
	if err != nil {
		return nil
	}
	return encoded
}

func projectSafeScalar(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	switch value.(type) {
	case nil, string, bool, float64:
		return append(json.RawMessage(nil), raw...)
	default:
		return nil
	}
}

func (i *responseInspector) Finish() {
	if len(i.pending) == 0 {
		return
	}
	i.pending = append(i.pending, '\n')
	i.Inspect(nil)
}

func extractMetadata(data []byte) responseMetadata {
	var root responsePayloadDTO
	if json.Unmarshal(data, &root) != nil {
		return responseMetadata{}
	}
	metadata := responseMetadata{ResponseID: root.ID, Model: root.Model}
	usage := root.Usage
	if root.Response != nil {
		if metadata.ResponseID == "" {
			metadata.ResponseID = root.Response.ID
		}
		if metadata.Model == "" {
			metadata.Model = root.Response.Model
		}
		if usage == nil {
			usage = root.Response.Usage
		}
	}
	if usage == nil {
		return metadata
	}
	metadata.Usage = usage.toGatewayUsage(metadata.Model)
	metadata.cacheCreationInputTokens = usage.CacheCreationInputTokens
	return metadata
}

type responsePayloadDTO struct {
	ID       string              `json:"id"`
	Model    string              `json:"model"`
	Usage    *responseUsageDTO   `json:"usage"`
	Response *responsePayloadDTO `json:"response"`
}

type responseUsageDTO struct {
	InputTokens            int64 `json:"input_tokens"`
	InputTokensCamel       int64 `json:"inputTokens"`
	OutputTokens           int64 `json:"output_tokens"`
	OutputTokensCamel      int64 `json:"outputTokens"`
	TotalTokens            int64 `json:"total_tokens"`
	TotalTokensCamel       int64 `json:"totalTokens"`
	CostInUSDTicks         int64 `json:"cost_in_usd_ticks"`
	NumSourcesUsed         int64 `json:"num_sources_used"`
	NumServerSideToolsUsed int64 `json:"num_server_side_tools_used"`
	// Responses protocol: input_tokens_details.cached_tokens
	InputTokensDetails responseInputDetailsDTO `json:"input_tokens_details"`
	// OpenAI Chat Completions protocol: prompt_tokens_details.cached_tokens
	PromptTokensDetails responseInputDetailsDTO `json:"prompt_tokens_details"`
	// Anthropic Messages protocol: top-level cache_read_input_tokens
	CacheReadInputTokens     int64                    `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64                    `json:"cache_creation_input_tokens"`
	OutputTokensDetails      responseOutputDetailsDTO `json:"output_tokens_details"`
	// OpenAI Chat Completions protocol: completion_tokens_details.reasoning_tokens
	CompletionTokensDetails responseOutputDetailsDTO  `json:"completion_tokens_details"`
	ContextDetails          responseContextDetailsDTO `json:"context_details"`
	PromptTokens            int64                     `json:"prompt_tokens"`
	CompletionTokens        int64                     `json:"completion_tokens"`
}

type responseInputDetailsDTO struct {
	CachedTokens int64 `json:"cached_tokens"`
}

type responseOutputDetailsDTO struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
	ThinkingTokens  int64 `json:"thinking_tokens"`
}

type responseContextDetailsDTO struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

func (value responseUsageDTO) toGatewayUsage(responseModel string) gateway.Usage {
	input := value.InputTokens
	if input == 0 {
		input = value.InputTokensCamel
	}
	if input == 0 {
		input = value.PromptTokens
	}
	output := value.OutputTokens
	if output == 0 {
		output = value.OutputTokensCamel
	}
	if output == 0 {
		output = value.CompletionTokens
	}
	total := value.TotalTokens
	if total == 0 {
		total = value.TotalTokensCamel
	}
	if total == 0 {
		total = input + output
	}
	// Unified cache hits: Responses / Chat Completions / Anthropic Messages
	cached := value.InputTokensDetails.CachedTokens
	if cached == 0 {
		cached = value.PromptTokensDetails.CachedTokens
	}
	if cached == 0 {
		cached = value.CacheReadInputTokens
	}
	reasoning := value.OutputTokensDetails.ReasoningTokens
	if reasoning == 0 {
		reasoning = value.CompletionTokensDetails.ReasoningTokens
	}
	if reasoning == 0 {
		reasoning = value.OutputTokensDetails.ThinkingTokens
	}
	return gateway.Usage{
		Reported:    true,
		InputTokens: input, CachedInputTokens: cached,
		OutputTokens: output, ReasoningTokens: reasoning,
		TotalTokens: total, CostInUSDTicks: value.CostInUSDTicks,
		NumSourcesUsed: value.NumSourcesUsed, NumServerSideToolsUsed: value.NumServerSideToolsUsed,
		ContextInputTokens: value.ContextDetails.InputTokens, ContextOutputTokens: value.ContextDetails.OutputTokens,
		ResponseModel: responseModel,
	}
}

func hasUsageMetadata(usage gateway.Usage) bool {
	return usage.Reported || usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.TotalTokens > 0 ||
		usage.CachedInputTokens > 0 || usage.ReasoningTokens > 0 || usage.CostInUSDTicks > 0 ||
		usage.NumSourcesUsed > 0 || usage.NumServerSideToolsUsed > 0 ||
		usage.ContextInputTokens > 0 || usage.ContextOutputTokens > 0
}

// mergeGatewayUsage merges usage from multiple streaming frames; non-zero fields overwrite,
// preventing a later partial frame from erasing an already parsed cache hit.
func mergeGatewayUsage(base, next gateway.Usage) gateway.Usage {
	base.Reported = base.Reported || next.Reported
	if next.InputTokens > 0 {
		base.InputTokens = next.InputTokens
	}
	if next.OutputTokens > 0 {
		base.OutputTokens = next.OutputTokens
	}
	if next.TotalTokens > 0 {
		base.TotalTokens = next.TotalTokens
	}
	if next.CachedInputTokens > 0 {
		base.CachedInputTokens = next.CachedInputTokens
	}
	if next.ReasoningTokens > 0 {
		base.ReasoningTokens = next.ReasoningTokens
	}
	if next.CostInUSDTicks > 0 {
		base.CostInUSDTicks = next.CostInUSDTicks
	}
	if next.NumSourcesUsed > 0 {
		base.NumSourcesUsed = next.NumSourcesUsed
	}
	if next.NumServerSideToolsUsed > 0 {
		base.NumServerSideToolsUsed = next.NumServerSideToolsUsed
	}
	if next.ContextInputTokens > 0 {
		base.ContextInputTokens = next.ContextInputTokens
	}
	if next.ContextOutputTokens > 0 {
		base.ContextOutputTokens = next.ContextOutputTokens
	}
	if next.ResponseModel != "" {
		base.ResponseModel = next.ResponseModel
	}
	if base.TotalTokens == 0 && (base.InputTokens > 0 || base.OutputTokens > 0) {
		base.TotalTokens = base.InputTokens + base.OutputTokens
	}
	return base
}

func copyHeaders(destination, source http.Header) {
	excluded := map[string]struct{}{
		"connection": {}, "content-length": {}, "keep-alive": {}, "proxy-authenticate": {},
		"proxy-authorization": {}, "set-cookie": {}, "te": {}, "trailer": {},
		"transfer-encoding": {}, "upgrade": {}, "x-models-etag": {},
	}
	for _, value := range source.Values("Connection") {
		for name := range strings.SplitSeq(value, ",") {
			name = strings.ToLower(strings.TrimSpace(name))
			if name != "" {
				excluded[name] = struct{}{}
			}
		}
	}
	for name, values := range source {
		lower := strings.ToLower(name)
		if _, skip := excluded[lower]; skip {
			continue
		}
		for _, value := range values {
			destination.Add(name, value)
		}
	}
}

func writeOpenAIError(c *gin.Context, status int, code, message string) {
	errorType := "invalid_request_error"
	switch {
	case status == http.StatusUnauthorized:
		errorType = "authentication_error"
	case status == http.StatusTooManyRequests:
		errorType = "rate_limit_error"
	case status >= 500:
		errorType = "server_error"
	}
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"message": message, "type": errorType, "code": code, "param": nil}})
}

func writeImageGenerationUserError(c *gin.Context, code, param, message string) {
	c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{
		"message": message, "type": "image_generation_user_error", "param": param, "code": code,
	}})
}

func writeGatewayError(c *gin.Context, err error) {
	status, code := http.StatusBadGateway, "upstream_unavailable"
	message := "上游服务暂不可用"
	var upstreamFailure *gateway.UpstreamFailure
	var selectionFailure *gateway.SelectionUnavailableError
	switch {
	case errors.Is(err, gateway.ErrLedgerUnavailable):
		status, code = http.StatusServiceUnavailable, "ledger_unavailable"
		message = gateway.ErrLedgerUnavailable.Error()
	case errors.Is(err, clientkeyapp.ErrBillingLimit):
		status, code = http.StatusTooManyRequests, "billing_limit_exceeded"
		message = clientkeyapp.ErrBillingLimit.Error()
	case errors.Is(err, clientkeyapp.ErrModelNotAllowed):
		status, code = http.StatusForbidden, "model_not_allowed"
		message = clientkeyapp.ErrModelNotAllowed.Error()
	case errors.Is(err, gateway.ErrModelNotFound):
		status, code = http.StatusNotFound, "model_not_found"
		message = "模型不存在"
	case errors.Is(err, gateway.ErrResponseNotFound):
		status, code = http.StatusNotFound, "response_not_found"
		message = "Response 不存在或已过期"
	case errors.Is(err, gateway.ErrResponseStateUnsupported), errors.Is(err, gateway.ErrConversationUnsupported):
		status, code = http.StatusBadRequest, "unsupported_parameter"
		message = err.Error()
	case errors.Is(err, gateway.ErrVideoInputTooLarge), errors.Is(err, gateway.ErrVideoInputUnavailable), errors.Is(err, gateway.ErrVideoParameterInvalid):
		status, code = http.StatusBadRequest, "invalid_request"
		message = err.Error()
	case errors.Is(err, gateway.ErrVideoOperationUnsupported):
		status, code = http.StatusBadRequest, "unsupported_model"
		message = err.Error()
	case errors.As(err, &upstreamFailure):
		if isSanitizedUpstreamAvailabilityFailure(upstreamFailure) {
			// Gateway mid-tier behavior: never expose upstream upgrade/billing prompts to clients.
			code = upstreamFailure.ClientCredentialErrorCode()
			if upstreamFailure.QuotaExhausted || upstreamFailure.FreeQuotaExhausted || upstreamFailure.HTTPStatus == http.StatusPaymentRequired {
				code = "upstream_unavailable"
			}
			status, message = http.StatusServiceUnavailable, credentialErrorMessage(code)
		} else {
			status, code, message = upstreamFailure.HTTPStatus, upstreamFailure.Code, upstreamFailure.PublicMessage
		}
		if !isUpstreamCredentialStatus(upstreamFailure.HTTPStatus) && upstreamFailure.RetryAfter > 0 {
			c.Header("Retry-After", strconv.FormatInt(max(1, int64(upstreamFailure.RetryAfter.Round(time.Second)/time.Second)), 10))
		}
	case errors.As(err, &selectionFailure):
		status, code, message = selectionErrorResponse(c, selectionFailure)
	case errors.Is(err, gateway.ErrResponseAccountUnavailable), errors.Is(err, gateway.ErrNoAvailableAccount):
		status, code = http.StatusServiceUnavailable, "upstream_unavailable"
		message = "当前没有可用的上游账号"
	}
	writeOpenAIError(c, status, code, message)
}

func writeGatewayAnthropicError(c *gin.Context, err error) {
	status, errorType := http.StatusBadGateway, "api_error"
	message := "上游服务暂不可用"
	clientCode := ""
	var upstreamFailure *gateway.UpstreamFailure
	var selectionFailure *gateway.SelectionUnavailableError
	switch {
	case errors.Is(err, gateway.ErrLedgerUnavailable):
		status, errorType = http.StatusServiceUnavailable, "overloaded_error"
		message = gateway.ErrLedgerUnavailable.Error()
	case errors.Is(err, clientkeyapp.ErrBillingLimit):
		status, errorType = http.StatusTooManyRequests, "rate_limit_error"
		message = clientkeyapp.ErrBillingLimit.Error()
	case errors.Is(err, clientkeyapp.ErrModelNotAllowed):
		status, errorType, clientCode = http.StatusForbidden, "permission_error", "model_not_allowed"
		message = clientkeyapp.ErrModelNotAllowed.Error()
	case errors.Is(err, gateway.ErrModelNotFound):
		status, errorType = http.StatusNotFound, "not_found_error"
		message = "模型不存在"
	case errors.Is(err, gateway.ErrResponseStateUnsupported), errors.Is(err, gateway.ErrConversationUnsupported):
		status, errorType = http.StatusBadRequest, "invalid_request_error"
		message = err.Error()
	case errors.As(err, &upstreamFailure):
		if isSanitizedUpstreamAvailabilityFailure(upstreamFailure) {
			clientCode = upstreamFailure.ClientCredentialErrorCode()
			if upstreamFailure.QuotaExhausted || upstreamFailure.FreeQuotaExhausted || upstreamFailure.HTTPStatus == http.StatusPaymentRequired {
				clientCode = "upstream_unavailable"
			}
			status, errorType, message = http.StatusServiceUnavailable, "overloaded_error", credentialErrorMessage(clientCode)
		} else {
			status, message = upstreamFailure.HTTPStatus, upstreamFailure.PublicMessage
			if upstreamFailure.Code == "upstream_header_timeout" {
				errorType = "timeout_error"
			}
		}
		if !isUpstreamCredentialStatus(upstreamFailure.HTTPStatus) && upstreamFailure.RetryAfter > 0 {
			c.Header("Retry-After", strconv.FormatInt(max(1, int64(upstreamFailure.RetryAfter.Round(time.Second)/time.Second)), 10))
		}
		if status == http.StatusTooManyRequests {
			errorType = "rate_limit_error"
		}
	case errors.As(err, &selectionFailure):
		status, clientCode, message = selectionErrorResponse(c, selectionFailure)
		if status == http.StatusTooManyRequests {
			errorType = "rate_limit_error"
		} else {
			errorType = "overloaded_error"
		}
	case errors.Is(err, gateway.ErrResponseAccountUnavailable), errors.Is(err, gateway.ErrNoAvailableAccount):
		status, errorType = http.StatusServiceUnavailable, "overloaded_error"
		message = "当前没有可用的上游账号"
	}
	writeAnthropicError(c, status, errorType, message, clientCode)
}

func isUpstreamCredentialStatus(status int) bool {
	// Include 402 so official "add credits / upgrade SuperGrok" bodies never reach clients (Grok CLI, etc.).
	return status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusPaymentRequired
}

func isSanitizedUpstreamAvailabilityFailure(failure *gateway.UpstreamFailure) bool {
	return failure != nil && (isUpstreamCredentialStatus(failure.HTTPStatus) || failure.QuotaExhausted || failure.FreeQuotaExhausted)
}

func selectionErrorResponse(c *gin.Context, failure *gateway.SelectionUnavailableError) (int, string, string) {
	status, code, message := http.StatusServiceUnavailable, "upstream_unavailable", "当前没有可用的上游账号"
	if failure == nil {
		return status, code, message
	}
	status, code = failure.HTTPStatus(), failure.Code()
	if failure.Scope.IsRestricted() {
		message = failure.Error()
	} else {
		switch failure.Reason {
		case gateway.SelectionCooling:
			message = "上游账号正在冷却"
		case gateway.SelectionModelCooling:
			message = "上游账号的目标模型正在冷却"
		case gateway.SelectionQuotaExhausted:
			message = "上游账号额度等待恢复"
		case gateway.SelectionSaturated:
			message = "上游账号当前均达到并发上限"
		case gateway.SelectionUnsupportedModel:
			message = "当前账号池不支持该模型"
		}
	}
	if failure.RetryAfter > 0 {
		seconds := max(int64(1), int64((failure.RetryAfter+time.Second-1)/time.Second))
		c.Header("Retry-After", strconv.FormatInt(seconds, 10))
	}
	return status, code, message
}

func writeAnthropicError(c *gin.Context, status int, errorType, message string, errorCode ...string) {
	errorPayload := gin.H{"type": errorType, "message": message}
	if len(errorCode) > 0 && errorCode[0] != "" && errorCode[0] != "upstream_unavailable" {
		errorPayload["code"] = errorCode[0]
	}
	c.AbortWithStatusJSON(status, gin.H{"type": "error", "error": errorPayload})
}

func readCredentialErrorCode(status int, source io.Reader) string {
	body, err := io.ReadAll(io.LimitReader(source, maxCredentialErrorInspectBytes+1))
	if err != nil || len(body) > maxCredentialErrorInspectBytes {
		return "upstream_unavailable"
	}
	return gateway.ClientCredentialErrorCodeFromBody(status, body)
}

func credentialErrorMessage(code string) string {
	if code == "permission-denied" {
		return "上游服务暂不可用，聊天端点访问被拒绝"
	}
	return "上游服务暂不可用"
}

func forceJSONBoolean(body []byte, key string, value bool) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	payload[key] = json.RawMessage("false")
	if value {
		payload[key] = json.RawMessage("true")
	}
	return json.Marshal(payload)
}
