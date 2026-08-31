package gateway

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
)

type segmentedSelectorActiveRequest struct {
	provider   account.Provider
	windowSize int
	cursor     uint64
}

type segmentedSelectorCohortBucket struct {
	cohort  segmentedSelectorCohort
	indexes []int
}

type segmentedCohortSelection struct {
	count   int
	start   int
	take    int
	seen    int
	indexes []int
}

type segmentedClaimResult struct {
	lease          *accountLease
	staleClaims    int
	capacityMisses int
}

const segmentedWindowsBeforeFullFallback = 4

func (s *Selector) nextSegmentedActiveRequest(provider account.Provider, upstreamModel, quotaMode string, candidateCount int) *segmentedSelectorActiveRequest {
	s.configMu.RLock()
	config := s.segmentedConfig
	s.configMu.RUnlock()
	if !config.enabled || candidateCount < config.minCandidates {
		return nil
	}
	shard := segmentedSelectorShard(provider, upstreamModel, quotaMode)
	cursor := s.segmentedState.activeCursors[shard].Add(uint64(config.windowSize)) - uint64(config.windowSize)
	return &segmentedSelectorActiveRequest{provider: provider, windowSize: config.windowSize, cursor: cursor}
}

func (s *Selector) acquireSegmentedCandidates(ctx context.Context, values []account.RoutingCandidate, indexes []int, quotaMode string, tierOrder []account.WebTier, request segmentedSelectorActiveRequest, materialFailures *credentialMaterialFailureTracker) (*accountLease, error) {
	startedAt := time.Now()
	_, _, _, capacityWait := s.routingConfig()
	waitDeadline := time.Now().Add(capacityWait)
	windowsScanned := 0
	candidatesScanned := 0
	fullPlannerOnly := false
	preferFreeBuild := s.preferFreeBuildEnabled()
	for {
		now := time.Now().UTC()
		if fullPlannerOnly {
			length := len(indexes)
			if indexes == nil {
				length = len(values)
			}
			candidatesScanned += length
			plan, err := s.planCandidateIndexesWithHints(ctx, values, indexes, now, tierOrder, nil, preferFreeBuild)
			if err != nil {
				observeSegmentedActive(request.provider, "error", "full_fallback", startedAt, windowsScanned, candidatesScanned)
				return nil, err
			}
			claim, err := s.claimSegmentedPlan(ctx, plan, request.provider, quotaMode, "full_fallback", materialFailures)
			if err != nil {
				observeSegmentedActive(request.provider, "error", "full_fallback", startedAt, windowsScanned, candidatesScanned)
				return nil, err
			}
			if claim.lease != nil {
				observeSegmentedActive(request.provider, "selected", "full_fallback", startedAt, windowsScanned, candidatesScanned)
				return claim.lease, nil
			}
			if claim.staleClaims > 0 && claim.capacityMisses == 0 {
				observeSegmentedActive(request.provider, "unavailable", "full_fallback", startedAt, windowsScanned, candidatesScanned)
				return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts}
			}
		} else {
			concurrencyHints := make(map[int]int, min(len(indexes), request.windowSize*segmentedWindowsBeforeFullFallback))
			cohorts := segmentedCandidateCohorts(values, indexes, now, tierOrder, preferFreeBuild, request.cursor, request.windowSize, segmentedWindowsBeforeFullFallback)
			roundWindows := 0
			fallbackToFull := false
			for cohortIndex, bucket := range cohorts {
				for windowOffset := 0; windowOffset < len(bucket.indexes); windowOffset += request.windowSize {
					windowIndexes := bucket.indexes[windowOffset:min(windowOffset+request.windowSize, len(bucket.indexes))]
					windowsScanned++
					roundWindows++
					candidatesScanned += len(windowIndexes)
					plan, err := s.planCandidateIndexesWithHints(ctx, values, windowIndexes, now, tierOrder, concurrencyHints, preferFreeBuild)
					if err != nil {
						observeSegmentedActive(request.provider, "error", "planning", startedAt, windowsScanned, candidatesScanned)
						return nil, err
					}
					stage := segmentedActiveSelectionStage(cohortIndex, windowOffset)
					claim, err := s.claimSegmentedPlan(ctx, plan, request.provider, quotaMode, stage, materialFailures)
					if err != nil {
						observeSegmentedActive(request.provider, "error", "claim", startedAt, windowsScanned, candidatesScanned)
						return nil, err
					}
					if claim.lease != nil {
						observeSegmentedActive(request.provider, "selected", stage, startedAt, windowsScanned, candidatesScanned)
						return claim.lease, nil
					}
					if roundWindows >= segmentedWindowsBeforeFullFallback {
						fallbackToFull = true
						break
					}
				}
				if fallbackToFull {
					break
				}
			}
			if fallbackToFull {
				length := len(indexes)
				if indexes == nil {
					length = len(values)
				}
				candidatesScanned += length
				plan, err := s.planCandidateIndexesWithHints(ctx, values, indexes, now, tierOrder, concurrencyHints, preferFreeBuild)
				if err != nil {
					observeSegmentedActive(request.provider, "error", "full_fallback", startedAt, windowsScanned, candidatesScanned)
					return nil, err
				}
				claim, err := s.claimSegmentedPlan(ctx, plan, request.provider, quotaMode, "full_fallback", materialFailures)
				if err != nil {
					observeSegmentedActive(request.provider, "error", "full_fallback", startedAt, windowsScanned, candidatesScanned)
					return nil, err
				}
				if claim.lease != nil {
					observeSegmentedActive(request.provider, "selected", "full_fallback", startedAt, windowsScanned, candidatesScanned)
					return claim.lease, nil
				}
				if claim.staleClaims > 0 && claim.capacityMisses == 0 {
					observeSegmentedActive(request.provider, "unavailable", "full_fallback", startedAt, windowsScanned, candidatesScanned)
					return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts}
				}
			}
			fullPlannerOnly = true
		}
		if capacityWait <= 0 {
			observeSegmentedActive(request.provider, "saturated", "exhausted", startedAt, windowsScanned, candidatesScanned)
			return nil, &SelectionUnavailableError{Reason: SelectionSaturated, RetryAfter: time.Second}
		}
		retry, err := s.awaitLeaseRetry(ctx, waitDeadline)
		if err != nil {
			observeSegmentedActive(request.provider, "error", "wait", startedAt, windowsScanned, candidatesScanned)
			return nil, err
		}
		if !retry {
			observeSegmentedActive(request.provider, "saturated", "timeout", startedAt, windowsScanned, candidatesScanned)
			return nil, &SelectionUnavailableError{Reason: SelectionSaturated, RetryAfter: time.Second}
		}
	}
}

