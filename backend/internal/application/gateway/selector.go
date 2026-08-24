package gateway

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/pkg/resultcache"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"golang.org/x/sync/singleflight"
)

type accountLease struct {
	Credential          account.Credential
	Billing             *account.Billing
	QuotaProbe          bool
	QuotaProbeKind      account.QuotaRecoveryKind
	QuotaMode           string
	routingCandidate    *account.RoutingCandidate
	selectorObservation *selectorLeaseObservation
	release             func()
}

const quotaProbeLease = 5 * time.Minute
const successPersistInterval = 30 * time.Second

// Routing writes publish precise invalidation events, so the TTL is only a
// safety net for out-of-process database changes and missed notifications.
// Keeping a one-second TTL made large pools rebuild continuously under load.
const candidateCacheTTL = 30 * time.Second
const candidateCacheStaleTTL = 5 * time.Minute
const candidateCacheRetryTTL = 5 * time.Second
const candidateCacheStaleLogInterval = time.Minute
const maxCandidateCacheSnapshots = 64
const maxCandidateCacheValues = 100_000
const maxRoutingBaseSnapshots = 8
const maxRoutingBaseValues = 150_000
const maxRoutingOverlaySnapshots = 64
const maxRoutingOverlayValues = 250_000
const concurrencySnapshotTTL = 25 * time.Millisecond
const maxConcurrencySnapshots = 256

// Health overrides bridge precise request-path mutations until the immutable
// provider snapshot naturally refreshes. Keep them through the snapshot's
// normal and stale lifetimes so a transient database error cannot resurrect a
// cooled account from an older snapshot.
const routingHealthOverrideTTL = candidateCacheTTL + candidateCacheStaleTTL

const modelAccessDeniedCooldown = 5 * time.Minute

// softNetworkCooldown 网络/超时/5xx 仅短暂隔离本号，避免指数冷却掏空热池。
const softNetworkCooldown = 5 * time.Second

const defaultFreeQuotaRecoveryPause = 24 * time.Hour

var errRoutingCredentialStale = errors.New("routing credential is no longer available")

type quotaRecoveryHints struct {
	Billing *account.Billing
}

type quotaConsumptionKey struct {
	provider  account.Provider
	accountID uint64
	mode      string
}

type accountQuotaConsumptionKey struct {
	accountID uint64
	mode      string
}

type routingHealthOverride struct {
	provider      account.Provider
	failureCount  int
	cooldownUntil *time.Time
	lastError     string
	updatedAt     time.Time
	revision      uint64
	expiresAt     time.Time
}

type candidateSnapshot struct {
	values     []account.RoutingCandidate
	byAccount  map[uint64]int
	expiresAt  time.Time
	staleUntil time.Time
	lastAccess time.Time
}

func newCandidateSnapshot(values []account.RoutingCandidate, expiresAt time.Time) candidateSnapshot {
	byAccount := make(map[uint64]int, len(values))
	for index, value := range values {
		if _, exists := byAccount[value.Credential.ID]; !exists {
			byAccount[value.Credential.ID] = index
		}
	}
	now := time.Now().UTC()
	return candidateSnapshot{values: values, byAccount: byAccount, expiresAt: expiresAt, staleUntil: expiresAt.Add(candidateCacheStaleTTL), lastAccess: now}
}

type candidateCacheKey struct {
	provider      account.Provider
	modelRouteID  uint64
	upstreamModel string
	quotaMode     string
}

type routingBaseCacheKey struct {
	provider  account.Provider
	quotaMode string
}

type routingOverlayCacheKey struct {
	provider      account.Provider
	modelRouteID  uint64
	upstreamModel string
}

type routingLayerVersion struct {
	global   uint64
	provider uint64
}

type routingBaseSnapshot struct {
	values     []account.RoutingAccountBase
	version    routingLayerVersion
	expiresAt  time.Time
	staleUntil time.Time
	lastAccess time.Time
}

type routingOverlaySnapshot struct {
	value      account.RoutingOverlaySnapshot
	version    routingLayerVersion
	expiresAt  time.Time
	staleUntil time.Time
	lastAccess time.Time
}

type SelectionUnavailableReason string

const (
	SelectionNoAccounts       SelectionUnavailableReason = "no_accounts"
	SelectionUnsupportedModel SelectionUnavailableReason = "unsupported_model"
	SelectionCooling          SelectionUnavailableReason = "cooling"
	SelectionModelCooling     SelectionUnavailableReason = "model_cooling"
	SelectionQuotaExhausted   SelectionUnavailableReason = "quota_exhausted"
	SelectionSaturated        SelectionUnavailableReason = "saturated"
)

// SelectionUnavailableError 保留选号失败的真实原因，避免所有情况都退化成模糊的 503。
type SelectionUnavailableError struct {
	Reason     SelectionUnavailableReason
	RetryAfter time.Duration
	Scope      clientkeydomain.AccountScope
}

func (e *SelectionUnavailableError) Error() string {
	if e == nil {
		return "没有可用上游账号"
	}
	prefix := ""
	if e.Scope.IsRestricted() {
		prefix = "Client Key 限定范围"
	}
	switch e.Reason {
	case SelectionUnsupportedModel:
		if prefix != "" {
			return prefix + "不支持该模型"
		}
		return "当前账号池不支持该模型"
	case SelectionCooling:
		if prefix != "" {
			return prefix + "中的可用账号正在冷却"
		}
		return "可用上游账号正在冷却"
	case SelectionModelCooling:
		if prefix != "" {
			return prefix + "中可用账号的目标模型正在冷却"
		}
		return "可用上游账号的目标模型正在冷却"
	case SelectionQuotaExhausted:
		if prefix != "" {
			return prefix + "中的可用账号额度等待恢复"
		}
		return "可用上游账号额度等待恢复"
	case SelectionSaturated:
		if prefix != "" {
			return prefix + "中的可用账号均达到并发上限"
		}
		return "可用上游账号均达到并发上限"
	default:
		if prefix != "" {
			return prefix + "当前没有可用上游账号"
		}
		return "没有可用上游账号"
	}
}

// HTTPStatus returns the client-facing status for a routing refusal.
func (e *SelectionUnavailableError) HTTPStatus() int {
	if e != nil {
		switch e.Reason {
		case SelectionCooling, SelectionModelCooling, SelectionQuotaExhausted:
			return http.StatusTooManyRequests
		}
	}
	return http.StatusServiceUnavailable
}

// Code returns the stable diagnostic code for a routing refusal.
func (e *SelectionUnavailableError) Code() string {
	if e != nil {
		switch e.Reason {
		case SelectionCooling:
			return "upstream_cooling"
		case SelectionModelCooling:
			return "upstream_model_cooling"
		case SelectionQuotaExhausted:
			return "upstream_quota_exhausted"
		case SelectionSaturated:
			return "upstream_saturated"
		case SelectionUnsupportedModel:
			return "upstream_model_unavailable"
		case SelectionNoAccounts:
			if e.Scope.IsRestricted() {
				return "client_key_account_scope_unavailable"
			}
		}
	}
	return "upstream_unavailable"
}

func (l *accountLease) Release() {
	if l == nil {
		return
	}
	if l.selectorObservation != nil {
		l.selectorObservation.completeRelease()
	}
	if l.release != nil {
		l.release()
		l.release = nil
	}
}

func (l *accountLease) markSelectorUpstreamStarted() {
	if l != nil && l.selectorObservation != nil {
		l.selectorObservation.upstreamStarted.Store(true)
	}
}

func (l *accountLease) completeSelectorObservation(success bool) {
	if l != nil && l.selectorObservation != nil {
		l.selectorObservation.complete(success)
	}
}

// Selector 实现可替换的 balanced 账号选择策略。
type Selector struct {
	accounts               repository.AccountRepository
	concurrency            repository.ConcurrencyLimiter
	sticky                 repository.StickySessionRepository
	stickyTTL              time.Duration
	cooldownBase           time.Duration
	cooldownMax            time.Duration
	capacityWait           time.Duration
	preferFreeBuild        bool
	excludeBuildBotFlagged bool
	segmentedConfig        segmentedSelectorConfig
	segmentedState         segmentedSelectorState
	configMu               sync.RWMutex
	candidateMu            sync.Mutex
	selectionMu            sync.RWMutex
	healthMu               sync.RWMutex
	quotaMu                sync.RWMutex
	staleLogMu             sync.Mutex
	logger                 *slog.Logger
	leaseWakeMu            sync.Mutex
	leaseWake              chan struct{}
	lastSelectedAt         map[uint64]time.Time
	lastSuccessAt          map[uint64]time.Time
	healthOverrides        map[uint64]routingHealthOverride
	quotaConsumed          map[quotaConsumptionKey]int
	staleFallbackLoggedAt  map[string]time.Time
	candidates             map[candidateCacheKey]candidateSnapshot
	routingBases           map[routingBaseCacheKey]routingBaseSnapshot
	routingOverlays        map[routingOverlayCacheKey]routingOverlaySnapshot
	routingAccountProvider map[uint64]account.Provider
	baseGlobalVersion      uint64
	overlayGlobalVersion   uint64
	baseProviderVersion    map[account.Provider]uint64
	overlayProviderVersion map[account.Provider]uint64
	candidateLoads         singleflight.Group
	concurrencySnapshots   *resultcache.Cache[[32]byte, map[string]int]
	tierOrders             interface {
		TierOrder(account.Provider, string) []account.WebTier
	}
}

func NewSelector(accounts repository.AccountRepository, concurrency repository.ConcurrencyLimiter, sticky repository.StickySessionRepository, tierOrders interface {
	TierOrder(account.Provider, string) []account.WebTier
}, stickyTTL, cooldownBase, cooldownMax time.Duration, capacityWait ...time.Duration) *Selector {
	wait := time.Duration(0)
	if len(capacityWait) > 0 && capacityWait[0] > 0 {
		wait = capacityWait[0]
	}
	return &Selector{accounts: accounts, concurrency: concurrency, sticky: sticky, tierOrders: tierOrders, stickyTTL: stickyTTL, cooldownBase: cooldownBase, cooldownMax: cooldownMax, capacityWait: wait, leaseWake: make(chan struct{}), logger: slog.Default(), lastSelectedAt: make(map[uint64]time.Time), lastSuccessAt: make(map[uint64]time.Time), healthOverrides: make(map[uint64]routingHealthOverride), quotaConsumed: make(map[quotaConsumptionKey]int), staleFallbackLoggedAt: make(map[string]time.Time), candidates: make(map[candidateCacheKey]candidateSnapshot), routingBases: make(map[routingBaseCacheKey]routingBaseSnapshot), routingOverlays: make(map[routingOverlayCacheKey]routingOverlaySnapshot), routingAccountProvider: make(map[uint64]account.Provider), baseProviderVersion: make(map[account.Provider]uint64), overlayProviderVersion: make(map[account.Provider]uint64), concurrencySnapshots: resultcache.New[[32]byte, map[string]int](maxConcurrencySnapshots, concurrencySnapshotTTL)}
}

