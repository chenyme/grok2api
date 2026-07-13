package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

var (
	ErrModelNotFound              = errors.New("模型不存在或未启用")
	ErrNoAvailableAccount         = errors.New("没有可用上游账号")
	ErrResponseNotFound           = errors.New("Response 不存在或已过期")
	ErrResponseAccountUnavailable = errors.New("Response 绑定的上游账号不可用")
)

const maxRetryableBodyBytes = 64 << 10
const responseOwnershipTTL = 30 * 24 * time.Hour
const finalizationTimeout = 5 * time.Second
const textBillingReservationTTL = 2 * time.Hour
const mediaBillingReservationTTL = 24 * time.Hour

var freeQuotaUsagePattern = regexp.MustCompile(`(?i)tokens\s*\(actual/limit\)\s*:\s*([0-9]+)\s*/\s*([0-9]+)`)

type Input struct {
	RequestID          string
	ClientKey          clientkey.Key
	PublicModel        string
	Body               []byte
	Streaming          bool
	PromptCacheKey     string
	PreviousResponseID string
	Operation          audit.Operation
}

type Usage struct {
	InputTokens            int64
	CachedInputTokens      int64
	OutputTokens           int64
	ReasoningTokens        int64
	TotalTokens            int64
	CostInUSDTicks         int64
	NumSourcesUsed         int64
	NumServerSideToolsUsed int64
	ContextInputTokens     int64
	ContextOutputTokens    int64
	ResponseModel          string
}

type Result struct {
	StatusCode int
	Status     string
	Header     http.Header
	Body       io.ReadCloser
	Finalize   func(usage Usage, responseID, errorCode string)
}

type auditRecorder interface {
	Create(ctx context.Context, value audit.Record) error
}

type routeResolver interface {
	GetByPublicID(ctx context.Context, publicID string) (modeldomain.Route, error)
}

// Service 负责模型路由、账号选择、故障切换与审计收口。
type Service struct {
	models         routeResolver
	audits         auditRecorder
	accounts       *accountapp.Service
	clientKeys     *clientkeyapp.Service
	providers      *provider.Registry
	selector       *Selector
	responses      repository.ResponseRepository
	maxAttempts    atomic.Int64
	mediaJobs      repository.MediaJobRepository
	mediaQueue     chan string
	mediaMu        sync.Mutex
	mediaQueued    map[string]struct{}
	mediaWorker    int
	mediaQueueFull atomic.Uint64
	logger         *slog.Logger
}

func (s *Service) ConfigureMedia(repository repository.MediaJobRepository, concurrency int) {
	if concurrency <= 0 {
		concurrency = 4
	}
	s.mediaJobs = repository
	s.mediaWorker = concurrency
	s.mediaQueue = make(chan string, min(2048, max(64, concurrency*32)))
	s.mediaQueued = make(map[string]struct{})
}

func NewService(models routeResolver, audits auditRecorder, accounts *accountapp.Service, clientKeys *clientkeyapp.Service, providers *provider.Registry, selector *Selector, responses repository.ResponseRepository, maxAttempts int) *Service {
	service := &Service{models: models, audits: audits, accounts: accounts, clientKeys: clientKeys, providers: providers, selector: selector, responses: responses, logger: slog.Default()}
	service.UpdateMaxAttempts(maxAttempts)
	return service
}

func (s *Service) SetLogger(logger *slog.Logger) {
	if logger != nil {
		s.logger = logger
	}
}

func (s *Service) UpdateMaxAttempts(maxAttempts int) { s.maxAttempts.Store(int64(maxAttempts)) }

func (s *Service) CreateResponse(ctx context.Context, input Input) (*Result, error) {
	input.Operation = audit.OperationResponses
	return s.createResponseAt(ctx, input, "/responses")
}

func (s *Service) CreateChatCompletion(ctx context.Context, input Input) (*Result, error) {
	input.Operation = audit.OperationChat
	return s.createResponseAt(ctx, input, "/responses")
}

// CreateMessage 通过统一 Responses 上游执行 Anthropic Messages 请求。
func (s *Service) CreateMessage(ctx context.Context, input Input) (*Result, error) {
	input.Operation = audit.OperationMessages
	return s.createResponseAt(ctx, input, "/responses")
}

