package gateway

import (
	"context"
	"errors"
	"fmt"
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

type VoiceWebSocketInput struct {
	RequestID   string
	ClientKey   clientkey.Key
	PublicModel string
	// Path is one of /realtime or /stt.
	Path string
}

type VoiceWebSocketSession struct {
	Conn        provider.VoiceWebSocketConn
	Finalize    func(VoiceWebSocketOutcome)
	RequestID   string
	PublicModel string
	Provider    accountdomain.Provider
	AccountID   uint64
	AccountName string
	Operation   audit.Operation
	Capability  modeldomain.Capability
}

type VoiceWebSocketOutcome struct {
	ErrorCode            string
	UpstreamFailed       bool
	AudioDurationSeconds float64
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
	startedAt := time.Now()
	eventID := newAuditEventID()

	routes, err := s.models.GetByPublicIDCandidates(ctx, publicModel)
	if err != nil {
		return nil, ErrModelNotFound
	}
	supportsVoiceWebSocket := func(providerValue accountdomain.Provider) bool {
		_, ok := s.providers.VoiceWebSocket(providerValue)
		return ok
	}
	route, preselectedSession, err := s.selectSchedulableMediaRoute(ctx, routes, input.ClientKey, capability, true, supportsVoiceWebSocket)
	if err != nil {
		// Preserve selection-failure audits when every eligible target is cooling
		// or exhausted. The regular request loop will reproduce the error.
		route, err = s.selectMediaRoute(routes, input.ClientKey, capability, supportsVoiceWebSocket)
		if err != nil {
			return nil, err
		}
		preselectedSession = nil
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
	selection := preselectedSession
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
		lease.markSelectorUpstreamStarted()
		conn, cleanup, dialErr := adapter.DialVoiceWebSocket(ctx, provider.VoiceWebSocketRequest{
			Credential: credential,
			Path:       pathValue,
			Model:      route.UpstreamModel,
		})
		if dialErr != nil {
			if cleanup != nil {
				cleanup()
			}
			if status, ok := provider.ErrorHTTPStatus(dialErr); ok {
				failure := newHTTPUpstreamFailure(status, nil, credential.ID, credential.Name)
				failure.RequestScopedForbidden = provider.IsRequestScopedError(dialErr)
				failure.RetryAfter = provider.ErrorRetryAfter(dialErr)
				failure.Cause = dialErr
				lastErr = failure
				switch {
				case status == http.StatusUnauthorized && credential.AuthType == accountdomain.AuthTypeSSO:
					s.markSSOCredentialRejected(ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
					failed := credential
					lastCredentialFailure = &failed
					lease.Release()
					continue
				case status == http.StatusForbidden && s.providers.RetryForbiddenAsEgress(credential.Provider) && !failure.RequestScopedForbidden && attempt == 0 && attemptPolicy.hasNext(attempt):
					delete(excluded, credential.ID)
					if selection != nil {
						selection.RetryAccount(credential.ID)
					}
					lease.Release()
					continue
				case status == http.StatusPaymentRequired || status == http.StatusTooManyRequests:
					if quotaKind, _ := s.providers.QuotaKind(credential.Provider); quotaKind == provider.QuotaRemoteWindow && lease.QuotaMode != "" {
						exhausted, reconcileErr := s.accounts.ReconcileRateLimit(ctx, credential.ID, lease.QuotaMode, failure.RetryAfter)
						s.selector.MarkQuotaStateChanged(credential.Provider, credential.ID)
						if reconcileErr != nil || !exhausted {
							s.selector.MarkFailure(ctx, credential, status, failure.RetryAfter)
						}
					} else {
						s.selector.MarkFailure(ctx, credential, status, failure.RetryAfter)
					}
					if attemptPolicy.hasNext(attempt) {
						lease.Release()
						continue
					}
				case status >= http.StatusInternalServerError && attemptPolicy.hasNext(attempt):
					lease.Release()
					continue
				}
				lease.Release()
				writeFailureAudit(failure.HTTPStatus, failure.AuditCode(), &credential)
				return nil, failure
			}

			lastErr = dialErr
			if isSSOCredentialRejected(dialErr, credential) {
				s.markSSOCredentialRejected(ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
				failed := credential
				lastCredentialFailure = &failed
				lease.Release()
				continue
			}
			failure := newTransportUpstreamFailure(dialErr, credential.ID, credential.Name)
			lastErr = failure
			if ctx.Err() == nil && isRetryableTransportFailure(credential.Provider, dialErr) && attempt == 0 && attemptPolicy.hasNext(attempt) {
				delete(excluded, credential.ID)
				if selection != nil {
					selection.RetryAccount(credential.ID)
				}
				lease.Release()
				continue
			}
			lease.Release()
			writeFailureAudit(failure.HTTPStatus, failure.AuditCode(), &credential)
			return nil, failure
		}

		// Keep the account lease until the websocket finishes so scheduling stays consistent.
		accountLeaseRef := lease
		accountCredential := credential
		var finalizeOnce sync.Once
		finalize := func(outcome VoiceWebSocketOutcome) {
			finalizeOnce.Do(func() {
				if cleanup != nil {
					cleanup()
				}
				successful := strings.TrimSpace(outcome.ErrorCode) == ""
				accountLeaseRef.completeSelectorObservation(!outcome.UpstreamFailed)
				accountLeaseRef.Release()

				budget := newFinalizationBudget(string(operation), string(route.Provider))
				accountID := accountCredential.ID
				if successful && quotaMode != "" && quotaMode != "weekly" {
					var updated bool
					err := budget.run("quota_decrement", finalizationQuotaBudget, func(stageCtx context.Context) error {
						var decrementErr error
						updated, decrementErr = s.accounts.DecrementQuota(stageCtx, accountID, quotaMode, 1)
						return decrementErr
					})
					if err != nil {
						s.logger.Warn("voice_quota_decrement_failed", "provider", route.Provider, "account_id", accountID, "mode", quotaMode, "units", 1, "error", err)
					} else if updated {
						s.selector.ConsumeQuota(route.Provider, accountID, quotaMode, 1)
					}
				}
				if successful && quotaMode != "" {
					if quotaKind, _ := s.providers.QuotaKind(route.Provider); quotaKind == provider.QuotaRemoteWindow {
						s.accounts.QueueQuotaRefresh(accountID, quotaMode)
					}
				}

				record := auditBase
				record.StatusCode, record.ErrorCode = voiceWebSocketAuditOutcome(outcome)
				record.DurationMS = time.Since(startedAt).Milliseconds()
				record.CreatedAt = time.Now().UTC()
				record.AccountID = &accountID
				record.AccountName = accountCredential.Name
				applyAuditEgress(&record, egressTrace, route.Provider)
				if successful && operation == audit.OperationSTT {
					if pricing, priced := audit.EstimateOfficialSTTCost(outcome.AudioDurationSeconds, true); priced {
						record.EstimatedCostInUSDTicks = pricing.CostInUSDTicks
						record.PricingModel = pricing.Model
						record.PricingVersion = audit.OfficialPricingAsOf
					}
				}
				if auditErr := budget.run("audit", finalizationAuditBudget, func(stageCtx context.Context) error {
					return s.audits.Create(stageCtx, record)
				}); auditErr != nil {
					s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", input.RequestID, "error", auditErr)
				}
			})
		}
		return &VoiceWebSocketSession{
			Conn: conn, Finalize: finalize, RequestID: input.RequestID, PublicModel: externalModel,
			Provider: route.Provider, AccountID: credential.ID, AccountName: credential.Name,
			Operation: operation, Capability: capability,
		}, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, ErrNoAvailableAccount
}

func voiceWebSocketAuditOutcome(outcome VoiceWebSocketOutcome) (int, string) {
	errorCode := strings.TrimSpace(outcome.ErrorCode)
	if errorCode == "" {
		// Audits store the logical request outcome. The transport handshake remains
		// HTTP 101, while successful request accounting uses the common 2xx predicate.
		return http.StatusOK, ""
	}
	return http.StatusBadGateway, errorCode
}

func voiceWebSocketRoute(pathValue string) (modeldomain.Capability, audit.Operation, string, error) {
	switch strings.TrimSpace(pathValue) {
	case "/realtime":
		return modeldomain.CapabilityRealtime, audit.OperationRealtime, "grok-voice-latest", nil
	case "/stt":
		return modeldomain.CapabilitySTT, audit.OperationSTT, "grok-stt", nil
	default:
		return "", "", "", errors.New("不支持的 voice websocket path")
	}
}
