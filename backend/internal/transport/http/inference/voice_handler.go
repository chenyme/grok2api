package inference

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

type ttsRequest struct {
	Model                    string          `json:"model"`
	Text                     string          `json:"text"`
	VoiceID                  string          `json:"voice_id"`
	Language                 string          `json:"language"`
	OutputFormat             json.RawMessage `json:"output_format"`
	Speed                    *float64        `json:"speed"`
	OptimizeStreamingLatency json.RawMessage `json:"optimize_streaming_latency"`
	TextNormalization        *bool           `json:"text_normalization"`
	WithTimestamps           *bool           `json:"with_timestamps"`
}

type realtimeClientSecretRequest struct {
	Model        string          `json:"model"`
	ExpiresAfter json.RawMessage `json:"expires_after"`
	Session      json.RawMessage `json:"session"`
}

func (h *Handler) synthesizeSpeech(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxBodyBytes)
	if !isJSONRequest(c) {
		writeOpenAIError(c, http.StatusUnsupportedMediaType, "invalid_request", "TTS 仅支持 application/json")
		return
	}
	var request ttsRequest
	if err := decodeSingleJSON(c.Request.Body, &request, false); err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "TTS 请求无效")
		return
	}
	text := strings.TrimSpace(request.Text)
	language := strings.TrimSpace(request.Language)
	if text == "" || language == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "TTS 缺少有效 text 或 language")
		return
	}
	model := strings.TrimSpace(request.Model)
	if model == "" {
		model = "grok-voice-latest"
	}
	format := provider.TTSOutputFormat{}
	if len(bytesTrim(request.OutputFormat)) > 0 {
		var raw struct {
			Codec      string `json:"codec"`
			SampleRate *int   `json:"sample_rate"`
			BitRate    *int   `json:"bit_rate"`
		}
		if err := json.Unmarshal(request.OutputFormat, &raw); err != nil {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "output_format 无效")
			return
		}
		format.Codec = strings.TrimSpace(raw.Codec)
		if raw.SampleRate != nil {
			format.SampleRate = *raw.SampleRate
		}
		if raw.BitRate != nil {
			format.BitRate = *raw.BitRate
		}
	}
	speed := 0.0
	if request.Speed != nil {
		speed = *request.Speed
	}
	optimize := 0
	if len(bytesTrim(request.OptimizeStreamingLatency)) > 0 {
		var asString string
		var asNumber float64
		if json.Unmarshal(request.OptimizeStreamingLatency, &asString) == nil {
			optimize, _ = strconv.Atoi(strings.TrimSpace(asString))
		} else if json.Unmarshal(request.OptimizeStreamingLatency, &asNumber) == nil {
			optimize = int(asNumber)
		}
	}
	clientKey, requestID, ok := requestIdentity(c)
	if !ok {
		return
	}
	input := gateway.TTSInput{
		RequestID: requestID, ClientKey: clientKey, PublicModel: model, Text: text, VoiceID: strings.TrimSpace(request.VoiceID),
		Language: language, OutputFormat: format, Speed: speed, OptimizeStreamingLatency: optimize,
	}
	if request.TextNormalization != nil {
		input.TextNormalization = *request.TextNormalization
	}
	if request.WithTimestamps != nil {
		input.WithTimestamps = *request.WithTimestamps
	}
	result, err := h.gateway.SynthesizeSpeech(c.Request.Context(), input)
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeMediaResult(c, result)
}

func (h *Handler) listTTSVoices(c *gin.Context) {
	clientKey, requestID, ok := requestIdentity(c)
	if !ok {
		return
	}
	model := strings.TrimSpace(c.Query("model"))
	if model == "" {
		model = "grok-voice-latest"
	}
	result, err := h.gateway.ListTTSVoices(c.Request.Context(), gateway.VoiceListInput{RequestID: requestID, ClientKey: clientKey, PublicModel: model})
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeMediaResult(c, result)
}

