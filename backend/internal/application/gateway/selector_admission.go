package gateway

import (
	"context"
	"sort"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
)

type candidateAdmissionState uint8

const (
	candidateAdmissionSkipped candidateAdmissionState = iota
	candidateAdmissionUnsupported
	candidateAdmissionModelCooling
	candidateAdmissionCooling
	candidateAdmissionQuotaBlocked
	candidateAdmissionQuotaProbe
	candidateAdmissionEligible
)

type candidateAdmission struct {
	state      candidateAdmissionState
	retryAfter time.Time
}

type candidateAdmissionOptions struct {
	excluded               map[uint64]bool
	allowQuotaProbe        bool
	forcedEgressNodeID     uint64
	ignoreEgressLeaseBlock bool
}

// EligibleAccountIDsForKey returns the exact pre-concurrency admission set used
// by AcquireForKey. It intentionally excludes transient slot saturation: the
// returned identities are those the selector may choose as soon as a slot is
// available, after every persisted route, scope, capability, health, and quota
// gate has been applied.
func (s *Selector) EligibleAccountIDsForKey(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel, quotaMode string, scope clientkeydomain.AccountScope) ([]uint64, error) {
	accountIDs, _, _, err := s.ImageCapacityEligibilityForKey(ctx, provider, modelRouteID, upstreamModel, quotaMode, scope)
	return accountIDs, err
}

// ImageCapacityEligibilityForKey projects the same persisted admission set as
// AcquireForKey plus the distinct bound egress nodes needed to prove safe
// pre-submit failover. allEgressBound is false when any otherwise eligible
// identity would receive an adapter-selected, unverifiable egress.
func (s *Selector) ImageCapacityEligibilityForKey(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel, quotaMode string, scope clientkeydomain.AccountScope) (accountIDs, egressNodeIDs []uint64, allEgressBound bool, err error) {
	accountScope, valid := clientkeydomain.NormalizeAccountScope(scope)
	if !valid || !accountScope.AllowsProvider(provider) {
		return nil, nil, false, &SelectionUnavailableError{Reason: SelectionNoAccounts, Scope: accountScope}
	}
	now := time.Now().UTC()
	values, err := s.loadCandidates(ctx, provider, modelRouteID, upstreamModel, quotaMode, now)
	if err != nil {
		return nil, nil, false, err
	}
	quotaConsumed := s.quotaConsumptionSnapshot(provider)
	healthOverrides := s.routingHealthSnapshot(provider, now)
	ids := make([]uint64, 0, len(values))
	egressIDs := make([]uint64, 0, len(values))
	allBound := true
	for _, candidate := range values {
		credential := applyHealthSnapshot(candidate.Credential, healthOverrides)
		admission := s.evaluateCandidateAdmission(provider, upstreamModel, quotaMode, candidate, credential, accountScope, quotaConsumed, now, candidateAdmissionOptions{})
		if admission.state == candidateAdmissionEligible {
			ids = append(ids, credential.ID)
			if credential.EgressNodeID == 0 {
				allBound = false
			} else {
				egressIDs = append(egressIDs, credential.EgressNodeID)
			}
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	sort.Slice(egressIDs, func(i, j int) bool { return egressIDs[i] < egressIDs[j] })
	return ids, uniqueUint64s(egressIDs), allBound, nil
}

func (s *Selector) ImageCapacityFairnessPolicy() string { return account.ImageProFairnessPolicy }

func uniqueUint64s(values []uint64) []uint64 {
	if len(values) < 2 {
		return values
	}
	write := 1
	for _, value := range values[1:] {
		if value == values[write-1] {
			continue
		}
		values[write] = value
		write++
	}
	return values[:write]
}

func (s *Selector) evaluateCandidateAdmission(
	provider account.Provider,
	upstreamModel string,
	quotaMode string,
	candidate account.RoutingCandidate,
	credential account.Credential,
	scope clientkeydomain.AccountScope,
	quotaConsumed map[accountQuotaConsumptionKey]int,
	now time.Time,
	options candidateAdmissionOptions,
) candidateAdmission {
	if options.forcedEgressNodeID != 0 && credential.EgressNodeID != options.forcedEgressNodeID {
		return candidateAdmission{state: candidateAdmissionSkipped}
	}
	if !accountScopeAllowsCandidate(provider, scope, candidate) || options.excluded[credential.ID] || !credential.Enabled || credential.AuthStatus != account.AuthStatusActive {
		return candidateAdmission{state: candidateAdmissionSkipped}
	}
	if !s.candidateSupportsModel(provider, upstreamModel, quotaMode, candidate) {
		return candidateAdmission{state: candidateAdmissionUnsupported}
	}
	if candidate.ModelQuotaBlock != nil && now.Before(candidate.ModelQuotaBlock.CooldownUntil) {
		return candidateAdmission{state: candidateAdmissionModelCooling, retryAfter: candidate.ModelQuotaBlock.CooldownUntil}
	}
	if !options.ignoreEgressLeaseBlock && candidateEgressLeaseCooling(candidate, credential, now) {
		return candidateAdmission{state: candidateAdmissionCooling, retryAfter: candidate.EgressLeaseBlock.CooldownUntil}
	}
	if credential.CooldownUntil != nil && now.Before(*credential.CooldownUntil) {
		return candidateAdmission{state: candidateAdmissionCooling, retryAfter: *credential.CooldownUntil}
	}
	if recovery := candidate.QuotaRecovery; recovery != nil && recovery.Status != account.QuotaRecoveryStatusActive {
		if options.allowQuotaProbe && recovery.NextProbeAt != nil && !now.Before(*recovery.NextProbeAt) {
			return candidateAdmission{state: candidateAdmissionQuotaProbe}
		}
		var retryAfter time.Time
		if recovery.NextProbeAt != nil {
			retryAfter = *recovery.NextProbeAt
		}
		return candidateAdmission{state: candidateAdmissionQuotaBlocked, retryAfter: retryAfter}
	}
	if candidate.Billing != nil && candidate.Billing.IsExhausted(credential.MinimumRemaining) {
		return candidateAdmission{state: candidateAdmissionQuotaBlocked}
	}
	// Owned-fork Web image resale is fail-closed: a schedulable identity must
	// have an authoritative, synchronized image_pro window. A missing window or
	// the older weekly fallback is not evidence of spendable image capacity.
	if provider == account.ProviderWeb && quotaMode == account.QuotaModeWebImagePro {
		window := candidate.QuotaWindow
		if window == nil || window.Mode != account.QuotaModeWebImagePro || window.SyncedAt == nil || window.Source != account.QuotaSourceUpstream {
			return candidateAdmission{state: candidateAdmissionQuotaBlocked}
		}
	}
	if quotaWindowExhausted(candidate, quotaConsumed) {
		var retryAfter time.Time
		if candidate.QuotaWindow != nil && candidate.QuotaWindow.ResetAt != nil {
			retryAfter = *candidate.QuotaWindow.ResetAt
		}
		return candidateAdmission{state: candidateAdmissionQuotaBlocked, retryAfter: retryAfter}
	}
	return candidateAdmission{state: candidateAdmissionEligible}
}
