package gateway

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// ImageGenerationInput 表示图片生成用例已经完成协议校验后的输入。
type ImageGenerationInput struct {
	RequestID      string
	ClientKey      clientkey.Key
	PublicModel    string
	Prompt         string
	Count          int
	Size           string
	AspectRatio    string
	Resolution     string
	ResponseFormat string
	Streaming      bool
	PartialImages  int
}

// ImageEditInput 表示图片编辑用例已经完成协议校验后的输入。
type ImageEditInput struct {
	RequestID      string
	ClientKey      clientkey.Key
	PublicModel    string
	Prompt         string
	ImageURLs      []string
	Count          int
	Size           string
	AspectRatio    string
	Resolution     string
	ResponseFormat string
	Streaming      bool
	PartialImages  int
}

type imageProviderSupport func(accountdomain.Provider) bool

type imageExecution func(context.Context, accountdomain.Provider, accountdomain.Credential, string) (*provider.Response, error)

// GenerateImage 选择支持图片生成的路由和账号，并返回可统一审计的上游响应。
func (s *Service) GenerateImage(ctx context.Context, input ImageGenerationInput) (*Result, error) {
	return s.executeImage(ctx, input.RequestID, input.ClientKey, input.PublicModel, audit.OperationImage, modeldomain.CapabilityImage, func(providerValue accountdomain.Provider) bool {
		_, ok := s.providers.ImageGeneration(providerValue)
		return ok
	}, func(executionCtx context.Context, providerValue accountdomain.Provider, credential accountdomain.Credential, upstream string) (*provider.Response, error) {
		adapter, ok := s.providers.ImageGeneration(providerValue)
		if !ok {
			return nil, ErrNoAvailableAccount
		}
		return adapter.GenerateImage(executionCtx, provider.ImageGenerationRequest{
			Credential: credential, Model: upstream, Prompt: input.Prompt, Count: input.Count,
			Size: input.Size, AspectRatio: input.AspectRatio, Resolution: input.Resolution,
			ResponseFormat: input.ResponseFormat, Streaming: input.Streaming, PartialImages: input.PartialImages,
		})
	}, input.Streaming, input.Resolution, input.Count, 0)
}

// EditImage 选择支持图片编辑的路由和账号，并返回可统一审计的上游响应。
func (s *Service) EditImage(ctx context.Context, input ImageEditInput) (*Result, error) {
	return s.executeImage(ctx, input.RequestID, input.ClientKey, input.PublicModel, audit.OperationImageEdit, modeldomain.CapabilityImageEdit, func(providerValue accountdomain.Provider) bool {
		_, ok := s.providers.ImageEdit(providerValue)
		return ok
	}, func(executionCtx context.Context, providerValue accountdomain.Provider, credential accountdomain.Credential, upstream string) (*provider.Response, error) {
		adapter, ok := s.providers.ImageEdit(providerValue)
		if !ok {
			return nil, ErrNoAvailableAccount
		}
		return adapter.EditImage(executionCtx, provider.ImageEditRequest{
			Credential: credential, Model: upstream, Prompt: input.Prompt,
			ImageURLs: input.ImageURLs, Count: input.Count, Size: input.Size, AspectRatio: input.AspectRatio,
			Resolution: input.Resolution, ResponseFormat: input.ResponseFormat,
			Streaming: input.Streaming, PartialImages: input.PartialImages,
		})
	}, input.Streaming, input.Resolution, input.Count, len(input.ImageURLs))
}