func (h *Handler) getTTSVoice(c *gin.Context) {
	clientKey, requestID, ok := requestIdentity(c)
	if !ok {
		return
	}
	model := strings.TrimSpace(c.Query("model"))
	if model == "" {
		model = "grok-voice-latest"
	}
	result, err := h.gateway.GetTTSVoice(c.Request.Context(), gateway.CustomVoiceIDInput{
		RequestID: requestID, ClientKey: clientKey, PublicModel: model, VoiceID: c.Param("voiceId"),
	})
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeMediaResult(c, result)
}

func (h *Handler) transcribeSpeech(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxBodyBytes)
	clientKey, requestID, ok := requestIdentity(c)
	if !ok {
		return
	}
	contentType := strings.ToLower(strings.TrimSpace(c.GetHeader("Content-Type")))
	input := gateway.STTInput{RequestID: requestID, ClientKey: clientKey, PublicModel: "grok-stt"}
	if strings.HasPrefix(contentType, "multipart/form-data") {
		if err := c.Request.ParseMultipartForm(h.maxBodyBytes); err != nil {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "STT multipart 请求无效")
			return
		}
		form := c.Request.MultipartForm
		get := func(name string) string {
			if form == nil {
				return ""
			}
			values := form.Value[name]
			if len(values) == 0 {
				return ""
			}
			return strings.TrimSpace(values[0])
		}
		if model := get("model"); model != "" {
			input.PublicModel = model
		}
		input.URL = get("url")
		input.AudioFormat = get("audio_format")
		input.SampleRate = firstNonEmpty(get("sample_rate"), get("sample_rate_hertz"))
		input.Language = get("language")
		input.Format = parseTruthy(get("format"))
		input.Multichannel = parseTruthy(get("multichannel"))
		if channels := get("channels"); channels != "" {
			if value, err := strconv.Atoi(channels); err == nil {
				input.Channels = value
			}
		}
		input.Diarize = parseTruthy(get("diarize"))
		input.FillerWords = parseTruthy(get("filler_words"))
		if form != nil {
			input.KeyTerms = append([]string(nil), form.Value["keyterm"]...)
		}
		if threshold := get("vad_threshold"); threshold != "" {
			if value, err := strconv.ParseFloat(threshold, 64); err == nil {
				input.VADThreshold = &value
			}
		}
		file, header, err := c.Request.FormFile("file")
		if err == nil {
			defer file.Close()
			data, readErr := io.ReadAll(io.LimitReader(file, h.maxBodyBytes+1))
			if readErr != nil {
				writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "读取音频文件失败")
				return
			}
			if int64(len(data)) > h.maxBodyBytes {
				writeOpenAIError(c, http.StatusRequestEntityTooLarge, "request_too_large", "音频文件超过限制")
				return
			}
			input.FileData = data
			if header != nil {
				input.FileName = header.Filename
				input.FileMIME = header.Header.Get("Content-Type")
			}
		}
	} else if isJSONRequest(c) {
		var payload struct {
			Model        string   `json:"model"`
			URL          string   `json:"url"`
			AudioFormat  string   `json:"audio_format"`
			SampleRate   any      `json:"sample_rate"`
			Language     string   `json:"language"`
			Format       any      `json:"format"`
			Multichannel any      `json:"multichannel"`
			Channels     any      `json:"channels"`
			Diarize      any      `json:"diarize"`
			KeyTerms     []string `json:"keyterm"`
			FillerWords  any      `json:"filler_words"`
			VADThreshold *float64 `json:"vad_threshold"`
		}
		if err := decodeSingleJSON(c.Request.Body, &payload, false); err != nil {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "STT JSON 请求无效")
			return
		}
		if strings.TrimSpace(payload.Model) != "" {
			input.PublicModel = strings.TrimSpace(payload.Model)
		}
		input.URL = strings.TrimSpace(payload.URL)
		input.AudioFormat = strings.TrimSpace(payload.AudioFormat)
		input.SampleRate = anyString(payload.SampleRate)
		input.Language = strings.TrimSpace(payload.Language)
		input.Format = anyTruthy(payload.Format)
		input.Multichannel = anyTruthy(payload.Multichannel)
		if channels := anyString(payload.Channels); channels != "" {
			if value, err := strconv.Atoi(channels); err == nil {
				input.Channels = value
			}
		}
		input.Diarize = anyTruthy(payload.Diarize)
		input.KeyTerms = payload.KeyTerms
		input.FillerWords = anyTruthy(payload.FillerWords)
		input.VADThreshold = payload.VADThreshold
	} else {
		writeOpenAIError(c, http.StatusUnsupportedMediaType, "invalid_request", "STT 支持 multipart/form-data 或 application/json")
		return
	}
	if len(input.FileData) == 0 && strings.TrimSpace(input.URL) == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "STT 必须提供 file 或 url")
		return
	}
	result, err := h.gateway.TranscribeSpeech(c.Request.Context(), input)
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeMediaResult(c, result)
}

