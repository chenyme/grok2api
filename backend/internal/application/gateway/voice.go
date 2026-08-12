package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

type TTSInput struct {
	RequestID               string
	ClientKey               clientkey.Key
	PublicModel             string
	Text                    string
	VoiceID                 string
	Language                string
	OutputFormat            provider.TTSOutputFormat
	Speed                   float64
	OptimizeStreamingLatency int
	TextNormalization       bool
	WithTimestamps          bool
}

type STTInput struct {
	RequestID   string
	ClientKey   clientkey.Key
	PublicModel string
	FileName    string
	FileMIME    string
	FileData    []byte
	URL         string
	AudioFormat string
	SampleRate  string
	Language    string
	Format      bool
	Multichannel bool
	Channels    int
	Diarize     bool
	KeyTerms    []string
	FillerWords bool
	VADThreshold *float64
}

type RealtimeClientSecretInput struct {
	RequestID    string
	ClientKey    clientkey.Key
	PublicModel  string
	ExpiresAfter int
	SessionJSON  []byte
}

type VoiceListInput struct {
	RequestID   string
	ClientKey   clientkey.Key
	PublicModel string
}

type CustomVoiceCreateInput struct {
	RequestID   string
	ClientKey   clientkey.Key
	PublicModel string
	Name        string
	Language    string
	Gender      string
	Tone        string
	UseCase     string
	FileName    string
	FileMIME    string
	FileData    []byte
}

type CustomVoiceUpdateInput struct {
	RequestID   string
	ClientKey   clientkey.Key
	PublicModel string
	VoiceID     string
	Name        *string
	Language    *string
	Gender      *string
	Tone        *string
	UseCase     *string
}

type CustomVoiceIDInput struct {
	RequestID   string
	ClientKey   clientkey.Key
	PublicModel string
	VoiceID     string
}

type voiceProviderSupport func(accountdomain.Provider) bool

func (s *Service) SynthesizeSpeech(ctx context.Context, input TTSInput) (*Result, error) {
	return s.executeVoice(ctx, input.RequestID, input.ClientKey, input.PublicModel, audit.OperationTTS, modeldomain.CapabilityTTS, func(providerValue accountdomain.Provider) bool {
		_, ok := s.providers.TTS(providerValue)
		return ok
	}, func(executionCtx context.Context, providerValue accountdomain.Provider, credential accountdomain.Credential, upstream string) (*provider.Response, error) {
		adapter, ok := s.providers.TTS(providerValue)
		if !ok {
			return nil, ErrNoAvailableAccount
		}
		result, err := adapter.SynthesizeSpeech(executionCtx, provider.TTSRequest{
			Credential: credential, Model: upstream, Text: input.Text, VoiceID: input.VoiceID, Language: input.Language,
			OutputFormat: input.OutputFormat, Speed: input.Speed, OptimizeStreamingLatency: input.OptimizeStreamingLatency,
			TextNormalization: input.TextNormalization, WithTimestamps: input.WithTimestamps,
		})
		if err != nil {
			return voiceErrorResponse(err)
		}
		if result.JSONEnvelope || input.WithTimestamps {
			payload := map[string]any{
				"audio":        firstNonEmpty(result.Base64Audio, base64.StdEncoding.EncodeToString(result.Audio)),
				"content_type": firstNonEmpty(result.ContentType, "audio/mpeg"),
				"duration":     result.Duration,
			}
			if result.Timestamps != nil {
				times := make([]map[string]any, 0, len(result.Timestamps.GraphTimes))
				for _, item := range result.Timestamps.GraphTimes {
					times = append(times, map[string]any{"start": item.Start, "end": item.End})
				}
				payload["audio_timestamps"] = map[string]any{"graph_chars": result.Timestamps.GraphChars, "graph_times": times}
			}
			return jsonVoiceResponse(http.StatusOK, payload), nil
		}
		header := http.Header{}
		header.Set("Content-Type", firstNonEmpty(result.ContentType, "audio/mpeg"))
		header.Set("Content-Length", fmt.Sprintf("%d", len(result.Audio)))
		return &provider.Response{
			StatusCode: http.StatusOK,
			Status:     fmt.Sprintf("%d %s", http.StatusOK, http.StatusText(http.StatusOK)),
			Header:     header,
			Body:       io.NopCloser(bytes.NewReader(result.Audio)),
			QuotaUnits: 1,
		}, nil
	})
}