func (s *Service) executeImage(
	ctx context.Context,
	requestID string,
	key clientkey.Key,
	publicModel string,
	operation audit.Operation,
	capability modeldomain.Capability,
	supports imageProviderSupport,
	execute imageExecution,
	streaming bool,
	resolution string,
	requestedCount int,
	inputImageCount int,
) (*Result, error) {
	ctx, egressTrace := infraegress.WithTrace(ctx)
	startedAt := time.Now()
	eventID := newAuditEventID()
	routes, err := s.models.GetByPublicIDCandidates(ctx, publicModel)
	if err != nil {
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
		Provider: string(route.Provider), Operation: operation, UsageSource: audit.UsageSourceNone, Streaming: streaming,
	}
	if operation == audit.OperationImageEdit {
		auditBase.MediaInputImages = int64(max(0, inputImageCount))
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
	pricingModel := s.providers.PricingModel(route.Provider, route.UpstreamModel)
	var reservation audit.PricingResult
	var priced bool
	switch operation {
	case audit.OperationImage:
		reservation, priced = audit.EstimateOfficialImageCost(pricingModel, resolution, requestedCount)
	case audit.OperationImageEdit:
		reservation, priced = audit.EstimateOfficialImageEditCost(pricingModel, resolution, requestedCount, inputImageCount)
	}
	reserved := false
	if priced {
		reserved, err = s.clientKeys.ReserveBilling(ctx, key, eventID, reservation.CostInUSDTicks, mediaBillingReservationTTL)
		if err != nil {
			return nil, err
		}
	}
	finalizationOwnsReservation := false
	defer func() {
		if reserved && !finalizationOwnsReservation {
			s.cancelBillingReservation(eventID)
		}
	}()
	quotaMode := s.providers.QuotaMode(route.Provider, route.UpstreamModel)
	attempts := int(s.maxAttempts.Load())
	if attempts <= 0 {
		attempts = 3
	}
	excluded := make(map[uint64]bool)
	var lease *accountLease
	var credential accountdomain.Credential
	var response *provider.Response
	var lastCredentialFailure *accountdomain.Credential
	var lastCredentialError error
	for attempt := 0; attempt < attempts; attempt++ {
		lease, err = s.selector.Acquire(ctx, route.Provider, route.UpstreamModel, quotaMode, "", excluded, false)
		if err != nil {
			writeFailureAudit(http.StatusServiceUnavailable, "upstream_unavailable", lastCredentialFailure)
			return nil, fmt.Errorf("%w: %w", ErrNoAvailableAccount, err)
		}
		excluded[lease.Credential.ID] = true
		credential, err = s.accounts.EnsureCredential(ctx, lease.Credential, false)
		if err != nil {
			s.logger.Error("image_credential_failed", "event_id", eventID, "request_id", requestID, "model", externalModel, "provider", route.Provider, "account_id", lease.Credential.ID, "error", err)
			failedCredential := lease.Credential
			lastCredentialFailure = &failedCredential
			lastCredentialError = err
			lease.Release()
			continue
		}
		response, err = execute(ctx, route.Provider, credential, route.UpstreamModel)
		if err != nil {
			s.logger.Error("image_upstream_failed", "event_id", eventID, "request_id", requestID, "model", externalModel, "provider", route.Provider, "account_id", credential.ID, "error", err)
			if !provider.IsMediaPostProcessingError(err) {
				s.selector.MarkFailure(ctx, credential, 0, 0)
			}
			lease.Release()
			errorCode := "upstream_unavailable"
			if provider.IsMediaPostProcessingError(err) {
				errorCode = "media_postprocessing_failed"
			}
			writeFailureAudit(http.StatusBadGateway, errorCode, &credential)
			return nil, err
		}
		if s.providers.RetryForbiddenAsEgress(credential.Provider) && response.StatusCode == http.StatusForbidden && attempt == 0 && attempt+1 < attempts {
			_, _ = readRetryableBody(response.Body)
			lease.Release()
			delete(excluded, credential.ID)
			continue
		}
		if quotaKind, _ := s.providers.QuotaKind(credential.Provider); quotaKind == provider.QuotaRemoteWindow && response.StatusCode == http.StatusTooManyRequests && lease.QuotaMode != "" {
			retryAfter := parseRetryAfter(response.Header.Get("Retry-After"), time.Now().UTC())
			exhausted, reconcileErr := s.accounts.ReconcileWebRateLimit(ctx, credential.ID, lease.QuotaMode, retryAfter)
			s.selector.MarkQuotaStateChanged(credential.Provider)
			if reconcileErr != nil || !exhausted {
				s.selector.MarkFailure(ctx, credential, response.StatusCode, retryAfter)
			}
			if attempt+1 < attempts {
				_, _ = readRetryableBody(response.Body)
				lease.Release()
				continue
			}
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
	if response.StatusCode == http.StatusUnauthorized && credential.AuthType == accountdomain.AuthTypeSSO {
		_ = s.accounts.MarkReauthRequired(ctx, credential.ID, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
		s.selector.MarkFailure(ctx, credential, http.StatusUnauthorized, 0)
	}
	effectiveQuotaMode := lease.QuotaMode
	accountID := credential.ID
	var once sync.Once
	finalize := func(_ Usage, _ string, errorCode string) {
		once.Do(func() {
			lease.Release()
			persistCtx, cancel := context.WithTimeout(context.Background(), finalizationTimeout)
			defer cancel()
			record := auditBase
			record.AccountID, record.AccountName, record.StatusCode = &accountID, credential.Name, response.StatusCode
			record.ErrorCode = errorCode
			record.DurationMS, record.CreatedAt = time.Since(startedAt).Milliseconds(), time.Now().UTC()
			applyAuditEgress(&record, egressTrace, route.Provider)
			if response.StatusCode >= 200 && response.StatusCode < 300 && errorCode == "" {
				record.MediaOutputImages = int64(max(0, requestedCount))
				var pricing audit.PricingResult
				var priced bool
				switch operation {
				case audit.OperationImage:
					pricing, priced = audit.EstimateOfficialImageCost(pricingModel, resolution, requestedCount)
				case audit.OperationImageEdit:
					pricing, priced = audit.EstimateOfficialImageEditCost(pricingModel, resolution, requestedCount, inputImageCount)
				}
				if priced {
					record.EstimatedCostInUSDTicks = pricing.CostInUSDTicks
					record.PricingModel = pricing.Model
					record.PricingVersion = audit.OfficialPricingAsOf
				}
			}
			if err := s.audits.Create(persistCtx, record); err != nil {
				s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", requestID, "error", err)
			}
			quotaKind, _ := s.providers.QuotaKind(route.Provider)
			if response.StatusCode >= 200 && response.StatusCode < 300 && errorCode == "" && quotaKind == provider.QuotaRemoteWindow && effectiveQuotaMode != "" {
				if effectiveQuotaMode != "weekly" {
					units := max(1, response.QuotaUnits)
					updated, err := s.accounts.DecrementWebQuota(persistCtx, accountID, effectiveQuotaMode, units)
					if err != nil {
						s.logger.Warn("web_quota_decrement_failed", "account_id", accountID, "mode", effectiveQuotaMode, "units", units, "error", err)
					} else if updated {
						s.selector.ConsumeQuota(route.Provider, accountID, effectiveQuotaMode, units)
					}
				}
				s.accounts.QueueQuotaRefresh(accountID, effectiveQuotaMode)
			}
		})
	}
	finalizationOwnsReservation = true
	return &Result{StatusCode: response.StatusCode, Status: response.Status, Header: response.Header, Body: &finalizingBody{ReadCloser: response.Body, finalize: func() { finalize(Usage{}, "", "stream_closed") }}, Finalize: finalize}, nil
}