func (h *Handler) createRealtimeClientSecret(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxBodyBytes)
	if !isJSONRequest(c) {
		writeOpenAIError(c, http.StatusUnsupportedMediaType, "invalid_request", "realtime client secret 仅支持 application/json")
		return
	}
	var request realtimeClientSecretRequest
	if err := decodeSingleJSON(c.Request.Body, &request, false); err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "realtime client secret 请求无效")
		return
	}
	model := strings.TrimSpace(request.Model)
	if model == "" {
		model = "grok-voice-latest"
	}
	expiresAfter := 0
	if len(bytesTrim(request.ExpiresAfter)) > 0 {
		var wrapper struct {
			Seconds int `json:"seconds"`
		}
		if err := json.Unmarshal(request.ExpiresAfter, &wrapper); err == nil {
			expiresAfter = wrapper.Seconds
		}
	}
	clientKey, requestID, ok := requestIdentity(c)
	if !ok {
		return
	}
	result, err := h.gateway.CreateRealtimeClientSecret(c.Request.Context(), gateway.RealtimeClientSecretInput{
		RequestID: requestID, ClientKey: clientKey, PublicModel: model, ExpiresAfter: expiresAfter, SessionJSON: request.Session,
	})
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeMediaResult(c, result)
}

func (h *Handler) createCustomVoice(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxBodyBytes)
	clientKey, requestID, ok := requestIdentity(c)
	if !ok {
		return
	}
	if err := c.Request.ParseMultipartForm(h.maxBodyBytes); err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "custom voice 请求无效")
		return
	}
	name := strings.TrimSpace(c.PostForm("name"))
	if name == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "name 不能为空")
		return
	}
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "必须提供参考音频 file")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, h.maxBodyBytes+1))
	if err != nil || int64(len(data)) > h.maxBodyBytes {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "读取参考音频失败")
		return
	}
	model := strings.TrimSpace(c.PostForm("model"))
	if model == "" {
		model = "grok-voice-latest"
	}
	input := gateway.CustomVoiceCreateInput{
		RequestID: requestID, ClientKey: clientKey, PublicModel: model, Name: name,
		Language: c.PostForm("language"), Gender: c.PostForm("gender"), Tone: c.PostForm("tone"), UseCase: c.PostForm("use_case"),
		FileData: data,
	}
	if header != nil {
		input.FileName = header.Filename
		input.FileMIME = header.Header.Get("Content-Type")
	}
	result, err := h.gateway.CreateCustomVoice(c.Request.Context(), input)
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeMediaResult(c, result)
}