// SetLogger wires the application logger into routing degradation diagnostics.
// It is intended to be called during startup before the selector serves traffic.
func (s *Selector) SetLogger(logger *slog.Logger) {
	if logger != nil {
		s.logger = logger
	}
}

func (s *Selector) UpdateConfig(stickyTTL, cooldownBase, cooldownMax time.Duration, capacityWait ...time.Duration) {
	s.configMu.Lock()
	s.stickyTTL = stickyTTL
	s.cooldownBase = cooldownBase
	s.cooldownMax = cooldownMax
	if len(capacityWait) > 0 {
		s.capacityWait = max(time.Duration(0), capacityWait[0])
	}
	s.configMu.Unlock()
}

// UpdatePreferFreeBuild 热更新 Build Free 账号优先策略。
func (s *Selector) UpdatePreferFreeBuild(value bool) {
	s.configMu.Lock()
	s.preferFreeBuild = value
	s.configMu.Unlock()
}

// UpdateSegmentedSelector changes the large-pool bounded planner policy.
func (s *Selector) UpdateSegmentedSelector(enabled bool, minCandidates, windowSize int) {
	s.configMu.Lock()
	s.segmentedConfig = normalizeSegmentedSelectorConfig(segmentedSelectorConfig{
		enabled: enabled, minCandidates: minCandidates, windowSize: windowSize,
	})
	s.configMu.Unlock()
}

func (s *Selector) routingConfig() (time.Duration, time.Duration, time.Duration, time.Duration) {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.stickyTTL, s.cooldownBase, s.cooldownMax, s.capacityWait
}

// UpdateExcludeBuildBotFlaggedFromScheduling toggles Build bot-risk exclusion from
// scheduling and invalidates Build candidate caches when the value changes.
func (s *Selector) UpdateExcludeBuildBotFlaggedFromScheduling(value bool) {
	s.configMu.Lock()
	changed := s.excludeBuildBotFlagged != value
	s.excludeBuildBotFlagged = value
	s.configMu.Unlock()
	if changed {
		s.invalidateProviderCandidateCache(account.ProviderBuild)
	}
}

func (s *Selector) preferFreeBuildEnabled() bool {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.preferFreeBuild
}

func (s *Selector) excludeBuildBotFlaggedEnabled() bool {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.excludeBuildBotFlagged
}

func (s *Selector) invalidateProviderCandidateCache(provider account.Provider) {
	s.candidateMu.Lock()
	defer s.candidateMu.Unlock()
	for key := range s.candidates {
		if key.provider == provider {
			delete(s.candidates, key)
		}
	}
	clearRoutingBases(s.routingBases, provider)
	clearRoutingOverlays(s.routingOverlays, provider)
	if provider != "" {
		s.baseProviderVersion[provider]++
		s.overlayProviderVersion[provider]++
	}
}

func (s *Selector) applyBuildBotFlaggedFilter(_ context.Context, provider account.Provider, values []account.RoutingCandidate) ([]account.RoutingCandidate, error) {
	if provider != account.ProviderBuild || len(values) == 0 {
		return values, nil
	}
	if !s.excludeBuildBotFlaggedEnabled() {
		return values, nil
	}
	filtered := make([]account.RoutingCandidate, 0, len(values))
	for _, candidate := range values {
		if source := candidate.Credential.BuildBotFlagSource; source == 1 || source == 2 {
			continue
		}
		filtered = append(filtered, candidate)
	}
	return filtered, nil
}

func (s *Selector) Acquire(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel, quotaMode, affinityKey string, excluded map[uint64]bool, allowQuotaProbe bool) (*accountLease, error) {
	return s.acquire(ctx, provider, modelRouteID, upstreamModel, quotaMode, affinityKey, excluded, allowQuotaProbe, clientkeydomain.AccountScope{}, 0)
}

func (s *Selector) AcquireForKey(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel, quotaMode, affinityKey string, excluded map[uint64]bool, allowQuotaProbe bool, scope clientkeydomain.AccountScope) (*accountLease, error) {
	return s.acquire(ctx, provider, modelRouteID, upstreamModel, quotaMode, affinityKey, excluded, allowQuotaProbe, scope, 0)
}

// AcquireForKeyOnEgressNode is reserved for administrator probes. It prefers a
// credential bound to the requested node, then borrows any schedulable
// credential when the node's own accounts are unavailable. The request layer
// still forces the physical call through nodeID.
func (s *Selector) AcquireForKeyOnEgressNode(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel, quotaMode, affinityKey string, excluded map[uint64]bool, allowQuotaProbe bool, scope clientkeydomain.AccountScope, nodeID uint64) (*accountLease, error) {
	if nodeID == 0 {
		return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts, Scope: scope}
	}
	lease, err := s.acquire(ctx, provider, modelRouteID, upstreamModel, quotaMode, affinityKey, excluded, allowQuotaProbe, scope, nodeID)
	if err == nil {
		return lease, nil
	}
	var unavailable *SelectionUnavailableError
	if !errors.As(err, &unavailable) {
		return nil, err
	}
	// Probe borrowing must not create or reuse ordinary sticky affinity.
	return s.acquire(ctx, provider, modelRouteID, upstreamModel, quotaMode, "", excluded, allowQuotaProbe, scope, 0)
}

func (s *Selector) acquire(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel, quotaMode, affinityKey string, excluded map[uint64]bool, allowQuotaProbe bool, requestedScope clientkeydomain.AccountScope, forcedEgressNodeID uint64) (lease *accountLease, err error) {
	accountScope, scopeValid := clientkeydomain.NormalizeAccountScope(requestedScope)
	defer annotateSelectionAccountScope(&err, accountScope)
	if !scopeValid || !accountScope.AllowsProvider(provider) {
		return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts, Scope: accountScope}
	}
	now := time.Now().UTC()
	stickyKey := stickySessionKey(affinityKey)
	values, err := s.loadCandidates(ctx, provider, modelRouteID, upstreamModel, quotaMode, now)
	if err != nil {
		return nil, err
	}
	quotaConsumed := s.quotaConsumptionSnapshot(provider)
	healthOverrides := s.routingHealthSnapshot(provider, now)
	// 仅保留候选下标，避免每个请求复制包含凭据、计费和额度结构的完整账号切片。
	normalCandidates := make([]int, 0, len(values))
	probeCandidates := make([]int, 0, len(values))
	supportedCandidates := 0
	consideredCandidates := 0
	coolingCandidates := 0
	modelCoolingCandidates := 0
	quotaCandidates := 0
	var earliestRetry time.Time
	for index, candidate := range values {
		value := applyHealthSnapshot(candidate.Credential, healthOverrides)
		if forcedEgressNodeID != 0 && value.EgressNodeID != forcedEgressNodeID {
			continue
		}
		if !accountScopeAllowsCandidate(provider, accountScope, candidate) {
			continue
		}
		if excluded[value.ID] || value.AuthStatus != account.AuthStatusActive {
			continue
		}
		consideredCandidates++
		if !s.candidateSupportsModel(provider, upstreamModel, quotaMode, candidate) {
			continue
		}
		supportedCandidates++
		if candidate.ModelQuotaBlock != nil && now.Before(candidate.ModelQuotaBlock.CooldownUntil) {
			modelCoolingCandidates++
			earliestRetry = earlierFuture(earliestRetry, candidate.ModelQuotaBlock.CooldownUntil, now)
			continue
		}
		if candidateEgressLeaseCooling(candidate, value, now) {
			coolingCandidates++
			earliestRetry = earlierFuture(earliestRetry, candidate.EgressLeaseBlock.CooldownUntil, now)
			continue
		}
		if value.CooldownUntil != nil && now.Before(*value.CooldownUntil) {
			coolingCandidates++
			earliestRetry = earlierFuture(earliestRetry, *value.CooldownUntil, now)
			continue
		}
		quotaRecovery := candidate.QuotaRecovery
		if quotaRecovery != nil && quotaRecovery.Status != account.QuotaRecoveryStatusActive {
			if allowQuotaProbe && quotaRecovery.NextProbeAt != nil && !now.Before(*quotaRecovery.NextProbeAt) {
				probeCandidates = append(probeCandidates, index)
			} else {
				quotaCandidates++
				if quotaRecovery.NextProbeAt != nil {
					earliestRetry = earlierFuture(earliestRetry, *quotaRecovery.NextProbeAt, now)
				}
			}
			continue
		}
		if candidate.Billing != nil && candidate.Billing.IsExhausted(value.MinimumRemaining) {
			quotaCandidates++
			continue
		}
		if quotaWindowExhausted(candidate, quotaConsumed) {
			quotaCandidates++
			if candidate.QuotaWindow.ResetAt != nil {
				earliestRetry = earlierFuture(earliestRetry, *candidate.QuotaWindow.ResetAt, now)
			}
			continue
		}
		normalCandidates = append(normalCandidates, index)
	}
	if len(normalCandidates) == 0 && len(probeCandidates) == 0 {
		reason := SelectionNoAccounts
		switch {
		case consideredCandidates > 0 && supportedCandidates == 0:
			reason = SelectionUnsupportedModel
		case modelCoolingCandidates > 0:
			reason = SelectionModelCooling
		case coolingCandidates > 0:
			reason = SelectionCooling
		case quotaCandidates > 0:
			reason = SelectionQuotaExhausted
		}
		return nil, &SelectionUnavailableError{Reason: reason, RetryAfter: retryDelay(now, earliestRetry)}
	}
	if len(probeCandidates) > 0 {
		staleClaims := 0
		capacityMisses := 0
		plan, err := s.planCandidateIndexes(ctx, values, probeCandidates, now, s.resolveTierOrder(provider, upstreamModel, quotaMode))
		if err != nil {
			return nil, err
		}
		for candidate, ok := plan.Next(); ok; candidate, ok = plan.Next() {
			lease, err := s.claimAccountSlot(ctx, candidate.Credential)
			if err != nil {
				if errors.Is(err, errRoutingCredentialStale) {
					staleClaims++
					continue
				}
				return nil, err
			}
			if lease == nil {
				capacityMisses++
				continue
			}
			claimed, err := s.accounts.ClaimQuotaProbe(ctx, candidate.Credential.ID, now, now.Add(quotaProbeLease))
			if err != nil || !claimed {
				lease.Release()
				if err != nil {
					return nil, err
				}
				continue
			}
			lease.QuotaProbe = true
			lease.QuotaProbeKind = candidate.QuotaRecovery.Kind
			lease.Billing = candidate.Billing
			return lease, nil
		}
		if len(normalCandidates) == 0 && staleClaims > 0 && capacityMisses == 0 {
			return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts}
		}
	}
	var saturatedStickyID uint64
	if stickyKey != "" {
		stickyID, ok, err := s.sticky.Get(ctx, stickyKey, now)
		if err != nil {
			return nil, fmt.Errorf("读取会话粘滞状态: %w", err)
		}
		if ok {
			candidate, eligible := routingCandidateByID(values, normalCandidates, stickyID)
			if eligible {
				stickyTTL, _, _, _ := s.routingConfig()
				boundID, bindErr := s.sticky.Bind(ctx, stickyKey, stickyID, now, now.Add(stickyTTL))
				if bindErr != nil {
					return nil, fmt.Errorf("刷新会话粘滞状态: %w", bindErr)
				}
				if boundID != stickyID {
					candidate, eligible = routingCandidateByID(values, normalCandidates, boundID)
					stickyID = boundID
				}
				if eligible {
					lease, acquireErr := s.acquirePinnedCapacity(ctx, candidate.Credential)
					if acquireErr == nil {
						lease.Billing = candidate.Billing
						lease.QuotaMode = effectiveQuotaMode(candidate, quotaMode)
						return lease, nil
					}
					if errors.Is(acquireErr, errRoutingCredentialStale) {
						_ = s.sticky.DeleteByAccount(ctx, stickyID)
					} else if !isSelectionUnavailable(acquireErr, SelectionSaturated) {
						return nil, acquireErr
					} else {
						saturatedStickyID = stickyID
					}
				}
			}
		}
	}
	// 粘性账号仅因并发满载而暂时不可用时，先等待该账号；超时后允许本次请求临时借用
	// 其他账号，但不覆盖原绑定，避免并行请求让活跃会话在账号池中来回抖动。
	if saturatedStickyID != 0 {
		plan, err := s.planCandidateIndexes(ctx, values, normalCandidates, time.Now().UTC(), s.resolveTierOrder(provider, upstreamModel, quotaMode))
		if err != nil {
			return nil, err
		}
		for candidate, ok := plan.Next(); ok; candidate, ok = plan.Next() {
			if candidate.Credential.ID == saturatedStickyID {
				continue
			}
			lease, claimErr := s.claimAccountSlot(ctx, candidate.Credential)
			if claimErr != nil {
				if errors.Is(claimErr, errRoutingCredentialStale) {
					continue
				}
				return nil, claimErr
			}
			if lease == nil {
				continue
			}
			lease.Billing = candidate.Billing
			lease.QuotaMode = effectiveQuotaMode(candidate, quotaMode)
			return lease, nil
		}
		return nil, &SelectionUnavailableError{Reason: SelectionSaturated, RetryAfter: time.Second}
	}
	activeRequest := s.nextSegmentedActiveRequest(provider, upstreamModel, quotaMode, len(normalCandidates))
	if activeRequest != nil {
		lease, acquireErr := s.acquireSegmentedCandidates(ctx, values, normalCandidates, quotaMode, s.resolveTierOrder(provider, upstreamModel, quotaMode), *activeRequest)
		if acquireErr != nil || lease == nil || stickyKey == "" {
			return lease, acquireErr
		}
		if lease.routingCandidate == nil {
			lease.Release()
			return nil, errors.New("分段选号缺少候选上下文")
		}
		return s.completeStickyLease(ctx, stickyKey, values, normalCandidates, *lease.routingCandidate, lease, quotaMode)
	}
	_, _, _, capacityWait := s.routingConfig()
	waitDeadline := time.Now().Add(capacityWait)
	for {
		currentTime := time.Now().UTC()
		staleClaims := 0
		capacityMisses := 0
		plan, err := s.planCandidateIndexes(ctx, values, normalCandidates, currentTime, s.resolveTierOrder(provider, upstreamModel, quotaMode))
		if err != nil {
			return nil, err
		}
		for candidate, ok := plan.Next(); ok; candidate, ok = plan.Next() {
			lease, err := s.claimAccountSlot(ctx, candidate.Credential)
			if err != nil {
				if errors.Is(err, errRoutingCredentialStale) {
					staleClaims++
					continue
				}
				return nil, err
			}
			if lease == nil {
				capacityMisses++
				continue
			}
			return s.completeStickyLease(ctx, stickyKey, values, normalCandidates, candidate, lease, quotaMode)
		}
		if staleClaims > 0 && capacityMisses == 0 {
			return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts}
		}
		if capacityWait <= 0 {
			return nil, &SelectionUnavailableError{Reason: SelectionSaturated, RetryAfter: time.Second}
		}
		retry, err := s.awaitLeaseRetry(ctx, waitDeadline)
		if err != nil {
			return nil, err
		}
		if !retry {
			return nil, &SelectionUnavailableError{Reason: SelectionSaturated, RetryAfter: time.Second}
		}
	}
}

