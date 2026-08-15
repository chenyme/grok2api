package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
)

// selectionSession 保存一次下游请求的候选快照和计划。账号切换时复用它，
// 避免每次失败都重新加载整个账号池、读取并发快照并建堆。
type selectionSession struct {
	selector         *Selector
	provider         account.Provider
	modelRouteID     uint64
	upstreamModel    string
	quotaMode        string
	stickyKey        string
	values           []account.RoutingCandidate
	quotaConsumed    map[accountQuotaConsumptionKey]int
	normalCandidates []int
	probeCandidates  []int
	normalPlan       *candidatePlan
	probePlan        *candidatePlan
	retryAccountID   uint64
	stickyTried      bool
	staleCandidates  map[uint64]bool
}

func (s *Selector) beginSelectionSession(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel, quotaMode, affinityKey string, excluded map[uint64]bool, allowQuotaProbe bool) (*selectionSession, error) {
	return s.beginSelectionSessionForKey(ctx, provider, modelRouteID, upstreamModel, quotaMode, affinityKey, excluded, allowQuotaProbe, clientkeydomain.AccountScope{})
}

func (s *Selector) beginSelectionSessionForKey(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel, quotaMode, affinityKey string, excluded map[uint64]bool, allowQuotaProbe bool, requestedScope clientkeydomain.AccountScope) (session *selectionSession, err error) {
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

	session = &selectionSession{
		selector:        s,
		provider:        provider,
		modelRouteID:    modelRouteID,
		upstreamModel:   upstreamModel,
		quotaMode:       quotaMode,
		stickyKey:       stickySessionKey(affinityKey),
		values:          values,
		quotaConsumed:   quotaConsumed,
		staleCandidates: make(map[uint64]bool),
	}
	consideredCandidates := 0
	supportedCandidates := 0
	coolingCandidates := 0
	modelCoolingCandidates := 0
	quotaCandidates := 0
	var earliestRetry time.Time

	for index, candidate := range values {
		value := applyHealthSnapshot(candidate.Credential, healthOverrides)
		if !accountScopeAllowsCandidate(provider, accountScope, candidate) {
			continue
		}
		if excluded[value.ID] || value.AuthStatus != account.AuthStatusActive {
			continue
		}
		consideredCandidates++
		if candidate.ModelCapabilityKnown && !candidate.SupportsModel {
			continue
		}
		supportedCandidates++
		if candidate.ModelQuotaBlock != nil && now.Before(candidate.ModelQuotaBlock.CooldownUntil) {
			modelCoolingCandidates++
			earliestRetry = earlierFuture(earliestRetry, candidate.ModelQuotaBlock.CooldownUntil, now)
			continue
		}
		if value.CooldownUntil != nil && now.Before(*value.CooldownUntil) {
			coolingCandidates++
			earliestRetry = earlierFuture(earliestRetry, *value.CooldownUntil, now)
			continue
		}
		if recovery := candidate.QuotaRecovery; recovery != nil && recovery.Status != account.QuotaRecoveryStatusActive {
			if recovery.NextProbeAt != nil && !now.Before(*recovery.NextProbeAt) {
				session.probeCandidates = append(session.probeCandidates, index)
			} else {
				quotaCandidates++
				if recovery.NextProbeAt != nil {
					earliestRetry = earlierFuture(earliestRetry, *recovery.NextProbeAt, now)
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
		session.normalCandidates = append(session.normalCandidates, index)
	}

	if len(session.normalCandidates) > 0 || (allowQuotaProbe && len(session.probeCandidates) > 0) {
		return session, nil
	}
	reason := SelectionNoAccounts
	switch {
	case consideredCandidates > 0 && supportedCandidates == 0:
		reason = SelectionUnsupportedModel
	case modelCoolingCandidates > 0:
		reason = SelectionModelCooling
	case coolingCandidates > 0:
		reason = SelectionCooling
	case quotaCandidates > 0 || len(session.probeCandidates) > 0:
		reason = SelectionQuotaExhausted
	}
	return nil, &SelectionUnavailableError{Reason: reason, RetryAfter: retryDelay(now, earliestRetry), Scope: accountScope}
}

// Acquire 从请求级候选计划中获取下一个账号。被 excluded 的账号不会重新入选。
func (session *selectionSession) Acquire(ctx context.Context, excluded map[uint64]bool, allowQuotaProbe bool) (*accountLease, error) {
	if session.retryAccountID != 0 {
		accountID := session.retryAccountID
		session.retryAccountID = 0
		if !session.candidateExcluded(excluded, accountID) {
			if candidate, ok := routingCandidateByID(session.values, session.normalCandidates, accountID); ok {
				lease, err := session.selector.claimAccountSlot(ctx, candidate.Credential)
				if err != nil {
					if errors.Is(err, errRoutingCredentialStale) {
						session.markCandidateStale(accountID)
					} else {
						return nil, err
					}
				} else if lease != nil {
					return session.completeNormalLease(ctx, lease, candidate, excluded)
				}
			}
		}
	}
	if allowQuotaProbe {
		lease, err := session.acquireQuotaProbe(ctx, excluded)
		if err != nil || lease != nil {
			return lease, err
		}
	}
	return session.acquireNormal(ctx, excluded)
}

// RetryAccount 将一个已被本请求取出的普通账号放回下一次选号的最前面。
// 仅用于出口重建后的无账号归因重试，不能用于账号级失败。
func (session *selectionSession) RetryAccount(accountID uint64) {
	if accountID == 0 {
		return
	}
	for _, index := range session.normalCandidates {
		if session.values[index].Credential.ID == accountID {
			session.retryAccountID = accountID
			return
		}
	}
}

func (session *selectionSession) acquireQuotaProbe(ctx context.Context, excluded map[uint64]bool) (*accountLease, error) {
	if len(session.probeCandidates) == 0 {
		return nil, nil
	}
	if session.probePlan == nil {
		plan, err := session.selector.planCandidateIndexes(ctx, session.values, session.probeCandidates, time.Now().UTC(), session.selector.resolveTierOrder(session.provider, session.upstreamModel))
		if err != nil {
			return nil, err
		}
		session.probePlan = plan
	}
	for candidate, ok := session.probePlan.Next(); ok; candidate, ok = session.probePlan.Next() {
		if session.candidateExcluded(excluded, candidate.Credential.ID) {
			continue
		}
		lease, err := session.selector.claimAccountSlot(ctx, candidate.Credential)
		if err != nil {
			if errors.Is(err, errRoutingCredentialStale) {
				session.markCandidateStale(candidate.Credential.ID)
				continue
			}
			return nil, err
		}
		if lease == nil {
			continue
		}
		now := time.Now().UTC()
		claimed, err := session.selector.accounts.ClaimQuotaProbe(ctx, candidate.Credential.ID, now, now.Add(quotaProbeLease))
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
	return nil, nil
}

func (session *selectionSession) acquireNormal(ctx context.Context, excluded map[uint64]bool) (*accountLease, error) {
	if len(session.normalCandidates) == 0 {
		return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts}
	}
	_, _, _, capacityWait := session.selector.routingConfig()
	if !session.stickyTried && session.stickyKey != "" && session.selector.sticky != nil {
		session.stickyTried = true
		stickyID, ok, err := session.selector.sticky.Get(ctx, session.stickyKey, time.Now().UTC())
		if err != nil {
			return nil, fmt.Errorf("读取会话粘滞状态: %w", err)
		}
		if ok && !session.candidateExcluded(excluded, stickyID) {
			for _, index := range session.normalCandidates {
				candidate := session.values[index]
				if candidate.Credential.ID != stickyID {
					continue
				}
				lease, err := session.selector.acquirePinnedCapacity(ctx, candidate.Credential)
				if err != nil {
					if errors.Is(err, errRoutingCredentialStale) {
						session.markCandidateStale(stickyID)
						_ = session.selector.sticky.DeleteByAccount(ctx, stickyID)
						break
					}
					if !isSelectionUnavailable(err, SelectionSaturated) {
						return nil, err
					}
					// The sticky account already consumed this request's bounded capacity wait.
					// Try an available fallback immediately without waiting for the whole pool again.
					capacityWait = 0
					break
				}
				return session.completeNormalLease(ctx, lease, candidate, excluded)
			}
		}
	}
	indexes := session.unexcludedNormalIndexes(excluded)
	activeRequest := session.selector.nextSegmentedActiveRequest(session.provider, session.upstreamModel, session.quotaMode, len(indexes))
	if activeRequest != nil {
		lease, err := session.selector.acquireSegmentedCandidates(ctx, session.values, indexes, session.quotaMode, session.selector.resolveTierOrder(session.provider, session.upstreamModel), *activeRequest)
		if err != nil || lease == nil || session.stickyKey == "" {
			return lease, err
		}
		if lease.routingCandidate == nil {
			lease.Release()
			return nil, errors.New("分段选号缺少候选上下文")
		}
		return session.completeNormalLease(ctx, lease, *lease.routingCandidate, excluded)
	}

	deadline := time.Now().Add(capacityWait)
	for {
		if session.normalPlan == nil {
			plan, err := session.selector.planCandidateIndexes(ctx, session.values, session.normalCandidates, time.Now().UTC(), session.selector.resolveTierOrder(session.provider, session.upstreamModel))
			if err != nil {
				return nil, err
			}
			session.normalPlan = plan
		}
		for candidate, ok := session.normalPlan.Next(); ok; candidate, ok = session.normalPlan.Next() {
			if session.candidateExcluded(excluded, candidate.Credential.ID) {
				continue
			}
			lease, err := session.selector.claimAccountSlot(ctx, candidate.Credential)
			if err != nil {
				if errors.Is(err, errRoutingCredentialStale) {
					session.markCandidateStale(candidate.Credential.ID)
					continue
				}
				return nil, err
			}
			if lease != nil {
				return session.completeNormalLease(ctx, lease, candidate, excluded)
			}
		}
		if !session.hasUnexcludedNormal(excluded) {
			return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts}
		}
		if capacityWait <= 0 {
			return nil, &SelectionUnavailableError{Reason: SelectionSaturated, RetryAfter: time.Second}
		}
		retry, err := session.selector.awaitLeaseRetry(ctx, deadline)
		if err != nil {
			return nil, err
		}
		if !retry {
			return nil, &SelectionUnavailableError{Reason: SelectionSaturated, RetryAfter: time.Second}
		}
		// 仅当池内账号都已饱和时重读剩余账号的动态并发状态；正常账号切换不会重建计划。
		session.normalPlan = nil
	}
}

func (session *selectionSession) completeNormalLease(ctx context.Context, lease *accountLease, candidate account.RoutingCandidate, excluded map[uint64]bool) (*accountLease, error) {
	if session.stickyKey != "" && session.selector.sticky != nil {
		stickyTTL, _, _, _ := session.selector.routingConfig()
		now := time.Now().UTC()
		boundID, err := session.selector.sticky.Bind(ctx, session.stickyKey, candidate.Credential.ID, now, now.Add(stickyTTL))
		if err != nil {
			lease.Release()
			return nil, fmt.Errorf("写入会话粘滞状态: %w", err)
		}
		if boundID != candidate.Credential.ID {
			if boundCandidate, eligible := routingCandidateByID(session.values, session.normalCandidates, boundID); eligible && !session.candidateExcluded(excluded, boundID) {
				boundLease, acquireErr := session.selector.claimAccountSlot(ctx, boundCandidate.Credential)
				if acquireErr != nil {
					if errors.Is(acquireErr, errRoutingCredentialStale) {
						session.markCandidateStale(boundID)
						_ = session.selector.sticky.DeleteByAccount(ctx, boundID)
						if err := session.selector.sticky.Set(ctx, session.stickyKey, candidate.Credential.ID, now.Add(stickyTTL)); err != nil {
							lease.Release()
							return nil, fmt.Errorf("重建会话粘滞状态: %w", err)
						}
					} else {
						lease.Release()
						return nil, acquireErr
					}
				} else if boundLease != nil {
					lease.Release()
					lease = boundLease
					candidate = boundCandidate
				}
			} else if err := session.selector.sticky.Set(ctx, session.stickyKey, candidate.Credential.ID, now.Add(stickyTTL)); err != nil {
				lease.Release()
				return nil, fmt.Errorf("重建会话粘滞状态: %w", err)
			}
		}
	}
	lease.Billing = candidate.Billing
	lease.QuotaMode = effectiveQuotaMode(candidate, session.quotaMode)
	return lease, nil
}

func (session *selectionSession) hasUnexcludedNormal(excluded map[uint64]bool) bool {
	for _, index := range session.normalCandidates {
		if !session.candidateExcluded(excluded, session.values[index].Credential.ID) {
			return true
		}
	}
	return false
}

func (session *selectionSession) unexcludedNormalIndexes(excluded map[uint64]bool) []int {
	if len(excluded) == 0 && len(session.staleCandidates) == 0 {
		return session.normalCandidates
	}
	indexes := make([]int, 0, len(session.normalCandidates))
	for _, index := range session.normalCandidates {
		if !session.candidateExcluded(excluded, session.values[index].Credential.ID) {
			indexes = append(indexes, index)
		}
	}
	return indexes
}

func (session *selectionSession) candidateExcluded(excluded map[uint64]bool, accountID uint64) bool {
	return excluded[accountID] || session.staleCandidates[accountID]
}

func (session *selectionSession) markCandidateStale(accountID uint64) {
	session.staleCandidates[accountID] = true
}
