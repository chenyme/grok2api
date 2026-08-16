package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
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
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

var (
	ErrModelNotFound              = errors.New("模型不存在或未启用")
	ErrNoAvailableAccount         = errors.New("没有可用上游账号")
	ErrResponseNotFound           = errors.New("Response 不存在或已过期")
	ErrResponseAccountUnavailable = errors.New("Response 绑定的上游账号不可用")
	ErrResponseStateUnsupported   = errors.New("目标模型不支持有状态 Response")
	ErrConversationUnsupported    = errors.New("目标模型不支持当前对话协议")
	ErrVideoInputTooLarge         = errors.New("视频参考图片编码后总输入超过 32 MiB")
	ErrVideoInputUnavailable      = errors.New("视频临时输入不存在或已过期")
	ErrVideoParameterInvalid      = errors.New("视频请求参数无效")
	ErrVideoOperationUnsupported  = errors.New("视频编辑/延长仅支持路由到 Console grok-imagine-video")
	ErrLedgerUnavailable          = errors.New("计费账本暂不可用")
)

const responseOwnershipTTL = 30 * 24 * time.Hour
const finalizationTimeout = 5 * time.Second
const minimumTextBillingReservationTTL = 2 * time.Hour
const billingReservationCrashGrace = 10 * time.Minute
const mediaBillingReservationTTL = 24 * time.Hour
const modelCatalogRefreshTimeout = 30 * time.Second
const accountStateWriteTimeout = 3 * time.Second
const unlimitedRoutingAttempts = -1

type routingAttemptPolicy struct {
	limit     int
	unlimited bool
}

func newRoutingAttemptPolicy(configured int) routingAttemptPolicy {
	if configured == unlimitedRoutingAttempts {
		return routingAttemptPolicy{unlimited: true}
	}
	if configured <= 0 {
		configured = 3
	}
	return routingAttemptPolicy{limit: configured}
}

func (p routingAttemptPolicy) allows(attempt int) bool {
	return p.unlimited || attempt < p.limit
}

func (p routingAttemptPolicy) hasNext(attempt int) bool {
	return p.unlimited || attempt+1 < p.limit
}

// nonAccountFailureFingerprintLimit 仅限制非账号归因故障（网络/5xx 等）。
// 账号级失败持续换号，避免少量瞬时上游故障过早放弃仍可用的凭证池。
const nonAccountFailureFingerprintLimit = 16

// Stream idle failures are commonly provider-wide rather than account-wide.
// Allow one compensating account switch, then stop to prevent a silent
// upstream from multiplying a long idle deadline across the whole pool.
const streamIdleFailureFingerprintLimit = 2

var freeQuotaUsagePattern = regexp.MustCompile(`(?i)tokens\s*\(actual/limit\)\s*:\s*([0-9]+)\s*/\s*([0-9]+)`)

type Input struct {
	RequestID       string
	ClientKey       clientkey.Key
	PublicModel     string
	Body            []byte
	Streaming       bool
	PromptCacheKey  string
	PromptCacheSeed string
	// AllowClientToolCacheRoute indicates that the client request is compatible with the Build mixed-tool cache route.
	// It only controls whether native x_search is added to existing client tools; it is not an authentication result.
	AllowClientToolCacheRoute bool
	PreviousResponseID        string
	// GrokTurnIndex forwards only the turn supplied by a real Grok Shell client; the server never infers or increments it.
	GrokTurnIndex string
	Operation     audit.Operation
	// ForcedEgressNodeID is an internal-only administrator probe constraint.
	// Public inference handlers never populate it.
	ForcedEgressNodeID uint64
}

type Usage struct {
	// Reported distinguishes a real upstream/estimated usage object from the
	// zero value used when a response fails before usage is available. Token
	// counts may legitimately all be zero, so the numeric fields cannot carry
	// this presence information by themselves.
	Reported               bool
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
	StatusCode          int
	Status              string
	Header              http.Header
	Body                io.ReadCloser
	MarkFirstToken      func()
	RecordStreamFailure func(StreamFailureDiagnostic)
	Finalize            func(usage Usage, responseID, errorCode string)
}

// StreamFailureDiagnostic safely projects a failure termination event returned in-stream after downstream 2xx headers.
// Body contains only transport-extracted error fields and still receives the standard redaction and size limits.
type StreamFailureDiagnostic struct {
	Body          []byte
	BodyTruncated bool
}

type auditRecorder interface {
	Create(ctx context.Context, value audit.Record) error
}

type ledgerReadinessChecker interface {
	CheckLedgerReady() error
}

type routeResolver interface {
	Get(ctx context.Context, id uint64) (modeldomain.Route, error)
	GetByPublicID(ctx context.Context, publicID string) (modeldomain.Route, error)
	GetByPublicIDCandidates(ctx context.Context, publicID string) ([]modeldomain.Route, error)
	GetByProviderUpstream(ctx context.Context, providerValue accountdomain.Provider, upstreamModel string) (modeldomain.Route, error)
}

// videoAssetStore archives and reads video results generated by a Provider.
type videoAssetStore interface {
	SaveVideo(ctx context.Context, jobID, contentType string, body io.Reader) (mediadomain.Asset, error)
	OpenVideo(ctx context.Context, id string) (mediadomain.Asset, io.ReadCloser, error)
	OpenInputAsset(ctx context.Context, id string) (mediadomain.Asset, io.ReadCloser, error)
	ReleaseInputAssets(ctx context.Context, references []string) error
}

type accountModelSyncer interface {
	SyncAccount(ctx context.Context, accountID uint64) (int, error)
}

// Service handles model routing, account selection, failover, and audit finalization.
type Service struct {
	models                      routeResolver
	audits                      auditRecorder
	accounts                    *accountapp.Service
	clientKeys                  *clientkeyapp.Service
	providers                   *provider.Registry
	selector                    *Selector
	responses                   repository.ResponseRepository
	maxAttempts                 atomic.Int64
	videoMaxAttempts            atomic.Int64
	buildForbiddenReauth        atomic.Pointer[buildForbiddenReauthPolicy]
	requestTimeout              atomic.Int64
	mediaJobs                   repository.MediaJobRepository
	mediaAssets                 videoAssetStore
	mediaQueue                  chan string
	mediaMu                     sync.Mutex
	mediaQueued                 map[string]struct{}
	mediaWorker                 int
	mediaInputSlots             chan struct{}
	mediaQueueFull              atomic.Uint64
	logger                      *slog.Logger
	rateLimitMu                 sync.Mutex
	rateLimitActive             atomic.Bool
	rateLimitNextExpiry         atomic.Int64
	rateLimits                  map[string]teamModelRateLimit
	rateLimitTeams              map[uint64]teamRateLimitObservation
	modelSyncMu                 sync.Mutex
	modelSyncing                map[uint64]struct{}
	markBuildChatDeniedAsReauth atomic.Bool
}

type teamModelRateLimit struct {
	TeamFingerprint string
	Until           time.Time
}

type teamRateLimitObservation struct {
	Fingerprint string
	ExpiresAt   time.Time
}

type buildForbiddenReauthPolicy struct {
	enabled bool
	codes   map[string]struct{}
}

func (s *Service) ConfigureMedia(repository repository.MediaJobRepository, concurrency int) {
	if concurrency <= 0 {
		concurrency = 4
	}
	s.mediaJobs = repository
	s.mediaWorker = concurrency
	s.mediaQueue = make(chan string, min(2048, max(64, concurrency*32)))
	s.mediaInputSlots = make(chan struct{}, min(concurrency, videoInputMaterializeConcurrency))
	s.mediaQueued = make(map[string]struct{})
}

// ConfigureMediaAssets injects optional local video asset archival and reading.
func (s *Service) ConfigureMediaAssets(store videoAssetStore) {
	s.mediaAssets = store
}

func NewService(models routeResolver, audits auditRecorder, accounts *accountapp.Service, clientKeys *clientkeyapp.Service, providers *provider.Registry, selector *Selector, responses repository.ResponseRepository, maxAttempts int) *Service {
	service := &Service{
		models: models, audits: audits, accounts: accounts, clientKeys: clientKeys, providers: providers,
		selector: selector, responses: responses, logger: slog.Default(),
		rateLimits: make(map[string]teamModelRateLimit), rateLimitTeams: make(map[uint64]teamRateLimitObservation),
		modelSyncing: make(map[uint64]struct{}),
	}
	service.UpdateMaxAttempts(maxAttempts)
	return service
}

// UpdateBuildForbiddenReauthPolicy atomically replaces the Build account invalidation policy.
func (s *Service) UpdateBuildForbiddenReauthPolicy(enabled bool, codes []string) {
	policy := &buildForbiddenReauthPolicy{enabled: enabled, codes: make(map[string]struct{}, len(codes))}
	for _, value := range codes {
		code := normalizeFailureCode(value)
		if code != "" {
			policy.codes[code] = struct{}{}
		}
	}
	s.buildForbiddenReauth.Store(policy)
}