func (s *Service) ListTTSVoices(ctx context.Context, input VoiceListInput) (*Result, error) {
	return s.executeVoice(ctx, input.RequestID, input.ClientKey, input.PublicModel, audit.OperationTTS, modeldomain.CapabilityTTS, func(providerValue accountdomain.Provider) bool {
		_, ok := s.providers.TTS(providerValue)
		return ok
	}, func(executionCtx context.Context, providerValue accountdomain.Provider, credential accountdomain.Credential, _ string) (*provider.Response, error) {
		adapter, ok := s.providers.TTS(providerValue)
		if !ok {
			return nil, ErrNoAvailableAccount
		}
		voices, err := adapter.ListTTSVoices(executionCtx, credential)
		if err != nil {
			return voiceErrorResponse(err)
		}
		items := make([]map[string]any, 0, len(voices))
		for _, voice := range voices {
			item := map[string]any{"voice_id": voice.VoiceID, "name": voice.Name}
			if voice.Language != "" {
				item["language"] = voice.Language
			} else {
				item["language"] = nil
			}
			items = append(items, item)
		}
		return jsonVoiceResponse(http.StatusOK, map[string]any{"voices": items}), nil
	})
}

func (s *Service) GetTTSVoice(ctx context.Context, input CustomVoiceIDInput) (*Result, error) {
	return s.executeVoice(ctx, input.RequestID, input.ClientKey, input.PublicModel, audit.OperationTTS, modeldomain.CapabilityTTS, func(providerValue accountdomain.Provider) bool {
		_, ok := s.providers.TTS(providerValue)
		return ok
	}, func(executionCtx context.Context, providerValue accountdomain.Provider, credential accountdomain.Credential, _ string) (*provider.Response, error) {
		adapter, ok := s.providers.TTS(providerValue)
		if !ok {
			return nil, ErrNoAvailableAccount
		}
		voice, err := adapter.GetTTSVoice(executionCtx, credential, input.VoiceID)
		if err != nil {
			return voiceErrorResponse(err)
		}
		payload := map[string]any{"voice_id": voice.VoiceID, "name": voice.Name}
		if voice.Language != "" {
			payload["language"] = voice.Language
		} else {
			payload["language"] = nil
		}
		return jsonVoiceResponse(http.StatusOK, payload), nil
	})
}

