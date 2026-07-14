package gateway

import (
	"context"
	"encoding/json"
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
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

var (
	ErrModelNotFound              = errors.New("模型不存在或未启用")
	ErrNoAvailableAccount         = errors.New("没有可用上游账号")
	ErrResponseNotFound           = errors.New("Response 不存在或已过期")
	ErrResponseAccountUnavailable = errors.New("Response 绑定的上游账号不可用")
	ErrResponseStateUnsupported   = errors.New("目标模型不支持有状态 Response")
	ErrConversationUnsupported    = errors.New("目标模型不支持当前对话协议")
)

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
	PromptCacheSeed    string
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
	Get(ctx context.Context, id uint64) (modeldomain.Route, error)
	GetByPublicID(ctx context.Context, publicID string) (modeldomain.Route, error)
	GetByPublicIDCandidates(ctx context.Context, publicID string) ([]modeldomain.Route, error)
	GetByProviderUpstream(ctx context.Context, providerValue accountdomain.Provider, upstreamModel string) (modeldomain.Route, error)
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

// resolvePublicModelRoutes 同时支持下游无前缀模型名和显式指定来源的兼容名称。
func (s *Service) resolvePublicModelRoutes(ctx context.Context, publicModel string) ([]modeldomain.Route, string, error) {
	routes, err := s.models.GetByPublicIDCandidates(ctx, publicModel)
	if err == nil {
		return routes, "", nil
	}
	if s.providers == nil {
		return nil, "", err
	}
	alias, ok := s.providers.ResolveModelAlias(publicModel)
	if !ok {
		return nil, "", err
	}
	if alias.Provider != "" && alias.UpstreamModel != "" {
		route, routeErr := s.models.GetByProviderUpstream(ctx, alias.Provider, alias.UpstreamModel)
		if routeErr != nil {
			return nil, "", routeErr
		}
		return []modeldomain.Route{route}, alias.ReasoningEffort, nil
	}
	routes, err = s.models.GetByPublicIDCandidates(ctx, alias.PublicModel)
	return routes, alias.ReasoningEffort, err
}

// selectConversationRoute 从同名模型的可用来源中选择满足权限、协议和会话归属的路由。
func (s *Service) selectConversationRoute(routes []modeldomain.Route, key clientkey.Key, operation audit.Operation, path string, requireStoredResponse bool, ownership *inferencedomain.ResponseOwnership) (modeldomain.Route, error) {
	if len(routes) == 0 || s.providers == nil {
		return modeldomain.Route{}, ErrModelNotFound
	}
	fallback := routes[0]
	matchedOwnership := ownership == nil
	allowed := false
	conversationSupported := false
	storedResponseUnsupported := false
	for _, route := range routes {
		if ownership != nil && route.Provider != ownership.Provider {
			continue
		}
		matchedOwnership = true
		fallback = route
		if !s.clientKeys.CanUseModel(key, route.ID) {
			continue
		}
		allowed = true
		if !s.providers.SupportsConversation(route.Provider, string(operation)) {
			continue
		}
		conversationSupported = true
		if path == "/responses/compact" && !s.providers.SupportsResponseCompaction(route.Provider) {
			continue
		}
		if requireStoredResponse && !s.providers.SupportsStoredResponses(route.Provider) {
			storedResponseUnsupported = true
			continue
		}
		return route, nil
	}
	if !matchedOwnership {
		return fallback, ErrResponseAccountUnavailable
	}
	if !allowed {
		return fallback, clientkeyapp.ErrModelNotAllowed
	}
	if storedResponseUnsupported {
		return fallback, ErrResponseStateUnsupported
	}
	if conversationSupported && path == "/responses/compact" {
		return fallback, ErrConversationUnsupported
	}
	return fallback, ErrConversationUnsupported
}

// selectMediaRoute 从同名路由中选择同时满足媒体能力、密钥权限和 Provider 实现的来源。
func (s *Service) selectMediaRoute(routes []modeldomain.Route, key clientkey.Key, capability modeldomain.Capability, providerSupported func(accountdomain.Provider) bool) (modeldomain.Route, error) {
	if len(routes) == 0 {
		return modeldomain.Route{}, ErrModelNotFound
	}
	fallback := routes[0]
	capabilityMatched := false
	allowed := false
	for _, route := range routes {
		if route.Capability != capability {
			continue
		}
		fallback = route
		capabilityMatched = true
		if !s.clientKeys.CanUseModel(key, route.ID) {
			continue
		}
		allowed = true
		if providerSupported(route.Provider) {
			return route, nil
		}
	}
	if !capabilityMatched {
		return fallback, ErrModelNotFound
	}
	if !allowed {
		return fallback, clientkeyapp.ErrModelNotAllowed
	}
	return fallback, ErrNoAvailableAccount
}

func (s *Service) createResponseAt(ctx context.Context, input Input, path string) (*Result, error) {
	ctx, egressTrace := infraegress.WithTrace(ctx)
	startedAt := time.Now()
	eventID := newAuditEventID()
	operation := input.Operation
	if operation == "" {
		operation = audit.OperationResponses
	}
	routes, aliasEffort, err := s.resolvePublicModelRoutes(ctx, input.PublicModel)
	if err != nil {
		return nil, ErrModelNotFound
	}
	route, routeErr := s.selectConversationRoute(routes, input.ClientKey, operation, path, input.PreviousResponseID != "", nil)
	var ownership *inferencedomain.ResponseOwnership
	if input.PreviousResponseID != "" && routeErr == nil {
		value, ownershipErr := s.responses.Get(ctx, input.PreviousResponseID, input.ClientKey.ID, time.Now().UTC())
		if ownershipErr != nil {
			return nil, ErrResponseNotFound
		}
		ownership = &value
		route, routeErr = s.selectConversationRoute(routes, input.ClientKey, operation, path, true, ownership)
	}
	publicModel := modeldomain.ExternalPublicID(route.Provider, route.PublicID)
	input.PublicModel = publicModel
	if aliasEffort != "" {
		input.Body, err = rewriteAliasedModel(input.Body, publicModel, aliasEffort, operation)
		if err != nil {
			return nil, err
		}
	}
	if routeErr != nil && !errors.Is(routeErr, clientkeyapp.ErrModelNotAllowed) {
		return nil, routeErr
	}
	timing := newGenerationTiming(publicModel, route.Provider)
	timingHandedOff := false
	defer func() {
		if !timingHandedOff {
			timing.finish(s.logger, "failed")
		}
	}()
	usageSource := audit.UsageSourceUpstream
	if usageKind, _ := s.providers.UsageKind(route.Provider); usageKind == provider.UsageEstimated {
		usageSource = audit.UsageSourceEstimated
	}
	auditBase := audit.Record{
		EventID: eventID, RequestID: input.RequestID, ClientKeyID: input.ClientKey.ID, ClientKeyName: input.ClientKey.Name,
		ModelRouteID: route.ID, ModelPublicID: publicModel, ModelUpstreamModel: modeldomain.DisplayUpstreamModel(route.Provider, route.UpstreamModel),
		Provider: string(route.Provider), Operation: operation, UsageSource: usageSource, Streaming: input.Streaming,
	}
	if errors.Is(routeErr, clientkeyapp.ErrModelNotAllowed) {
		record := auditBase
		record.StatusCode = http.StatusForbidden
		record.DurationMS = time.Since(startedAt).Milliseconds()
		record.ErrorCode = "model_not_allowed"
		record.CreatedAt = time.Now().UTC()
		applyAuditEgress(&record, egressTrace, route.Provider)
		if err := s.audits.Create(ctx, record); err != nil {
			s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", input.RequestID, "error", err)
		}
		return nil, clientkeyapp.ErrModelNotAllowed
	}
	if route.Provider == accountdomain.ProviderBuild {
		input.PromptCacheKey = resolvePromptCacheIdentity(
			input.ClientKey.ID,
			route.Provider,
			route.UpstreamModel,
			operation,
			input.PromptCacheKey,
			input.PromptCacheSeed,
		)
	}
	adapter, ok := s.providers.Responses(route.Provider)
	if !ok {
		return nil, ErrNoAvailableAccount
	}
	supportsStoredResponses := s.providers.SupportsStoredResponses(route.Provider)
	if input.PreviousResponseID != "" && !supportsStoredResponses {
		return nil, ErrResponseStateUnsupported
	}
	attempts := int(s.maxAttempts.Load())
	if attempts <= 0 {
		attempts = 3
	}
	idempotencyID, _ := security.NewOpaqueToken(18)
	if ownership != nil {
		attempts = 1
	}
	pricingModel := s.providers.PricingModel(route.Provider, route.UpstreamModel)
	if reservation, priced := audit.EstimateOfficialTextReservation(pricingModel, input.Body); priced {
		if _, err := s.clientKeys.ReserveBilling(ctx, input.ClientKey, eventID, reservation.CostInUSDTicks, textBillingReservationTTL); err != nil {
			return nil, err
		}
	}
	excluded := make(map[uint64]bool)
	failureFingerprints := make(map[string]int)
	authRecoveryAttempted := make(map[uint64]bool)
	quotaMode := s.providers.QuotaMode(route.Provider, route.UpstreamModel)
	quotaProbeAttempted := false
	var lastErr error
	var lastFailure *UpstreamFailure
	failureAttempts := newFailureAttemptRecorder(http.MethodPost, path)
	forwardResponse := func(credential accountdomain.Credential) (*provider.Response, error) {
		started := time.Now()
		response, err := adapter.ForwardResponse(ctx, provider.ResponseResourceRequest{Credential: credential, Method: http.MethodPost, Path: path, Model: route.UpstreamModel, PromptCacheKey: input.PromptCacheKey, IdempotencyID: idempotencyID, Body: input.Body, Streaming: input.Streaming, NormalizeBody: true, Operation: string(operation)})
		err = failureAttempts.captureResponse(credential, started, response, err)
		timing.markUpstream(time.Since(started))
		return response, err
	}
	ensureCredential := func(credential accountdomain.Credential, force bool) (accountdomain.Credential, error) {
		started := time.Now()
		result, err := s.accounts.EnsureCredential(ctx, credential, force)
		failureAttempts.captureCredentialFailure(credential, started, force, err)
		timing.markCredential(time.Since(started))
		return result, err
	}
attemptLoop:
	for attempt := 0; attempt < attempts; attempt++ {
		var lease *accountLease
		var err error
		selectionStarted := time.Now()
		if ownership != nil {
			lease, err = s.selector.AcquirePinned(ctx, route.Provider, ownership.AccountID, route.UpstreamModel, quotaMode, true)
		} else {
			lease, err = s.selector.Acquire(ctx, route.Provider, route.UpstreamModel, quotaMode, input.PromptCacheKey, excluded, !quotaProbeAttempted)
		}
		timing.markSelection(time.Since(selectionStarted))
		if err != nil {
			if lastFailure == nil {
				lastErr = err
			}
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
		credential, err := ensureCredential(lease.Credential, false)
		if err != nil {
			lease.Release()
			lastErr = err
			lastFailure = newCredentialUpstreamFailure(err, lease.Credential.ID, lease.Credential.Name)
			continue
		}
		response, err := forwardResponse(credential)
		if err != nil {
			lease.Release()
			lastErr = err
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				lastFailure = &UpstreamFailure{HTTPStatus: 499, Code: "request_canceled", PublicMessage: "请求已取消", AccountID: credential.ID, AccountName: credential.Name, Cause: firstError(ctx.Err(), err)}
				break
			}
			lastFailure = newTransportUpstreamFailure(err, credential.ID, credential.Name)
			failureFingerprints[lastFailure.Fingerprint]++
			if failureFingerprints[lastFailure.Fingerprint] >= 2 {
				break
			}
			continue
		}
	handleResponse:
		if response.StatusCode == http.StatusUnauthorized {
			response.Body.Close()
			if credential.AuthType == accountdomain.AuthTypeSSO {
				_ = s.accounts.MarkReauthRequired(ctx, credential.ID, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
				s.selector.MarkFailure(ctx, credential, http.StatusUnauthorized, 0)
				lease.Release()
				lastErr = fmt.Errorf("%s SSO 凭据已失效", credential.Provider)
				lastFailure = newHTTPUpstreamFailure(http.StatusUnauthorized, nil, credential.ID, credential.Name)
				continue
			}
			authRecoveryAttempted[credential.ID] = true
			refreshed, refreshErr := ensureCredential(credential, true)
			if refreshErr == nil {
				response, err = forwardResponse(refreshed)
				credential = refreshed
			}
			if refreshErr != nil || err != nil {
				lease.Release()
				lastErr = firstError(refreshErr, err)
				if refreshErr != nil {
					lastFailure = newCredentialUpstreamFailure(refreshErr, credential.ID, credential.Name)
				} else if ctx.Err() != nil || errors.Is(err, context.Canceled) {
					lastFailure = &UpstreamFailure{HTTPStatus: 499, Code: "request_canceled", PublicMessage: "请求已取消", AccountID: credential.ID, AccountName: credential.Name, Cause: firstError(ctx.Err(), err)}
					break
				} else {
					lastFailure = newTransportUpstreamFailure(err, credential.ID, credential.Name)
				}
				continue
			}
			if response.StatusCode == http.StatusUnauthorized {
				body, _ := readRetryableBody(response.Body)
				_ = s.accounts.MarkReauthRequired(ctx, credential.ID, "Grok Build OAuth credential rejected after refresh")
				s.selector.MarkQuotaStateChanged(credential.Provider)
				lease.Release()
				lastErr = fmt.Errorf("刷新后上游仍返回 401")
				lastFailure = newHTTPUpstreamFailure(http.StatusUnauthorized, body, credential.ID, credential.Name)
				continue
			}
		}
		egressForbidden := s.providers.RetryForbiddenAsEgress(credential.Provider) && response.StatusCode == http.StatusForbidden
		finalEgressForbidden := egressForbidden && (attempt > 0 || attempt+1 >= attempts)
		if isRetryable(response.StatusCode) && !finalEgressForbidden {
			retryAfter := parseRetryAfter(response.Header.Get("Retry-After"), time.Now().UTC())
			body, _ := readRetryableBody(response.Body)
			if egressForbidden {
				// Web 403/code 7 表示出口浏览器会话被拒绝；Provider 已重建会话并降低节点健康，不应误伤账号。
				delete(excluded, credential.ID)
				lease.Release()
				lastErr = fmt.Errorf("Grok Web 出口会话被反机器人规则拒绝")
				lastFailure = newHTTPUpstreamFailure(response.StatusCode, body, credential.ID, credential.Name)
				continue
			}
			lastFailure = newHTTPUpstreamFailure(response.StatusCode, body, credential.ID, credential.Name)
			if s.providers.SupportsCredentialRefresh(credential.Provider) && !authRecoveryAttempted[credential.ID] && credential.EncryptedRefreshToken != "" && (lastFailure.PermanentAccountDenial || lastFailure.CredentialRejected) {
				authRecoveryAttempted[credential.ID] = true
				refreshed, refreshErr := ensureCredential(credential, true)
				if refreshErr != nil {
					lease.Release()
					lastErr = refreshErr
					lastFailure = newCredentialUpstreamFailure(refreshErr, credential.ID, credential.Name)
					continue attemptLoop
				}
				response, err = forwardResponse(refreshed)
				credential = refreshed
				if err != nil {
					lease.Release()
					lastErr = err
					if ctx.Err() != nil || errors.Is(err, context.Canceled) {
						lastFailure = &UpstreamFailure{HTTPStatus: 499, Code: "request_canceled", PublicMessage: "请求已取消", AccountID: credential.ID, AccountName: credential.Name, Cause: firstError(ctx.Err(), err)}
						break attemptLoop
					}
					lastFailure = newTransportUpstreamFailure(err, credential.ID, credential.Name)
					continue attemptLoop
				}
				goto handleResponse
			}
			failureHandled := false
			if lease.QuotaMode != "" && response.StatusCode == http.StatusTooManyRequests {
				exhausted, reconcileErr := s.accounts.ReconcileRateLimit(ctx, credential.ID, lease.QuotaMode, retryAfter)
				s.selector.MarkQuotaStateChanged(credential.Provider)
				failureHandled = reconcileErr == nil && exhausted
			} else if used, limit, exhausted := parseFreeQuotaExhaustion(body); exhausted {
				s.selector.MarkFreeQuotaExhausted(ctx, credential, used, limit)
				failureHandled = true
			} else if lastFailure.ModelQuotaExhausted {
				s.selector.MarkModelQuotaExhausted(ctx, credential, route.UpstreamModel, retryAfter)
				failureHandled = true
			} else if lastFailure.FreeQuotaExhausted {
				s.selector.MarkFreeQuotaExhausted(ctx, credential, 0, 0)
				failureHandled = true
			} else if lastFailure.QuotaExhausted {
				failureHandled = s.selector.MarkPaidQuotaExhausted(ctx, credential, lease.Billing)
			}
			if s.providers.SupportsCredentialRefresh(credential.Provider) && lastFailure.PermanentAccountDenial {
				_ = s.accounts.MarkReauthRequired(ctx, credential.ID, fmt.Sprintf("%s chat endpoint access denied", credential.Provider))
				s.selector.MarkQuotaStateChanged(credential.Provider)
				failureHandled = true
			} else if s.providers.SupportsCredentialRefresh(credential.Provider) && lastFailure.CredentialRejected {
				_ = s.accounts.MarkReauthRequired(ctx, credential.ID, fmt.Sprintf("%s credential rejected", credential.Provider))
				s.selector.MarkQuotaStateChanged(credential.Provider)
				failureHandled = true
			}
			if lastFailure.AccountScoped && !failureHandled {
				s.selector.MarkFailure(ctx, credential, response.StatusCode, retryAfter)
			}
			lease.Release()
			lastErr = fmt.Errorf("上游返回 %d", response.StatusCode)
			s.logger.Warn("upstream_request_failed", "request_id", input.RequestID, "account_id", credential.ID, "provider", credential.Provider, "status", response.StatusCode, "upstream_code", lastFailure.UpstreamCode, "account_scoped", lastFailure.AccountScoped)
			if !lastFailure.AccountScoped {
				failureFingerprints[lastFailure.Fingerprint]++
				if failureFingerprints[lastFailure.Fingerprint] >= 2 {
					break
				}
			}
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
				imagePricing, imagePriced := audit.EstimateOfficialImageCost(pricingModel, "", response.QuotaUnits)
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
				if response.StatusCode < 200 || response.StatusCode >= 300 || errorCode != "" {
					record.Attempts = failureAttempts.snapshot()
				}
				record.CreatedAt = now
				applyAuditEgress(&record, egressTrace, route.Provider)
				if err := s.audits.Create(persistCtx, record); err != nil {
					s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", input.RequestID, "error", err)
				}
				if usage.ResponseModel != "" {
					_ = s.accounts.ObserveResponseModel(persistCtx, accountID, usage.ResponseModel)
				}
				if response.StatusCode >= 200 && response.StatusCode < 300 && errorCode == "" && lease.QuotaMode != "" {
					if lease.QuotaMode != "weekly" {
						units := max(1, response.QuotaUnits)
						updated, err := s.accounts.DecrementQuota(persistCtx, accountID, lease.QuotaMode, units)
						if err != nil {
							s.logger.Warn("provider_quota_decrement_failed", "provider", credential.Provider, "account_id", accountID, "mode", lease.QuotaMode, "units", units, "error", err)
						} else if updated {
							s.selector.ConsumeQuota(credential.Provider, accountID, lease.QuotaMode, units)
						}
					}
					if quotaKind, _ := s.providers.QuotaKind(credential.Provider); quotaKind == provider.QuotaRemoteWindow {
						s.accounts.QueueQuotaRefresh(accountID, lease.QuotaMode)
					}
				}
				if supportsStoredResponses && operation == audit.OperationResponses && responseID != "" && response.StatusCode >= 200 && response.StatusCode < 300 {
					_ = s.responses.Save(persistCtx, inferencedomain.ResponseOwnership{ResponseID: responseID, AccountID: accountID, ClientKeyID: input.ClientKey.ID, Provider: route.Provider, ExpiresAt: now.Add(responseOwnershipTTL), CreatedAt: now, UpdatedAt: now})
				}
				outcome := "failed"
				if response.StatusCode >= 200 && response.StatusCode < 300 && errorCode == "" {
					outcome = "success"
				}
				timing.finish(s.logger, outcome)
			})
		}
		response.Body = &firstByteReadCloser{ReadCloser: response.Body, mark: timing.markFirstBody}
		timingHandedOff = true
		return &Result{StatusCode: response.StatusCode, Status: response.Status, Header: response.Header, Body: &finalizingBody{ReadCloser: response.Body, finalize: func() { finalize(Usage{}, "", "stream_closed") }}, Finalize: finalize}, nil
	}
	if lastFailure != nil {
		record := auditBase
		record.StatusCode = lastFailure.HTTPStatus
		record.DurationMS = time.Since(startedAt).Milliseconds()
		record.ErrorCode = lastFailure.AuditCode()
		record.Attempts = failureAttempts.snapshot()
		record.CreatedAt = time.Now().UTC()
		applyAuditEgress(&record, egressTrace, route.Provider)
		if lastFailure.AccountID != 0 {
			accountID := lastFailure.AccountID
			record.AccountID = &accountID
			record.AccountName = lastFailure.AccountName
		}
		persistCtx, cancel := context.WithTimeout(context.Background(), finalizationTimeout)
		defer cancel()
		if err := s.audits.Create(persistCtx, record); err != nil {
			s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", input.RequestID, "error", err)
		}
		return nil, lastFailure
	}
	if lastErr == nil {
		lastErr = ErrNoAvailableAccount
	}
	record := auditBase
	record.StatusCode = http.StatusServiceUnavailable
	record.DurationMS = time.Since(startedAt).Milliseconds()
	record.ErrorCode = "upstream_unavailable"
	record.Attempts = failureAttempts.snapshot()
	record.CreatedAt = time.Now().UTC()
	applyAuditEgress(&record, egressTrace, route.Provider)
	persistCtx, cancel := context.WithTimeout(context.Background(), finalizationTimeout)
	defer cancel()
	if err := s.audits.Create(persistCtx, record); err != nil {
		s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", input.RequestID, "error", err)
	}
	return nil, fmt.Errorf("%w: %w", ErrNoAvailableAccount, lastErr)
}