func (s *Selector) completeStickyLease(ctx context.Context, stickyKey string, values []account.RoutingCandidate, normalCandidates []int, candidate account.RoutingCandidate, lease *accountLease, quotaMode string) (*accountLease, error) {
	if stickyKey != "" {
		stickyTTL, _, _, _ := s.routingConfig()
		now := time.Now().UTC()
		boundID, err := s.sticky.Bind(ctx, stickyKey, candidate.Credential.ID, now, now.Add(stickyTTL))
		if err != nil {
			lease.Release()
			return nil, fmt.Errorf("写入会话粘滞状态: %w", err)
		}
		if boundID != candidate.Credential.ID {
			if boundCandidate, eligible := routingCandidateByID(values, normalCandidates, boundID); eligible {
				boundLease, acquireErr := s.acquirePinnedCapacity(ctx, boundCandidate.Credential)
				if acquireErr == nil {
					lease.Release()
					lease = boundLease
					candidate = boundCandidate
				} else if errors.Is(acquireErr, errRoutingCredentialStale) {
					_ = s.sticky.DeleteByAccount(ctx, boundID)
					if err := s.sticky.Set(ctx, stickyKey, candidate.Credential.ID, now.Add(stickyTTL)); err != nil {
						lease.Release()
						return nil, fmt.Errorf("重建会话粘滞状态: %w", err)
					}
				} else if !isSelectionUnavailable(acquireErr, SelectionSaturated) {
					lease.Release()
					return nil, acquireErr
				}
				// 已绑定账号满载时保留原绑定，本次请求使用已获取的临时账号。
			} else if err := s.sticky.Set(ctx, stickyKey, candidate.Credential.ID, now.Add(stickyTTL)); err != nil {
				lease.Release()
				return nil, fmt.Errorf("重建会话粘滞状态: %w", err)
			}
		}
	}
	lease.Billing = candidate.Billing
	lease.QuotaMode = effectiveQuotaMode(candidate, quotaMode)
	return lease, nil
}

// stickySessionKey 将调用方粘滞 identity 压缩为固定长度，仅用于账号粘滞索引。
func stickySessionKey(value string) string {
	if value == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func routingCandidateByID(values []account.RoutingCandidate, indexes []int, accountID uint64) (account.RoutingCandidate, bool) {
	for _, index := range indexes {
		candidate := values[index]
		if candidate.Credential.ID == accountID {
			return candidate, true
		}
	}
	return account.RoutingCandidate{}, false
}

func isSelectionUnavailable(err error, reason SelectionUnavailableReason) bool {
	var unavailable *SelectionUnavailableError
	return errors.As(err, &unavailable) && unavailable.Reason == reason
}

// AcquirePinned 为 previous_response_id 等账号归属请求获取指定账号租约。
func (s *Selector) AcquirePinned(ctx context.Context, provider account.Provider, accountID, modelRouteID uint64, upstreamModel, quotaMode string, inference bool) (*accountLease, error) {
	return s.acquirePinned(ctx, provider, accountID, modelRouteID, upstreamModel, quotaMode, inference, false, clientkeydomain.AccountScope{})
}

func (s *Selector) AcquirePinnedForKey(ctx context.Context, provider account.Provider, accountID, modelRouteID uint64, upstreamModel, quotaMode string, inference bool, scope clientkeydomain.AccountScope) (*accountLease, error) {
	return s.acquirePinned(ctx, provider, accountID, modelRouteID, upstreamModel, quotaMode, inference, false, scope)
}

// AcquirePinnedForQualityProbe keeps every ordinary eligibility check while
// bypassing only the exact account+node lease block being recovery-tested.
func (s *Selector) AcquirePinnedForQualityProbe(ctx context.Context, provider account.Provider, accountID, modelRouteID uint64, upstreamModel, quotaMode string, scope clientkeydomain.AccountScope) (*accountLease, error) {
	return s.acquirePinned(ctx, provider, accountID, modelRouteID, upstreamModel, quotaMode, true, true, scope)
}

func (s *Selector) acquirePinned(ctx context.Context, provider account.Provider, accountID, modelRouteID uint64, upstreamModel, quotaMode string, inference, ignoreEgressLeaseBlock bool, requestedScope clientkeydomain.AccountScope) (lease *accountLease, err error) {
	accountScope, scopeValid := clientkeydomain.NormalizeAccountScope(requestedScope)
	defer annotateSelectionAccountScope(&err, accountScope)
	if !scopeValid || !accountScope.AllowsProvider(provider) {
		return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts, Scope: accountScope}
	}
	now := time.Now().UTC()
	values, err := s.loadCandidates(ctx, provider, modelRouteID, upstreamModel, quotaMode, now)
	if err != nil {
		return nil, err
	}
	quotaConsumed := s.quotaConsumptionSnapshot(provider)
	healthOverrides := s.routingHealthSnapshot(provider, now)
	for _, candidate := range values {
		value := applyHealthSnapshot(candidate.Credential, healthOverrides)
		if value.ID != accountID {
			continue
		}
		if !accountScopeAllowsCandidate(provider, accountScope, candidate) {
			return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts}
		}
		if !value.Enabled || value.AuthStatus != account.AuthStatusActive {
			return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts}
		}
		if inference {
			if !s.candidateSupportsModel(provider, upstreamModel, quotaMode, candidate) {
				return nil, &SelectionUnavailableError{Reason: SelectionUnsupportedModel}
			}
			if candidate.ModelQuotaBlock != nil && now.Before(candidate.ModelQuotaBlock.CooldownUntil) {
				return nil, &SelectionUnavailableError{Reason: SelectionModelCooling, RetryAfter: retryDelay(now, candidate.ModelQuotaBlock.CooldownUntil)}
			}
			if !ignoreEgressLeaseBlock && candidateEgressLeaseCooling(candidate, value, now) {
				return nil, &SelectionUnavailableError{Reason: SelectionCooling, RetryAfter: retryDelay(now, candidate.EgressLeaseBlock.CooldownUntil)}
			}
			if value.CooldownUntil != nil && now.Before(*value.CooldownUntil) {
				return nil, &SelectionUnavailableError{Reason: SelectionCooling, RetryAfter: retryDelay(now, *value.CooldownUntil)}
			}
			if recovery := candidate.QuotaRecovery; recovery != nil && recovery.Status != account.QuotaRecoveryStatusActive {
				if recovery.NextProbeAt == nil || now.Before(*recovery.NextProbeAt) {
					var retryAfter time.Duration
					if recovery.NextProbeAt != nil {
						retryAfter = retryDelay(now, *recovery.NextProbeAt)
					}
					return nil, &SelectionUnavailableError{Reason: SelectionQuotaExhausted, RetryAfter: retryAfter}
				}
				lease, err := s.acquirePinnedCapacity(ctx, value)
				if err != nil {
					if errors.Is(err, errRoutingCredentialStale) {
						return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts}
					}
					return nil, err
				}
				claimed, err := s.accounts.ClaimQuotaProbe(ctx, value.ID, now, now.Add(quotaProbeLease))
				if err != nil || !claimed {
					lease.Release()
					if err != nil {
						return nil, err
					}
					return nil, fmt.Errorf("绑定的上游账号恢复探测已被占用")
				}
				lease.QuotaProbe = true
				lease.QuotaProbeKind = recovery.Kind
				lease.Billing = candidate.Billing
				return lease, nil
			}
			if candidate.Billing != nil && candidate.Billing.IsExhausted(value.MinimumRemaining) {
				return nil, &SelectionUnavailableError{Reason: SelectionQuotaExhausted}
			}
			if quotaWindowExhausted(candidate, quotaConsumed) {
				var retryAfter time.Duration
				if candidate.QuotaWindow.ResetAt != nil {
					retryAfter = retryDelay(now, *candidate.QuotaWindow.ResetAt)
				}
				return nil, &SelectionUnavailableError{Reason: SelectionQuotaExhausted, RetryAfter: retryAfter}
			}
		}
		lease, err := s.acquirePinnedCapacity(ctx, value)
		if err != nil {
			if errors.Is(err, errRoutingCredentialStale) {
				return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts}
			}
			return nil, err
		}
		lease.Billing = candidate.Billing
		lease.QuotaMode = effectiveQuotaMode(candidate, quotaMode)
		return lease, nil
	}
	return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts}
}

