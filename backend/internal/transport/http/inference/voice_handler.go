package inference

import (
	"encoding/json"
	"errors"
	"io"
	"math"
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
	format, err := parseTTSOutputFormat(request.OutputFormat)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", err.Error())
		return
	}
	speed, err := parseTTSSpeed(request.Speed)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", err.Error())
		return
	}
	optimize, err := parseOptimizeStreamingLatency(request.OptimizeStreamingLatency)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", err.Error())
		return
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
	result, err := h.gateway.GetTTSVoice(c.Request.Context(), gateway.VoiceIDInput{
		RequestID: requestID, ClientKey: clientKey, PublicModel: model, VoiceID: c.Param("voiceId"),
	})
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeMediaResult(c, result)
}

func (h *Handler) transcribeSpeech(c *gin.Context) {
	h.transcribeSpeechRequest(c, false)
}

func (h *Handler) transcribeSpeechRequest(c *gin.Context, openAICompatible bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxBodyBytes)
	contentType := strings.ToLower(strings.TrimSpace(c.GetHeader("Content-Type")))
	input := gateway.STTInput{PublicModel: "grok-stt"}
	unsupportedOpenAIParameter := ""
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
		if openAICompatible {
			input.ResponseFormat = get("response_format")
			if get("prompt") != "" {
				unsupportedOpenAIParameter = "prompt"
			}
			if temperature := get("temperature"); temperature != "" && temperature != "0" && temperature != "0.0" {
				unsupportedOpenAIParameter = "temperature"
			}
			if form != nil && len(form.Value["timestamp_granularities[]"]) > 0 {
				unsupportedOpenAIParameter = "timestamp_granularities[]"
			}
		}
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
			Model                  string   `json:"model"`
			URL                    string   `json:"url"`
			AudioFormat            string   `json:"audio_format"`
			SampleRate             any      `json:"sample_rate"`
			Language               string   `json:"language"`
			Format                 any      `json:"format"`
			Multichannel           any      `json:"multichannel"`
			Channels               any      `json:"channels"`
			Diarize                any      `json:"diarize"`
			KeyTerms               []string `json:"keyterm"`
			FillerWords            any      `json:"filler_words"`
			VADThreshold           *float64 `json:"vad_threshold"`
			ResponseFormat         string   `json:"response_format"`
			Prompt                 string   `json:"prompt"`
			Temperature            *float64 `json:"temperature"`
			TimestampGranularities []string `json:"timestamp_granularities"`
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
		if openAICompatible {
			input.ResponseFormat = strings.TrimSpace(payload.ResponseFormat)
			if strings.TrimSpace(payload.Prompt) != "" {
				unsupportedOpenAIParameter = "prompt"
			}
			if payload.Temperature != nil && *payload.Temperature != 0 {
				unsupportedOpenAIParameter = "temperature"
			}
			if len(payload.TimestampGranularities) > 0 {
				unsupportedOpenAIParameter = "timestamp_granularities"
			}
		}
	} else {
		writeOpenAIError(c, http.StatusUnsupportedMediaType, "invalid_request", "STT 支持 multipart/form-data 或 application/json")
		return
	}
	if openAICompatible {
		if unsupportedOpenAIParameter != "" {
			writeOpenAIError(c, http.StatusBadRequest, "unsupported_parameter", unsupportedOpenAIParameter+" 暂不支持，不能无损转换到 Console STT")
			return
		}
		input.ResponseFormat = strings.ToLower(strings.TrimSpace(input.ResponseFormat))
		if input.ResponseFormat == "" {
			input.ResponseFormat = "json"
		}
		switch input.ResponseFormat {
		case "json", "verbose_json", "text":
		default:
			writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "response_format 必须是 json、verbose_json 或 text")
			return
		}
	}
	if len(input.FileData) == 0 && strings.TrimSpace(input.URL) == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "STT 必须提供 file 或 url")
		return
	}
	clientKey, requestID, ok := requestIdentity(c)
	if !ok {
		return
	}
	input.ClientKey = clientKey
	input.RequestID = requestID
	result, err := h.gateway.TranscribeSpeech(c.Request.Context(), input)
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeMediaResult(c, result)
}

func bytesTrim(value json.RawMessage) []byte {
	return []byte(strings.TrimSpace(string(value)))
}

func parseTTSOutputFormat(value json.RawMessage) (provider.TTSOutputFormat, error) {
	format := provider.TTSOutputFormat{}
	if len(bytesTrim(value)) == 0 {
		return format, nil
	}
	var raw struct {
		Codec      string `json:"codec"`
		SampleRate *int   `json:"sample_rate"`
		BitRate    *int   `json:"bit_rate"`
	}
	if err := json.Unmarshal(value, &raw); err != nil {
		return format, errors.New("output_format 无效")
	}
	format.Codec = strings.ToLower(strings.TrimSpace(raw.Codec))
	if raw.SampleRate != nil {
		if *raw.SampleRate <= 0 {
			return provider.TTSOutputFormat{}, errors.New("output_format.sample_rate 必须大于 0")
		}
		format.SampleRate = *raw.SampleRate
	}
	if raw.BitRate != nil {
		if *raw.BitRate <= 0 {
			return provider.TTSOutputFormat{}, errors.New("output_format.bit_rate 必须大于 0")
		}
		format.BitRate = *raw.BitRate
	}
	return format, nil
}

func parseTTSSpeed(value *float64) (float64, error) {
	if value == nil {
		return 0, nil
	}
	if math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0.25 || *value > 4 {
		return 0, errors.New("speed 必须在 0.25 到 4.0 之间")
	}
	return *value, nil
}

func parseOptimizeStreamingLatency(value json.RawMessage) (int, error) {
	if len(bytesTrim(value)) == 0 {
		return 0, nil
	}
	var result int
	var asString string
	if json.Unmarshal(value, &asString) == nil {
		parsed, err := strconv.Atoi(strings.TrimSpace(asString))
		if err != nil {
			return 0, errors.New("optimize_streaming_latency 必须是 0 到 4 的整数")
		}
		result = parsed
	} else {
		var asNumber float64
		if json.Unmarshal(value, &asNumber) != nil || math.IsNaN(asNumber) || math.IsInf(asNumber, 0) || math.Trunc(asNumber) != asNumber {
			return 0, errors.New("optimize_streaming_latency 必须是 0 到 4 的整数")
		}
		result = int(asNumber)
	}
	if result < 0 || result > 4 {
		return 0, errors.New("optimize_streaming_latency 必须是 0 到 4 的整数")
	}
	return result, nil
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
