package inference

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/gin-gonic/gin"
)

// OpenAI-compatible audio request shapes.
// https://platform.openai.com/docs/api-reference/audio
type openAISpeechRequest struct {
	Model          string   `json:"model"`
	Input          string   `json:"input"`
	Voice          string   `json:"voice"`
	ResponseFormat string   `json:"response_format"`
	Speed          *float64 `json:"speed"`
	// Optional Grok/Console extensions accepted without breaking OpenAI clients.
	Language                 string          `json:"language"`
	VoiceID                  string          `json:"voice_id"`
	OutputFormat             json.RawMessage `json:"output_format"`
	OptimizeStreamingLatency json.RawMessage `json:"optimize_streaming_latency"`
	TextNormalization        *bool           `json:"text_normalization"`
	WithTimestamps           *bool           `json:"with_timestamps"`
}

func (h *Handler) synthesizeOpenAISpeech(c *gin.Context) {
	h.handleOpenAISpeech(c, false)
}

// synthesizeOpenAIAudioTask keeps a compatibility path used by some OpenAI-style
// clients that post speech jobs to /v1/audio/tasks instead of /v1/audio/speech.
func (h *Handler) synthesizeOpenAIAudioTask(c *gin.Context) {
	h.handleOpenAISpeech(c, true)
}

func (h *Handler) handleOpenAISpeech(c *gin.Context, asTask bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxBodyBytes)
	if !isJSONRequest(c) {
		writeOpenAIError(c, http.StatusUnsupportedMediaType, "invalid_request", "audio speech 仅支持 application/json")
		return
	}
	var request openAISpeechRequest
	if err := decodeSingleJSON(c.Request.Body, &request, false); err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "audio speech 请求无效")
		return
	}

	text := strings.TrimSpace(request.Input)
	if text == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "input 不能为空")
		return
	}
	language := strings.TrimSpace(request.Language)
	if language == "" {
		// Console TTS requires language; default keeps OpenAI clients working.
		language = "en"
	}
	model := strings.TrimSpace(request.Model)
	if model == "" {
		model = "grok-voice-latest"
	}
	voiceID := firstNonEmpty(strings.TrimSpace(request.VoiceID), strings.TrimSpace(request.Voice))
	if mapped := mapOpenAIVoiceID(voiceID); mapped != "" {
		voiceID = mapped
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
	if format.Codec == "" {
		if codec := mapOpenAIResponseFormat(request.ResponseFormat); codec != "" {
			format.Codec = codec
		} else if strings.TrimSpace(request.ResponseFormat) != "" {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "response_format 不受支持")
			return
		} else {
			format.Codec = "mp3"
		}
	}

	speed := 0.0
	if request.Speed != nil {
		if *request.Speed < 0.25 || *request.Speed > 4.0 {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "speed 必须在 0.25 到 4.0 之间")
			return
		}
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
		RequestID:                requestID,
		ClientKey:                clientKey,
		PublicModel:              model,
		Text:                     text,
		VoiceID:                  voiceID,
		Language:                 language,
		OutputFormat:             format,
		Speed:                    speed,
		OptimizeStreamingLatency: optimize,
	}
	if request.TextNormalization != nil {
		input.TextNormalization = *request.TextNormalization
	}
	if request.WithTimestamps != nil {
		input.WithTimestamps = *request.WithTimestamps
	} else if asTask {
		// Task-style clients usually expect a JSON envelope instead of raw bytes.
		input.WithTimestamps = true
	}

	result, err := h.gateway.SynthesizeSpeech(c.Request.Context(), input)
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeMediaResult(c, result)
}

func (h *Handler) transcribeOpenAIAudio(c *gin.Context) {
	// OpenAI uses multipart form field "file" plus optional model/language/prompt.
	// Reuse the existing STT handler which already accepts multipart and JSON.
	h.transcribeSpeech(c)
}

func mapOpenAIResponseFormat(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "mp3":
		return "mp3"
	case "opus", "ogg":
		return "opus"
	case "aac":
		return "aac"
	case "flac":
		return "flac"
	case "wav", "wave":
		return "wav"
	case "pcm", "pcm16":
		return "pcm"
	default:
		return ""
	}
}

// mapOpenAIVoiceID keeps common OpenAI voice names usable against Console voices.
// Unknown values pass through so custom voice_id and built-in Grok ids still work.
func mapOpenAIVoiceID(voice string) string {
	switch strings.ToLower(strings.TrimSpace(voice)) {
	case "alloy", "verse":
		return "ara"
	case "echo", "ballad":
		return "eve"
	case "fable", "coral":
		return "sal"
	case "onyx", "ash":
		return "rex"
	case "nova", "sage":
		return "leo"
	case "shimmer", "marin":
		return "sia"
	default:
		return strings.TrimSpace(voice)
	}
}
