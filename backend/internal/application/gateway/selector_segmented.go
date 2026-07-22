package gateway

import (
	"hash/fnv"
	"sync/atomic"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
)

const segmentedSelectorShards = 1024

type segmentedSelectorConfig struct {
	enabled       bool
	minCandidates int
	windowSize    int
}

type segmentedSelectorState struct {
	activeCursors [segmentedSelectorShards]atomic.Uint64
}

type segmentedSelectorCohort struct {
	supportsModel   bool
	capabilityKnown bool
	preferFreeBuild bool
	tier            int
	priority        int
	billingFresh    bool
}

type selectorLeaseObservation struct {
	provider        account.Provider
	stage           string
	upstreamStarted atomic.Bool
	completed       atomic.Bool
}

func normalizeSegmentedSelectorConfig(value segmentedSelectorConfig) segmentedSelectorConfig {
	if value.minCandidates < 100 {
		value.minCandidates = 3000
	}
	if value.windowSize < 8 || value.windowSize > value.minCandidates {
		value.windowSize = 64
	}
	return value
}

func segmentedSelectorShard(provider account.Provider, upstreamModel, quotaMode string) uint64 {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(provider))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(upstreamModel))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(quotaMode))
	return hash.Sum64() % segmentedSelectorShards
}

func (o *selectorLeaseObservation) completeRelease() {
	if o == nil {
		return
	}
	if o.upstreamStarted.Load() {
		o.record("failed")
		return
	}
	o.record("skipped")
}

func (o *selectorLeaseObservation) complete(success bool) {
	if o == nil {
		return
	}
	outcome := "failed"
	if success {
		outcome = "success"
	}
	o.record(outcome)
}

func (o *selectorLeaseObservation) record(outcome string) {
	if o == nil || !o.completed.CompareAndSwap(false, true) {
		return
	}
	perfmetrics.Default.Inc("selector_segmented_active_upstream_total", perfmetrics.Labels{
		Subsystem: "selector", Operation: "segmented_active_upstream", Provider: string(o.provider),
		Stage: o.stage, Outcome: outcome,
	})
}

func segmentedSelectorCohortBetter(left, right segmentedSelectorCohort) bool {
	if left.supportsModel != right.supportsModel {
		return left.supportsModel
	}
	if left.capabilityKnown != right.capabilityKnown {
		return left.capabilityKnown
	}
	if left.preferFreeBuild != right.preferFreeBuild {
		return left.preferFreeBuild
	}
	if left.tier != right.tier {
		return left.tier < right.tier
	}
	if left.priority != right.priority {
		return left.priority > right.priority
	}
	if left.billingFresh != right.billingFresh {
		return left.billingFresh
	}
	return false
}