func accountScopeAllowsCandidate(provider account.Provider, scope clientkeydomain.AccountScope, candidate account.RoutingCandidate) bool {
	if provider == account.ProviderConsole {
		return true
	}
	tier := clientkeydomain.AccountTierUnknown
	switch provider {
	case account.ProviderBuild:
		if candidate.IsKnownFreeBuild() {
			tier = clientkeydomain.AccountTierFree
		} else if account.IsBuildSuper(candidate.Credential, candidate.Billing) {
			tier = clientkeydomain.AccountTierSuper
		}
	case account.ProviderWeb:
		switch candidate.Credential.WebTier {
		case account.WebTierBasic:
			tier = clientkeydomain.AccountTierFree
		case account.WebTierSuper, account.WebTierHeavy:
			tier = clientkeydomain.AccountTierSuper
		}
	}
	switch tier {
	case clientkeydomain.AccountTierFree:
		return scope.Tiers&clientkeydomain.TierScopeFree != 0
	case clientkeydomain.AccountTierSuper:
		return scope.Tiers&clientkeydomain.TierScopeSuper != 0
	default:
		return scope.Tiers&clientkeydomain.TierScopeUnknown != 0
	}
}

func annotateSelectionAccountScope(err *error, scope clientkeydomain.AccountScope) {
	if err == nil || *err == nil || !scope.IsRestricted() {
		return
	}
	var unavailable *SelectionUnavailableError
	if errors.As(*err, &unavailable) {
		unavailable.Scope = scope
	}
}

func effectiveQuotaMode(candidate account.RoutingCandidate, fallback string) string {
	if candidate.QuotaWindow != nil && candidate.QuotaWindow.Mode != "" {
		return candidate.QuotaWindow.Mode
	}
	if candidate.Credential.Provider == account.ProviderWeb && fallback == account.QuotaModeWebImageEdit {
		switch candidate.Credential.WebTier {
		case account.WebTierSuper, account.WebTierHeavy:
			return account.QuotaModeWebImageEdit
		default:
			return account.QuotaModeWebImagePro
		}
	}
	return fallback
}

func candidateEgressLeaseCooling(candidate account.RoutingCandidate, credential account.Credential, now time.Time) bool {
	block := candidate.EgressLeaseBlock
	return block != nil && block.AccountID == credential.ID && block.NodeID != 0 && credential.EgressNodeID == block.NodeID && now.Before(block.CooldownUntil)
}

// candidateSupportsModel treats a recognized Web catalog entry as an
// effective capability for tiers that the adapter explicitly allows. This
// prevents a historical capability snapshot from blocking a newly enabled
// catalog feature, while unknown/manual Web routes and all other providers
// retain the persisted snapshot semantics.
func (s *Selector) candidateSupportsModel(provider account.Provider, upstreamModel, quotaMode string, candidate account.RoutingCandidate) bool {
	if provider == account.ProviderWeb {
		order := s.resolveTierOrder(provider, upstreamModel, quotaMode)
		if len(order) > 0 {
			return webTierInOrder(order, candidate.Credential.WebTier)
		}
	}
	return !candidate.ModelCapabilityKnown || candidate.SupportsModel
}

func (s *Selector) MarkSuccess(ctx context.Context, credential account.Credential) {
	s.markSuccess(ctx, credential, true)
}

func isMissingThinkingStrike(lastError string) bool {
	return lastError == lastErrorMissingThinking || lastError == lastErrorMissingThinkingDisabled
}

type missingThinkingPenaltyResult string

const (
	missingThinkingPenaltyUnchanged missingThinkingPenaltyResult = "unchanged"
	missingThinkingPenaltyCooled    missingThinkingPenaltyResult = "cooled"
	missingThinkingPenaltyDisabled  missingThinkingPenaltyResult = "disabled"
)

func (s *Selector) markSuccess(ctx context.Context, credential account.Credential, quotaProbe bool) {
	now := time.Now().UTC()
	keepThinkingStrike := isMissingThinkingStrike(credential.LastError)
	healthChanged := credential.FailureCount > 0 || credential.CooldownUntil != nil || credential.LastError != ""
	touchLastUsed := healthChanged
	s.selectionMu.Lock()
	if last := s.lastSuccessAt[credential.ID]; last.IsZero() || now.Sub(last) >= successPersistInterval {
		touchLastUsed = true
	}
	if touchLastUsed {
		s.lastSuccessAt[credential.ID] = now
	}
	s.selectionMu.Unlock()
	if healthChanged {
		lastError := ""
		if keepThinkingStrike {
			lastError = lastErrorMissingThinking
		}
		if err := s.accounts.UpdateHealth(ctx, credential.ID, credential.Provider, 0, nil, lastError, true); err == nil {
			s.ApplyInvalidation(repository.InvalidationEvent{
				Kind: repository.InvalidationAccountHealthChanged, Provider: credential.Provider, AccountID: credential.ID,
				HealthMarker: account.NormalizeHealthMarker(lastError),
			})
		}
	} else if touchLastUsed {
		_ = s.accounts.TouchLastUsed(ctx, credential.ID, now)
	}
	if quotaProbe {
		_ = s.accounts.ClearQuotaRecovery(ctx, credential.ID)
	}
	if quotaProbe {
		s.evictCandidate(credential.Provider, credential.ID)
	}
}

func (s *Selector) MarkFreeQuotaExhausted(ctx context.Context, credential account.Credential, used, limit int64) {
	now := time.Now().UTC()
	nextProbeAt := now.Add(defaultFreeQuotaRecoveryPause)
	_ = s.markFreeQuotaExhaustedAt(ctx, credential, used, limit, now, nextProbeAt)
}

func (s *Selector) markFreeQuotaExhaustedAt(ctx context.Context, credential account.Credential, used, limit int64, now, nextProbeAt time.Time) error {
	if err := s.accounts.SaveQuotaRecovery(ctx, account.QuotaRecovery{
		AccountID: credential.ID, Kind: account.QuotaRecoveryKindFree, Status: account.QuotaRecoveryStatusExhausted,
		ConfirmedUsed: used, ConfirmedLimit: limit, ExhaustedAt: &now,
		NextProbeAt: &nextProbeAt, LastConfirmedAt: &now, UpdatedAt: now,
	}); err != nil {
		return err
	}
	_ = s.sticky.DeleteByAccount(ctx, credential.ID)
	s.invalidateCandidates(credential.Provider)
	return nil
}

func (s *Selector) MarkModelQuotaExhausted(ctx context.Context, credential account.Credential, billing *account.Billing, upstreamModel string, retryAfter time.Duration) {
	upstreamModel = strings.TrimSpace(upstreamModel)
	if upstreamModel == "" {
		s.MarkFreeQuotaExhausted(ctx, credential, 0, 0)
		return
	}
	knownFreeBuild := (account.RoutingCandidate{Credential: credential, Billing: billing}).IsKnownFreeBuild()
	if knownFreeBuild || retryAfter <= 0 {
		retryAfter = defaultFreeQuotaRecoveryPause
	}
	until := time.Now().UTC().Add(retryAfter)
	_ = s.accounts.UpsertModelQuotaBlock(ctx, account.ModelQuotaBlock{
		AccountID: credential.ID, UpstreamModel: upstreamModel, Reason: "model_quota_depleted", CooldownUntil: until, UpdatedAt: time.Now().UTC(),
	})
	// The model block makes affected bindings ineligible and they are rebound on
	// the next request. Preserve unrelated model/session affinity for this account.
	s.invalidateCandidates(credential.Provider)
}

// MarkModelAccessDenied isolates a permission failure to the rejected model.
// Build OAuth accounts may still have valid video access when a chat endpoint
// returns 403, so a model denial must not invalidate the whole credential.
func (s *Selector) MarkModelAccessDenied(ctx context.Context, credential account.Credential, upstreamModel string, retryAfter time.Duration) error {
	upstreamModel = strings.TrimSpace(upstreamModel)
	if upstreamModel == "" {
		return nil
	}
	if retryAfter <= 0 {
		retryAfter = modelAccessDeniedCooldown
	}
	now := time.Now().UTC()
	if err := s.accounts.UpsertModelQuotaBlock(ctx, account.ModelQuotaBlock{
		AccountID: credential.ID, UpstreamModel: upstreamModel, Reason: "model_access_denied",
		CooldownUntil: now.Add(retryAfter), UpdatedAt: now,
	}); err != nil {
		return err
	}
	s.evictCandidate(credential.Provider, credential.ID)
	return nil
}