func (s *Service) shouldInvalidateBuildForbidden(failure *UpstreamFailure) bool {
	if failure == nil || failure.HTTPStatus != http.StatusForbidden {
		return false
	}
	// A configured code is only a second factor. The response body must also
	// contain a high-confidence account-scoped signal; permission-denied alone
	// is shared by content, policy, and other request-level failures.
	if !failure.AccountScoped || failure.SafetyRejection || failure.RequestScopedForbidden {
		return false
	}
	policy := s.buildForbiddenReauth.Load()
	if policy == nil || !policy.enabled {
		return false
	}
	_, matched := policy.codes[normalizeFailureCode(failure.UpstreamCode)]
	return matched
}

func (s *Service) markReauthRequired(ctx context.Context, requestID string, credential accountdomain.Credential, reason string) bool {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), accountStateWriteTimeout)
	defer cancel()
	if err := s.accounts.MarkReauthRequired(writeCtx, credential.ID, reason); err != nil {
		s.logger.Error("account_reauth_required_write_failed", "request_id", requestID, "account_id", credential.ID, "provider", credential.Provider, "error", err)
		return false
	}
	s.selector.MarkQuotaStateChanged(credential.Provider)
	return true
}

func teamModelRateLimitKey(providerValue accountdomain.Provider, teamFingerprint, upstreamModel string) string {
	return string(providerValue) + "\x00" + teamFingerprint + "\x00" + strings.TrimSpace(upstreamModel)
}

func rateLimitTeamFingerprint(teamID string) string {
	teamID = strings.ToLower(strings.TrimSpace(teamID))
	if teamID == "" {
		return ""
	}
	return security.HashToken(teamID)
}

func shortTeamFingerprint(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

func (s *Service) activeTeamModelRateLimit(credential accountdomain.Credential, upstreamModel string, now time.Time) (teamModelRateLimit, bool) {
	if !s.rateLimitActive.Load() {
		return teamModelRateLimit{}, false
	}
	credentialFingerprint := rateLimitTeamFingerprint(credential.TeamID)
	s.rateLimitMu.Lock()
	defer s.rateLimitMu.Unlock()
	if !s.rateLimitActive.Load() {
		return teamModelRateLimit{}, false
	}
	nextExpiry := s.rateLimitNextExpiry.Load()
	if nextExpiry <= 0 || now.UnixNano() >= nextExpiry {
		s.pruneTeamModelRateLimitsLocked(now)
		if len(s.rateLimits) == 0 {
			return teamModelRateLimit{}, false
		}
	}
	// Check the TeamID observed in an upstream response first, then current
	// credential metadata. The fallback prevents a historical observation from
	// permanently masking a later server-side team reassignment.
	observation := s.rateLimitTeams[credential.ID]
	observedFingerprint := observation.Fingerprint
	if observedFingerprint != "" && !now.Before(observation.ExpiresAt) {
		delete(s.rateLimitTeams, credential.ID)
		observedFingerprint = ""
	}
	teamFingerprints := [2]string{observedFingerprint, credentialFingerprint}
	fingerprintCount := 1
	if credentialFingerprint != observedFingerprint {
		fingerprintCount = 2
	}
	for index := 0; index < fingerprintCount; index++ {
		teamFingerprint := teamFingerprints[index]
		if teamFingerprint == "" {
			continue
		}
		key := teamModelRateLimitKey(credential.Provider, teamFingerprint, upstreamModel)
		value, ok := s.rateLimits[key]
		if !ok {
			continue
		}
		if !now.Before(value.Until) {
			delete(s.rateLimits, key)
			s.refreshTeamModelRateLimitStateLocked()
			continue
		}
		return value, true
	}
	return teamModelRateLimit{}, false
}

func (s *Service) pruneTeamModelRateLimitsLocked(now time.Time) {
	for key, value := range s.rateLimits {
		if !now.Before(value.Until) {
			delete(s.rateLimits, key)
		}
	}
	for accountID, observation := range s.rateLimitTeams {
		if !now.Before(observation.ExpiresAt) {
			delete(s.rateLimitTeams, accountID)
		}
	}
	s.refreshTeamModelRateLimitStateLocked()
}

func (s *Service) refreshTeamModelRateLimitStateLocked() {
	if len(s.rateLimits) == 0 {
		clear(s.rateLimitTeams)
		s.rateLimitNextExpiry.Store(0)
		s.rateLimitActive.Store(false)
		return
	}
	var nextExpiry time.Time
	for _, value := range s.rateLimits {
		if nextExpiry.IsZero() || value.Until.Before(nextExpiry) {
			nextExpiry = value.Until
		}
	}
	for _, observation := range s.rateLimitTeams {
		if nextExpiry.IsZero() || observation.ExpiresAt.Before(nextExpiry) {
			nextExpiry = observation.ExpiresAt
		}
	}
	s.rateLimitNextExpiry.Store(nextExpiry.UnixNano())
	s.rateLimitActive.Store(true)
}

func (s *Service) markTeamModelRateLimit(credential accountdomain.Credential, upstreamModel string, metadata provider.RateLimitMetadata, now time.Time) teamModelRateLimit {
	retryAfter := metadata.RetryAfter
	if retryAfter <= 0 {
		// RPS limits recover within about one second; do not apply the generic 1m cooldown.
		if strings.EqualFold(metadata.Scope, provider.RateLimitScopeRPS) {
			retryAfter = 2 * time.Second
		} else {
			retryAfter = time.Minute
		}
	}
	teamFingerprint := rateLimitTeamFingerprint(metadata.TeamID)
	value := teamModelRateLimit{TeamFingerprint: shortTeamFingerprint(teamFingerprint), Until: now.Add(retryAfter)}
	key := teamModelRateLimitKey(credential.Provider, teamFingerprint, upstreamModel)
	until := now.Add(retryAfter)
	s.rateLimitMu.Lock()
	s.rateLimitActive.Store(true)
	if s.rateLimits == nil {
		s.rateLimits = make(map[string]teamModelRateLimit)
	}
	if s.rateLimitTeams == nil {
		s.rateLimitTeams = make(map[uint64]teamRateLimitObservation)
	}
	if teamFingerprint != rateLimitTeamFingerprint(credential.TeamID) {
		s.rateLimitTeams[credential.ID] = teamRateLimitObservation{Fingerprint: teamFingerprint, ExpiresAt: until}
	} else {
		delete(s.rateLimitTeams, credential.ID)
	}
	for existingKey, value := range s.rateLimits {
		if !now.Before(value.Until) {
			delete(s.rateLimits, existingKey)
		}
	}
	for accountID, observation := range s.rateLimitTeams {
		if !now.Before(observation.ExpiresAt) {
			delete(s.rateLimitTeams, accountID)
		}
	}
	if current, ok := s.rateLimits[key]; ok && !current.Until.Before(until) {
		value = current
	} else {
		s.rateLimits[key] = value
	}
	s.refreshTeamModelRateLimitStateLocked()
	s.rateLimitMu.Unlock()
	return value
}

func (s *Service) SetLogger(logger *slog.Logger) {
	if logger != nil {
		s.logger = logger
	}
}

func (s *Service) UpdateMaxAttempts(maxAttempts int) { s.maxAttempts.Store(int64(maxAttempts)) }

// UpdateVideoMaxAttempts configures create-phase account failover for video jobs.
// 0 is treated as the general default pool size for legacy configs.
func (s *Service) UpdateVideoMaxAttempts(maxAttempts int) { s.videoMaxAttempts.Store(int64(maxAttempts)) }

// UpdateMarkBuildChatDeniedAsReauth 热更新 Build chat 永久拒绝是否标 reauthRequired。
// 默认 false：仅模型级冷却；true 时按旧逻辑将账号标为失效并出池。
func (s *Service) UpdateMarkBuildChatDeniedAsReauth(enabled bool) {
	s.markBuildChatDeniedAsReauth.Store(enabled)
}

func (s *Service) UpdateRequestTimeout(value time.Duration) {
	if value <= 0 {
		value = minimumTextBillingReservationTTL
	}
	s.requestTimeout.Store(int64(value))
}

func (s *Service) textBillingReservationTTL() time.Duration {
	ttl := time.Duration(s.requestTimeout.Load()) + finalizationTimeout + billingReservationCrashGrace
	return max(minimumTextBillingReservationTTL, ttl)
}

func (s *Service) checkLedgerReady() error {
	checker, ok := s.audits.(ledgerReadinessChecker)
	if !ok {
		return nil
	}
	if err := checker.CheckLedgerReady(); err != nil {
		return ErrLedgerUnavailable
	}
	return nil
}

func (s *Service) CreateResponse(ctx context.Context, input Input) (*Result, error) {
	input.Operation = audit.OperationResponses
	if isResponsesCompactionRequest(input.Body) {
		input.Operation = audit.OperationCompaction
	}
	return s.createResponseAt(ctx, input, "/responses")
}

func (s *Service) CreateChatCompletion(ctx context.Context, input Input) (*Result, error) {
	input.Operation = audit.OperationChat
	return s.createResponseAt(ctx, input, "/responses")
}

// CreateMessage executes an Anthropic Messages request through the unified Responses upstream.
func (s *Service) CreateMessage(ctx context.Context, input Input) (*Result, error) {
	input.Operation = audit.OperationMessages
	return s.createResponseAt(ctx, input, "/responses")
}

func (s *Service) CompactResponse(ctx context.Context, input Input) (*Result, error) {
	input.Streaming = false
	input.Operation = audit.OperationCompaction
	return s.createResponseAt(ctx, input, "/responses/compact")
}

// resolvePublicModelRoutes supports both unprefixed downstream model names and explicitly sourced compatibility names.
// Registered Provider aliases are stable compatibility contracts. allowModelAliases gates only dynamically generated
// reasoning-effort aliases so existing clients keep working after the per-key discovery switch is introduced.
func (s *Service) resolvePublicModelRoutes(ctx context.Context, publicModel string, allowModelAliases bool) ([]modeldomain.Route, string, error) {
	routes, err := s.models.GetByPublicIDCandidates(ctx, publicModel)
	if err == nil {
		return routes, "", nil
	}
	if s.providers != nil {
		if alias, ok := s.providers.ResolveModelAlias(publicModel); ok {
			if alias.Provider != "" && alias.UpstreamModel != "" {
				route, routeErr := s.models.GetByProviderUpstream(ctx, alias.Provider, alias.UpstreamModel)
				if routeErr != nil {
					return nil, "", routeErr
				}
				return []modeldomain.Route{route}, alias.ReasoningEffort, nil
			}
			routes, resolveErr := s.models.GetByPublicIDCandidates(ctx, alias.PublicModel)
			return routes, alias.ReasoningEffort, resolveErr
		}
	}
	// Dynamic effort-suffix aliases (e.g. grok-4.5-low) for any Provider that
	// exposes the base model. Fixed-reasoning Providers may compatibility-accept
	// an alias while their wire normalizer drops the unsupported effort.
	if base, effort, ok := modeldomain.ParseReasoningModelAlias(publicModel); ok {
		if !allowModelAliases {
			return nil, "", err
		}
		routes, resolveErr := s.models.GetByPublicIDCandidates(ctx, base)
		if resolveErr != nil {
			return nil, "", resolveErr
		}
		eligible := make([]modeldomain.Route, 0, len(routes))
		for _, route := range routes {
			if modeldomain.SupportsReasoningEffortForProvider(route.Provider, route.PublicID, effort) ||
				modeldomain.IsFixedReasoningForProvider(route.Provider, route.PublicID) {
				eligible = append(eligible, route)
			}
		}
		if len(eligible) == 0 {
			return nil, "", repository.ErrNotFound
		}
		return eligible, effort, nil
	}
	return nil, "", err
}

// eligibleConversationRoutes filters route targets without choosing one. Keeping
// this separate from ordering lets one public name form a schedulable target pool.
func (s *Service) eligibleConversationRoutes(routes []modeldomain.Route, key clientkey.Key, operation audit.Operation, path string, requireStoredResponse bool, ownership *inferencedomain.ResponseOwnership) ([]modeldomain.Route, modeldomain.Route, error) {
	if len(routes) == 0 || s.providers == nil {
		return nil, modeldomain.Route{}, ErrModelNotFound
	}
	fallback := routes[0]
	eligible := make([]modeldomain.Route, 0, len(routes))
	accountScope := key.AccountScope()
	matchedOwnership := ownership == nil
	scopeMatched := false
	allowed := false
	conversationSupported := false
	storedResponseUnsupported := false
	for _, route := range routes {
		if ownership != nil {
			if ownership.ModelRouteID != 0 {
				if route.ID != ownership.ModelRouteID {
					continue
				}
			} else if route.Provider != ownership.Provider {
				// Backward compatibility for ownership rows created before route IDs
				// were persisted: retain the original Provider-scoped pin.
				continue
			}
		}
		matchedOwnership = true
		fallback = route
		if !accountScope.AllowsProvider(route.Provider) {
			continue
		}
		scopeMatched = true
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
		eligible = append(eligible, route)
	}
	if len(eligible) > 0 {
		return eligible, fallback, nil
	}
	if !matchedOwnership {
		return nil, fallback, ErrResponseAccountUnavailable
	}
	if !scopeMatched {
		return nil, fallback, &SelectionUnavailableError{Reason: SelectionNoAccounts, Scope: accountScope}
	}
	if !allowed {
		return nil, fallback, clientkeyapp.ErrModelNotAllowed
	}
	if storedResponseUnsupported {
		return nil, fallback, ErrResponseStateUnsupported
	}
	if conversationSupported && path == "/responses/compact" {
		return nil, fallback, ErrConversationUnsupported
	}
	return nil, fallback, ErrConversationUnsupported
}

// selectConversationRoute retains the legacy single-target helper for callers
// that do not need target-pool ordering.
func (s *Service) selectConversationRoute(routes []modeldomain.Route, key clientkey.Key, operation audit.Operation, path string, requireStoredResponse bool, ownership *inferencedomain.ResponseOwnership) (modeldomain.Route, error) {
	eligible, fallback, err := s.eligibleConversationRoutes(routes, key, operation, path, requireStoredResponse, ownership)
	if err != nil {
		return fallback, err
	}
	return eligible[0], nil
}

// orderConversationRouteTargets randomizes targets within the same Provider by
// rendezvous score. Provider priority remains stable, while a session seed keeps
// Codex/Claude continuations on the same target without global mutable state.
func orderConversationRouteTargets(routes []modeldomain.Route, seed string) []modeldomain.Route {
	ordered := append([]modeldomain.Route(nil), routes...)
	sort.SliceStable(ordered, func(left, right int) bool {
		leftPriority := routeProviderPriority(ordered[left].Provider)
		rightPriority := routeProviderPriority(ordered[right].Provider)
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}
		leftScore := routeTargetScore(seed, ordered[left].ID)
		rightScore := routeTargetScore(seed, ordered[right].ID)
		if leftScore != rightScore {
			return leftScore > rightScore
		}
		return ordered[left].ID < ordered[right].ID
	})
	return ordered
}