func (s *Service) CompactResponse(ctx context.Context, input Input) (*Result, error) {
	input.Streaming = false
	input.Operation = audit.OperationResponses
	return s.createResponseAt(ctx, input, "/responses/compact")
}

func (s *Service) createResponseAt(ctx context.Context, input Input, path string) (*Result, error) {
	startedAt := time.Now()
	eventID := newAuditEventID()
	route, err := s.models.GetByPublicID(ctx, input.PublicModel)
	if err != nil {
		return nil, ErrModelNotFound
	}
	operation := input.Operation
	if operation == "" {
		operation = audit.OperationResponses
	}
	usageSource := audit.UsageSourceUpstream
	if route.Provider == accountdomain.ProviderWeb {
		usageSource = audit.UsageSourceEstimated
	}
	auditBase := audit.Record{
		EventID: eventID, RequestID: input.RequestID, ClientKeyID: input.ClientKey.ID, ClientKeyName: input.ClientKey.Name,
		ModelRouteID: route.ID, ModelPublicID: route.PublicID, ModelUpstreamModel: route.UpstreamModel,
		Provider: string(route.Provider), Operation: operation, UsageSource: usageSource, Streaming: input.Streaming,
	}
	if !s.clientKeys.CanUseModel(input.ClientKey, route.ID) {
		record := auditBase
		record.StatusCode = http.StatusForbidden
		record.DurationMS = time.Since(startedAt).Milliseconds()
		record.ErrorCode = "model_not_allowed"
		record.CreatedAt = time.Now().UTC()
		if err := s.audits.Create(ctx, record); err != nil {
			s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", input.RequestID, "error", err)
		}
		return nil, clientkeyapp.ErrModelNotAllowed
	}
	adapter, ok := s.providers.Responses(route.Provider)
	if !ok {
		return nil, ErrNoAvailableAccount
	}
	attempts := int(s.maxAttempts.Load())
	if attempts <= 0 {
		attempts = 3
	}
	idempotencyID, _ := security.NewOpaqueToken(18)
	var ownership *inferencedomain.ResponseOwnership
	if input.PreviousResponseID != "" {
		value, err := s.responses.Get(ctx, input.PreviousResponseID, input.ClientKey.ID, time.Now().UTC())
		if err != nil {
			return nil, ErrResponseNotFound
		}
		if value.Provider != route.Provider {
			return nil, ErrResponseAccountUnavailable
		}
		ownership = &value
		attempts = 1
	}
	pricingModel := s.providers.PricingModel(route.Provider, route.UpstreamModel)
	if reservation, priced := audit.EstimateOfficialTextReservation(pricingModel, input.Body); priced {
		if _, err := s.clientKeys.ReserveBilling(ctx, input.ClientKey, eventID, reservation.CostInUSDTicks, textBillingReservationTTL); err != nil {
			return nil, err
		}
	}
	excluded := make(map[uint64]bool)
	quotaMode := s.providers.QuotaMode(route.Provider, route.UpstreamModel)
	quotaProbeAttempted := false
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		var lease *accountLease
		var err error
		if ownership != nil {
			lease, err = s.selector.AcquirePinned(ctx, route.Provider, ownership.AccountID, route.UpstreamModel, quotaMode, true)
		} else {
			lease, err = s.selector.Acquire(ctx, route.Provider, route.UpstreamModel, quotaMode, input.PromptCacheKey, excluded, !quotaProbeAttempted)
		}
		if err != nil {
			lastErr = err
			break
		}
		if lease.QuotaProbe {
			quotaProbeAttempted = true
		}
		excluded[lease.Credential.ID] = true
		if lease.QuotaProbeKind == accountdomain.QuotaRecoveryKindPaid {
			recovered, probeErr := s.accounts.ProbePaidQuota(ctx, lease.Credential)
			s.selector.MarkQuotaStateChanged(lease.Credential.Provider)
			if probeErr != nil || !recovered {
				lease.Release()
				lastErr = firstError(probeErr, fmt.Errorf("付费额度尚未恢复"))
				continue
			}
			lease.QuotaProbe = false
			lease.QuotaProbeKind = ""
			lease.Billing = nil
		}
		credential, err := s.accounts.EnsureCredential(ctx, lease.Credential, false)
		if err != nil {
			lease.Release()
			lastErr = err
			continue
		}
		response, err := adapter.ForwardResponse(ctx, provider.ResponseResourceRequest{Credential: credential, Method: http.MethodPost, Path: path, Model: route.UpstreamModel, PromptCacheKey: input.PromptCacheKey, IdempotencyID: idempotencyID, Body: input.Body, Streaming: input.Streaming, NormalizeBody: true, Operation: string(operation)})
		if err != nil {
			s.selector.MarkFailure(ctx, credential, 0, 0)
			lease.Release()
			lastErr = err
			continue
		}
		if response.StatusCode == http.StatusUnauthorized {
			response.Body.Close()
			if credential.Provider == accountdomain.ProviderWeb {
				_ = s.accounts.MarkReauthRequired(ctx, credential.ID, "Grok Web SSO credential rejected")
				s.selector.MarkFailure(ctx, credential, http.StatusUnauthorized, 0)
				lease.Release()
				lastErr = fmt.Errorf("Grok Web SSO 凭据已失效")
				continue
			}
			refreshed, refreshErr := s.accounts.EnsureCredential(ctx, credential, true)
			if refreshErr == nil {
				response, err = adapter.ForwardResponse(ctx, provider.ResponseResourceRequest{Credential: refreshed, Method: http.MethodPost, Path: path, Model: route.UpstreamModel, PromptCacheKey: input.PromptCacheKey, IdempotencyID: idempotencyID, Body: input.Body, Streaming: input.Streaming, NormalizeBody: true, Operation: string(operation)})
				credential = refreshed
			}
			if refreshErr != nil || err != nil {
				s.selector.MarkFailure(ctx, credential, http.StatusUnauthorized, 0)
				lease.Release()
				lastErr = firstError(refreshErr, err)
				continue
			}
			if response.StatusCode == http.StatusUnauthorized {
				response.Body.Close()
				s.selector.MarkFailure(ctx, credential, http.StatusUnauthorized, 0)
				lease.Release()
				lastErr = fmt.Errorf("刷新后上游仍返回 401")
				continue
			}
		}
		finalWebAntiBot := credential.Provider == accountdomain.ProviderWeb && response.StatusCode == http.StatusForbidden && (attempt > 0 || attempt+1 >= attempts)
		if isRetryable(response.StatusCode) && !finalWebAntiBot {
			retryAfter := parseRetryAfter(response.Header.Get("Retry-After"), time.Now().UTC())
			body, _ := readRetryableBody(response.Body)
			if credential.Provider == accountdomain.ProviderWeb && response.StatusCode == http.StatusForbidden {
				// Web 403/code 7 表示出口浏览器会话被拒绝；Provider 已重建会话并降低节点健康，不应误伤账号。
				delete(excluded, credential.ID)
				lease.Release()
				lastErr = fmt.Errorf("Grok Web 出口会话被反机器人规则拒绝")
				continue
			}
			if credential.Provider == accountdomain.ProviderWeb {
				if lease.QuotaMode != "" && response.StatusCode == http.StatusTooManyRequests {
					exhausted, reconcileErr := s.accounts.ReconcileWebRateLimit(ctx, credential.ID, lease.QuotaMode, retryAfter)
					s.selector.MarkQuotaStateChanged(credential.Provider)
					if reconcileErr != nil || !exhausted {
						s.selector.MarkFailure(ctx, credential, response.StatusCode, retryAfter)
					}
				}
			} else if used, limit, exhausted := parseFreeQuotaExhaustion(body); exhausted {
				s.selector.MarkFreeQuotaExhausted(ctx, credential, used, limit)
			} else {
				s.selector.MarkPaidQuotaExhausted(ctx, credential, lease.Billing)
			}
			if credential.Provider != accountdomain.ProviderWeb || response.StatusCode != http.StatusTooManyRequests {
				s.selector.MarkFailure(ctx, credential, response.StatusCode, retryAfter)
			}
			lease.Release()
			lastErr = fmt.Errorf("上游返回 %d", response.StatusCode)
			continue
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			s.selector.markSuccess(ctx, credential, lease.QuotaProbe)
		}
		accountID := credential.ID
		var once sync.Once
		finalize := func(usage Usage, responseID, errorCode string) {
			once.Do(func() {
				lease.Release()
				persistCtx, cancel := context.WithTimeout(context.Background(), finalizationTimeout)
				defer cancel()
				now := time.Now().UTC()
				record := auditBase
				record.AccountID = &accountID
				record.AccountName = credential.Name
				record.StatusCode = response.StatusCode
				record.InputTokens = usage.InputTokens
				record.CachedInputTokens = usage.CachedInputTokens
				record.OutputTokens = usage.OutputTokens
				record.ReasoningTokens = usage.ReasoningTokens
				record.TotalTokens = usage.TotalTokens
				record.CostInUSDTicks = usage.CostInUSDTicks
				imagePricing, imagePriced := audit.EstimateOfficialImageCost(route.PublicID, "", response.QuotaUnits)
				if imagePriced {
					record.MediaOutputImages = int64(max(0, response.QuotaUnits))
				}
				tokenPricing, tokenPriced := audit.EstimateOfficialCost(pricingModel, usage.InputTokens, usage.CachedInputTokens, usage.OutputTokens, usage.ContextInputTokens)
				if response.StatusCode >= 200 && response.StatusCode < 300 && errorCode == "" && imagePriced {
					record.EstimatedCostInUSDTicks = imagePricing.CostInUSDTicks
					record.PricingModel = imagePricing.Model
					record.PricingVersion = audit.OfficialPricingAsOf
				} else if tokenPriced {
					record.EstimatedCostInUSDTicks = tokenPricing.CostInUSDTicks
					record.PricingModel = tokenPricing.Model
					record.PricingVersion = audit.OfficialPricingAsOf
				}
				record.NumSourcesUsed = usage.NumSourcesUsed
				record.NumServerSideToolsUsed = usage.NumServerSideToolsUsed
				record.ContextInputTokens = usage.ContextInputTokens
				record.ContextOutputTokens = usage.ContextOutputTokens
				record.DurationMS = time.Since(startedAt).Milliseconds()
				record.ErrorCode = errorCode
				record.CreatedAt = now
				if err := s.audits.Create(persistCtx, record); err != nil {
					s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", input.RequestID, "error", err)
				}
				if usage.ResponseModel != "" {
					_ = s.accounts.ObserveResponseModel(persistCtx, accountID, usage.ResponseModel)
				}
				if response.StatusCode >= 200 && response.StatusCode < 300 && errorCode == "" && credential.Provider == accountdomain.ProviderWeb && lease.QuotaMode != "" {
					if lease.QuotaMode != "weekly" {
						units := max(1, response.QuotaUnits)
						updated, err := s.accounts.DecrementWebQuota(persistCtx, accountID, lease.QuotaMode, units)
						if err != nil {
							s.logger.Warn("web_quota_decrement_failed", "account_id", accountID, "mode", lease.QuotaMode, "units", units, "error", err)
						} else if updated {
							s.selector.ConsumeQuota(credential.Provider, accountID, lease.QuotaMode, units)
						}
					}
					s.accounts.QueueWebQuotaRefresh(accountID, lease.QuotaMode)
				}
				if operation == audit.OperationResponses && responseID != "" && response.StatusCode >= 200 && response.StatusCode < 300 {
					_ = s.responses.Save(persistCtx, inferencedomain.ResponseOwnership{ResponseID: responseID, AccountID: accountID, ClientKeyID: input.ClientKey.ID, Provider: route.Provider, ExpiresAt: now.Add(responseOwnershipTTL), CreatedAt: now, UpdatedAt: now})
				}
			})
		}
		return &Result{StatusCode: response.StatusCode, Status: response.Status, Header: response.Header, Body: &finalizingBody{ReadCloser: response.Body, finalize: func() { finalize(Usage{}, "", "stream_closed") }}, Finalize: finalize}, nil
	}
	if lastErr == nil {
		lastErr = ErrNoAvailableAccount
	}
	record := auditBase
	record.StatusCode = http.StatusServiceUnavailable
	record.DurationMS = time.Since(startedAt).Milliseconds()
	record.ErrorCode = "upstream_unavailable"
	record.CreatedAt = time.Now().UTC()
	persistCtx, cancel := context.WithTimeout(context.Background(), finalizationTimeout)
	defer cancel()
	if err := s.audits.Create(persistCtx, record); err != nil {
		s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", input.RequestID, "error", err)
	}
	return nil, fmt.Errorf("%w: %v", ErrNoAvailableAccount, lastErr)
}