func (h *Handler) listCustomVoices(c *gin.Context) {
	clientKey, requestID, ok := requestIdentity(c)
	if !ok {
		return
	}
	model := strings.TrimSpace(c.Query("model"))
	if model == "" {
		model = "grok-voice-latest"
	}
	limit := 0
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil {
			limit = value
		}
	}
	result, err := h.gateway.ListCustomVoices(c.Request.Context(), gateway.VoiceListInput{RequestID: requestID, ClientKey: clientKey, PublicModel: model}, limit, c.Query("pagination_token"))
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeMediaResult(c, result)
}

func (h *Handler) getCustomVoice(c *gin.Context) {
	clientKey, requestID, ok := requestIdentity(c)
	if !ok {
		return
	}
	model := strings.TrimSpace(c.Query("model"))
	if model == "" {
		model = "grok-voice-latest"
	}
	result, err := h.gateway.GetCustomVoice(c.Request.Context(), gateway.CustomVoiceIDInput{RequestID: requestID, ClientKey: clientKey, PublicModel: model, VoiceID: c.Param("voiceId")})
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeMediaResult(c, result)
}

func (h *Handler) updateCustomVoice(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxBodyBytes)
	if !isJSONRequest(c) {
		writeOpenAIError(c, http.StatusUnsupportedMediaType, "invalid_request", "custom voice 更新仅支持 application/json")
		return
	}
	var payload struct {
		Model    string  `json:"model"`
		Name     *string `json:"name"`
		Language *string `json:"language"`
		Gender   *string `json:"gender"`
		Tone     *string `json:"tone"`
		UseCase  *string `json:"use_case"`
	}
	if err := decodeSingleJSON(c.Request.Body, &payload, false); err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "custom voice 更新请求无效")
		return
	}
	model := strings.TrimSpace(payload.Model)
	if model == "" {
		model = "grok-voice-latest"
	}
	clientKey, requestID, ok := requestIdentity(c)
	if !ok {
		return
	}
	result, err := h.gateway.UpdateCustomVoice(c.Request.Context(), gateway.CustomVoiceUpdateInput{
		RequestID: requestID, ClientKey: clientKey, PublicModel: model, VoiceID: c.Param("voiceId"),
		Name: payload.Name, Language: payload.Language, Gender: payload.Gender, Tone: payload.Tone, UseCase: payload.UseCase,
	})
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeMediaResult(c, result)
}

func (h *Handler) deleteCustomVoice(c *gin.Context) {
	clientKey, requestID, ok := requestIdentity(c)
	if !ok {
		return
	}
	model := strings.TrimSpace(c.Query("model"))
	if model == "" {
		model = "grok-voice-latest"
	}
	result, err := h.gateway.DeleteCustomVoice(c.Request.Context(), gateway.CustomVoiceIDInput{RequestID: requestID, ClientKey: clientKey, PublicModel: model, VoiceID: c.Param("voiceId")})
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeMediaResult(c, result)
}

func (h *Handler) getCustomVoiceAudio(c *gin.Context) {
	clientKey, requestID, ok := requestIdentity(c)
	if !ok {
		return
	}
	model := strings.TrimSpace(c.Query("model"))
	if model == "" {
		model = "grok-voice-latest"
	}
	result, err := h.gateway.GetCustomVoiceAudio(c.Request.Context(), gateway.CustomVoiceIDInput{RequestID: requestID, ClientKey: clientKey, PublicModel: model, VoiceID: c.Param("voiceId")})
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeMediaResult(c, result)
}

func bytesTrim(value json.RawMessage) []byte {
	return []byte(strings.TrimSpace(string(value)))
}

func parseTruthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func anyTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return parseTruthy(typed)
	case float64:
		return typed != 0
	default:
		return false
	}
}

func anyString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(typed)
	case float64:
		return strconv.FormatInt(int64(typed), 10)
	case json.Number:
		return typed.String()
	default:
		return strings.TrimSpace(stringify(value))
	}
}

func stringify(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return strings.Trim(string(data), `"`)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// keep middleware import used for request identity compatibility in package docs.
var _ = middleware.ClientKey