func (s *Service) TranscribeSpeech(ctx context.Context, input STTInput) (*Result, error) {
	return s.executeVoice(ctx, input.RequestID, input.ClientKey, input.PublicModel, audit.OperationSTT, modeldomain.CapabilitySTT, func(providerValue accountdomain.Provider) bool {
		_, ok := s.providers.STT(providerValue)
		return ok
	}, func(executionCtx context.Context, providerValue accountdomain.Provider, credential accountdomain.Credential, upstream string) (*provider.Response, error) {
		adapter, ok := s.providers.STT(providerValue)
		if !ok {
			return nil, ErrNoAvailableAccount
		}
		result, err := adapter.TranscribeSpeech(executionCtx, provider.STTRequest{
			Credential: credential, Model: upstream, FileName: input.FileName, FileMIME: input.FileMIME, FileData: input.FileData,
			URL: input.URL, AudioFormat: input.AudioFormat, SampleRate: input.SampleRate, Language: input.Language, Format: input.Format,
			Multichannel: input.Multichannel, Channels: input.Channels, Diarize: input.Diarize, KeyTerms: input.KeyTerms,
			FillerWords: input.FillerWords, VADThreshold: input.VADThreshold,
		})
		if err != nil {
			return voiceErrorResponse(err)
		}
		if len(result.RawJSON) > 0 {
			header := http.Header{}
			header.Set("Content-Type", "application/json")
			header.Set("Content-Length", fmt.Sprintf("%d", len(result.RawJSON)))
			return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: header, Body: io.NopCloser(bytes.NewReader(result.RawJSON)), QuotaUnits: 1}, nil
		}
		payload := map[string]any{"text": result.Text, "language": result.Language, "duration": result.Duration}
		if len(result.Words) > 0 {
			words := make([]map[string]any, 0, len(result.Words))
			for _, word := range result.Words {
				item := map[string]any{"text": word.Text, "start": word.Start, "end": word.End}
				if word.Speaker != nil {
					item["speaker"] = *word.Speaker
				}
				words = append(words, item)
			}
			payload["words"] = words
		}
		if len(result.Channels) > 0 {
			channels := make([]map[string]any, 0, len(result.Channels))
			for _, channel := range result.Channels {
				item := map[string]any{"index": channel.Index, "text": channel.Text}
				if len(channel.Words) > 0 {
					words := make([]map[string]any, 0, len(channel.Words))
					for _, word := range channel.Words {
						wordItem := map[string]any{"text": word.Text, "start": word.Start, "end": word.End}
						if word.Speaker != nil {
							wordItem["speaker"] = *word.Speaker
						}
						words = append(words, wordItem)
					}
					item["words"] = words
				}
				channels = append(channels, item)
			}
			payload["channels"] = channels
		}
		return jsonVoiceResponse(http.StatusOK, payload), nil
	})
}

func (s *Service) CreateRealtimeClientSecret(ctx context.Context, input RealtimeClientSecretInput) (*Result, error) {
	return s.executeVoice(ctx, input.RequestID, input.ClientKey, input.PublicModel, audit.OperationRealtime, modeldomain.CapabilityRealtime, func(providerValue accountdomain.Provider) bool {
		_, ok := s.providers.RealtimeVoice(providerValue)
		return ok
	}, func(executionCtx context.Context, providerValue accountdomain.Provider, credential accountdomain.Credential, upstream string) (*provider.Response, error) {
		adapter, ok := s.providers.RealtimeVoice(providerValue)
		if !ok {
			return nil, ErrNoAvailableAccount
		}
		result, err := adapter.CreateRealtimeClientSecret(executionCtx, provider.RealtimeClientSecretRequest{
			Credential: credential, Model: firstNonEmpty(upstream, input.PublicModel), ExpiresAfter: input.ExpiresAfter, SessionJSON: input.SessionJSON,
		})
		if err != nil {
			return voiceErrorResponse(err)
		}
		if len(result.RawJSON) > 0 {
			header := http.Header{}
			header.Set("Content-Type", "application/json")
			header.Set("Content-Length", fmt.Sprintf("%d", len(result.RawJSON)))
			return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: header, Body: io.NopCloser(bytes.NewReader(result.RawJSON)), QuotaUnits: 1}, nil
		}
		return jsonVoiceResponse(http.StatusOK, map[string]any{"value": result.Value, "expires_at": result.ExpiresAt}), nil
	})
}