func (s *Selector) claimSegmentedPlan(ctx context.Context, plan *candidatePlan, provider account.Provider, quotaMode, stage string, materialFailures *credentialMaterialFailureTracker) (segmentedClaimResult, error) {
	result := segmentedClaimResult{}
	for candidate, ok := plan.Next(); ok; candidate, ok = plan.Next() {
		lease, err := s.claimAccountSlotTracked(ctx, candidate.Credential, materialFailures)
		if err != nil {
			if errors.Is(err, errRoutingCredentialStale) {
				result.staleClaims++
				continue
			}
			return segmentedClaimResult{}, err
		}
		if lease == nil {
			result.capacityMisses++
			continue
		}
		lease.Billing = candidate.Billing
		lease.QuotaMode = effectiveQuotaMode(candidate, quotaMode)
		selected := candidate
		lease.routingCandidate = &selected
		lease.selectorObservation = &selectorLeaseObservation{provider: provider, stage: stage}
		result.lease = lease
		return result, nil
	}
	return result, nil
}

func segmentedCandidateCohorts(values []account.RoutingCandidate, indexes []int, now time.Time, tierOrder []account.WebTier, preferFreeBuild bool, cursor uint64, windowSize, maxWindows int) []segmentedSelectorCohortBucket {
	if windowSize <= 0 || maxWindows <= 0 {
		return nil
	}
	cohortFor := func(index int) segmentedSelectorCohort {
		candidate := values[index]
		supportsModel, capabilityKnown := candidate.SupportsModel, candidate.ModelCapabilityKnown
		if candidate.Credential.Provider == account.ProviderWeb && len(tierOrder) > 0 && webTierInOrder(tierOrder, candidate.Credential.WebTier) {
			supportsModel, capabilityKnown = true, true
		}
		cohort := segmentedSelectorCohort{
			supportsModel: supportsModel, capabilityKnown: capabilityKnown,
			preferFreeBuild: preferFreeBuild && candidate.IsKnownFreeBuild(),
			tier:            tierOrderRank(tierOrder, candidate.Credential.WebTier), priority: candidate.Credential.Priority,
		}
		if candidate.QuotaWindow != nil && candidate.QuotaWindow.Source == account.QuotaSourceUpstream {
			cohort.quotaKnown = true
			cohort.quotaAvailable = candidate.QuotaWindow.Remaining > 0
		}
		if candidate.Billing != nil {
			cohort.billingFresh = now.Sub(candidate.Billing.SyncedAt) <= 30*time.Minute
		}
		return cohort
	}
	counts := make(map[segmentedSelectorCohort]int)
	countCandidate := func(index int) { counts[cohortFor(index)]++ }
	if indexes == nil {
		for index := range values {
			countCandidate(index)
		}
	} else {
		for _, index := range indexes {
			countCandidate(index)
		}
	}
	ordered := make([]segmentedSelectorCohort, 0, len(counts))
	for cohort := range counts {
		ordered = append(ordered, cohort)
	}
	sort.Slice(ordered, func(left, right int) bool {
		return segmentedSelectorCohortBetter(ordered[left], ordered[right])
	})

	remainingWindows := maxWindows
	selections := make(map[segmentedSelectorCohort]*segmentedCohortSelection)
	result := make([]segmentedSelectorCohortBucket, 0, min(len(ordered), maxWindows))
	for _, cohort := range ordered {
		if remainingWindows == 0 {
			break
		}
		count := counts[cohort]
		take := min(count, remainingWindows*windowSize)
		windows := (take + windowSize - 1) / windowSize
		remainingWindows -= windows
		selection := &segmentedCohortSelection{
			count: count, start: int(cursor % uint64(count)), take: take, indexes: make([]int, take),
		}
		selections[cohort] = selection
		result = append(result, segmentedSelectorCohortBucket{cohort: cohort, indexes: selection.indexes})
	}
	fillCandidate := func(index int) {
		selection := selections[cohortFor(index)]
		if selection == nil {
			return
		}
		position := selection.seen
		selection.seen++
		relative := position - selection.start
		if relative < 0 {
			relative += selection.count
		}
		if relative < selection.take {
			selection.indexes[relative] = index
		}
	}
	if indexes == nil {
		for index := range values {
			fillCandidate(index)
		}
	} else {
		for _, index := range indexes {
			fillCandidate(index)
		}
	}
	return result
}

func segmentedActiveSelectionStage(cohortIndex, windowOffset int) string {
	if cohortIndex > 0 {
		return "later_cohort"
	}
	if windowOffset > 0 {
		return "later_window"
	}
	return "first_window"
}

func observeSegmentedActive(provider account.Provider, outcome, stage string, startedAt time.Time, windows, candidates int) {
	labels := perfmetrics.Labels{
		Subsystem: "selector", Operation: "segmented_active", Provider: string(provider),
		Stage: stage, Outcome: outcome,
	}
	perfmetrics.Default.Inc("selector_segmented_active_total", labels)
	perfmetrics.Default.ObserveDuration("selector_segmented_active_duration_us", labels, time.Since(startedAt))
	perfmetrics.Default.Add("selector_segmented_active_windows", labels, int64(windows))
	perfmetrics.Default.Add("selector_segmented_active_candidates", labels, int64(candidates))
}