type ResourceInput struct {
	ClientKey  clientkey.Key
	ResponseID string
	RawQuery   string
}

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
}

type ImageEditInput struct {
	RequestID      string
	ClientKey      clientkey.Key
	PublicModel    string
	Prompt         string
	ImageURLs      []string
	Count          int
	Resolution     string
	ResponseFormat string
}

func (s *Service) GenerateImage(ctx context.Context, input ImageGenerationInput) (*Result, error) {
	return s.executeImage(ctx, input.RequestID, input.ClientKey, input.PublicModel, audit.OperationImage, func(adapter provider.ImageAdapter, credential accountdomain.Credential, upstream string) (*provider.Response, error) {
		return adapter.GenerateImage(ctx, provider.ImageGenerationRequest{
			Credential: credential, Model: upstream, Prompt: input.Prompt, Count: input.Count,
			Size: input.Size, AspectRatio: input.AspectRatio, Resolution: input.Resolution,
			ResponseFormat: input.ResponseFormat, Streaming: input.Streaming,
		})
	}, input.Streaming, input.Resolution, input.Count, 0)
}

func (s *Service) EditImage(ctx context.Context, input ImageEditInput) (*Result, error) {
	return s.executeImage(ctx, input.RequestID, input.ClientKey, input.PublicModel, audit.OperationImageEdit, func(adapter provider.ImageAdapter, credential accountdomain.Credential, upstream string) (*provider.Response, error) {
		return adapter.EditImage(ctx, provider.ImageEditRequest{
			Credential: credential, Model: upstream, Prompt: input.Prompt,
			ImageURLs: input.ImageURLs, Count: input.Count, Resolution: input.Resolution, ResponseFormat: input.ResponseFormat,
		})
	}, false, input.Resolution, input.Count, len(input.ImageURLs))
}