// MarkPaymentQuotaExhausted removes a spending-limited account from routing.
// Paid accounts follow their upstream billing period; Free or unknown accounts
// use the fixed local recovery window.
func (s *Selector) MarkPaymentQuotaExhausted(ctx context.Context, credential account.Credential, hints quotaRecoveryHints) error {
	now := time.Now().UTC()
	if hints.Billing != nil && hints.Billing.IsPaid() {
		if periodEnd, ok := hints.Billing.PeriodEnd(); ok && periodEnd.After(now) {
			if err := s.accounts.SaveQuotaRecovery(ctx, account.QuotaRecovery{
				AccountID: credential.ID, Kind: account.QuotaRecoveryKindPaid, Status: account.QuotaRecoveryStatusExhausted,
				ExhaustedAt: &now, NextProbeAt: &periodEnd, LastConfirmedAt: &now, UpdatedAt: now,
			}); err != nil {
				return err
			}
			_ = s.sticky.DeleteByAccount(ctx, credential.ID)
			s.invalidateCandidates(credential.Provider)
			return nil
		}
	}
	return s.markFreeQuotaExhaustedAt(ctx, credential, 0, 0, now, now.Add(defaultFreeQuotaRecoveryPause))
}

// MarkQuotaStateChanged 在 Billing 探测改变持久化额度状态后更新对应账号的候选快照。
// 未提供账号 ID 时保留全量失效语义，供无法确定变更范围的调用方使用。
func (s *Selector) MarkQuotaStateChanged(provider account.Provider, accountIDs ...uint64) {
	if len(accountIDs) == 0 {
		s.invalidateCandidates(provider)
		return
	}
	for _, accountID := range accountIDs {
		s.clearQuotaConsumptionAccount(provider, accountID)
		s.evictCandidate(provider, accountID)
	}
}

// ConsumeQuota records a small local delta instead of Copy-on-Write cloning
// every cached candidate/base slice. Selection snapshots remain immutable for
// concurrent requests, while the next request observes the consumed amount.
func (s *Selector) ConsumeQuota(provider account.Provider, accountID uint64, mode string, amount int) {
	if accountID == 0 || mode == "" || mode == "weekly" || amount <= 0 {
		return
	}
	s.quotaMu.Lock()
	if s.quotaConsumed == nil {
		s.quotaConsumed = make(map[quotaConsumptionKey]int)
	}
	key := quotaConsumptionKey{provider: provider, accountID: accountID, mode: mode}
	s.quotaConsumed[key] += amount
	s.quotaMu.Unlock()
}

func (s *Selector) quotaConsumptionSnapshot(provider account.Provider) map[accountQuotaConsumptionKey]int {
	s.quotaMu.RLock()
	if len(s.quotaConsumed) == 0 {
		s.quotaMu.RUnlock()
		return nil
	}
	result := make(map[accountQuotaConsumptionKey]int)
	for key, amount := range s.quotaConsumed {
		if key.provider == provider {
			result[accountQuotaConsumptionKey{accountID: key.accountID, mode: key.mode}] = amount
		}
	}
	s.quotaMu.RUnlock()
	return result
}

func quotaWindowExhausted(candidate account.RoutingCandidate, consumed map[accountQuotaConsumptionKey]int) bool {
	if candidate.QuotaWindow == nil {
		return false
	}
	remaining := candidate.QuotaWindow.Remaining - consumed[accountQuotaConsumptionKey{accountID: candidate.Credential.ID, mode: candidate.QuotaWindow.Mode}]
	return remaining <= 0
}

func (s *Selector) clearQuotaConsumption(provider account.Provider) {
	s.quotaMu.Lock()
	if provider == "" {
		clear(s.quotaConsumed)
	} else {
		for key := range s.quotaConsumed {
			if key.provider == provider {
				delete(s.quotaConsumed, key)
			}
		}
	}
	s.quotaMu.Unlock()
}

func (s *Selector) clearQuotaConsumptionAccount(provider account.Provider, accountID uint64) {
	s.quotaMu.Lock()
	for key := range s.quotaConsumed {
		if key.provider == provider && key.accountID == accountID {
			delete(s.quotaConsumed, key)
		}
	}
	s.quotaMu.Unlock()
}

// markMissingThinking cools an account for the first no-thinking hit and
// disables it if missing thinking appears again after that cooldown.
func (s *Selector) markMissingThinking(ctx context.Context, credential account.Credential, cooldown time.Duration) (missingThinkingPenaltyResult, error) {
	if cooldown <= 0 {
		cooldown = defaultMissingThinkingCooldown
	}
	now := time.Now().UTC()
	inCooldown := credential.CooldownUntil != nil && now.Before(*credential.CooldownUntil)
	if isMissingThinkingStrike(credential.LastError) && !inCooldown {
		disabled := false
		if _, err := s.accounts.UpdateMany(ctx, credential.Provider, []uint64{credential.ID}, repository.AccountUpdates{Enabled: &disabled}); err != nil {
			return missingThinkingPenaltyUnchanged, err
		}
		healthErr := s.accounts.UpdateHealth(ctx, credential.ID, credential.Provider, credential.FailureCount, nil, lastErrorMissingThinkingDisabled, false)
		s.ApplyInvalidation(repository.InvalidationEvent{
			Kind: repository.InvalidationAccountStateChanged, Provider: credential.Provider, AccountID: credential.ID,
		})
		s.evictCandidate(credential.Provider, credential.ID)
		if s.sticky != nil {
			_ = s.sticky.DeleteByAccount(ctx, credential.ID)
		}
		return missingThinkingPenaltyDisabled, healthErr
	}
	if inCooldown {
		return missingThinkingPenaltyUnchanged, nil
	}
	until := now.Add(cooldown)
	if err := s.accounts.UpdateHealth(ctx, credential.ID, credential.Provider, credential.FailureCount, &until, lastErrorMissingThinking, false); err != nil {
		return missingThinkingPenaltyUnchanged, err
	}
	s.ApplyInvalidation(repository.InvalidationEvent{
		Kind: repository.InvalidationAccountHealthChanged, Provider: credential.Provider, AccountID: credential.ID,
		FailureCount: credential.FailureCount, CooldownUntil: &until, HealthMarker: account.LastErrorMissingThinking,
	})
	s.evictCandidate(credential.Provider, credential.ID)
	return missingThinkingPenaltyCooled, nil
}

func (s *Selector) MarkFailure(ctx context.Context, credential account.Credential, status int, retryAfter time.Duration) {
	_ = s.markFailure(ctx, credential, credential.FailureCount, credential.FailureCount+1, status, retryAfter, status == 0)
}

// MarkFailureAfterSuccess records a stream failure from a fresh health baseline.
// The upstream already returned a successful response header, so failures that
// preceded this request must not be carried into the new cooldown calculation.
func (s *Selector) MarkFailureAfterSuccess(ctx context.Context, credential account.Credential, status int, retryAfter time.Duration) error {
	return s.markFailure(ctx, credential, 0, 1, status, retryAfter, status == 0)
}

// markSoftFailure preserves the real upstream status for diagnostics while
// applying the bounded, non-accumulating health penalty used for transient
// network and provider-wide failures.
func (s *Selector) markSoftFailure(ctx context.Context, credential account.Credential, status int, retryAfter time.Duration) error {
	return s.markFailure(ctx, credential, credential.FailureCount, credential.FailureCount+1, status, retryAfter, true)
}

func (s *Selector) markFailure(ctx context.Context, credential account.Credential, baselineFailureCount, nextFailureCount, status int, retryAfter time.Duration, soft bool) error {
	_, cooldownBase, cooldownMax, _ := s.routingConfig()
	// Soft failures only isolate this account briefly and never accumulate the
	// durable failure count. Hard account-scoped 4xx responses retain the
	// exponential policy below.
	effectiveFailureCount := nextFailureCount
	cooldown := cooldownBase
	if soft {
		effectiveFailureCount = baselineFailureCount
		cooldown = softNetworkCooldown
		if retryAfter > cooldown {
			cooldown = retryAfter
		}
	} else {
		for i := 1; i < effectiveFailureCount && cooldown < cooldownMax; i++ {
			cooldown *= 2
		}
		if cooldown > cooldownMax {
			cooldown = cooldownMax
		}
		if retryAfter > cooldown {
			cooldown = retryAfter
		}
	}
	until := time.Now().UTC().Add(cooldown)
	lastError := fmt.Sprintf("upstream status %d", status)
	healthErr := s.accounts.UpdateHealth(ctx, credential.ID, credential.Provider, effectiveFailureCount, &until, lastError, false)
	if healthErr == nil {
		s.ApplyInvalidation(repository.InvalidationEvent{
			Kind: repository.InvalidationAccountHealthChanged, Provider: credential.Provider, AccountID: credential.ID,
			FailureCount: effectiveFailureCount, CooldownUntil: &until,
		})
	}
	if status == 401 || status == 402 || status == 403 || status == 429 {
		_ = s.sticky.DeleteByAccount(ctx, credential.ID)
	}
	return healthErr
}

func (s *Selector) loadCandidates(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel, quotaMode string, now time.Time) ([]account.RoutingCandidate, error) {
	if _, ok := s.accounts.(repository.RoutingLayerRepository); ok {
		return s.loadLayeredCandidates(ctx, provider, modelRouteID, upstreamModel, quotaMode, now)
	}
	return s.loadCombinedCandidates(ctx, provider, modelRouteID, upstreamModel, quotaMode, now)
}

func (s *Selector) loadCombinedCandidates(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel, quotaMode string, now time.Time) ([]account.RoutingCandidate, error) {
	key := candidateCacheKey{provider: provider, modelRouteID: modelRouteID, upstreamModel: upstreamModel, quotaMode: quotaMode}
	s.candidateMu.Lock()
	if snapshot, ok := s.candidates[key]; ok && now.Before(snapshot.expiresAt) {
		snapshot.lastAccess = now
		s.candidates[key] = snapshot
		s.candidateMu.Unlock()
		return snapshot.values, nil
	}
	s.candidateMu.Unlock()
	loadKey := fmt.Sprintf("%s\x00%d\x00%s\x00%s", provider, modelRouteID, upstreamModel, quotaMode)
	loaded, err, _ := s.candidateLoads.Do(loadKey, func() (any, error) {
		checkTime := time.Now().UTC()
		var stale candidateSnapshot
		hasStale := false
		s.candidateMu.Lock()
		if snapshot, ok := s.candidates[key]; ok {
			if checkTime.Before(snapshot.expiresAt) {
				snapshot.lastAccess = checkTime
				s.candidates[key] = snapshot
				s.candidateMu.Unlock()
				return snapshot.values, nil
			}
			if checkTime.Before(snapshot.staleUntil) {
				stale, hasStale = snapshot, true
			}
		}
		s.candidateMu.Unlock()
		values, err := s.accounts.ListRoutingCandidates(ctx, provider, modelRouteID, upstreamModel, quotaMode)
		if err != nil {
			if hasStale && canUseStaleRoutingSnapshot(ctx, err) {
				s.candidateMu.Lock()
				stale.lastAccess = checkTime
				stale.expiresAt = staleRetryExpiry(checkTime, stale.staleUntil)
				s.storeCandidateSnapshotLocked(key, stale, checkTime)
				s.candidateMu.Unlock()
				s.logStaleRoutingFallback("combined", provider, checkTime, stale.staleUntil, err)
				return stale.values, nil
			}
			return nil, err
		}
		values, err = s.applyBuildBotFlaggedFilter(ctx, provider, values)
		if err != nil {
			return nil, err
		}
		s.candidateMu.Lock()
		s.storeCandidateSnapshotLocked(key, newCandidateSnapshot(values, checkTime.Add(candidateCacheTTL)), checkTime)
		s.candidateMu.Unlock()
		return values, nil
	})
	if err != nil {
		return nil, err
	}
	return loaded.([]account.RoutingCandidate), nil
}

