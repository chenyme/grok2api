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

func (s *Selector) acquireSegmentedCandidates(ctx context.Context, values []account.RoutingCandidate, indexes []int, quotaMode string, tierOrder []account.WebTier, request segmentedSelectorActiveRequest) (*accountLease, error) {
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
			claim, err := s.claimSegmentedPlan(ctx, plan, request.provider, quotaMode, "full_fallback")
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
			concurrencyHints := make([]int, len(values))
			cohorts := segmentedCandidateCohorts(values, indexes, now, tierOrder, preferFreeBuild)
			roundWindows := 0
			fallbackToFull := false
			for cohortIndex, bucket := range cohorts {
				for windowOffset := 0; windowOffset < len(bucket.indexes); windowOffset += request.windowSize {
					windowIndexes := segmentedCohortWindow(bucket.indexes, request.cursor, windowOffset, request.windowSize)
					windowsScanned++
					roundWindows++
					candidatesScanned += len(windowIndexes)
					plan, err := s.planCandidateIndexesWithHints(ctx, values, windowIndexes, now, tierOrder, concurrencyHints, preferFreeBuild)
					if err != nil {
						observeSegmentedActive(request.provider, "error", "planning", startedAt, windowsScanned, candidatesScanned)
						return nil, err
					}
					stage := segmentedActiveSelectionStage(cohortIndex, windowOffset)
					claim, err := s.claimSegmentedPlan(ctx, plan, request.provider, quotaMode, stage)
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
				claim, err := s.claimSegmentedPlan(ctx, plan, request.provider, quotaMode, "full_fallback")
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

func (s *Selector) claimSegmentedPlan(ctx context.Context, plan *candidatePlan, provider account.Provider, quotaMode, stage string) (segmentedClaimResult, error) {
	result := segmentedClaimResult{}
	for candidate, ok := plan.Next(); ok; candidate, ok = plan.Next() {
		lease, err := s.claimAccountSlot(ctx, candidate.Credential)
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
		lease.selectorObservation = &selectorLeaseObservation{provider: provider, stage: stage}
		result.lease = lease
		return result, nil
	}
	return result, nil
}

func segmentedCandidateCohorts(values []account.RoutingCandidate, indexes []int, now time.Time, tierOrder []account.WebTier, preferFreeBuild bool) []segmentedSelectorCohortBucket {
	buckets := make(map[segmentedSelectorCohort][]int)
	appendCandidate := func(index int) {
		candidate := values[index]
		cohort := segmentedSelectorCohort{
			supportsModel: candidate.SupportsModel, capabilityKnown: candidate.ModelCapabilityKnown,
			preferFreeBuild: preferFreeBuild && candidate.IsKnownFreeBuild(),
			tier:            tierOrderRank(tierOrder, candidate.Credential.WebTier), priority: candidate.Credential.Priority,
		}
		if candidate.Billing != nil {
			cohort.billingFresh = now.Sub(candidate.Billing.SyncedAt) <= 30*time.Minute
		}
		buckets[cohort] = append(buckets[cohort], index)
	}
	if indexes == nil {
		for index := range values {
			appendCandidate(index)
		}
	} else {
		for _, index := range indexes {
			appendCandidate(index)
		}
	}
	result := make([]segmentedSelectorCohortBucket, 0, len(buckets))
	for cohort, cohortIndexes := range buckets {
		result = append(result, segmentedSelectorCohortBucket{cohort: cohort, indexes: cohortIndexes})
	}
	sort.Slice(result, func(left, right int) bool {
		return segmentedSelectorCohortBetter(result[left].cohort, result[right].cohort)
	})
	return result
}

func segmentedCohortWindow(indexes []int, cursor uint64, offset, windowSize int) []int {
	if len(indexes) == 0 || offset >= len(indexes) || windowSize <= 0 {
		return nil
	}
	count := min(windowSize, len(indexes)-offset)
	start := int(cursor % uint64(len(indexes)))
	result := make([]int, count)
	for position := range count {
		result[position] = indexes[(start+offset+position)%len(indexes)]
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