func (s *Service) executeImage(ctx context.Context, requestID string, key clientkey.Key, publicModel string, operation audit.Operation, execute func(provider.ImageAdapter, accountdomain.Credential, string) (*provider.Response, error), streaming bool, resolution string, requestedCount, inputImageCount int) (*Result, error) {
	startedAt := time.Now()
	eventID := newAuditEventID()
	route, err := s.models.GetByPublicID(ctx, publicModel)
	if err != nil {
		return nil, ErrModelNotFound
	}
	if (operation == audit.OperationImage && route.Capability != modeldomain.CapabilityImage) || (operation == audit.OperationImageEdit && route.Capability != modeldomain.CapabilityImageEdit) {
		return nil, ErrModelNotFound
	}
	if !s.clientKeys.CanUseModel(key, route.ID) {
		return nil, clientkeyapp.ErrModelNotAllowed
	}
	adapter, ok := s.providers.Images(route.Provider)
	if !ok {
		return nil, ErrNoAvailableAccount
	}
	var reservation audit.PricingResult
	var priced bool
	switch operation {
	case audit.OperationImage:
		reservation, priced = audit.EstimateOfficialImageCost(route.PublicID, resolution, requestedCount)
	case audit.OperationImageEdit:
		reservation, priced = audit.EstimateOfficialImageEditCost(route.PublicID, resolution, requestedCount, inputImageCount)
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
	for attempt := 0; attempt < attempts; attempt++ {
		lease, err = s.selector.Acquire(ctx, route.Provider, route.UpstreamModel, quotaMode, "", excluded, false)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrNoAvailableAccount, err)
		}
		excluded[lease.Credential.ID] = true
		credential, err = s.accounts.EnsureCredential(ctx, lease.Credential, false)
		if err != nil {
			s.logger.Error("image_credential_failed", "event_id", eventID, "request_id", requestID, "model", route.PublicID, "provider", route.Provider, "account_id", lease.Credential.ID, "error", err)
			lease.Release()
			return nil, err
		}
		response, err = execute(adapter, credential, route.UpstreamModel)
		if err != nil {
			s.logger.Error("image_upstream_failed", "event_id", eventID, "request_id", requestID, "model", route.PublicID, "provider", route.Provider, "account_id", credential.ID, "error", err)
			s.selector.MarkFailure(ctx, credential, 0, 0)
			lease.Release()
			return nil, err
		}
		if credential.Provider == accountdomain.ProviderWeb && response.StatusCode == http.StatusForbidden && attempt == 0 && attempt+1 < attempts {
			_, _ = readRetryableBody(response.Body)
			lease.Release()
			delete(excluded, credential.ID)
			continue
		}
		if credential.Provider == accountdomain.ProviderWeb && response.StatusCode == http.StatusTooManyRequests && lease.QuotaMode != "" {
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
	if response.StatusCode == http.StatusUnauthorized && credential.Provider == accountdomain.ProviderWeb {
		_ = s.accounts.MarkReauthRequired(ctx, credential.ID, "Grok Web SSO credential rejected")
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
			record := audit.Record{
				EventID: eventID, RequestID: requestID, ClientKeyID: key.ID, ClientKeyName: key.Name,
				ModelRouteID: route.ID, ModelPublicID: route.PublicID, ModelUpstreamModel: route.UpstreamModel,
				Provider: string(route.Provider), Operation: operation, UsageSource: audit.UsageSourceNone,
				AccountID: &accountID, AccountName: credential.Name, StatusCode: response.StatusCode,
				Streaming: streaming, ErrorCode: errorCode,
				DurationMS: time.Since(startedAt).Milliseconds(), CreatedAt: time.Now().UTC(),
			}
			switch operation {
			case audit.OperationImage:
				record.MediaOutputImages = int64(max(0, requestedCount))
			case audit.OperationImageEdit:
				record.MediaInputImages = int64(max(0, inputImageCount))
				record.MediaOutputImages = int64(max(0, requestedCount))
			}
			if response.StatusCode >= 200 && response.StatusCode < 300 && errorCode == "" {
				var pricing audit.PricingResult
				var priced bool
				switch operation {
				case audit.OperationImage:
					pricing, priced = audit.EstimateOfficialImageCost(route.PublicID, resolution, requestedCount)
				case audit.OperationImageEdit:
					pricing, priced = audit.EstimateOfficialImageEditCost(route.PublicID, resolution, requestedCount, inputImageCount)
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
			if response.StatusCode >= 200 && response.StatusCode < 300 && errorCode == "" && route.Provider == accountdomain.ProviderWeb && effectiveQuotaMode != "" {
				if effectiveQuotaMode != "weekly" {
					units := max(1, response.QuotaUnits)
					updated, err := s.accounts.DecrementWebQuota(persistCtx, accountID, effectiveQuotaMode, units)
					if err != nil {
						s.logger.Warn("web_quota_decrement_failed", "account_id", accountID, "mode", effectiveQuotaMode, "units", units, "error", err)
					} else if updated {
						s.selector.ConsumeQuota(route.Provider, accountID, effectiveQuotaMode, units)
					}
				}
				s.accounts.QueueWebQuotaRefresh(accountID, effectiveQuotaMode)
			}
		})
	}
	finalizationOwnsReservation = true
	return &Result{StatusCode: response.StatusCode, Status: response.Status, Header: response.Header, Body: &finalizingBody{ReadCloser: response.Body, finalize: func() { finalize(Usage{}, "", "stream_closed") }}, Finalize: finalize}, nil
}

func (s *Service) cancelBillingReservation(eventID string) {
	ctx, cancel := context.WithTimeout(context.Background(), finalizationTimeout)
	defer cancel()
	if err := s.clientKeys.CancelBilling(ctx, eventID); err != nil {
		s.logger.Error("billing_reservation_cancel_failed", "event_id", eventID, "error", err)
	}
}

func newAuditEventID() string {
	value, err := security.NewOpaqueToken(18)
	if err != nil || value == "" {
		return fmt.Sprintf("evt_%d", time.Now().UnixNano())
	}
	return "evt_" + value
}

func (s *Service) GetResponse(ctx context.Context, input ResourceInput) (*Result, error) {
	return s.forwardOwnedResponse(ctx, input, http.MethodGet)
}

func (s *Service) DeleteResponse(ctx context.Context, input ResourceInput) (*Result, error) {
	return s.forwardOwnedResponse(ctx, input, http.MethodDelete)
}

func (s *Service) forwardOwnedResponse(ctx context.Context, input ResourceInput, method string) (*Result, error) {
	ownership, err := s.responses.Get(ctx, input.ResponseID, input.ClientKey.ID, time.Now().UTC())
	if err != nil {
		return nil, ErrResponseNotFound
	}
	adapter, ok := s.providers.Responses(ownership.Provider)
	if !ok {
		return nil, ErrResponseAccountUnavailable
	}
	lease, err := s.selector.AcquirePinned(ctx, ownership.Provider, ownership.AccountID, "", "", false)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrResponseAccountUnavailable, err)
	}
	credential, err := s.accounts.EnsureCredential(ctx, lease.Credential, false)
	if err != nil {
		lease.Release()
		return nil, fmt.Errorf("%w: %v", ErrResponseAccountUnavailable, err)
	}
	path := "/responses/" + url.PathEscape(input.ResponseID)
	if input.RawQuery != "" {
		path += "?" + input.RawQuery
	}
	response, err := adapter.ForwardResponse(ctx, provider.ResponseResourceRequest{Credential: credential, Method: method, Path: path})
	if err != nil {
		lease.Release()
		return nil, err
	}
	if response.StatusCode == http.StatusUnauthorized {
		response.Body.Close()
		refreshed, refreshErr := s.accounts.EnsureCredential(ctx, credential, true)
		if refreshErr != nil {
			lease.Release()
			return nil, refreshErr
		}
		response, err = adapter.ForwardResponse(ctx, provider.ResponseResourceRequest{Credential: refreshed, Method: method, Path: path})
		credential = refreshed
		if err != nil {
			lease.Release()
			return nil, err
		}
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		s.selector.markSuccess(ctx, credential, false)
		if method == http.MethodDelete {
			_ = s.responses.Delete(ctx, input.ResponseID, input.ClientKey.ID)
		}
	} else if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone {
		_ = s.responses.Delete(ctx, input.ResponseID, input.ClientKey.ID)
	}
	var once sync.Once
	release := func() { once.Do(lease.Release) }
	finalize := func(Usage, string, string) { release() }
	return &Result{StatusCode: response.StatusCode, Status: response.Status, Header: response.Header, Body: &finalizingBody{ReadCloser: response.Body, finalize: release}, Finalize: finalize}, nil
}