func (s *Selector) loadLayeredCandidates(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel, quotaMode string, now time.Time) ([]account.RoutingCandidate, error) {
	key := candidateCacheKey{provider: provider, modelRouteID: modelRouteID, upstreamModel: upstreamModel, quotaMode: quotaMode}
	s.candidateMu.Lock()
	if snapshot, ok := s.candidates[key]; ok && now.Before(snapshot.expiresAt) {
		snapshot.lastAccess = now
		s.candidates[key] = snapshot
		s.candidateMu.Unlock()
		return snapshot.values, nil
	}
	s.candidateMu.Unlock()
	loadKey := fmt.Sprintf("assembled\x00%s\x00%d\x00%s\x00%s", provider, modelRouteID, upstreamModel, quotaMode)
	loaded, err, _ := s.candidateLoads.Do(loadKey, func() (any, error) {
		checkTime := time.Now().UTC()
		s.candidateMu.Lock()
		if snapshot, ok := s.candidates[key]; ok && checkTime.Before(snapshot.expiresAt) {
			snapshot.lastAccess = checkTime
			s.candidates[key] = snapshot
			s.candidateMu.Unlock()
			return snapshot.values, nil
		}
		s.candidateMu.Unlock()
		layered := s.accounts.(repository.RoutingLayerRepository)
		for attempt := 0; attempt < 4; attempt++ {
			bases, baseVersion, loadErr := s.loadRoutingBases(ctx, layered, provider, quotaMode, checkTime)
			if loadErr != nil {
				return nil, loadErr
			}
			overlay, overlayVersion, loadErr := s.loadRoutingOverlay(ctx, layered, provider, modelRouteID, upstreamModel, checkTime)
			if loadErr != nil {
				return nil, loadErr
			}
			if !s.routingVersionsStable(provider, baseVersion, overlayVersion) {
				checkTime = time.Now().UTC()
				continue
			}
			values := assembleRoutingCandidates(provider, quotaMode, bases, overlay)
			values, filterErr := s.applyBuildBotFlaggedFilter(ctx, provider, values)
			if filterErr != nil {
				return nil, filterErr
			}
			s.candidateMu.Lock()
			stable := baseVersion == s.routingBaseVersionLocked(provider) && overlayVersion == s.routingOverlayVersionLocked(provider)
			if stable {
				s.storeCandidateSnapshotLocked(key, newCandidateSnapshot(values, checkTime.Add(candidateCacheTTL)), checkTime)
			}
			s.candidateMu.Unlock()
			if stable {
				return values, nil
			}
			checkTime = time.Now().UTC()
		}
		// Sustained account synchronization must not turn cache churn into user-facing
		// failures. Fall back to the established authoritative combined query.
		values, err := s.accounts.ListRoutingCandidates(ctx, provider, modelRouteID, upstreamModel, quotaMode)
		if err != nil {
			return nil, err
		}
		return s.applyBuildBotFlaggedFilter(ctx, provider, values)
	})
	if err != nil {
		return nil, err
	}
	return loaded.([]account.RoutingCandidate), nil
}

func (s *Selector) loadRoutingBases(ctx context.Context, layered repository.RoutingLayerRepository, provider account.Provider, quotaMode string, now time.Time) ([]account.RoutingAccountBase, routingLayerVersion, error) {
	key := routingBaseCacheKey{provider: provider, quotaMode: quotaMode}
	version := s.routingBaseVersion(provider)
	s.candidateMu.Lock()
	if snapshot, ok := s.routingBases[key]; ok && now.Before(snapshot.expiresAt) && snapshot.version == version {
		snapshot.lastAccess = now
		s.routingBases[key] = snapshot
		values := snapshot.values
		s.candidateMu.Unlock()
		return values, version, nil
	}
	s.candidateMu.Unlock()
	loadKey := "base\x00" + string(provider) + "\x00" + quotaMode
	loaded, err, _ := s.candidateLoads.Do(loadKey, func() (any, error) {
		checkTime := time.Now().UTC()
		checkVersion := s.routingBaseVersion(provider)
		var stale routingBaseSnapshot
		hasStale := false
		s.candidateMu.Lock()
		if snapshot, ok := s.routingBases[key]; ok && snapshot.version == checkVersion {
			if checkTime.Before(snapshot.expiresAt) {
				snapshot.lastAccess = checkTime
				s.routingBases[key] = snapshot
				values := snapshot.values
				s.candidateMu.Unlock()
				return routingBaseLoadResult{values: values, version: checkVersion}, nil
			}
			if checkTime.Before(snapshot.staleUntil) {
				stale, hasStale = snapshot, true
			}
		}
		s.candidateMu.Unlock()
		values, loadErr := layered.ListRoutingAccountBases(ctx, provider, quotaMode)
		if loadErr != nil {
			if hasStale && canUseStaleRoutingSnapshot(ctx, loadErr) {
				s.candidateMu.Lock()
				stale.lastAccess = checkTime
				stale.expiresAt = staleRetryExpiry(checkTime, stale.staleUntil)
				s.storeRoutingBaseSnapshotLocked(key, stale, checkTime)
				s.candidateMu.Unlock()
				s.logStaleRoutingFallback("base", provider, checkTime, stale.staleUntil, loadErr)
				return routingBaseLoadResult{values: stale.values, version: checkVersion}, nil
			}
			return nil, loadErr
		}
		s.candidateMu.Lock()
		currentVersion := s.routingBaseVersionLocked(provider)
		if currentVersion == checkVersion {
			s.clearQuotaConsumption(provider)
			s.storeRoutingBaseSnapshotLocked(key, routingBaseSnapshot{values: values, version: checkVersion, expiresAt: checkTime.Add(candidateCacheTTL)}, checkTime)
			for accountID, cachedProvider := range s.routingAccountProvider {
				if cachedProvider == provider {
					delete(s.routingAccountProvider, accountID)
				}
			}
			for _, value := range values {
				s.routingAccountProvider[value.Credential.ID] = provider
			}
		}
		s.candidateMu.Unlock()
		return routingBaseLoadResult{values: values, version: checkVersion}, nil
	})
	if err != nil {
		return nil, routingLayerVersion{}, err
	}
	result := loaded.(routingBaseLoadResult)
	return result.values, result.version, nil
}

func (s *Selector) loadRoutingOverlay(ctx context.Context, layered repository.RoutingLayerRepository, provider account.Provider, modelRouteID uint64, upstreamModel string, now time.Time) (account.RoutingOverlaySnapshot, routingLayerVersion, error) {
	key := routingOverlayCacheKey{provider: provider, modelRouteID: modelRouteID, upstreamModel: upstreamModel}
	version := s.routingOverlayVersion(provider)
	s.candidateMu.Lock()
	if snapshot, ok := s.routingOverlays[key]; ok && now.Before(snapshot.expiresAt) && snapshot.version == version {
		snapshot.lastAccess = now
		s.routingOverlays[key] = snapshot
		value := snapshot.value
		s.candidateMu.Unlock()
		return value, version, nil
	}
	s.candidateMu.Unlock()
	loadKey := fmt.Sprintf("overlay\x00%s\x00%d\x00%s", provider, modelRouteID, upstreamModel)
	loaded, err, _ := s.candidateLoads.Do(loadKey, func() (any, error) {
		checkTime := time.Now().UTC()
		checkVersion := s.routingOverlayVersion(provider)
		var stale routingOverlaySnapshot
		hasStale := false
		s.candidateMu.Lock()
		if snapshot, ok := s.routingOverlays[key]; ok && snapshot.version == checkVersion {
			if checkTime.Before(snapshot.expiresAt) {
				snapshot.lastAccess = checkTime
				s.routingOverlays[key] = snapshot
				value := snapshot.value
				s.candidateMu.Unlock()
				return routingOverlayLoadResult{value: value, version: checkVersion}, nil
			}
			if checkTime.Before(snapshot.staleUntil) {
				stale, hasStale = snapshot, true
			}
		}
		s.candidateMu.Unlock()
		value, loadErr := layered.ListRoutingAccountOverlays(ctx, provider, modelRouteID, upstreamModel)
		if loadErr != nil {
			if hasStale && canUseStaleRoutingSnapshot(ctx, loadErr) {
				s.candidateMu.Lock()
				stale.lastAccess = checkTime
				stale.expiresAt = staleRetryExpiry(checkTime, stale.staleUntil)
				s.storeRoutingOverlaySnapshotLocked(key, stale, checkTime)
				s.candidateMu.Unlock()
				s.logStaleRoutingFallback("overlay", provider, checkTime, stale.staleUntil, loadErr)
				return routingOverlayLoadResult{value: stale.value, version: checkVersion}, nil
			}
			return nil, loadErr
		}
		s.candidateMu.Lock()
		currentVersion := s.routingOverlayVersionLocked(provider)
		if currentVersion == checkVersion {
			s.storeRoutingOverlaySnapshotLocked(key, routingOverlaySnapshot{value: value, version: checkVersion, expiresAt: checkTime.Add(candidateCacheTTL)}, checkTime)
		}
		s.candidateMu.Unlock()
		return routingOverlayLoadResult{value: value, version: checkVersion}, nil
	})
	if err != nil {
		return account.RoutingOverlaySnapshot{}, routingLayerVersion{}, err
	}
	result := loaded.(routingOverlayLoadResult)
	return result.value, result.version, nil
}

func (s *Selector) routingBaseVersion(provider account.Provider) routingLayerVersion {
	s.candidateMu.Lock()
	defer s.candidateMu.Unlock()
	return s.routingBaseVersionLocked(provider)
}

func (s *Selector) routingBaseVersionLocked(provider account.Provider) routingLayerVersion {
	return routingLayerVersion{global: s.baseGlobalVersion, provider: s.baseProviderVersion[provider]}
}

func (s *Selector) routingOverlayVersion(provider account.Provider) routingLayerVersion {
	s.candidateMu.Lock()
	defer s.candidateMu.Unlock()
	return s.routingOverlayVersionLocked(provider)
}