func (s *Service) CreateCustomVoice(ctx context.Context, input CustomVoiceCreateInput) (*Result, error) {
	return s.executeVoice(ctx, input.RequestID, input.ClientKey, input.PublicModel, audit.OperationVoice, modeldomain.CapabilityTTS, func(providerValue accountdomain.Provider) bool {
		_, ok := s.providers.CustomVoices(providerValue)
		return ok
	}, func(executionCtx context.Context, providerValue accountdomain.Provider, credential accountdomain.Credential, _ string) (*provider.Response, error) {
		adapter, ok := s.providers.CustomVoices(providerValue)
		if !ok {
			return nil, ErrNoAvailableAccount
		}
		result, err := adapter.CreateCustomVoice(executionCtx, provider.CustomVoiceCreateRequest{
			Credential: credential, Name: input.Name, Language: input.Language, Gender: input.Gender, Tone: input.Tone,
			UseCase: input.UseCase, FileName: input.FileName, FileMIME: input.FileMIME, FileData: input.FileData,
		})
		if err != nil {
			return voiceErrorResponse(err)
		}
		if len(result.RawJSON) > 0 {
			header := http.Header{}
			header.Set("Content-Type", "application/json")
			header.Set("Content-Length", fmt.Sprintf("%d", len(result.RawJSON)))
			return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: header, Body: io.NopCloser(bytes.NewReader(result.RawJSON)), QuotaUnits: 1}, nil
		}
		return jsonVoiceResponse(http.StatusOK, customVoicePayload(result)), nil
	})
}

func (s *Service) ListCustomVoices(ctx context.Context, input VoiceListInput, limit int, paginationToken string) (*Result, error) {
	return s.executeVoice(ctx, input.RequestID, input.ClientKey, input.PublicModel, audit.OperationVoice, modeldomain.CapabilityTTS, func(providerValue accountdomain.Provider) bool {
		_, ok := s.providers.CustomVoices(providerValue)
		return ok
	}, func(executionCtx context.Context, providerValue accountdomain.Provider, credential accountdomain.Credential, _ string) (*provider.Response, error) {
		adapter, ok := s.providers.CustomVoices(providerValue)
		if !ok {
			return nil, ErrNoAvailableAccount
		}
		voices, next, err := adapter.ListCustomVoices(executionCtx, credential, limit, paginationToken)
		if err != nil {
			return voiceErrorResponse(err)
		}
		items := make([]map[string]any, 0, len(voices))
		for _, voice := range voices {
			items = append(items, customVoicePayload(voice))
		}
		payload := map[string]any{"voices": items}
		if next != "" {
			payload["pagination_token"] = next
		}
		return jsonVoiceResponse(http.StatusOK, payload), nil
	})
}

func (s *Service) GetCustomVoice(ctx context.Context, input CustomVoiceIDInput) (*Result, error) {
	return s.executeVoice(ctx, input.RequestID, input.ClientKey, input.PublicModel, audit.OperationVoice, modeldomain.CapabilityTTS, func(providerValue accountdomain.Provider) bool {
		_, ok := s.providers.CustomVoices(providerValue)
		return ok
	}, func(executionCtx context.Context, providerValue accountdomain.Provider, credential accountdomain.Credential, _ string) (*provider.Response, error) {
		adapter, ok := s.providers.CustomVoices(providerValue)
		if !ok {
			return nil, ErrNoAvailableAccount
		}
		result, err := adapter.GetCustomVoice(executionCtx, credential, input.VoiceID)
		if err != nil {
			return voiceErrorResponse(err)
		}
		if len(result.RawJSON) > 0 {
			header := http.Header{}
			header.Set("Content-Type", "application/json")
			header.Set("Content-Length", fmt.Sprintf("%d", len(result.RawJSON)))
			return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: header, Body: io.NopCloser(bytes.NewReader(result.RawJSON)), QuotaUnits: 1}, nil
		}
		return jsonVoiceResponse(http.StatusOK, customVoicePayload(result)), nil
	})
}

