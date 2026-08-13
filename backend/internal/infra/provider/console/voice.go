package console

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
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

const (
	consoleVoiceBodyLimit  = 64 << 20
	consoleVoiceAudioLimit = 128 << 20
)

func (a *Adapter) SynthesizeSpeech(ctx context.Context, request provider.TTSRequest) (provider.TTSResult, error) {
	text := strings.TrimSpace(request.Text)
	if text == "" {
		return provider.TTSResult{}, invalidConsoleVoiceError("text 不能为空")
	}
	if utf8.RuneCountInString(text) > 15000 {
		return provider.TTSResult{}, invalidConsoleVoiceError("text 最多 15000 个字符")
	}
	language := strings.TrimSpace(request.Language)
	if language == "" {
		return provider.TTSResult{}, invalidConsoleVoiceError("language 不能为空")
	}
	payload := map[string]any{"text": text, "language": language}
	if voiceID := strings.TrimSpace(request.VoiceID); voiceID != "" {
		payload["voice_id"] = voiceID
	}
	if format := normalizeTTSOutputFormat(request.OutputFormat); format != nil {
		payload["output_format"] = format
	}
	if request.Speed > 0 {
		payload["speed"] = request.Speed
	}
	if request.OptimizeStreamingLatency > 0 {
		payload["optimize_streaming_latency"] = strconv.Itoa(request.OptimizeStreamingLatency)
	}
	if request.TextNormalization {
		payload["text_normalization"] = true
	}
	if request.WithTimestamps {
		payload["with_timestamps"] = true
	}
	response, err := a.forwardConsoleVoice(ctx, request.Credential, http.MethodPost, "/tts", payload, "application/json", "*/*")
	if err != nil {
		return provider.TTSResult{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return provider.TTSResult{}, consoleVoiceResponseError(response)
	}
	contentType := strings.ToLower(strings.TrimSpace(response.Header.Get("Content-Type")))
	data, err := io.ReadAll(io.LimitReader(response.Body, consoleVoiceAudioLimit+1))
	if err != nil {
		return provider.TTSResult{}, err
	}
	if len(data) > consoleVoiceAudioLimit {
		return provider.TTSResult{}, errors.New("Console TTS 响应超过安全上限")
	}
	if strings.Contains(contentType, "application/json") || request.WithTimestamps {
		var envelope struct {
			Audio           string  `json:"audio"`
			ContentType     string  `json:"content_type"`
			Duration        float64 `json:"duration"`
			AudioTimestamps *struct {
				GraphChars []string `json:"graph_chars"`
				GraphTimes []struct {
					Start float64 `json:"start"`
					End   float64 `json:"end"`
				} `json:"graph_times"`
			} `json:"audio_timestamps"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			if !strings.Contains(contentType, "application/json") {
				return provider.TTSResult{Audio: data, ContentType: firstNonEmpty(contentType, "audio/mpeg")}, nil
			}
			return provider.TTSResult{}, fmt.Errorf("解析 Console TTS JSON 响应失败: %w", err)
		}
		audio, err := base64.StdEncoding.DecodeString(strings.TrimSpace(envelope.Audio))
		if err != nil {
			return provider.TTSResult{}, fmt.Errorf("解码 Console TTS audio 失败: %w", err)
		}
		result := provider.TTSResult{
			Audio: audio, ContentType: firstNonEmpty(envelope.ContentType, contentType, "audio/mpeg"),
			Duration: envelope.Duration, Base64Audio: envelope.Audio, JSONEnvelope: true,
		}
		if envelope.AudioTimestamps != nil {
			times := make([]provider.TTSTimestampSpan, 0, len(envelope.AudioTimestamps.GraphTimes))
			for _, item := range envelope.AudioTimestamps.GraphTimes {
				times = append(times, provider.TTSTimestampSpan{Start: item.Start, End: item.End})
			}
			result.Timestamps = &provider.TTSTimestamps{
				GraphChars: append([]string(nil), envelope.AudioTimestamps.GraphChars...),
				GraphTimes: times,
			}
		}
		return result, nil
	}
	return provider.TTSResult{Audio: data, ContentType: firstNonEmpty(contentType, "audio/mpeg")}, nil
}

func (a *Adapter) ListTTSVoices(ctx context.Context, credential account.Credential) ([]provider.VoiceInfo, error) {
	response, err := a.forwardConsoleVoice(ctx, credential, http.MethodGet, "/tts/voices", nil, "", "application/json")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, consoleVoiceResponseError(response)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, consoleVoiceBodyLimit+1))
	if err != nil {
		return nil, err
	}
	var payload struct {
		Voices []struct {
			VoiceID  string  `json:"voice_id"`
			Name     string  `json:"name"`
			Language *string `json:"language"`
		} `json:"voices"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("解析 Console TTS voices 失败: %w", err)
	}
	result := make([]provider.VoiceInfo, 0, len(payload.Voices))
	for _, item := range payload.Voices {
		language := ""
		if item.Language != nil {
			language = strings.TrimSpace(*item.Language)
		}
		result = append(result, provider.VoiceInfo{VoiceID: strings.TrimSpace(item.VoiceID), Name: strings.TrimSpace(item.Name), Language: language})
	}
	return result, nil
}

func (a *Adapter) GetTTSVoice(ctx context.Context, credential account.Credential, voiceID string) (provider.VoiceInfo, error) {
	voiceID = strings.TrimSpace(voiceID)
	if voiceID == "" {
		return provider.VoiceInfo{}, invalidConsoleVoiceError("voice_id 不能为空")
	}
	response, err := a.forwardConsoleVoice(ctx, credential, http.MethodGet, "/tts/voices/"+url.PathEscape(voiceID), nil, "", "application/json")
	if err != nil {
		return provider.VoiceInfo{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return provider.VoiceInfo{}, consoleVoiceResponseError(response)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, consoleVoiceBodyLimit+1))
	if err != nil {
		return provider.VoiceInfo{}, err
	}
	var payload struct {
		VoiceID  string  `json:"voice_id"`
		Name     string  `json:"name"`
		Language *string `json:"language"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return provider.VoiceInfo{}, fmt.Errorf("解析 Console TTS voice 失败: %w", err)
	}
	language := ""
	if payload.Language != nil {
		language = strings.TrimSpace(*payload.Language)
	}
	return provider.VoiceInfo{VoiceID: strings.TrimSpace(payload.VoiceID), Name: strings.TrimSpace(payload.Name), Language: language}, nil
}

func (a *Adapter) TranscribeSpeech(ctx context.Context, request provider.STTRequest) (provider.STTResult, error) {
	hasFile := len(request.FileData) > 0
	hasURL := strings.TrimSpace(request.URL) != ""
	if hasFile == hasURL {
		return provider.STTResult{}, invalidConsoleVoiceError("STT 必须提供 file 或 url 其中之一")
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	writeField := func(name, value string) error {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil
		}
		return writer.WriteField(name, value)
	}
	_ = writeField("audio_format", request.AudioFormat)
	_ = writeField("sample_rate", request.SampleRate)
	_ = writeField("language", request.Language)
	if request.Format {
		_ = writer.WriteField("format", "true")
	}
	if request.Multichannel {
		_ = writer.WriteField("multichannel", "true")
	}
	if request.Channels > 0 {
		_ = writer.WriteField("channels", strconv.Itoa(request.Channels))
	}
	if request.Diarize {
		_ = writer.WriteField("diarize", "true")
	}
	for _, term := range request.KeyTerms {
		_ = writeField("keyterm", term)
	}
	if request.FillerWords {
		_ = writer.WriteField("filler_words", "true")
	}
	if request.VADThreshold != nil {
		_ = writer.WriteField("vad_threshold", strconv.FormatFloat(*request.VADThreshold, 'f', -1, 64))
	}
	if modelName := strings.TrimSpace(request.Model); modelName != "" {
		_ = writer.WriteField("model", modelName)
	}
	if hasURL {
		_ = writer.WriteField("url", strings.TrimSpace(request.URL))
	}
	if hasFile {
		fileName := strings.TrimSpace(request.FileName)
		if fileName == "" {
			fileName = "audio.bin"
		}
		part, err := writer.CreateFormFile("file", path.Base(fileName))
		if err != nil {
			return provider.STTResult{}, err
		}
		if _, err := part.Write(request.FileData); err != nil {
			return provider.STTResult{}, err
		}
	}
	if err := writer.Close(); err != nil {
		return provider.STTResult{}, err
	}
	response, err := a.forwardConsoleVoiceBytes(ctx, request.Credential, http.MethodPost, "/stt", body.Bytes(), writer.FormDataContentType(), "application/json")
	if err != nil {
		return provider.STTResult{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return provider.STTResult{}, consoleVoiceResponseError(response)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, consoleVoiceBodyLimit+1))
	if err != nil {
		return provider.STTResult{}, err
	}
	result, err := parseSTTResult(data)
	if err != nil {
		return provider.STTResult{}, err
	}
	result.RawJSON = append([]byte(nil), data...)
	return result, nil
}

func (a *Adapter) forwardConsoleVoice(ctx context.Context, credential account.Credential, method, pathValue string, payload any, contentType, accept string) (*http.Response, error) {
	var body []byte
	var err error
	if payload != nil {
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		if contentType == "" {
			contentType = "application/json"
		}
	}
	return a.forwardConsoleVoiceBytes(ctx, credential, method, pathValue, body, contentType, accept)
}

func (a *Adapter) forwardConsoleVoiceBytes(ctx context.Context, credential account.Credential, method, pathValue string, body []byte, contentType, accept string) (*http.Response, error) {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "multipart/") {
		return a.forwardConsoleVoiceMultipart(ctx, credential, method, pathValue, body, contentType, accept)
	}
	token, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
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
	if accept == "" {
		accept = "*/*"
	}
	response, err := a.doDPoPRequestWithContentType(requestCtx, credential, token, lease, method, consoleV1Endpoint(cfg.BaseURL, pathValue), body, contentType, accept)
	if err != nil {
		a.egress.FeedbackForScope(context.WithoutCancel(ctx), egressdomain.ScopeConsole, lease.NodeID, 0, err)
		lease.Release()
		cancel()
		return nil, err
	}
	response.Body = &releaseBody{ReadCloser: response.Body, release: func() {
		a.egress.FeedbackForScope(context.WithoutCancel(ctx), egressdomain.ScopeConsole, lease.NodeID, response.StatusCode, nil)
		lease.Release()
		cancel()
	}}
	return response, nil
}

func (a *Adapter) forwardConsoleVoiceMultipart(ctx context.Context, credential account.Credential, method, pathValue string, body []byte, contentType, accept string) (*http.Response, error) {
	token, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
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
	if accept == "" {
		accept = "*/*"
	}
	response, err := a.doDPoPRequestWithContentType(requestCtx, credential, token, lease, method, consoleV1Endpoint(cfg.BaseURL, pathValue), body, contentType, accept)
	if err != nil {
		a.egress.FeedbackForScope(context.WithoutCancel(ctx), egressdomain.ScopeConsole, lease.NodeID, 0, err)
		lease.Release()
		cancel()
		return nil, err
	}
	response.Body = &releaseBody{ReadCloser: response.Body, release: func() {
		a.egress.FeedbackForScope(context.WithoutCancel(ctx), egressdomain.ScopeConsole, lease.NodeID, response.StatusCode, nil)
		lease.Release()
		cancel()
	}}
	return response, nil
}

func normalizeTTSOutputFormat(format provider.TTSOutputFormat) map[string]any {
	codec := strings.ToLower(strings.TrimSpace(format.Codec))
	if codec == "" && format.SampleRate == 0 && format.BitRate == 0 {
		return nil
	}
	if codec == "" {
		codec = "mp3"
	}
	result := map[string]any{"codec": codec}
	if format.SampleRate > 0 {
		result["sample_rate"] = format.SampleRate
	}
	if format.BitRate > 0 {
		result["bit_rate"] = format.BitRate
	}
	return result
}

func parseSTTResult(data []byte) (provider.STTResult, error) {
	var payload struct {
		Text     string  `json:"text"`
		Language string  `json:"language"`
		Duration float64 `json:"duration"`
		Words    []struct {
			Text    string  `json:"text"`
			Start   float64 `json:"start"`
			End     float64 `json:"end"`
			Speaker *int    `json:"speaker"`
		} `json:"words"`
		Channels []struct {
			Index int    `json:"index"`
			Text  string `json:"text"`
			Words []struct {
				Text    string  `json:"text"`
				Start   float64 `json:"start"`
				End     float64 `json:"end"`
				Speaker *int    `json:"speaker"`
			} `json:"words"`
		} `json:"channels"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return provider.STTResult{}, fmt.Errorf("解析 Console STT 响应失败: %w", err)
	}
	result := provider.STTResult{Text: payload.Text, Language: payload.Language, Duration: payload.Duration}
	for _, word := range payload.Words {
		result.Words = append(result.Words, provider.STTWord{Text: word.Text, Start: word.Start, End: word.End, Speaker: word.Speaker})
	}
	for _, channel := range payload.Channels {
		item := provider.STTChannel{Index: channel.Index, Text: channel.Text}
		for _, word := range channel.Words {
			item.Words = append(item.Words, provider.STTWord{Text: word.Text, Start: word.Start, End: word.End, Speaker: word.Speaker})
		}
		result.Channels = append(result.Channels, item)
	}
	return result, nil
}

func consoleVoiceResponseError(response *http.Response) error {
	data, _, err := provider.ReadDiagnosticBody(response.Body)
	if err != nil {
		return err
	}
	retryAfter := parseConsoleRetryAfterHeader(response.Header.Get("Retry-After"), time.Now().UTC())
	return newConsoleMediaUpstreamError(response.StatusCode, data, retryAfter)
}

func invalidConsoleVoiceError(message string) error {
	return &consoleMediaUpstreamError{status: http.StatusBadRequest, summary: message}
}