func rewriteAliasedModel(body []byte, publicModel, reasoningEffort string, operation audit.Operation) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("解析兼容模型请求: %w", err)
	}
	payload["model"] = publicModel
	if reasoningEffort != "" {
		switch operation {
		case audit.OperationChat:
			payload["reasoning_effort"] = reasoningEffort
		case audit.OperationMessages:
			config, _ := payload["output_config"].(map[string]any)
			if config == nil {
				config = make(map[string]any)
			}
			config["effort"] = reasoningEffort
			payload["output_config"] = config
		default:
			reasoning, _ := payload["reasoning"].(map[string]any)
			if reasoning == nil {
				reasoning = make(map[string]any)
			}
			reasoning["effort"] = reasoningEffort
			payload["reasoning"] = reasoning
		}
	}
	return json.Marshal(payload)
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
	return s.executeImage(ctx, input.RequestID, input.ClientKey, input.PublicModel, audit.OperationImage, func(executionCtx context.Context, adapter provider.ImageAdapter, credential accountdomain.Credential, upstream string) (*provider.Response, error) {
		return adapter.GenerateImage(executionCtx, provider.ImageGenerationRequest{
			Credential: credential, Model: upstream, Prompt: input.Prompt, Count: input.Count,
			Size: input.Size, AspectRatio: input.AspectRatio, Resolution: input.Resolution,
			ResponseFormat: input.ResponseFormat, Streaming: input.Streaming,
		})
	}, input.Streaming, input.Resolution, input.Count, 0)
}