func (s *Service) UpdateCustomVoice(ctx context.Context, input CustomVoiceUpdateInput) (*Result, error) {
	return s.executeVoice(ctx, input.RequestID, input.ClientKey, input.PublicModel, audit.OperationVoice, modeldomain.CapabilityTTS, func(providerValue accountdomain.Provider) bool {
		_, ok := s.providers.CustomVoices(providerValue)
		return ok
	}, func(executionCtx context.Context, providerValue accountdomain.Provider, credential accountdomain.Credential, _ string) (*provider.Response, error) {
		adapter, ok := s.providers.CustomVoices(providerValue)
		if !ok {
			return nil, ErrNoAvailableAccount
		}
		result, err := adapter.UpdateCustomVoice(executionCtx, provider.CustomVoiceUpdateRequest{
			Credential: credential, VoiceID: input.VoiceID, Name: input.Name, Language: input.Language, Gender: input.Gender, Tone: input.Tone, UseCase: input.UseCase,
		})
		if err != nil {
			return voiceErrorResponse(err)
		}
		if len(result.RawJSON) > 0 {
			header := http.Header{}
			header.Set("Content-Type", "application/json")
			header.Set("Content-Length", fmt.Sprintf("%d", len(result.RawJSON)))
			return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: header, Body: io.NopCloser(bytes.NewReader(result.RawJSON)), QuotaUnits: 1}, nil
		}
		return jsonVoiceResponse(http.StatusOK, customVoicePayload(result)), nil
	})
}

func (s *Service) DeleteCustomVoice(ctx context.Context, input CustomVoiceIDInput) (*Result, error) {
	return s.executeVoice(ctx, input.RequestID, input.ClientKey, input.PublicModel, audit.OperationVoice, modeldomain.CapabilityTTS, func(providerValue accountdomain.Provider) bool {
		_, ok := s.providers.CustomVoices(providerValue)
		return ok
	}, func(executionCtx context.Context, providerValue accountdomain.Provider, credential accountdomain.Credential, _ string) (*provider.Response, error) {
		adapter, ok := s.providers.CustomVoices(providerValue)
		if !ok {
			return nil, ErrNoAvailableAccount
		}
		if err := adapter.DeleteCustomVoice(executionCtx, credential, input.VoiceID); err != nil {
			return voiceErrorResponse(err)
		}
		return jsonVoiceResponse(http.StatusOK, map[string]any{"deleted": true, "voice_id": input.VoiceID}), nil
	})
}

func (s *Service) GetCustomVoiceAudio(ctx context.Context, input CustomVoiceIDInput) (*Result, error) {
	return s.executeVoice(ctx, input.RequestID, input.ClientKey, input.PublicModel, audit.OperationVoice, modeldomain.CapabilityTTS, func(providerValue accountdomain.Provider) bool {
		_, ok := s.providers.CustomVoices(providerValue)
		return ok
	}, func(executionCtx context.Context, providerValue accountdomain.Provider, credential accountdomain.Credential, _ string) (*provider.Response, error) {
		adapter, ok := s.providers.CustomVoices(providerValue)
		if !ok {
			return nil, ErrNoAvailableAccount
		}
		data, contentType, err := adapter.GetCustomVoiceAudio(executionCtx, credential, input.VoiceID)
		if err != nil {
			return voiceErrorResponse(err)
		}
		header := http.Header{}
		header.Set("Content-Type", firstNonEmpty(contentType, "audio/mpeg"))
		header.Set("Content-Length", fmt.Sprintf("%d", len(data)))
		return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: header, Body: io.NopCloser(bytes.NewReader(data)), QuotaUnits: 1}, nil
	})
}