func readRetryableBody(body io.ReadCloser) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	defer body.Close()
	return io.ReadAll(io.LimitReader(body, maxRetryableBodyBytes))
}

func parseFreeQuotaExhaustion(body []byte) (int64, int64, bool) {
	text := strings.ToLower(string(body))
	if !strings.Contains(text, "subscription:free-usage-exhausted") {
		return 0, 0, false
	}
	matches := freeQuotaUsagePattern.FindSubmatch(body)
	if len(matches) != 3 {
		return 0, 0, true
	}
	used, usedErr := strconv.ParseInt(string(matches[1]), 10, 64)
	limit, limitErr := strconv.ParseInt(string(matches[2]), 10, 64)
	if usedErr != nil || limitErr != nil {
		return 0, 0, true
	}
	return used, limit, true
}

type finalizingBody struct {
	io.ReadCloser
	finalize func()
}

func (b *finalizingBody) Close() error {
	err := b.ReadCloser.Close()
	if b.finalize != nil {
		b.finalize()
	}
	return err
}

func isRetryable(status int) bool {
	return status == 402 || status == 403 || status == 429 || status >= 500
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if parsed, err := http.ParseTime(value); err == nil && parsed.After(now) {
		return parsed.Sub(now)
	}
	return 0
}

func firstError(values ...error) error {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return errors.New("未知上游错误")
}