func (s *Selector) routingOverlayVersionLocked(provider account.Provider) routingLayerVersion {
	return routingLayerVersion{global: s.overlayGlobalVersion, provider: s.overlayProviderVersion[provider]}
}

func (s *Selector) routingVersionsStable(provider account.Provider, base, overlay routingLayerVersion) bool {
	s.candidateMu.Lock()
	defer s.candidateMu.Unlock()
	return base == s.routingBaseVersionLocked(provider) && overlay == s.routingOverlayVersionLocked(provider)
}

func (s *Selector) applyHealthInvalidation(event repository.InvalidationEvent) {
	updatedAt := event.PublishedAt.UTC()
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	expiresAt := updatedAt.Add(routingHealthOverrideTTL)
	if !time.Now().UTC().Before(expiresAt) {
		return
	}
	var cooldownUntil *time.Time
	if event.CooldownUntil != nil {
		value := event.CooldownUntil.UTC()
		cooldownUntil = &value
	}
	overrideLastError := account.NormalizeHealthMarker(event.HealthMarker)
	if overrideLastError == "" && (event.FailureCount > 0 || cooldownUntil != nil) {
		overrideLastError = "upstream failure"
	}
	value := routingHealthOverride{
		provider: event.Provider, failureCount: max(0, event.FailureCount), cooldownUntil: cooldownUntil,
		lastError: overrideLastError, updatedAt: updatedAt, revision: event.Revision, expiresAt: expiresAt,
	}
	s.healthMu.Lock()
	current, exists := s.healthOverrides[event.AccountID]
	if exists {
		if current.revision > 0 && value.revision > 0 && value.revision < current.revision {
			s.healthMu.Unlock()
			return
		}
		if (current.revision == 0 || value.revision == 0) && value.updatedAt.Before(current.updatedAt) {
			s.healthMu.Unlock()
			return
		}
	}
	s.healthOverrides[event.AccountID] = value
	s.healthMu.Unlock()
}

func (s *Selector) applyRoutingHealth(value account.Credential, now time.Time) account.Credential {
	s.healthMu.RLock()
	override, exists := s.healthOverrides[value.ID]
	s.healthMu.RUnlock()
	if !exists || override.provider != value.Provider {
		return value
	}
	if !now.Before(override.expiresAt) {
		s.healthMu.Lock()
		if current, ok := s.healthOverrides[value.ID]; ok && !now.Before(current.expiresAt) {
			delete(s.healthOverrides, value.ID)
		}
		s.healthMu.Unlock()
		return value
	}
	value.FailureCount = override.failureCount
	value.CooldownUntil = override.cooldownUntil
	value.LastError = override.lastError
	return value
}

func (s *Selector) routingHealthSnapshot(provider account.Provider, now time.Time) map[uint64]routingHealthOverride {
	var result map[uint64]routingHealthOverride
	expired := make([]uint64, 0)
	s.healthMu.RLock()
	for accountID, value := range s.healthOverrides {
		if !now.Before(value.expiresAt) {
			expired = append(expired, accountID)
			continue
		}
		if value.provider == provider {
			if result == nil {
				result = make(map[uint64]routingHealthOverride)
			}
			result[accountID] = value
		}
	}
	s.healthMu.RUnlock()
	if len(expired) > 0 {
		s.healthMu.Lock()
		for _, accountID := range expired {
			if value, ok := s.healthOverrides[accountID]; ok && !now.Before(value.expiresAt) {
				delete(s.healthOverrides, accountID)
			}
		}
		s.healthMu.Unlock()
	}
	return result
}

func applyHealthSnapshot(value account.Credential, overrides map[uint64]routingHealthOverride) account.Credential {
	if override, ok := overrides[value.ID]; ok && override.provider == value.Provider {
		value.FailureCount = override.failureCount
		value.CooldownUntil = override.cooldownUntil
		value.LastError = override.lastError
	}
	return value
}

func (s *Selector) clearHealthOverrides(provider account.Provider, accountID uint64) {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	if accountID != 0 {
		delete(s.healthOverrides, accountID)
		return
	}
	if provider == "" {
		clear(s.healthOverrides)
		return
	}
	for id, value := range s.healthOverrides {
		if value.provider == provider {
			delete(s.healthOverrides, id)
		}
	}
}

// ApplyInvalidation advances local layer generations before any remote publish.
func (s *Selector) ApplyInvalidation(event repository.InvalidationEvent) {
	if !event.Valid() {
		return
	}
	if event.Kind == repository.InvalidationAccountHealthChanged {
		s.applyHealthInvalidation(event)
		return
	}
	s.clearHealthOverrides(event.Provider, event.AccountID)
	layer := event.Layer()
	if layer != repository.InvalidationLayerRoute && layer != repository.InvalidationLayerBase && layer != repository.InvalidationLayerOverlay {
		return
	}
	s.candidateMu.Lock()
	provider := event.Provider
	if provider == "" && event.AccountID != 0 {
		provider = s.routingAccountProvider[event.AccountID]
		if provider == "" {
			for key, snapshot := range s.candidates {
				if _, ok := snapshot.byAccount[event.AccountID]; ok {
					provider = key.provider
					break
				}
			}
		}
	}
	base := layer == repository.InvalidationLayerBase
	overlay := layer == repository.InvalidationLayerOverlay || layer == repository.InvalidationLayerRoute
	if base {
		s.clearQuotaConsumption(provider)
		if provider == "" {
			s.baseGlobalVersion++
			clearRoutingBases(s.routingBases, "")
		} else {
			s.baseProviderVersion[provider]++
			clearRoutingBases(s.routingBases, provider)
		}
	}
	if overlay {
		if provider == "" {
			s.overlayGlobalVersion++
			clearRoutingOverlays(s.routingOverlays, "")
		} else {
			s.overlayProviderVersion[provider]++
			clearRoutingOverlays(s.routingOverlays, provider)
		}
	}
	for key := range s.candidates {
		if provider == "" || key.provider == provider {
			delete(s.candidates, key)
		}
	}
	s.candidateMu.Unlock()
}

func clearRoutingBases(values map[routingBaseCacheKey]routingBaseSnapshot, provider account.Provider) {
	for key := range values {
		if provider == "" || key.provider == provider {
			delete(values, key)
		}
	}
}

func clearRoutingOverlays(values map[routingOverlayCacheKey]routingOverlaySnapshot, provider account.Provider) {
	for key := range values {
		if provider == "" || key.provider == provider {
			delete(values, key)
		}
	}
}

func cacheSnapshotAccess(lastAccess, expiresAt time.Time) time.Time {
	if !lastAccess.IsZero() {
		return lastAccess
	}
	return expiresAt
}

func staleRetryExpiry(now, staleUntil time.Time) time.Time {
	expiresAt := now.Add(candidateCacheRetryTTL)
	if !staleUntil.IsZero() && expiresAt.After(staleUntil) {
		return staleUntil
	}
	return expiresAt
}

// canUseStaleRoutingSnapshot deliberately accepts only errors that carry a
// transient signal. Serving stale data for cancellations, schema/query bugs,
// or repository validation failures would hide correctness problems.
func canUseStaleRoutingSnapshot(ctx context.Context, err error) bool {
	if err == nil || ctx != nil && ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, repository.ErrNotFound) || errors.Is(err, repository.ErrConflict) || errors.Is(err, repository.ErrLimitExceeded) || errors.Is(err, repository.ErrInvalidRecord) || errors.Is(err, repository.ErrAccountPoolMismatch) {
		return false
	}
	if errors.Is(err, sql.ErrConnDone) || errors.Is(err, driver.ErrBadConn) {
		return true
	}
	var networkError net.Error
	if errors.As(err, &networkError) && (networkError.Timeout() || networkError.Temporary()) {
		return true
	}
	var temporary interface{ Temporary() bool }
	if errors.As(err, &temporary) && temporary.Temporary() {
		return true
	}
	// modernc SQLite exposes the primary/extended result through Code().
	// SQLITE_BUSY (5) and SQLITE_LOCKED (6) are safe to retry.
	var sqliteError interface{ Code() int }
	if errors.As(err, &sqliteError) {
		switch sqliteError.Code() & 0xff {
		case 5, 6:
			return true
		}
	}
	// pgx exposes SQLSTATE without requiring the application layer to depend on
	// a concrete PostgreSQL driver type.
	var postgresError interface{ SQLState() string }
	if errors.As(err, &postgresError) {
		switch state := postgresError.SQLState(); {
		case strings.HasPrefix(state, "08"), strings.HasPrefix(state, "40"), state == "55P03", state == "57P01", state == "57P02", state == "57P03":
			return true
		}
	}
	return false
}

func (s *Selector) logStaleRoutingFallback(layer string, provider account.Provider, now, staleUntil time.Time, err error) {
	key := layer + "\x00" + string(provider)
	s.staleLogMu.Lock()
	if s.staleFallbackLoggedAt == nil {
		s.staleFallbackLoggedAt = make(map[string]time.Time)
	}
	last := s.staleFallbackLoggedAt[key]
	if !last.IsZero() && now.Sub(last) < candidateCacheStaleLogInterval {
		s.staleLogMu.Unlock()
		return
	}
	s.staleFallbackLoggedAt[key] = now
	s.staleLogMu.Unlock()

	staleFor := now.Sub(staleUntil.Add(-candidateCacheStaleTTL))
	if staleFor < 0 {
		staleFor = 0
	}
	logger := s.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("routing_snapshot_stale_fallback",
		"provider", provider,
		"layer", layer,
		"stale_for_ms", staleFor.Milliseconds(),
		"retry_after_ms", candidateCacheRetryTTL.Milliseconds(),
		"error_type", fmt.Sprintf("%T", err),
	)
}

type snapshotCacheMetadata struct {
	values     int
	staleUntil time.Time
	accessedAt time.Time
}

func pruneSnapshotCache[K comparable, V any](values map[K]V, protected K, now time.Time, maxSnapshots, maxValues int, metadata func(V) snapshotCacheMetadata) {
	for key, value := range values {
		entry := metadata(value)
		if key != protected && !entry.staleUntil.IsZero() && !now.Before(entry.staleUntil) {
			delete(values, key)
		}
	}
	for {
		total := 0
		for _, value := range values {
			total += metadata(value).values
		}
		// A single oversized provider pool must remain usable. The budget becomes
		// a strict bound as soon as there is another evictable snapshot.
		if len(values) <= maxSnapshots && (total <= maxValues || len(values) == 1) {
			return
		}
		var oldestKey K
		var oldestAt time.Time
		found := false
		for key, value := range values {
			if key == protected {
				continue
			}
			accessedAt := metadata(value).accessedAt
			if !found || accessedAt.Before(oldestAt) {
				oldestKey, oldestAt, found = key, accessedAt, true
			}
		}
		if !found {
			return
		}
		delete(values, oldestKey)
	}
}