func (s *Service) executeVoice(
	ctx context.Context,
	requestID string,
	key clientkey.Key,
	publicModel string,
	operation audit.Operation,
	capability modeldomain.Capability,
	supports voiceProviderSupport,
	execute func(context.Context, accountdomain.Provider, accountdomain.Credential, string) (*provider.Response, error),
) (*Result, error) {
	ctx, egressTrace := infraegress.WithTrace(ctx)
	startedAt := time.Now()
	eventID := newAuditEventID()
	routes, err := s.models.GetByPublicIDCandidates(ctx, publicModel)
	if err != nil {
		// Voice catalog helpers may not require a specific model when only voices are listed.
		// Fall back to any enabled route for the capability so built-in voice listing still works.
		if publicModel == "" || publicModel == "grok-voice-latest" || publicModel == "grok-stt" {
			return nil, ErrModelNotFound
		}
		return nil, ErrModelNotFound
	}
	route, err := s.selectMediaRoute(routes, key, capability, supports)
	if err != nil {
		return nil, err
	}
	externalModel := modeldomain.ExternalPublicID(route.Provider, route.PublicID)
	auditBase := audit.Record{
		EventID: eventID, RequestID: requestID, ClientKeyID: key.ID, ClientKeyName: key.Name,
		ModelRouteID: route.ID, ModelPublicID: externalModel, ModelUpstreamModel: modeldomain.DisplayUpstreamModel(route.Provider, route.UpstreamModel),
		Provider: string(route.Provider), Operation: operation, UsageSource: audit.UsageSourceNone,
	}
	if err := s.checkLedgerReady(); err != nil {
		return nil, err
	}
	writeFailureAudit := func(statusCode int, errorCode string, credential *accountdomain.Credential) {
		record := auditBase
		record.StatusCode = statusCode
		record.ErrorCode = errorCode
		record.DurationMS = time.Since(startedAt).Milliseconds()
		record.CreatedAt = time.Now().UTC()
		if credential != nil {
			accountID := credential.ID
			record.AccountID = &accountID
			record.AccountName = credential.Name
		}
		applyAuditEgress(&record, egressTrace, route.Provider)
		persistCtx, cancel := context.WithTimeout(context.Background(), finalizationTimeout)
		defer cancel()
		if auditErr := s.audits.Create(persistCtx, record); auditErr != nil {
			s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", requestID, "error", auditErr)
		}
	}
	quotaMode := s.providers.QuotaMode(route.Provider, route.UpstreamModel)
	attemptPolicy := newRoutingAttemptPolicy(int(s.maxAttempts.Load()))
	excluded := make(map[uint64]bool)
	var selection *selectionSession
	var lease *accountLease
	var credential accountdomain.Credential
	var response *provider.Response
	var lastCredentialFailure *accountdomain.Credential
	var lastCredentialError error
	for attempt := 0; attemptPolicy.allows(attempt); attempt++ {
		if selection == nil {
			selection, err = s.selector.beginSelectionSessionForKey(ctx, route.Provider, route.ID, route.UpstreamModel, quotaMode, "", excluded, false, key.AccountScope())
		}
		if err == nil {
			lease, err = selection.Acquire(ctx, excluded, false)
		}
		if err != nil {
			errorCode := "upstream_unavailable"
			var selectionFailure *SelectionUnavailableError
			if errors.As(err, &selectionFailure) {
				errorCode = selectionFailure.Code()
			}
			writeFailureAudit(http.StatusServiceUnavailable, errorCode, lastCredentialFailure)
			return nil, fmt.Errorf("%w: %w", ErrNoAvailableAccount, err)
		}
		excluded[lease.Credential.ID] = true
		credential, err = s.accounts.EnsureCredential(ctx, lease.Credential, false)
		if err != nil {
			failedCredential := lease.Credential
			lastCredentialFailure = &failedCredential
			lastCredentialError = err
			lease.Release()
			continue
		}
		lease.markSelectorUpstreamStarted()
		response, err = execute(ctx, route.Provider, credential, route.UpstreamModel)
		if err != nil {
			if isSSOCredentialRejected(err, credential) {
				s.markSSOCredentialRejected(ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
				failedCredential := credential
				lastCredentialFailure = &failedCredential
				lastCredentialError = provider.ErrUnauthorized
				lease.Release()
				continue
			}
			s.selector.MarkFailure(ctx, credential, 0, 0)
			lease.Release()
			writeFailureAudit(http.StatusBadGateway, "upstream_unavailable", &credential)
			return nil, err
		}
		if response.StatusCode == http.StatusUnauthorized && credential.AuthType == accountdomain.AuthTypeSSO {
			_, _ = readRetryableBody(response.Body)
			s.markSSOCredentialRejected(ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
			failedCredential := credential
			lastCredentialFailure = &failedCredential
			lastCredentialError = provider.ErrUnauthorized
			response = nil
			lease.Release()
			continue
		}
		if s.providers.RetryForbiddenAsEgress(credential.Provider) && response.StatusCode == http.StatusForbidden && attempt == 0 && attemptPolicy.hasNext(attempt) {
			_, _ = readRetryableBody(response.Body)
			delete(excluded, credential.ID)
			if selection != nil {
				selection.RetryAccount(credential.ID)
			}
			lease.Release()
			continue
		}
		if response.StatusCode == http.StatusTooManyRequests && attemptPolicy.hasNext(attempt) {
			retryAfter := parseRetryAfter(response.Header.Get("Retry-After"), time.Now().UTC())
			body, _ := readRetryableBody(response.Body)
			s.selector.MarkFailure(ctx, credential, response.StatusCode, retryAfter)
			response.Body = io.NopCloser(bytes.NewReader(body))
			_, _ = readRetryableBody(response.Body)
			lease.Release()
			continue
		}
		break
	}
	if response == nil {
		writeFailureAudit(http.StatusServiceUnavailable, "upstream_unavailable", lastCredentialFailure)
		if lastCredentialError == nil {
			lastCredentialError = ErrNoAvailableAccount
		}
		return nil, fmt.Errorf("%w: %w", ErrNoAvailableAccount, lastCredentialError)
	}
	accountID := credential.ID
	var once sync.Once
	finalize := func(_ Usage, _ string, errorCode string) {
		once.Do(func() {
			successful := auditRequestSucceeded(response.StatusCode, errorCode)
			lease.completeSelectorObservation(successful)
			lease.Release()
			record := auditBase
			record.AccountID, record.AccountName, record.StatusCode = &accountID, credential.Name, response.StatusCode
			record.ErrorCode = errorCode
			record.DurationMS, record.CreatedAt = time.Since(startedAt).Milliseconds(), time.Now().UTC()
			applyAuditEgress(&record, egressTrace, route.Provider)
			persistCtx, cancel := context.WithTimeout(context.Background(), finalizationTimeout)
			defer cancel()
			if err := s.audits.Create(persistCtx, record); err != nil {
				s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", requestID, "error", err)
			}
		})
	}
	return &Result{StatusCode: response.StatusCode, Status: response.Status, Header: response.Header, Body: &finalizingBody{ReadCloser: response.Body, finalize: func() { finalize(Usage{}, "", "stream_closed") }}, Finalize: finalize}, nil
}

func jsonVoiceResponse(status int, value any) *provider.Response {
	data, _ := json.Marshal(value)
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("Content-Length", fmt.Sprintf("%d", len(data)))
	return &provider.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     header,
		Body:       io.NopCloser(bytes.NewReader(data)),
		QuotaUnits: 1,
	}
}

func voiceErrorResponse(err error) (*provider.Response, error) {
	var upstream interface{ HTTPStatusCode() int; Error() string }
	if errors.As(err, &upstream) && upstream.HTTPStatusCode() > 0 {
		return jsonVoiceResponse(upstream.HTTPStatusCode(), map[string]any{"error": map[string]any{"type": "upstream_error", "message": upstream.Error()}}), nil
	}
	return nil, err
}

func customVoicePayload(value provider.CustomVoice) map[string]any {
	payload := map[string]any{
		"voice_id": value.VoiceID,
		"name":     value.Name,
	}
	if value.Language != "" {
		payload["language"] = value.Language
	}
	if value.Gender != "" {
		payload["gender"] = value.Gender
	}
	if value.Tone != "" {
		payload["tone"] = value.Tone
	}
	if value.UseCase != "" {
		payload["use_case"] = value.UseCase
	}
	if value.CreatedAt != "" {
		payload["created_at"] = value.CreatedAt
	}
	if value.UpdatedAt != "" {
		payload["updated_at"] = value.UpdatedAt
	}
	return payload
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
