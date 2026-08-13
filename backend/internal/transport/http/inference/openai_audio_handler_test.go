package inference

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestStreamingSTTDurationAcceptsOnlyCompletedFiniteEvents(t *testing.T) {
	duration, ok := streamingSTTDuration([]byte(`{"type":"transcript.done","duration":3.45,"text":"hello"}`))
	if !ok || math.Abs(duration-3.45) > 1e-9 {
		t.Fatalf("completed duration = %v, ok = %t", duration, ok)
	}
	for _, payload := range [][]byte{
		[]byte(`{"type":"transcript.delta","duration":3.45}`),
		[]byte(`{"type":"transcript.done","duration":0}`),
		[]byte(`{"type":"transcript.done","duration":"3.45"}`),
		[]byte(`not-json`),
	} {
		if duration, ok := streamingSTTDuration(payload); ok || duration != 0 {
			t.Fatalf("invalid payload %q produced duration %v, ok = %t", payload, duration, ok)
		}
	}
}

func TestOpenAIAudioSpeechValidatesAndMapsFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewHandler(nil, nil, 1<<20).Register(router.Group("/v1"))

	missingInput := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":"grok-voice-latest","voice":"alloy"}`))
	missingInput.Header.Set("Content-Type", "application/json")
	missingRecorder := httptest.NewRecorder()
	router.ServeHTTP(missingRecorder, missingInput)
	if missingRecorder.Code != http.StatusBadRequest || !strings.Contains(missingRecorder.Body.String(), "input") {
		t.Fatalf("missing input status=%d body=%s", missingRecorder.Code, missingRecorder.Body.String())
	}

	badFormat := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":"grok-voice-latest","input":"hi","response_format":"midi"}`))
	badFormat.Header.Set("Content-Type", "application/json")
	badFormatRecorder := httptest.NewRecorder()
	router.ServeHTTP(badFormatRecorder, badFormat)
	if badFormatRecorder.Code != http.StatusBadRequest || !strings.Contains(badFormatRecorder.Body.String(), "response_format") {
		t.Fatalf("bad format status=%d body=%s", badFormatRecorder.Code, badFormatRecorder.Body.String())
	}

	// Reach auth after successful mapping/validation.
	valid := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{
		"model":"grok-voice-latest","input":"Hello from OpenAI clients.","voice":"alloy","response_format":"mp3","speed":1.0
	}`))
	valid.Header.Set("Content-Type", "application/json")
	validRecorder := httptest.NewRecorder()
	router.ServeHTTP(validRecorder, valid)
	if validRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("valid openai speech status=%d body=%s", validRecorder.Code, validRecorder.Body.String())
	}

	task := httptest.NewRequest(http.MethodPost, "/v1/audio/tasks", strings.NewReader(`{
		"model":"grok-voice-latest","input":"Hello from OpenAI clients.","voice":"nova"
	}`))
	task.Header.Set("Content-Type", "application/json")
	taskRecorder := httptest.NewRecorder()
	router.ServeHTTP(taskRecorder, task)
	if taskRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("valid openai audio task status=%d body=%s", taskRecorder.Code, taskRecorder.Body.String())
	}

	transcription := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", strings.NewReader(`{"model":"grok-stt","url":"https://example.com/a.wav"}`))
	transcription.Header.Set("Content-Type", "application/json")
	transcriptionRecorder := httptest.NewRecorder()
	router.ServeHTTP(transcriptionRecorder, transcription)
	if transcriptionRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("openai transcription status=%d body=%s", transcriptionRecorder.Code, transcriptionRecorder.Body.String())
	}

	badTranscriptionFormat := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", strings.NewReader(`{"model":"grok-stt","url":"https://example.com/a.wav","response_format":"srt"}`))
	badTranscriptionFormat.Header.Set("Content-Type", "application/json")
	badTranscriptionRecorder := httptest.NewRecorder()
	router.ServeHTTP(badTranscriptionRecorder, badTranscriptionFormat)
	if badTranscriptionRecorder.Code != http.StatusBadRequest || !strings.Contains(badTranscriptionRecorder.Body.String(), "response_format") {
		t.Fatalf("bad transcription format status=%d body=%s", badTranscriptionRecorder.Code, badTranscriptionRecorder.Body.String())
	}

	unsupportedPrompt := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", strings.NewReader(`{"model":"grok-stt","url":"https://example.com/a.wav","prompt":"special spelling"}`))
	unsupportedPrompt.Header.Set("Content-Type", "application/json")
	unsupportedPromptRecorder := httptest.NewRecorder()
	router.ServeHTTP(unsupportedPromptRecorder, unsupportedPrompt)
	if unsupportedPromptRecorder.Code != http.StatusBadRequest || !strings.Contains(unsupportedPromptRecorder.Body.String(), "prompt") {
		t.Fatalf("unsupported prompt status=%d body=%s", unsupportedPromptRecorder.Code, unsupportedPromptRecorder.Body.String())
	}

	translation := httptest.NewRequest(http.MethodPost, "/v1/audio/translations", strings.NewReader(`{"model":"grok-stt","url":"https://example.com/a.wav"}`))
	translation.Header.Set("Content-Type", "application/json")
	translationRecorder := httptest.NewRecorder()
	router.ServeHTTP(translationRecorder, translation)
	if translationRecorder.Code != http.StatusNotFound {
		t.Fatalf("unsupported translation status=%d body=%s", translationRecorder.Code, translationRecorder.Body.String())
	}
}

func TestTTSOptionValidationIsSharedByNativeAndOpenAIHandlers(t *testing.T) {
	if _, err := parseTTSSpeed(float64Pointer(0.2)); err == nil {
		t.Fatal("invalid speed was accepted")
	}
	if _, err := parseOptimizeStreamingLatency([]byte(`2.5`)); err == nil {
		t.Fatal("fractional optimize_streaming_latency was accepted")
	}
	if _, err := parseOptimizeStreamingLatency([]byte(`"invalid"`)); err == nil {
		t.Fatal("invalid optimize_streaming_latency was accepted")
	}
	if _, err := parseTTSOutputFormat([]byte(`{"codec":"mp3","sample_rate":0}`)); err == nil {
		t.Fatal("invalid sample rate was accepted")
	}
	if value, err := parseOptimizeStreamingLatency([]byte(`"4"`)); err != nil || value != 4 {
		t.Fatalf("valid optimize_streaming_latency = %d, %v", value, err)
	}
}

func float64Pointer(value float64) *float64 { return &value }

func TestMapOpenAIVoiceAndFormat(t *testing.T) {
	if got := mapOpenAIVoiceID("alloy"); got != "ara" {
		t.Fatalf("alloy map = %q", got)
	}
	if got := mapOpenAIVoiceID("custom_voice_123"); got != "custom_voice_123" {
		t.Fatalf("passthrough map = %q", got)
	}
	if got := mapOpenAIResponseFormat("wav"); got != "wav" {
		t.Fatalf("wav map = %q", got)
	}
	if got := mapOpenAIResponseFormat("midi"); got != "" {
		t.Fatalf("midi map = %q", got)
	}
}