func routeTargetScore(seed string, routeID uint64) uint64 {
	digest := sha256.Sum256([]byte(seed + ":" + strconv.FormatUint(routeID, 10)))
	return binary.BigEndian.Uint64(digest[:8])
}

func routeProviderPriority(providerValue accountdomain.Provider) int {
	switch providerValue {
	case accountdomain.ProviderBuild:
		return 0
	case accountdomain.ProviderWeb:
		return 1
	case accountdomain.ProviderConsole:
		return 2
	default:
		return 3
	}
}

func routeTargetSeed(input Input) string {
	// Match the Build account-affinity precedence so Codex and Claude Code keep
	// both the route target and account stable across one logical session.
	anchor := strings.TrimSpace(input.PromptCacheSeed)
	if anchor == "" {
		anchor = strings.TrimSpace(input.PromptCacheKey)
	}
	if anchor == "" {
		system, firstUser, _ := extractMessageAnchors(input.Body)
		system = truncateAnchor(system, 100)
		firstUser = truncateAnchor(firstUser, 200)
		if firstUser != "" {
			anchor = "soft:" + system + ":" + firstUser
		}
	}
	if anchor == "" {
		anchor = strings.TrimSpace(input.RequestID)
	}
	return strconv.FormatUint(input.ClientKey.ID, 10) + ":" + anchor
}

// selectMediaRoute selects a same-name route that satisfies media capability, key permissions, and Provider support.
func (s *Service) selectMediaRoute(routes []modeldomain.Route, key clientkey.Key, capability modeldomain.Capability, providerSupported func(accountdomain.Provider) bool) (modeldomain.Route, error) {
	eligible, fallback, err := s.eligibleMediaRoutes(routes, key, capability, providerSupported)
	if err != nil {
		return fallback, err
	}
	return eligible[0], nil
}

func (s *Service) eligibleMediaRoutes(routes []modeldomain.Route, key clientkey.Key, capability modeldomain.Capability, providerSupported func(accountdomain.Provider) bool) ([]modeldomain.Route, modeldomain.Route, error) {
	if len(routes) == 0 {
		return nil, modeldomain.Route{}, ErrModelNotFound
	}
	fallback := routes[0]
	eligible := make([]modeldomain.Route, 0, len(routes))
	accountScope := key.AccountScope()
	capabilityMatched := false
	scopeMatched := false
	allowed := false
	for _, route := range routes {
		if route.Capability != capability {
			continue
		}
		fallback = route
		capabilityMatched = true
		if !accountScope.AllowsProvider(route.Provider) {
			continue
		}
		scopeMatched = true
		if !s.clientKeys.CanUseModel(key, route.ID) {
			continue
		}
		allowed = true
		if providerSupported(route.Provider) {
			eligible = append(eligible, route)
		}
	}
	if len(eligible) > 0 {
		return eligible, fallback, nil
	}
	if !capabilityMatched {
		return nil, fallback, ErrModelNotFound
	}
	if !scopeMatched {
		return nil, fallback, &SelectionUnavailableError{Reason: SelectionNoAccounts, Scope: accountScope}
	}
	if !allowed {
		return nil, fallback, clientkeyapp.ErrModelNotAllowed
	}
	return nil, fallback, ErrNoAvailableAccount
}

