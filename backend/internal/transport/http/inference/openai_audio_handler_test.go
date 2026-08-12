package inference

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

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
}

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