func (s *Selector) storeCandidateSnapshotLocked(key candidateCacheKey, snapshot candidateSnapshot, now time.Time) {
	snapshot.lastAccess = now
	if snapshot.staleUntil.IsZero() {
		snapshot.staleUntil = snapshot.expiresAt.Add(candidateCacheStaleTTL)
	}
	s.candidates[key] = snapshot
	pruneSnapshotCache(s.candidates, key, now, maxCandidateCacheSnapshots, maxCandidateCacheValues, func(value candidateSnapshot) snapshotCacheMetadata {
		return snapshotCacheMetadata{values: len(value.values), staleUntil: value.staleUntil, accessedAt: cacheSnapshotAccess(value.lastAccess, value.expiresAt)}
	})
}

func (s *Selector) storeRoutingBaseSnapshotLocked(key routingBaseCacheKey, snapshot routingBaseSnapshot, now time.Time) {
	snapshot.lastAccess = now
	if snapshot.staleUntil.IsZero() {
		snapshot.staleUntil = snapshot.expiresAt.Add(candidateCacheStaleTTL)
	}
	s.routingBases[key] = snapshot
	pruneSnapshotCache(s.routingBases, key, now, maxRoutingBaseSnapshots, maxRoutingBaseValues, func(value routingBaseSnapshot) snapshotCacheMetadata {
		return snapshotCacheMetadata{values: len(value.values), staleUntil: value.staleUntil, accessedAt: cacheSnapshotAccess(value.lastAccess, value.expiresAt)}
	})
}

func (s *Selector) storeRoutingOverlaySnapshotLocked(key routingOverlayCacheKey, snapshot routingOverlaySnapshot, now time.Time) {
	snapshot.lastAccess = now
	if snapshot.staleUntil.IsZero() {
		snapshot.staleUntil = snapshot.expiresAt.Add(candidateCacheStaleTTL)
	}
	s.routingOverlays[key] = snapshot
	pruneSnapshotCache(s.routingOverlays, key, now, maxRoutingOverlaySnapshots, maxRoutingOverlayValues, func(value routingOverlaySnapshot) snapshotCacheMetadata {
		return snapshotCacheMetadata{values: len(value.value.Values), staleUntil: value.staleUntil, accessedAt: cacheSnapshotAccess(value.lastAccess, value.expiresAt)}
	})
}

type routingBaseLoadResult struct {
	values  []account.RoutingAccountBase
	version routingLayerVersion
}

type routingOverlayLoadResult struct {
	value   account.RoutingOverlaySnapshot
	version routingLayerVersion
}

func assembleRoutingCandidates(provider account.Provider, quotaMode string, bases []account.RoutingAccountBase, overlay account.RoutingOverlaySnapshot) []account.RoutingCandidate {
	byAccount := make(map[uint64]account.RoutingAccountOverlay, len(overlay.Values))
	for _, value := range overlay.Values {
		byAccount[value.AccountID] = value
	}
	sharedSuperBuildModel := false
	if provider == account.ProviderBuild && !overlay.HasBindings {
		for _, base := range bases {
			value, exists := byAccount[base.Credential.ID]
			if exists && value.SupportsModel && account.IsBuildSuper(base.Credential, base.Billing) {
				sharedSuperBuildModel = true
				break
			}
		}
	}
	result := make([]account.RoutingCandidate, 0, len(bases))
	staticProviderModel := (provider == account.ProviderConsole && strings.TrimSpace(quotaMode) != "") ||
		(provider == account.ProviderWeb && account.IsWebImagineQuotaMode(quotaMode))
	for _, base := range bases {
		overlayValue := byAccount[base.Credential.ID]
		if overlay.HasBindings && !overlayValue.Bound {
			continue
		}
		known, supports := overlayValue.ModelCapabilityKnown, overlayValue.SupportsModel
		if staticProviderModel {
			known, supports = true, true
		} else if overlay.HasBindings {
			known, supports = true, true
		} else if sharedSuperBuildModel && account.IsBuildSuper(base.Credential, base.Billing) {
			known, supports = true, true
		}
		result = append(result, account.RoutingCandidate{
			Credential: base.Credential, Billing: base.Billing, QuotaWindow: base.QuotaWindow, QuotaRecovery: base.QuotaRecovery,
			EgressLeaseBlock: base.EgressLeaseBlock, ModelQuotaBlock: overlayValue.ModelQuotaBlock, ModelCapabilityKnown: known, SupportsModel: supports,
		})
	}
	return result
}

func (s *Selector) invalidateCandidates(provider account.Provider) {
	s.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: provider})
	s.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationAccountCapabilityChanged, Provider: provider})
}

// evictCandidate 从当前进程的候选快照中移除一个账号。持久化状态仍由调用方先写入；
// 下一个缓存周期会以数据库中的新状态重新加载该账号，不会因单账号变化清空整个 Provider。
func (s *Selector) evictCandidate(provider account.Provider, accountID uint64) {
	if accountID == 0 {
		return
	}
	s.candidateMu.Lock()
	defer s.candidateMu.Unlock()
	for key, snapshot := range s.candidates {
		if key.provider != provider {
			continue
		}
		// 候选快照会被并发请求的 selectionSession 只读复用；必须 copy-on-write，
		// 不能复用底层数组，否则会改写正在执行的请求视图。
		values := make([]account.RoutingCandidate, 0, len(snapshot.values))
		removed := false
		for _, candidate := range snapshot.values {
			if candidate.Credential.ID == accountID {
				removed = true
				continue
			}
			values = append(values, candidate)
		}
		if removed {
			// also update byAccount index if present
			if snapshot.byAccount != nil {
				byAccount := make(map[uint64]int, len(values))
				for idx, candidate := range values {
					byAccount[candidate.Credential.ID] = idx
				}
				snapshot.byAccount = byAccount
			}
			snapshot.values = values
			s.candidates[key] = snapshot
		}
	}
}

func (s *Selector) claimAccountSlot(ctx context.Context, value account.Credential) (*accountLease, error) {
	now := time.Now().UTC()
	value = s.applyRoutingHealth(value, now)
	if value.CooldownUntil != nil && now.Before(*value.CooldownUntil) {
		return nil, nil
	}
	limit := value.MaxConcurrent
	if limit <= 0 {
		limit = account.DefaultMaxConcurrent
	}
	release, acquired, err := s.concurrency.Acquire(ctx, accountConcurrencyKey(value.ID), limit)
	if err != nil {
		return nil, fmt.Errorf("获取账号并发租约: %w", err)
	}
	if !acquired {
		return nil, nil
	}
	releaseSlot := func() {
		release()
		s.announceLeaseReturn()
	}
	if s.accounts != nil {
		material, loadErr := s.accounts.GetCredentialMaterial(ctx, value.ID, value.Provider)
		if loadErr != nil {
			releaseSlot()
			if errors.Is(loadErr, repository.ErrNotFound) {
				s.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: value.Provider, AccountID: value.ID})
				return nil, errRoutingCredentialStale
			}
			return nil, fmt.Errorf("加载账号执行凭据: %w", loadErr)
		}
		hydrated, matched := material.ApplyTo(value)
		if !matched {
			releaseSlot()
			s.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: value.Provider, AccountID: value.ID})
			return nil, errRoutingCredentialStale
		}
		value = hydrated
	}
	s.selectionMu.Lock()
	s.lastSelectedAt[value.ID] = time.Now().UTC()
	s.selectionMu.Unlock()
	return &accountLease{Credential: value, release: func() {
		releaseSlot()
	}}, nil
}

func (s *Selector) acquirePinnedCapacity(ctx context.Context, value account.Credential) (*accountLease, error) {
	_, _, _, capacityWait := s.routingConfig()
	deadline := time.Now().Add(capacityWait)
	for {
		lease, err := s.claimAccountSlot(ctx, value)
		if err != nil || lease != nil {
			return lease, err
		}
		if capacityWait <= 0 {
			return nil, &SelectionUnavailableError{Reason: SelectionSaturated, RetryAfter: time.Second}
		}
		retry, err := s.awaitLeaseRetry(ctx, deadline)
		if err != nil {
			return nil, err
		}
		if !retry {
			return nil, &SelectionUnavailableError{Reason: SelectionSaturated, RetryAfter: time.Second}
		}
	}
}

func (s *Selector) leaseReturnNotice() <-chan struct{} {
	s.leaseWakeMu.Lock()
	defer s.leaseWakeMu.Unlock()
	if s.leaseWake == nil {
		s.leaseWake = make(chan struct{})
	}
	return s.leaseWake
}

func (s *Selector) announceLeaseReturn() {
	s.leaseWakeMu.Lock()
	if s.leaseWake != nil {
		close(s.leaseWake)
	}
	s.leaseWake = make(chan struct{})
	s.leaseWakeMu.Unlock()
}

// awaitLeaseRetry 在本实例归还租约时立即重试；短轮询用于感知其他实例释放的共享并发名额。
func (s *Selector) awaitLeaseRetry(ctx context.Context, deadline time.Time) (bool, error) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false, nil
	}
	notice := s.leaseReturnNotice()
	timer := time.NewTimer(min(remaining, 100*time.Millisecond))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-notice:
		return true, nil
	case <-timer.C:
		return time.Now().Before(deadline), nil
	}
}

func earlierFuture(current, candidate, now time.Time) time.Time {
	if candidate.IsZero() || !now.Before(candidate) {
		return current
	}
	if current.IsZero() || candidate.Before(current) {
		return candidate
	}
	return current
}

func retryDelay(now, retryAt time.Time) time.Duration {
	if retryAt.IsZero() || !now.Before(retryAt) {
		return 0
	}
	return retryAt.Sub(now)
}

func (s *Selector) resolveTierOrder(provider account.Provider, upstreamModel, quotaMode string) []account.WebTier {
	if s.tierOrders == nil {
		return nil
	}
	if resolver, ok := s.tierOrders.(interface {
		TierOrderForQuotaMode(account.Provider, string, string) []account.WebTier
	}); ok {
		return resolver.TierOrderForQuotaMode(provider, upstreamModel, quotaMode)
	}
	return s.tierOrders.TierOrder(provider, upstreamModel)
}

func tierOrderRank(order []account.WebTier, tier account.WebTier) int {
	tier = normalizedRoutingWebTier(tier)
	for index, value := range order {
		if value == tier {
			return index
		}
	}
	return len(order)
}

func normalizedRoutingWebTier(tier account.WebTier) account.WebTier {
	if tier == "" || tier == account.WebTierAuto {
		return account.WebTierBasic
	}
	return tier
}

func webTierInOrder(order []account.WebTier, tier account.WebTier) bool {
	tier = normalizedRoutingWebTier(tier)
	for _, allowed := range order {
		if allowed == tier {
			return true
		}
	}
	return false
}