// selectSchedulableMediaRoute resolves a concrete same-name media target and
// its immutable account plan together. A cooling or exhausted first target
// therefore cannot hide a healthy target from another Provider.
func (s *Service) selectSchedulableMediaRoute(ctx context.Context, routes []modeldomain.Route, key clientkey.Key, capability modeldomain.Capability, consumesQuota bool, providerSupported func(accountdomain.Provider) bool) (modeldomain.Route, *selectionSession, error) {
	return s.selectSchedulableMediaRouteWithQuotaMode(ctx, routes, key, capability, consumesQuota, providerSupported, nil)
}

func (s *Service) selectSchedulableMediaRouteWithQuotaMode(ctx context.Context, routes []modeldomain.Route, key clientkey.Key, capability modeldomain.Capability, consumesQuota bool, providerSupported func(accountdomain.Provider) bool, resolveQuotaMode func(modeldomain.Route) string) (modeldomain.Route, *selectionSession, error) {
	eligible, fallback, err := s.eligibleMediaRoutes(routes, key, capability, providerSupported)
	if err != nil {
		return fallback, nil, err
	}
	return s.selectSchedulableEligibleMediaRouteWithQuotaMode(ctx, eligible, key, consumesQuota, resolveQuotaMode)
}

// selectSchedulableEligibleMediaRouteWithQuotaMode selects an account plan
// from routes that already passed capability, client-key, and Provider support
// checks. Callers may apply request-specific route constraints between the
// eligibility and scheduling phases without evaluating disallowed routes.
func (s *Service) selectSchedulableEligibleMediaRouteWithQuotaMode(ctx context.Context, eligible []modeldomain.Route, key clientkey.Key, consumesQuota bool, resolveQuotaMode func(modeldomain.Route) string) (modeldomain.Route, *selectionSession, error) {
	if len(eligible) == 0 {
		return modeldomain.Route{}, nil, ErrNoAvailableAccount
	}
	var firstSelectionErr error
	for _, route := range eligible {
		quotaMode := ""
		if consumesQuota {
			if resolveQuotaMode != nil {
				quotaMode = resolveQuotaMode(route)
			} else {
				quotaMode = s.providers.QuotaMode(route.Provider, route.UpstreamModel)
			}
		}
		session, selectionErr := s.selector.beginSelectionSessionForKey(
			ctx,
			route.Provider,
			route.ID,
			route.UpstreamModel,
			quotaMode,
			"",
			nil,
			false,
			key.AccountScope(),
		)
		if selectionErr == nil {
			return route, session, nil
		}
		if firstSelectionErr == nil {
			firstSelectionErr = selectionErr
		}
	}
	if firstSelectionErr == nil {
		firstSelectionErr = ErrNoAvailableAccount
	}
	return eligible[0], nil, firstSelectionErr
}

