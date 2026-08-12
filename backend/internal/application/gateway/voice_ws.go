package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

type VoiceWebSocketInput struct {
	RequestID   string
	ClientKey   clientkey.Key
	PublicModel string
	// Path is one of /realtime, /tts, /stt.
	Path  string
	Query string
}

type VoiceWebSocketSession struct {
	Conn       provider.VoiceWebSocketConn
	Close      func()
	RequestID  string
	PublicModel string
	Provider   accountdomain.Provider
	AccountID  uint64
	AccountName string
	Operation  audit.Operation
	Capability modeldomain.Capability
}

// OpenVoiceWebSocket selects a Console account and dials the upstream voice websocket.
func (s *Service) OpenVoiceWebSocket(ctx context.Context, input VoiceWebSocketInput) (*VoiceWebSocketSession, error) {
	pathValue := strings.TrimSpace(input.Path)
	capability, operation, defaultModel, err := voiceWebSocketRoute(pathValue)
	if err != nil {
		return nil, err
	}
	publicModel := strings.TrimSpace(input.PublicModel)
	if publicModel == "" {
		publicModel = defaultModel
	}

	ctx, egressTrace := infraegress.WithTrace(ctx)
	_ = egressTrace
	startedAt := time.Now()
	eventID := newAuditEventID()

	routes, err := s.models.GetByPublicIDCandidates(ctx, publicModel)
	if err != nil {
		return nil, ErrModelNotFound
	}
	route, err := s.selectMediaRoute(routes, input.ClientKey, capability, func(providerValue accountdomain.Provider) bool {
		_, ok := s.providers.VoiceWebSocket(providerValue)
		return ok
	})
	if err != nil {
		return nil, err
	}
	externalModel := modeldomain.ExternalPublicID(route.Provider, route.PublicID)
	auditBase := audit.Record{
		EventID: eventID, RequestID: input.RequestID, ClientKeyID: input.ClientKey.ID, ClientKeyName: input.ClientKey.Name,
		ModelRouteID: route.ID, ModelPublicID: externalModel, ModelUpstreamModel: modeldomain.DisplayUpstreamModel(route.Provider, route.UpstreamModel),
		Provider: string(route.Provider), Operation: operation, UsageSource: audit.UsageSourceNone, Streaming: true,
	}
	if err := s.checkLedgerReady(); err != nil {
		return nil, err
	}

	quotaMode := s.providers.QuotaMode(route.Provider, route.UpstreamModel)
	attemptPolicy := newRoutingAttemptPolicy(int(s.maxAttempts.Load()))
	excluded := make(map[uint64]bool)
	var selection *selectionSession
	var lease *accountLease
	var credential accountdomain.Credential
	var lastCredentialFailure *accountdomain.Credential
	var lastErr error

	writeFailureAudit := func(statusCode int, errorCode string, cred *accountdomain.Credential) {
		record := auditBase
		record.StatusCode = statusCode
		record.ErrorCode = errorCode
		record.DurationMS = time.Since(startedAt).Milliseconds()
		record.CreatedAt = time.Now().UTC()
		if cred != nil {
			accountID := cred.ID
			record.AccountID = &accountID
			record.AccountName = cred.Name
		}
		applyAuditEgress(&record, egressTrace, route.Provider)
		persistCtx, cancel := context.WithTimeout(context.Background(), finalizationTimeout)
		defer cancel()
		if auditErr := s.audits.Create(persistCtx, record); auditErr != nil {
			s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", input.RequestID, "error", auditErr)
		}
	}

	for attempt := 0; attemptPolicy.allows(attempt); attempt++ {
		if selection == nil {
			selection, err = s.selector.beginSelectionSessionForKey(ctx, route.Provider, route.ID, route.UpstreamModel, quotaMode, "", excluded, false, input.ClientKey.AccountScope())
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
			failed := lease.Credential
			lastCredentialFailure = &failed
			lastErr = err
			lease.Release()
			continue
		}
		adapter, ok := s.providers.VoiceWebSocket(route.Provider)
		if !ok {
			lease.Release()
			writeFailureAudit(http.StatusBadGateway, "upstream_unavailable", &credential)
			return nil, ErrNoAvailableAccount
		}
		conn, cleanup, dialErr := adapter.DialVoiceWebSocket(ctx, provider.VoiceWebSocketRequest{
			Credential: credential,
			Path:       pathValue,
			Query:      input.Query,
			Model:      route.UpstreamModel,
		})
		if dialErr != nil {
			lastErr = dialErr
			if isSSOCredentialRejected(dialErr, credential) {
				s.markSSOCredentialRejected(ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
				failed := credential
				lastCredentialFailure = &failed
				lease.Release()
				continue
			}
			s.selector.MarkFailure(ctx, credential, 0, 0)
			lease.Release()
			if cleanup != nil {
				cleanup()
			}
			writeFailureAudit(http.StatusBadGateway, "upstream_unavailable", &credential)
			return nil, dialErr
		}

		// Keep the account lease until the websocket finishes so scheduling stays consistent.
		accountLeaseRef := lease
		accountCredential := credential
		closeFn := func() {
			if cleanup != nil {
				cleanup()
			}
			s.selector.MarkSuccess(context.WithoutCancel(ctx), accountCredential)
			accountLeaseRef.Release()
			record := auditBase
			record.StatusCode = http.StatusSwitchingProtocols
			record.DurationMS = time.Since(startedAt).Milliseconds()
			record.CreatedAt = time.Now().UTC()
			accountID := accountCredential.ID
			record.AccountID = &accountID
			record.AccountName = accountCredential.Name
			applyAuditEgress(&record, egressTrace, route.Provider)
			persistCtx, cancel := context.WithTimeout(context.Background(), finalizationTimeout)
			defer cancel()
			if auditErr := s.audits.Create(persistCtx, record); auditErr != nil {
				s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", input.RequestID, "error", auditErr)
			}
		}
		return &VoiceWebSocketSession{
			Conn: conn, Close: closeFn, RequestID: input.RequestID, PublicModel: externalModel,
			Provider: route.Provider, AccountID: credential.ID, AccountName: credential.Name,
			Operation: operation, Capability: capability,
		}, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, ErrNoAvailableAccount
}

func voiceWebSocketRoute(pathValue string) (modeldomain.Capability, audit.Operation, string, error) {
	switch strings.TrimSpace(pathValue) {
	case "/realtime":
		return modeldomain.CapabilityRealtime, audit.OperationRealtime, "grok-voice-latest", nil
	case "/tts":
		return modeldomain.CapabilityTTS, audit.OperationTTS, "grok-voice-latest", nil
	case "/stt":
		return modeldomain.CapabilitySTT, audit.OperationSTT, "grok-stt", nil
	default:
		return "", "", "", errors.New("不支持的 voice websocket path")
	}
}