func (s *Service) EditImage(ctx context.Context, input ImageEditInput) (*Result, error) {
	return s.executeImage(ctx, input.RequestID, input.ClientKey, input.PublicModel, audit.OperationImageEdit, func(executionCtx context.Context, adapter provider.ImageAdapter, credential accountdomain.Credential, upstream string) (*provider.Response, error) {
		return adapter.EditImage(executionCtx, provider.ImageEditRequest{
			Credential: credential, Model: upstream, Prompt: input.Prompt,
			ImageURLs: input.ImageURLs, Count: input.Count, Resolution: input.Resolution, ResponseFormat: input.ResponseFormat,
		})
	}, false, input.Resolution, input.Count, len(input.ImageURLs))
}

func (s *Service) executeImage(ctx context.Context, requestID string, key clientkey.Key, publicModel string, operation audit.Operation, execute func(context.Context, provider.ImageAdapter, accountdomain.Credential, string) (*provider.Response, error), streaming bool, resolution string, requestedCount, inputImageCount int) (*Result, error) {
	ctx, egressTrace := infraegress.WithTrace(ctx)
	startedAt := time.Now()
	eventID := newAuditEventID()
	routes, err := s.models.GetByPublicIDCandidates(ctx, publicModel)
	if err != nil {
		return nil, ErrModelNotFound
	}
	capability := modeldomain.CapabilityImage
	if operation == audit.OperationImageEdit {
		capability = modeldomain.CapabilityImageEdit
	}
	route, err := s.selectMediaRoute(routes, key, capability, func(providerValue accountdomain.Provider) bool {
		_, ok := s.providers.Images(providerValue)
		return ok
	})
	if err != nil {
		return nil, err
	}
	externalModel := modeldomain.ExternalPublicID(route.Provider, route.PublicID)
	adapter, ok := s.providers.Images(route.Provider)
	if !ok {
		return nil, ErrNoAvailableAccount
	}
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
			lease.Release()
			continue
		}
		response, err = execute(ctx, adapter, credential, route.UpstreamModel)
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
	if !s.providers.SupportsStoredResponses(ownership.Provider) {
		_ = s.responses.Delete(ctx, input.ResponseID, input.ClientKey.ID)
		return nil, ErrResponseNotFound
	}
	adapter, ok := s.providers.Responses(ownership.Provider)
	if !ok {
		return nil, ErrResponseAccountUnavailable
	}
	lease, err := s.selector.AcquirePinned(ctx, ownership.Provider, ownership.AccountID, "", "", false)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrResponseAccountUnavailable, err)
	}
	credential, err := s.accounts.EnsureCredential(ctx, lease.Credential, false)
	if err != nil {
		lease.Release()
		return nil, fmt.Errorf("%w: %w", ErrResponseAccountUnavailable, err)
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
	data, _, err := provider.ReadDiagnosticBody(body)
	return data, err
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