func (s *Service) createResponseAt(ctx context.Context, input Input, path string) (*Result, error) {
	ctx, egressTrace := infraegress.WithTrace(ctx)
	startedAt := time.Now()
	var firstToken *firstTokenTimer
	if input.Streaming {
		firstToken = newFirstTokenTimer(startedAt)
	}
	eventID := newAuditEventID()
	// Use a server-generated scope so repeated or absent client request IDs
	// cannot accidentally join independent Composer conversations.
	requestSessionScope := eventID
	operation := input.Operation
	if operation == "" {
		operation = audit.OperationResponses
	}
	routes, aliasEffort, err := s.resolvePublicModelRoutes(ctx, input.PublicModel, input.ClientKey.AllowModelAliases)
	if err != nil {
		return nil, ErrModelNotFound
	}
	// Select an initial route only to preserve the existing stateful/stateless
	// previous_response_id boundary. The actual target is chosen from the eligible
	// pool below after ownership and account availability are known.
	initialRoute, routeErr := s.selectConversationRoute(routes, input.ClientKey, operation, path, false, nil)
	var ownership *inferencedomain.ResponseOwnership
	if input.PreviousResponseID != "" && routeErr == nil {
		if s.providers.SupportsStoredResponses(initialRoute.Provider) {
			value, ownershipErr := s.responses.Get(ctx, input.PreviousResponseID, input.ClientKey.ID, time.Now().UTC())
			if ownershipErr != nil {
				return nil, ErrResponseNotFound
			}
			ownership = &value
		} else if initialRoute.Provider == accountdomain.ProviderConsole {
			// Console does not retain Response state, so replay the history statelessly here;
			// Provider normalization removes stale Response IDs.
			input.PreviousResponseID = ""
		} else {
			return nil, ErrResponseStateUnsupported
		}
	}
	eligibleRoutes, fallbackRoute, routeErr := s.eligibleConversationRoutes(routes, input.ClientKey, operation, path, ownership != nil, ownership)
	route := fallbackRoute
	orderedRoutes := eligibleRoutes
	if routeErr == nil {
		orderedRoutes = orderConversationRouteTargets(eligibleRoutes, routeTargetSeed(input))
		route = orderedRoutes[0]
	}
	accountScope := input.ClientKey.AccountScope()
	var preselectedSession *selectionSession
	// Skip targets whose account pool is already known to be unavailable. This
	// gives same-name targets failover before any physical upstream request while
	// preserving pinned Responses and forced administrator probes.
	if routeErr == nil && ownership == nil && input.ForcedEgressNodeID == 0 {
		for _, candidate := range orderedRoutes {
			affinityKey := ""
			if candidate.Provider == accountdomain.ProviderBuild {
				identity := resolveBuildSessionIdentity(
					input.ClientKey.ID,
					candidate.Provider,
					candidate.UpstreamModel,
					input.PromptCacheKey,
					input.PromptCacheSeed,
					input.Body,
				)
				identity = ensureBuildComposerSessionIdentity(identity, input.ClientKey.ID, candidate.Provider, candidate.UpstreamModel, requestSessionScope)
				affinityKey = identity.affinityKey
			}
			candidateSession, selectionErr := s.selector.beginSelectionSessionForKey(
				ctx,
				candidate.Provider,
				candidate.ID,
				candidate.UpstreamModel,
				s.providers.QuotaMode(candidate.Provider, candidate.UpstreamModel),
				affinityKey,
				nil,
				true,
				accountScope,
			)
			if selectionErr != nil {
				continue
			}
			route = candidate
			preselectedSession = candidateSession
			break
		}
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
	mediaSummary, _ := summarizeResponseMedia(input.Body)
	logResponseMediaSummary(s.logger, input.RequestID, mediaSummary)
	auditBase := audit.Record{
		EventID: eventID, RequestID: input.RequestID, ClientKeyID: input.ClientKey.ID, ClientKeyName: input.ClientKey.Name,
		ModelRouteID: route.ID, ModelPublicID: publicModel, ModelUpstreamModel: modeldomain.DisplayUpstreamModel(route.Provider, route.UpstreamModel),
		Provider: string(route.Provider), Operation: operation, UsageSource: audit.UsageSourceNone, Streaming: input.Streaming,
		MediaInputImages: mediaSummary.InputImages,
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
	affinityKey := ""
	ownershipPromptCacheKey := ""
	reasoningReplayKey := ""
	if route.Provider == accountdomain.ProviderBuild {
		// Derive a stable identity from explicit session signals, message anchors,
		// and model. Composer replaces message-only fallback identities with an
		// isolated request identity that remains stable across retries.
		identity := buildSessionIdentity{}
		if ownership != nil && ownership.PromptCacheKey != "" {
			// previous_response_id belongs to an existing Response chain and must inherit the root session identity;
			// do not recompute the soft key from this turn's incremental input.
			identity.upstreamID = ownership.PromptCacheKey
			identity.replayKey = ownership.ReasoningReplayKey
		} else {
			identity = resolveBuildSessionIdentity(
				input.ClientKey.ID,
				route.Provider,
				route.UpstreamModel,
				input.PromptCacheKey,
				input.PromptCacheSeed,
				input.Body,
			)
		}
		identity = ensureBuildComposerSessionIdentity(identity, input.ClientKey.ID, route.Provider, route.UpstreamModel, requestSessionScope)
		input.PromptCacheKey = identity.upstreamID
		affinityKey = identity.affinityKey
		ownershipPromptCacheKey = identity.upstreamID
		reasoningReplayKey = identity.replayKey
		if identity.upstreamID == "" {
			s.logger.Debug("prompt_cache_session_empty", "request_id", input.RequestID, "model", route.UpstreamModel, "provider", route.Provider)
		} else if identity.soft {
			s.logger.Debug("prompt_cache_session_soft", "request_id", input.RequestID, "model", route.UpstreamModel)
		} else if identity.isolated {
			s.logger.Debug("prompt_cache_session_isolated", "request_id", input.RequestID, "model", route.UpstreamModel)
		}
	}
	adapter, ok := s.providers.Responses(route.Provider)
	if !ok {
		return nil, ErrNoAvailableAccount
	}
	physicalCallCtx := infraegress.WithPhysicalCallTrace(ctx, string(route.Provider), string(operation))
	supportsStoredResponses := s.providers.SupportsStoredResponses(route.Provider)
	if input.PreviousResponseID != "" && !supportsStoredResponses {
		return nil, ErrResponseStateUnsupported
	}
	attemptPolicy := newRoutingAttemptPolicy(int(s.maxAttempts.Load()))
	idempotencyID, _ := security.NewOpaqueToken(18)
	if ownership != nil {
		attemptPolicy = newRoutingAttemptPolicy(1)
	}
	pricingModel := s.providers.PricingModel(route.Provider, route.UpstreamModel)
	if err := s.checkLedgerReady(); err != nil {
		return nil, err
	}
	if reservation, priced := audit.EstimateOfficialTextReservation(pricingModel, input.Body); priced {
		if _, err := s.clientKeys.ReserveBilling(ctx, input.ClientKey, eventID, reservation.CostInUSDTicks, s.textBillingReservationTTL()); err != nil {
			return nil, err
		}
	}
	excluded := make(map[uint64]bool)
	failureFingerprints := make(map[string]int)
	authRecoveryAttempted := make(map[uint64]bool)
	quotaMode := s.providers.QuotaMode(route.Provider, route.UpstreamModel)
	quotaProbeAttempted := false
	selection := preselectedSession
	var lastErr error
	var lastFailure *UpstreamFailure
	failureAttempts := newFailureAttemptRecorder(http.MethodPost, path)
	responseStartedAt := startedAt
	forwardResponse := func(lease *accountLease, credential accountdomain.Credential, billing *accountdomain.Billing) (*provider.Response, error) {
		started := time.Now()
		responseStartedAt = started
		lease.markSelectorUpstreamStarted()
		response, err := adapter.ForwardResponse(physicalCallCtx, provider.ResponseResourceRequest{Credential: credential, ForcedEgressNodeID: input.ForcedEgressNodeID, Billing: billing, Method: http.MethodPost, Path: path, Model: route.UpstreamModel, PromptCacheKey: input.PromptCacheKey, ReasoningReplayKey: reasoningReplayKey, AllowClientToolCacheRoute: input.AllowClientToolCacheRoute, GrokTurnIndex: input.GrokTurnIndex, IdempotencyID: idempotencyID, Body: input.Body, Streaming: input.Streaming, NormalizeBody: true, Operation: string(operation)})
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
	for attempt := 0; attemptPolicy.allows(attempt); attempt++ {
		var lease *accountLease
		var err error
		selectionStarted := time.Now()
		if ownership != nil {
			lease, err = s.selector.AcquirePinnedForKey(ctx, route.Provider, ownership.AccountID, route.ID, route.UpstreamModel, quotaMode, true, accountScope)
		} else if input.ForcedEgressNodeID != 0 {
			lease, err = s.selector.AcquireForKeyOnEgressNode(ctx, route.Provider, route.ID, route.UpstreamModel, quotaMode, affinityKey, excluded, !quotaProbeAttempted, accountScope, input.ForcedEgressNodeID)
		} else {
			if selection == nil {
				selection, err = s.selector.beginSelectionSessionForKey(ctx, route.Provider, route.ID, route.UpstreamModel, quotaMode, affinityKey, excluded, !quotaProbeAttempted, accountScope)
			}
			if err == nil {
				lease, err = selection.Acquire(ctx, excluded, !quotaProbeAttempted)
			}
		}
		timing.markSelection(time.Since(selectionStarted))
		if err != nil {
			if lastFailure == nil {
				lastErr = err
			}
			break
		}
		excluded[lease.Credential.ID] = true
		if limited, ok := s.activeTeamModelRateLimit(lease.Credential, route.UpstreamModel, time.Now().UTC()); ok {
			lease.Release()
			lastFailure = &UpstreamFailure{
				HTTPStatus: http.StatusTooManyRequests, Code: "upstream_rate_limited", PublicMessage: "上游请求频率受限",
				AccountID: lease.Credential.ID, AccountName: lease.Credential.Name,
				Fingerprint: "429:team_model_rate_limit", RetryAfter: time.Until(limited.Until),
			}
			lastErr = fmt.Errorf("上游 Team 与模型请求频率受限")
			s.logger.Warn("upstream_team_model_rate_limit_active", "request_id", input.RequestID, "account_id", lease.Credential.ID, "provider", route.Provider, "model", route.UpstreamModel, "team_fingerprint", limited.TeamFingerprint, "retry_after", lastFailure.RetryAfter.Round(time.Second))
			// Stored Responses are pinned to one account. Return the cached 429
			// immediately instead of spinning until the cooldown expires or
			// replaying the request on the same account.
			if ownership != nil {
				break attemptLoop
			}
			attempt--
			continue
		}
		if lease.QuotaProbe {
			quotaProbeAttempted = true
		}
		if lease.QuotaProbeKind == accountdomain.QuotaRecoveryKindPaid {
			recovered, probeErr := s.accounts.ProbePaidQuota(ctx, lease.Credential)
			s.selector.MarkQuotaStateChanged(lease.Credential.Provider, lease.Credential.ID)
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
		response, err := forwardResponse(lease, credential, lease.Billing)
		if err != nil {
			lease.Release()
			lastErr = err
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				lastFailure = &UpstreamFailure{HTTPStatus: 499, Code: "request_canceled", PublicMessage: "请求已取消", AccountID: credential.ID, AccountName: credential.Name, Cause: firstError(ctx.Err(), err)}
				break
			}
			if isSSOCredentialRejected(err, credential) {
				s.markSSOCredentialRejected(ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
				lastFailure = newHTTPUpstreamFailure(http.StatusUnauthorized, nil, credential.ID, credential.Name)
				continue
			}
			lastFailure = newTransportUpstreamFailure(err, credential.ID, credential.Name)
			if !isRetryableTransportFailure(credential.Provider, err) {
				break
			}
			if !neterrorpkg.IsUpstreamStreamIdleTimeout(err) {
				s.selector.MarkFailure(ctx, credential, 0, 0)
			}
			if shouldStopForNonAccountFingerprint(failureFingerprints, lastFailure) {
				break
			}
			continue
		}
	handleResponse:
		if response.ModelCatalogChanged {
			s.queueAccountModelSync(credential.ID)
		}
		if response.StatusCode == http.StatusUnauthorized {
			response.Body.Close()
			if credential.AuthType == accountdomain.AuthTypeSSO {
				s.markSSOCredentialRejected(ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
				lease.Release()
				lastErr = fmt.Errorf("%s SSO 凭据已失效", credential.Provider)
				lastFailure = newHTTPUpstreamFailure(http.StatusUnauthorized, nil, credential.ID, credential.Name)
				continue
			}
			if s.markPermanentlyUnrefreshableCredentialRejected(ctx, credential) {
				lease.Release()
				lastErr = accountapp.ErrCredentialRefreshPermanent
				lastFailure = newHTTPUpstreamFailure(http.StatusUnauthorized, nil, credential.ID, credential.Name)
				continue
			}
			authRecoveryAttempted[credential.ID] = true
			refreshed, refreshErr := ensureCredential(credential, true)
			if refreshErr == nil {
				response, err = forwardResponse(lease, refreshed, lease.Billing)
				credential = refreshed
			}
			if refreshErr != nil || err != nil {
				if errors.Is(refreshErr, accountapp.ErrCredentialRefreshPermanent) {
					s.markCredentialRejectedAfterPermanentRefresh(ctx, credential)
				}
				lease.Release()
				lastErr = firstError(refreshErr, err)
				if refreshErr != nil {
					lastFailure = newCredentialUpstreamFailure(refreshErr, credential.ID, credential.Name)
				} else if ctx.Err() != nil || errors.Is(err, context.Canceled) {
					lastFailure = &UpstreamFailure{HTTPStatus: 499, Code: "request_canceled", PublicMessage: "请求已取消", AccountID: credential.ID, AccountName: credential.Name, Cause: firstError(ctx.Err(), err)}
					break
				} else {
					lastFailure = newTransportUpstreamFailure(err, credential.ID, credential.Name)
					if !isRetryableTransportFailure(credential.Provider, err) {
						break attemptLoop
					}
					if shouldStopForNonAccountFingerprint(failureFingerprints, lastFailure) {
						break attemptLoop
					}
				}
				continue
			}
			if response.StatusCode == http.StatusUnauthorized {
				body, _ := readRetryableBody(response.Body)
				_ = s.accounts.MarkReauthRequired(ctx, credential.ID, "Grok Build OAuth credential rejected after refresh")
				s.selector.MarkQuotaStateChanged(credential.Provider, credential.ID)
				lease.Release()
				lastErr = fmt.Errorf("刷新后上游仍返回 401")
				lastFailure = newHTTPUpstreamFailure(http.StatusUnauthorized, body, credential.ID, credential.Name)
				continue
			}
		}
		egressForbidden := s.providers.RetryForbiddenAsEgress(credential.Provider) && response.StatusCode == http.StatusForbidden
		finalEgressForbidden := egressForbidden && (attempt > 0 || !attemptPolicy.hasNext(attempt))
		// Classify 403 bodies before egress retry. Definitive blocked-account signals invalidate and rotate the account;
		// request-level safety rejections are returned as-is without account side effects;
		// all other 403 responses retain the egress retry path without penalizing the account.
		if response.StatusCode == http.StatusForbidden {
			retryAfter := parseRetryAfter(response.Header.Get("Retry-After"), time.Now().UTC())
			body, _ := readRetryableBody(response.Body)
			lastFailure = newHTTPUpstreamFailure(response.StatusCode, body, credential.ID, credential.Name)
			if isTerminalRequestForbidden(credential.Provider, lastFailure) {
				// Deterministic request-scoped 403: restore the original body and return it
				// without OAuth refresh, account rotation, cooldown, or invalidation.
				response.Body = io.NopCloser(bytes.NewReader(body))
				lease.completeSelectorObservation(false)
				lease.Release()
				if lastFailure.SafetyRejection {
					s.logger.Warn("upstream_safety_rejection", "request_id", input.RequestID, "account_id", credential.ID, "provider", credential.Provider, "status", response.StatusCode, "upstream_code", lastFailure.UpstreamCode)
				} else {
					s.logger.Warn("upstream_request_scoped_forbidden", "request_id", input.RequestID, "account_id", credential.ID, "provider", credential.Provider, "status", response.StatusCode, "upstream_code", lastFailure.UpstreamCode)
				}
				// Fall through to the common success/error response path so the client receives the original 403.
			} else if lastFailure.AccountBlocked {
				failureHandled := s.markReauthRequired(ctx, input.RequestID, credential, fmt.Sprintf("%s account is blocked", credential.Provider))
				if lastFailure.AccountScoped && !failureHandled {
					s.selector.MarkFailure(ctx, credential, response.StatusCode, retryAfter)
				}
				lease.Release()
				lastErr = fmt.Errorf("上游返回 %d", response.StatusCode)
				s.logger.Warn("upstream_request_failed", "request_id", input.RequestID, "account_id", credential.ID, "provider", credential.Provider, "status", response.StatusCode, "upstream_code", lastFailure.UpstreamCode, "account_scoped", lastFailure.AccountScoped, "account_blocked", true)
				continue
			} else if egressForbidden && !finalEgressForbidden {
				// A non-blocking 403 is an egress/browser-session failure and must not penalize the account.
				delete(excluded, credential.ID)
				if selection != nil {
					selection.RetryAccount(credential.ID)
				}
				lease.Release()
				lastErr = fmt.Errorf("上游出口会话被拒绝")
				continue
			} else {
				// Restore the consumed final non-blocking 403 body for the common response path.
				response.Body = io.NopCloser(bytes.NewReader(body))
			}
		}
		if isTerminalRequestForbidden(credential.Provider, lastFailure) {
			// already prepared as a terminal 403 response for the client
		} else if isRetryableResponse(response, route.Provider) && !finalEgressForbidden {
			retryAfter := parseRetryAfter(response.Header.Get("Retry-After"), time.Now().UTC())
			body, _ := readRetryableBody(response.Body)
			lastFailure = newHTTPUpstreamFailure(response.StatusCode, body, credential.ID, credential.Name)
			if response.StatusCode == http.StatusTooManyRequests && response.RateLimit == nil {
				if metadata := provider.ParseRateLimitMetadata(body); metadata != nil {
					response.RateLimit = metadata
					if retryAfter <= 0 && metadata.RetryAfter > 0 {
						retryAfter = metadata.RetryAfter
					}
				}
			}
			buildForbiddenReauth := credential.Provider == accountdomain.ProviderBuild && s.shouldInvalidateBuildForbidden(lastFailure)
			if response.StatusCode == http.StatusTooManyRequests && response.RateLimit != nil && response.RateLimit.Model == route.UpstreamModel {
				rateLimitMeta := *response.RateLimit
				if strings.TrimSpace(rateLimitMeta.TeamID) == "" {
					rateLimitMeta.TeamID = strings.TrimSpace(credential.TeamID)
				}
				if rateLimitMeta.TeamID == "" {
					// Team+Model shielding requires a team identity; fall through to account-scoped 429 handling.
					goto afterTeamRateLimit
				}
				limited := s.markTeamModelRateLimit(credential, route.UpstreamModel, rateLimitMeta, time.Now().UTC())
				lastFailure.AccountScoped = false
				lastFailure.Fingerprint = "429:team_model_rate_limit"
				lastFailure.RetryAfter = time.Until(limited.Until)
				lease.Release()
				lastErr = fmt.Errorf("上游 Team 与模型请求频率受限")
				s.logger.Warn("upstream_team_model_rate_limited", "request_id", input.RequestID, "provider", credential.Provider, "model", route.UpstreamModel, "team_fingerprint", limited.TeamFingerprint, "scope", rateLimitMeta.Scope, "actual", rateLimitMeta.Actual, "limit", rateLimitMeta.Limit, "retry_after", lastFailure.RetryAfter)
				continue
			}
		afterTeamRateLimit:
			// Grok Build treats only HTTP 401 as an OAuth authentication failure.
			// A 403 is already authenticated and must not trigger token rotation or
			// replay the same request with freshly issued credentials.
			if credential.Provider != accountdomain.ProviderBuild && s.providers.SupportsCredentialRefresh(credential.Provider) && !authRecoveryAttempted[credential.ID] && credential.EncryptedRefreshToken != "" && !lastFailure.AccountBlocked && !buildForbiddenReauth && (lastFailure.PermanentAccountDenial || lastFailure.CredentialRejected) {
				authRecoveryAttempted[credential.ID] = true
				refreshed, refreshErr := ensureCredential(credential, true)
				if refreshErr != nil {
					lease.Release()
					lastErr = refreshErr
					lastFailure = newCredentialUpstreamFailure(refreshErr, credential.ID, credential.Name)
					continue attemptLoop
				}
				response, err = forwardResponse(lease, refreshed, lease.Billing)
				credential = refreshed
				if err != nil {
					lease.Release()
					lastErr = err
					if ctx.Err() != nil || errors.Is(err, context.Canceled) {
						lastFailure = &UpstreamFailure{HTTPStatus: 499, Code: "request_canceled", PublicMessage: "请求已取消", AccountID: credential.ID, AccountName: credential.Name, Cause: firstError(ctx.Err(), err)}
						break attemptLoop
					}
					lastFailure = newTransportUpstreamFailure(err, credential.ID, credential.Name)
					if !isRetryableTransportFailure(credential.Provider, err) {
						break attemptLoop
					}
					if shouldStopForNonAccountFingerprint(failureFingerprints, lastFailure) {
						break attemptLoop
					}
					continue attemptLoop
				}
				goto handleResponse
			}
			failureHandled := false
			if lease.QuotaMode != "" && response.StatusCode == http.StatusTooManyRequests {
				exhausted, reconcileErr := s.accounts.ReconcileRateLimit(ctx, credential.ID, lease.QuotaMode, retryAfter)
				s.selector.MarkQuotaStateChanged(credential.Provider, credential.ID)
				failureHandled = reconcileErr == nil && exhausted
			} else if used, limit, exhausted := parseFreeQuotaExhaustion(body); exhausted {
				// The Free subscription signal is account-scoped, but its billing
				// period is not a reliable reset promise. Probe again after 24 hours.
				s.selector.MarkFreeQuotaExhausted(ctx, credential, used, limit)
				failureHandled = true
			} else if lastFailure.ModelQuotaExhausted {
				s.selector.MarkModelQuotaExhausted(ctx, credential, lease.Billing, route.UpstreamModel, retryAfter)
				failureHandled = true
			} else if lastFailure.FreeQuotaExhausted {
				s.selector.MarkFreeQuotaExhausted(ctx, credential, 0, 0)
				failureHandled = true
			} else if lastFailure.SpendingLimitBlocked || lastFailure.QuotaExhausted {
				err := s.selector.MarkPaymentQuotaExhausted(ctx, credential, quotaRecoveryHints{Billing: lease.Billing})
				failureHandled = err == nil
				if err != nil {
					s.logger.Error("account_quota_recovery_write_failed", "request_id", input.RequestID, "account_id", credential.ID, "provider", credential.Provider, "error", err)
				}
			}
			if lastFailure.AccountBlocked {
				failureHandled = s.markReauthRequired(ctx, input.RequestID, credential, fmt.Sprintf("%s account is blocked", credential.Provider))
			} else if buildForbiddenReauth {
				failureHandled = s.markReauthRequired(ctx, input.RequestID, credential, fmt.Sprintf("%s upstream error code %s matched the invalidation policy", credential.Provider, lastFailure.UpstreamCode))
			} else if s.providers.SupportsCredentialRefresh(credential.Provider) && lastFailure.PermanentAccountDenial {
				if credential.Provider == accountdomain.ProviderBuild {
					// 默认 model-scoped，视频拒绝时配额/OAuth 仍可能可用。
					// 开启 markBuildChatDeniedAsReauth 时再额外标 reauth，便于号池摘除。
					// 同时写入模型 block，避免在候选缓存窗口内本请求再次选中。
					modelErr := s.selector.MarkModelAccessDenied(ctx, credential, route.UpstreamModel, retryAfter)
					failureHandled = modelErr == nil
					if modelErr != nil {
						s.logger.Error("account_model_access_denied_write_failed", "request_id", input.RequestID, "account_id", credential.ID, "provider", credential.Provider, "model", route.UpstreamModel, "error", modelErr)
					}
					if s.markBuildChatDeniedAsReauth.Load() {
						reauthHandled := s.markReauthRequired(ctx, input.RequestID, credential, fmt.Sprintf("%s chat endpoint access denied", credential.Provider))
						failureHandled = failureHandled || reauthHandled
					}
				} else {
					failureHandled = s.markReauthRequired(ctx, input.RequestID, credential, fmt.Sprintf("%s chat endpoint access denied", credential.Provider))
				}
			} else if s.providers.SupportsCredentialRefresh(credential.Provider) && lastFailure.CredentialRejected {
				failureHandled = s.markReauthRequired(ctx, input.RequestID, credential, fmt.Sprintf("%s credential rejected", credential.Provider))
			}
			if lastFailure.AccountScoped && !failureHandled {
				s.selector.MarkFailure(ctx, credential, response.StatusCode, retryAfter)
			} else if !lastFailure.AccountScoped && response.StatusCode >= http.StatusInternalServerError {
				// 5xx 短冷却：本请求已 excluded，跨请求避免立刻再打同一坏号。
				s.selector.MarkFailure(ctx, credential, response.StatusCode, retryAfter)
			}
			lease.Release()
			lastErr = fmt.Errorf("上游返回 %d", response.StatusCode)
			s.logger.Warn("upstream_request_failed", "request_id", input.RequestID, "account_id", credential.ID, "provider", credential.Provider, "status", response.StatusCode, "upstream_code", lastFailure.UpstreamCode, "account_scoped", lastFailure.AccountScoped)
			if shouldStopForNonAccountFingerprint(failureFingerprints, lastFailure) {
				break
			}
			continue
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			s.selector.markSuccess(ctx, credential, lease.QuotaProbe)
			if diagnostic := response.RecoveredPrimaryFailure; diagnostic != nil {
				recoveredFailure := newHTTPUpstreamFailure(diagnostic.StatusCode, diagnostic.Body, credential.ID, credential.Name)
				if recoveredFailure.AccountBlocked || (credential.Provider == accountdomain.ProviderBuild && s.shouldInvalidateBuildForbidden(recoveredFailure)) {
					reason := fmt.Sprintf("%s primary endpoint denied account access", credential.Provider)
					if !s.markReauthRequired(ctx, input.RequestID, credential, reason) {
						s.selector.MarkModelAccessDenied(ctx, credential, route.UpstreamModel, 0)
					}
				}
			}
		}
		accountID := credential.ID
		var once sync.Once
		finalize := func(usage Usage, responseID, errorCode string) {
			once.Do(func() {
				// HTTP 状态码保留线上真实值；流在 2xx 响应头之后失败时由 errorCode
				// 决定最终结果，避免把协议状态与业务结果混为一谈。
				successful := auditRequestSucceeded(response.StatusCode, errorCode)
				lease.completeSelectorObservation(successful)
				budget := newFinalizationBudget(string(operation), string(route.Provider))
				if isUpstreamStreamFailure(errorCode) {
					if err := budget.run("account_health", finalizationHealthBudget, func(stageCtx context.Context) error {
						return s.selector.MarkFailureAfterSuccess(stageCtx, credential, http.StatusBadGateway, 0)
					}); err != nil {
						s.logger.Warn("stream_failure_health_write_failed", "account_id", credential.ID, "provider", credential.Provider, "error", err)
					}
				}
				lease.Release()
				now := time.Now().UTC()
				record := auditBase
				if usage.Reported {
					record.UsageSource = usageSource
				}
				record.AccountID = &accountID
				record.AccountName = credential.Name
				record.StatusCode = response.StatusCode
				record.InputTokens = usage.InputTokens
				record.CachedInputTokens = usage.CachedInputTokens
				record.OutputTokens = usage.OutputTokens
				record.ReasoningTokens = usage.ReasoningTokens
				record.TotalTokens = usage.TotalTokens
				record.CostInUSDTicks = usage.CostInUSDTicks
				imagePricing, imagePriced := audit.EstimateOfficialImageCost(pricingModel, "", "", response.QuotaUnits)
				if imagePriced {
					record.MediaOutputImages = int64(max(0, response.QuotaUnits))
				}
				tokenPricing, tokenPriced := audit.EstimateOfficialCost(pricingModel, usage.InputTokens, usage.CachedInputTokens, usage.OutputTokens, usage.ContextInputTokens)
				if successful && imagePriced {
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
				if successful && input.Streaming {
					record.FirstTokenMS = firstToken.milliseconds()
				}
				record.DurationMS = time.Since(startedAt).Milliseconds()
				record.ErrorCode = errorCode
				attempts := failureAttempts.snapshot()
				if !successful || len(attempts) > 0 {
					record.Attempts = attempts
				}
				record.CreatedAt = now
				applyAuditEgress(&record, egressTrace, route.Provider)
				if supportsStoredResponses && operation == audit.OperationResponses && responseID != "" && successful {
					err := budget.run("response_ownership", finalizationOwnershipBudget, func(stageCtx context.Context) error {
						return s.responses.Save(stageCtx, inferencedomain.ResponseOwnership{ResponseID: responseID, AccountID: accountID, ClientKeyID: input.ClientKey.ID, ModelRouteID: route.ID, Provider: route.Provider, PromptCacheKey: ownershipPromptCacheKey, ReasoningReplayKey: reasoningReplayKey, ExpiresAt: now.Add(responseOwnershipTTL), CreatedAt: now, UpdatedAt: now})
					})
					if err != nil {
						s.logger.Error("response_ownership_save_failed", "response_id", responseID, "client_key_id", input.ClientKey.ID, "account_id", accountID, "provider", route.Provider, "error", err)
					}
				}
				if successful && lease.QuotaMode != "" {
					if lease.QuotaMode != "weekly" {
						units := max(1, response.QuotaUnits)
						var updated bool
						err := budget.run("quota_decrement", finalizationQuotaBudget, func(stageCtx context.Context) error {
							var decrementErr error
							updated, decrementErr = s.accounts.DecrementQuota(stageCtx, accountID, lease.QuotaMode, units)
							return decrementErr
						})
						if err != nil {
							s.logger.Warn("provider_quota_decrement_failed", "provider", credential.Provider, "account_id", accountID, "mode", lease.QuotaMode, "units", units, "error", err)
						} else if updated {
							s.selector.ConsumeQuota(credential.Provider, accountID, lease.QuotaMode, units)
						}
					}
				}
				if err := budget.run("audit", finalizationAuditBudget, func(stageCtx context.Context) error {
					return s.audits.Create(stageCtx, record)
				}); err != nil {
					s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", input.RequestID, "error", err)
				}
				if usage.ResponseModel != "" {
					_ = budget.run("observed_model", finalizationMetadataBudget, func(stageCtx context.Context) error {
						return s.accounts.ObserveResponseModel(stageCtx, accountID, usage.ResponseModel)
					})
				}
				if successful && lease.QuotaMode != "" {
					if quotaKind, _ := s.providers.QuotaKind(credential.Provider); quotaKind == provider.QuotaRemoteWindow {
						s.accounts.QueueQuotaRefresh(accountID, lease.QuotaMode)
					}
				}
				outcome := "failed"
				if successful {
					outcome = "success"
				}
				timing.finish(s.logger, outcome)
			})
		}
		response.Body = &firstByteReadCloser{ReadCloser: response.Body, mark: timing.markFirstBody}
		recordStreamFailure := func(diagnostic StreamFailureDiagnostic) {
			failureAttempts.captureStreamFailure(credential, responseStartedAt, response, diagnostic)
		}
		var markFirstToken func()
		if firstToken != nil {
			markFirstToken = firstToken.mark
		}
		timingHandedOff = true
		return &Result{StatusCode: response.StatusCode, Status: response.Status, Header: response.Header, Body: &finalizingBody{ReadCloser: response.Body, finalize: func() { finalize(Usage{}, "", "stream_closed") }}, MarkFirstToken: markFirstToken, RecordStreamFailure: recordStreamFailure, Finalize: finalize}, nil
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
	var selectionFailure *SelectionUnavailableError
	if errors.As(lastErr, &selectionFailure) {
		record.StatusCode = selectionFailure.HTTPStatus()
		record.ErrorCode = selectionFailure.Code()
	}
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

func isUpstreamStreamFailure(errorCode string) bool {
	switch errorCode {
	case "upstream_stream_incomplete", "upstream_stream_interrupted":
		return true
	default:
		return false
	}
}

// auditRequestSucceeded keeps transport truth (the HTTP status) separate from
// the terminal request outcome. A stream that fails after 2xx headers is not a
// successful request even though its HTTP status remains 2xx.
func auditRequestSucceeded(statusCode int, errorCode string) bool {
	return statusCode >= 200 && statusCode < 300 && errorCode == ""
}

func isRetryableTransportFailure(providerValue accountdomain.Provider, err error) bool {
	return providerValue != accountdomain.ProviderBuild || !neterrorpkg.IsResponseHeaderTimeout(err)
}

func isSSOCredentialRejected(err error, credential accountdomain.Credential) bool {
	if credential.AuthType != accountdomain.AuthTypeSSO || err == nil {
		return false
	}
	if errors.Is(err, provider.ErrUnauthorized) {
		return true
	}
	status, ok := provider.ErrorHTTPStatus(err)
	return ok && status == http.StatusUnauthorized
}

func (s *Service) markSSOCredentialRejected(ctx context.Context, credential accountdomain.Credential, reason string) {
	if credential.AuthType != accountdomain.AuthTypeSSO {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalizationTimeout)
	defer cancel()
	if err := s.accounts.MarkReauthRequired(writeCtx, credential.ID, reason); err != nil {
		s.logger.Error("account_reauth_required_write_failed", "account_id", credential.ID, "provider", credential.Provider, "error", err)
	}
	// Discard the process-local one-second candidate snapshot even if persistence fails,
	// preventing the invalid account from being selected by the next request.
	s.selector.MarkQuotaStateChanged(credential.Provider)
}

func (s *Service) queueAccountModelSync(accountID uint64) {
	syncer, ok := s.models.(accountModelSyncer)
	if !ok || accountID == 0 {
		return
	}
	s.modelSyncMu.Lock()
	if s.modelSyncing == nil {
		s.modelSyncing = make(map[uint64]struct{})
	}
	if _, exists := s.modelSyncing[accountID]; exists {
		s.modelSyncMu.Unlock()
		return
	}
	s.modelSyncing[accountID] = struct{}{}
	s.modelSyncMu.Unlock()

	go func() {
		defer func() {
			s.modelSyncMu.Lock()
			delete(s.modelSyncing, accountID)
			s.modelSyncMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), modelCatalogRefreshTimeout)
		defer cancel()
		logger := s.logger
		if logger == nil {
			logger = slog.Default()
		}
		count, err := syncer.SyncAccount(ctx, accountID)
		if err != nil {
			logger.Warn("model_etag_refresh_failed", "account_id", accountID, "error", err)
			return
		}
		logger.Info("model_etag_refresh_completed", "account_id", accountID, "models", count)
	}()
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
			if reasoningEffort == modeldomain.ReasoningEffortNone {
				if config != nil {
					delete(config, "effort")
				}
				if len(config) == 0 {
					delete(payload, "output_config")
				} else {
					payload["output_config"] = config
				}
				payload["thinking"] = map[string]any{"type": "disabled"}
				break
			}
			if config == nil {
				config = make(map[string]any)
			}
			config["effort"] = reasoningEffort
			payload["output_config"] = config
			payload["thinking"] = map[string]any{"type": "adaptive"}
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
	accountScope := input.ClientKey.AccountScope()
	if !accountScope.AllowsProvider(ownership.Provider) {
		return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts, Scope: accountScope}
	}
	adapter, ok := s.providers.Responses(ownership.Provider)
	if !ok {
		return nil, ErrResponseAccountUnavailable
	}
	operation := "response_get"
	if method == http.MethodDelete {
		operation = "response_delete"
	}
	physicalCallCtx := infraegress.WithPhysicalCallTrace(ctx, string(ownership.Provider), operation)
	lease, err := s.selector.AcquirePinnedForKey(ctx, ownership.Provider, ownership.AccountID, 0, "", "", false, accountScope)
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
	response, err := adapter.ForwardResponse(physicalCallCtx, provider.ResponseResourceRequest{Credential: credential, Method: method, Path: path})
	if err != nil {
		if isSSOCredentialRejected(err, credential) {
			s.markSSOCredentialRejected(ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
		}
		lease.Release()
		return nil, err
	}
	if response.StatusCode == http.StatusUnauthorized {
		response.Body.Close()
		if credential.AuthType == accountdomain.AuthTypeSSO {
			s.markSSOCredentialRejected(ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
			lease.Release()
			return nil, ErrResponseAccountUnavailable
		}
		if s.markPermanentlyUnrefreshableCredentialRejected(ctx, credential) {
			lease.Release()
			return nil, fmt.Errorf("%w: %w", ErrResponseAccountUnavailable, accountapp.ErrCredentialRefreshPermanent)
		}
		refreshed, refreshErr := s.accounts.EnsureCredential(ctx, credential, true)
		if refreshErr != nil {
			if errors.Is(refreshErr, accountapp.ErrCredentialRefreshPermanent) {
				s.markCredentialRejectedAfterPermanentRefresh(ctx, credential)
			}
			lease.Release()
			return nil, refreshErr
		}
		response, err = adapter.ForwardResponse(physicalCallCtx, provider.ResponseResourceRequest{Credential: refreshed, Method: method, Path: path})
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

// markPermanentlyUnrefreshableCredentialRejected removes an account from the pool after a real upstream request confirms its access token is invalid.
func (s *Service) markPermanentlyUnrefreshableCredentialRejected(ctx context.Context, credential accountdomain.Credential) bool {
	if !credential.RefreshPermanent {
		return false
	}
	s.markCredentialRejectedAfterPermanentRefresh(ctx, credential)
	return true
}

func (s *Service) markCredentialRejectedAfterPermanentRefresh(ctx context.Context, credential accountdomain.Credential) {
	_ = s.accounts.MarkReauthRequired(ctx, credential.ID, fmt.Sprintf("%s OAuth access token rejected after permanent refresh failure", credential.Provider))
	s.selector.MarkQuotaStateChanged(credential.Provider, credential.ID)
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

// shouldStopForNonAccountFingerprint 仅对非账号归因故障累计指纹并在达到阈值后停止换号。
// 账号级失败（额度、鉴权、冷却等）继续轮询其它凭证。
// 未知 403、Team 模型限流只跳过当前号，不累计指纹、不提前结束整次请求。
func shouldStopForNonAccountFingerprint(fingerprints map[string]int, failure *UpstreamFailure) bool {
	if failure == nil || failure.AccountScoped || failure.Fingerprint == "" {
		return false
	}
	if failure.HTTPStatus == http.StatusForbidden {
		return false
	}
	if failure.Fingerprint == "429:team_model_rate_limit" {
		return false
	}
	fingerprints[failure.Fingerprint]++
	limit := nonAccountFailureFingerprintLimit
	if failure.Code == "upstream_stream_idle_timeout" || failure.Fingerprint == "upstream_stream_idle_timeout" {
		limit = streamIdleFailureFingerprintLimit
	}
	return fingerprints[failure.Fingerprint] >= limit
}

func isRetryable(status int) bool {
	return status == 402 || status == 403 || status == 429 || status >= 500
}

func isRetryableResponse(response *provider.Response, upstreamProvider accountdomain.Provider) bool {
	if response == nil || !isRetryable(response.StatusCode) {
		return false
	}
	// Account-scoped payment failures must always rotate accounts.
	// Upstream X-Should-Retry:false is only honored for non-account errors (e.g. 5xx history).
	if forcesAccountFailover(response.StatusCode, upstreamProvider) {
		return true
	}
	return !strings.EqualFold(strings.TrimSpace(response.Header.Get("X-Should-Retry")), "false")
}

// isTerminalRequestForbidden identifies request-level 403 responses that must
// be returned without account or egress side effects. Unknown 403 responses,
// including bare permission-denied, remain on the credential traversal path.
// General request policy classification is Build-specific so Web and Console
// keep their browser/clearance recovery behavior. The exact Console DPoP rollout
// error is also terminal because changing account or egress cannot satisfy it.
func isTerminalRequestForbidden(upstreamProvider accountdomain.Provider, failure *UpstreamFailure) bool {
	if failure == nil {
		return false
	}
	return failure.SafetyRejection ||
		(upstreamProvider == accountdomain.ProviderBuild && failure.RequestScopedForbidden) ||
		(upstreamProvider == accountdomain.ProviderConsole && failure.RequestScopedForbidden && isDPoPProofRequired(failure.UpstreamCode))
}

// forcesAccountFailover keeps Build account-scoped billing, permission, and rate-limit
// failures on the account-rotation path so their state can be recorded before another
// account is selected. free-usage 429 and Team RPS 429 both need rotation even when
// upstream sets X-Should-Retry:false.
func forcesAccountFailover(status int, upstreamProvider accountdomain.Provider) bool {
	return upstreamProvider == accountdomain.ProviderBuild &&
		(status == http.StatusPaymentRequired || status == http.StatusForbidden || status == http.StatusTooManyRequests)
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
