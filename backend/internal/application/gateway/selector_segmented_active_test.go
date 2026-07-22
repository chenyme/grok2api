package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
	"github.com/chenyme/grok2api/backend/internal/pkg/resultcache"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func BenchmarkSelectorSegmentedCandidatePlanning(b *testing.B) {
	for _, candidateCount := range []int{3000, 10000} {
		b.Run(fmt.Sprintf("%d/full", candidateCount), func(b *testing.B) {
			benchmarkSegmentedSelector(b, candidateCount, false, false)
		})
		b.Run(fmt.Sprintf("%d/active_segmented_64", candidateCount), func(b *testing.B) {
			benchmarkSegmentedSelector(b, candidateCount, true, false)
		})
		b.Run(fmt.Sprintf("%d/active_segmented_64_full_fallback", candidateCount), func(b *testing.B) {
			benchmarkSegmentedSelector(b, candidateCount, true, true)
		})
	}
}

func benchmarkSegmentedSelector(b *testing.B, candidateCount int, enabled, forceFullFallback bool) {
	b.Helper()
	limiter := newSegmentedSelectiveLimiter()
	if forceFullFallback {
		for accountID := uint64(1); accountID <= segmentedWindowsBeforeFullFallback*64; accountID++ {
			limiter.SetSaturated(accountID, true)
		}
	}
	selector := newSegmentedActiveTestSelector(candidateCount, limiter, nil)
	selector.UpdateSegmentedSelector(enabled, 3000, 64)
	selector.concurrencySnapshots = resultcache.New[[32]byte, map[string]int](maxConcurrencySnapshots, time.Nanosecond)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if forceFullFallback {
			shard := segmentedSelectorShard(account.ProviderBuild, "benchmark-model", "")
			selector.segmentedState.activeCursors[shard].Store(0)
		}
		lease, err := selector.Acquire(context.Background(), account.ProviderBuild, "benchmark-model", "", "", nil, false)
		if err != nil {
			b.Fatal(err)
		}
		lease.Release()
	}
}

func TestSegmentedActiveReadsOnlyFirstAvailableWindow(t *testing.T) {
	limiter := newSegmentedSelectiveLimiter()
	selector := newSegmentedActiveTestSelector(100, limiter, nil)
	selector.UpdateSegmentedSelector(true, 100, 8)

	lease, err := selector.Acquire(context.Background(), account.ProviderBuild, "model", "", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Credential.ID != 1 {
		t.Fatalf("selected account = %d, want 1", lease.Credential.ID)
	}
	if sizes := limiter.BatchSizes(); fmt.Sprint(sizes) != "[8]" {
		t.Fatalf("concurrency batch sizes = %v, want one window", sizes)
	}
	if observation := lease.selectorObservation; observation == nil || observation.stage != "first_window" {
		t.Fatalf("active observation = %#v", observation)
	}
}

func TestSegmentedActiveCursorIsIndependentPerRouteShard(t *testing.T) {
	selector := NewSelector(nil, nil, nil, nil, time.Hour, time.Second, time.Minute)
	selector.UpdateSegmentedSelector(true, 100, 8)
	firstModel := "model-a"
	firstShard := segmentedSelectorShard(account.ProviderBuild, firstModel, "")
	secondModel := "model-b"
	for segmentedSelectorShard(account.ProviderBuild, secondModel, "") == firstShard {
		secondModel += "-next"
	}
	first := selector.nextSegmentedActiveRequest(account.ProviderBuild, firstModel, "", 100)
	second := selector.nextSegmentedActiveRequest(account.ProviderBuild, firstModel, "", 100)
	independent := selector.nextSegmentedActiveRequest(account.ProviderBuild, secondModel, "", 100)
	if first == nil || first.cursor != 0 || second == nil || second.cursor != 8 || independent == nil || independent.cursor != 0 {
		t.Fatalf("active cursors = first:%#v second:%#v independent:%#v", first, second, independent)
	}
}

func TestSegmentedActiveRotatesWindowStartPerRoute(t *testing.T) {
	limiter := newSegmentedSelectiveLimiter()
	selector := newSegmentedActiveTestSelector(100, limiter, nil)
	selector.UpdateSegmentedSelector(true, 100, 8)
	wanted := []uint64{1, 9, 17}
	for index, expected := range wanted {
		lease, err := selector.Acquire(context.Background(), account.ProviderBuild, "model", "", "", nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if lease.Credential.ID != expected {
			lease.Release()
			t.Fatalf("selection %d = %d, want %d", index, lease.Credential.ID, expected)
		}
		lease.Release()
	}
}

func TestSegmentedActiveContinuesAfterSaturatedWindow(t *testing.T) {
	limiter := newSegmentedSelectiveLimiter()
	for id := uint64(1); id <= 8; id++ {
		limiter.SetSaturated(id, true)
	}
	selector := newSegmentedActiveTestSelector(100, limiter, nil)
	selector.UpdateSegmentedSelector(true, 100, 8)

	lease, err := selector.Acquire(context.Background(), account.ProviderBuild, "model", "", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Credential.ID != 9 {
		t.Fatalf("selected account = %d, want 9", lease.Credential.ID)
	}
	if sizes := limiter.BatchSizes(); fmt.Sprint(sizes) != "[8 8]" {
		t.Fatalf("concurrency batch sizes = %v, want two windows", sizes)
	}
	if lease.selectorObservation == nil || lease.selectorObservation.stage != "later_window" {
		t.Fatalf("selection stage = %#v", lease.selectorObservation)
	}
}

func TestSegmentedActiveExhaustsHigherPriorityCohortBeforeFallingBack(t *testing.T) {
	limiter := newSegmentedSelectiveLimiter()
	priorities := make(map[uint64]int)
	for id := uint64(1); id <= 8; id++ {
		priorities[id] = 10
		limiter.SetSaturated(id, true)
	}
	for id := uint64(9); id <= 100; id++ {
		priorities[id] = 1
	}
	selector := newSegmentedActiveTestSelector(100, limiter, priorities)
	selector.UpdateSegmentedSelector(true, 100, 8)

	lease, err := selector.Acquire(context.Background(), account.ProviderBuild, "model", "", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Credential.ID != 9 {
		t.Fatalf("selected account = %d, want first lower-priority available account 9", lease.Credential.ID)
	}
	if lease.selectorObservation == nil || lease.selectorObservation.stage != "later_cohort" {
		t.Fatalf("selection stage = %#v", lease.selectorObservation)
	}
}

func TestSegmentedActiveCohortOrderingMatchesFullPlannerHardOrder(t *testing.T) {
	cohorts := make([]segmentedSelectorCohort, 0, 64)
	for _, supportsModel := range []bool{false, true} {
		for _, capabilityKnown := range []bool{false, true} {
			for _, preferFreeBuild := range []bool{false, true} {
				for _, tier := range []int{0, 2} {
					for _, priority := range []int{1, 10} {
						for _, billingFresh := range []bool{false, true} {
							cohorts = append(cohorts, segmentedSelectorCohort{
								supportsModel: supportsModel, capabilityKnown: capabilityKnown,
								preferFreeBuild: preferFreeBuild, tier: tier, priority: priority,
								billingFresh: billingFresh,
							})
						}
					}
				}
			}
		}
	}
	for leftIndex, left := range cohorts {
		for rightIndex, right := range cohorts {
			if left == right {
				continue
			}
			values := []account.RoutingCandidate{
				{Credential: account.Credential{ID: 1, Priority: left.priority}, SupportsModel: left.supportsModel, ModelCapabilityKnown: left.capabilityKnown},
				{Credential: account.Credential{ID: 2, Priority: right.priority}, SupportsModel: right.supportsModel, ModelCapabilityKnown: right.capabilityKnown},
			}
			scores := []candidateScore{
				{index: 0, tier: left.tier, preferFreeBuild: left.preferFreeBuild, billingFresh: left.billingFresh},
				{index: 1, tier: right.tier, preferFreeBuild: right.preferFreeBuild, billingFresh: right.billingFresh},
			}
			if got, want := segmentedSelectorCohortBetter(left, right), candidateScoreBetter(values, scores[0], scores[1]); got != want {
				t.Fatalf("cohort order mismatch at %d/%d: got %t want %t", leftIndex, rightIndex, got, want)
			}
		}
	}
}

func TestSegmentedPlannerUsesOnePreferFreeBuildSnapshot(t *testing.T) {
	now := time.Now().UTC()
	values := []account.RoutingCandidate{
		{
			Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, Priority: 100},
			Billing:    &account.Billing{PlanName: "SuperGrok", SyncedAt: now},
		},
		{
			Credential: account.Credential{ID: 2, Provider: account.ProviderBuild, Priority: 1},
			Billing:    &account.Billing{PlanName: "Free", SyncedAt: now},
		},
	}
	selector := NewSelector(nil, memory.NewConcurrencyLimiter(), nil, nil, time.Hour, time.Second, time.Minute)
	selector.UpdatePreferFreeBuild(true)
	plan, err := selector.planCandidateIndexesWithHints(context.Background(), values, nil, now, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	selected, ok := plan.Next()
	if !ok || selected.Credential.ID != 1 {
		t.Fatalf("disabled snapshot selected account %d, want higher-priority account 1", selected.Credential.ID)
	}

	selector.UpdatePreferFreeBuild(false)
	plan, err = selector.planCandidateIndexesWithHints(context.Background(), values, nil, now, nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	selected, ok = plan.Next()
	if !ok || selected.Credential.ID != 2 {
		t.Fatalf("enabled snapshot selected account %d, want Free account 2", selected.Credential.ID)
	}
}

func TestSegmentedActiveScansEveryCandidateBeforeSaturated(t *testing.T) {
	limiter := newSegmentedSelectiveLimiter()
	for id := uint64(1); id <= 100; id++ {
		limiter.SetSaturated(id, true)
	}
	selector := newSegmentedActiveTestSelector(100, limiter, nil)
	selector.UpdateSegmentedSelector(true, 100, 8)

	_, err := selector.Acquire(context.Background(), account.ProviderBuild, "model", "", "", nil, false)
	var unavailable *SelectionUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Reason != SelectionSaturated {
		t.Fatalf("error = %v", err)
	}
	if sizes := limiter.BatchSizes(); fmt.Sprint(sizes) != "[8 8 8 8 68]" {
		t.Fatalf("concurrency batch sizes = %v, want bounded windows followed by one full fallback", sizes)
	}
}

func TestSegmentedActiveFallsBackToFullPlannerAfterBoundedWindows(t *testing.T) {
	limiter := newSegmentedSelectiveLimiter()
	priorities := make(map[uint64]int)
	for id := uint64(1); id <= 32; id++ {
		limiter.SetSaturated(id, true)
	}
	for id := uint64(1); id <= 40; id++ {
		priorities[id] = 10
	}
	for id := uint64(41); id <= 100; id++ {
		priorities[id] = 1
	}
	selector := newSegmentedActiveTestSelector(100, limiter, priorities)
	selector.UpdateSegmentedSelector(true, 100, 8)
	lease, err := selector.Acquire(context.Background(), account.ProviderBuild, "model", "", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Credential.ID != 33 {
		t.Fatalf("selected account = %d, want higher-priority full fallback account 33", lease.Credential.ID)
	}
	if lease.selectorObservation == nil || lease.selectorObservation.stage != "full_fallback" {
		t.Fatalf("selection stage = %#v", lease.selectorObservation)
	}
	if sizes := limiter.BatchSizes(); fmt.Sprint(sizes) != "[8 8 8 8 68]" {
		t.Fatalf("concurrency batch sizes = %v", sizes)
	}
}

func TestSegmentedActiveWaitsAndRescansAfterCapacityReturns(t *testing.T) {
	limiter := newSegmentedSelectiveLimiter()
	for id := uint64(1); id <= 100; id++ {
		limiter.SetSaturated(id, true)
	}
	selector := newSegmentedActiveTestSelectorWithWait(100, limiter, nil, 200*time.Millisecond)
	selector.UpdateSegmentedSelector(true, 100, 8)
	startedAt := time.Now()
	go func() {
		time.Sleep(10 * time.Millisecond)
		limiter.SetSaturated(1, false)
		selector.announceLeaseReturn()
	}()

	lease, err := selector.Acquire(context.Background(), account.ProviderBuild, "model", "", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Credential.ID != 1 {
		t.Fatalf("selected account = %d, want released account 1", lease.Credential.ID)
	}
	if time.Since(startedAt) < 5*time.Millisecond {
		t.Fatal("selector returned before capacity was released")
	}
	if sizes := limiter.BatchSizes(); fmt.Sprint(sizes) != "[8 8 8 8 68 100]" {
		t.Fatalf("capacity retry did not switch to one full plan: %v", sizes)
	}
}

func TestSegmentedActiveDoesNotRepeatWindowsAfterFullFallback(t *testing.T) {
	limiter := newSegmentedSelectiveLimiter()
	for id := uint64(1); id <= 100; id++ {
		limiter.SetSaturated(id, true)
	}
	selector := newSegmentedActiveTestSelectorWithWait(100, limiter, nil, 100*time.Millisecond)
	selector.UpdateSegmentedSelector(true, 100, 8)
	go func() {
		time.Sleep(5 * time.Millisecond)
		selector.announceLeaseReturn()
	}()

	_, err := selector.Acquire(context.Background(), account.ProviderBuild, "model", "", "", nil, false)
	var unavailable *SelectionUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Reason != SelectionSaturated {
		t.Fatalf("error = %v", err)
	}
	sizes := limiter.BatchSizes()
	if len(sizes) < 6 || fmt.Sprint(sizes[:5]) != "[8 8 8 8 68]" {
		t.Fatalf("initial segmented round = %v", sizes)
	}
	for _, size := range sizes[5:] {
		if size != 100 {
			t.Fatalf("capacity retry repeated segmented windows: %v", sizes)
		}
	}
}

func TestSegmentedActiveUsesFullPlannerAfterExhaustingSmallPool(t *testing.T) {
	limiter := newSegmentedSelectiveLimiter()
	for id := uint64(1); id <= 100; id++ {
		limiter.SetSaturated(id, true)
	}
	selector := newSegmentedActiveTestSelectorWithWait(100, limiter, nil, 100*time.Millisecond)
	selector.UpdateSegmentedSelector(true, 100, 64)
	go func() {
		time.Sleep(5 * time.Millisecond)
		selector.announceLeaseReturn()
	}()

	_, err := selector.Acquire(context.Background(), account.ProviderBuild, "model", "", "", nil, false)
	var unavailable *SelectionUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Reason != SelectionSaturated {
		t.Fatalf("error = %v", err)
	}
	sizes := limiter.BatchSizes()
	if len(sizes) < 3 || fmt.Sprint(sizes[:2]) != "[64 36]" {
		t.Fatalf("initial complete-pool windows = %v", sizes)
	}
	for _, size := range sizes[2:] {
		if size != 100 {
			t.Fatalf("capacity retry repeated small-pool windows: %v", sizes)
		}
	}
}

func TestSegmentedActiveSkipsStickyPinnedAndSmallPools(t *testing.T) {
	tests := []struct {
		name     string
		count    int
		affinity string
		pinned   bool
		enabled  bool
	}{
		{name: "disabled", count: 100},
		{name: "small pool", count: 99, enabled: true},
		{name: "sticky", count: 100, affinity: "session", enabled: true},
		{name: "pinned", count: 100, pinned: true, enabled: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			limiter := newSegmentedSelectiveLimiter()
			selector := newSegmentedActiveTestSelector(test.count, limiter, nil)
			selector.UpdateSegmentedSelector(test.enabled, 100, 8)
			var lease *accountLease
			var err error
			if test.pinned {
				lease, err = selector.AcquirePinned(context.Background(), account.ProviderBuild, 1, "model", "", true)
			} else {
				lease, err = selector.Acquire(context.Background(), account.ProviderBuild, "model", "", test.affinity, nil, false)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Release()
			if lease.selectorObservation != nil {
				t.Fatalf("non-active path received observation: %#v", lease.selectorObservation)
			}
			if activeSegmentedCursorCount(selector) != 0 {
				t.Fatal("non-active path advanced the segmented cursor")
			}
		})
	}
}

func TestSegmentedActiveLifecycleRecordsFinalOutcomeOnce(t *testing.T) {
	tests := []struct {
		name    string
		outcome string
		record  func(*selectorLeaseObservation)
	}{
		{name: "success", outcome: "success", record: func(value *selectorLeaseObservation) {
			value.upstreamStarted.Store(true)
			value.complete(true)
			value.completeRelease()
		}},
		{name: "explicit failure", outcome: "failed", record: func(value *selectorLeaseObservation) {
			value.upstreamStarted.Store(true)
			value.complete(false)
			value.completeRelease()
		}},
		{name: "abandoned after upstream start", outcome: "failed", record: func(value *selectorLeaseObservation) {
			value.upstreamStarted.Store(true)
			value.completeRelease()
		}},
		{name: "released before upstream start", outcome: "skipped", record: func(value *selectorLeaseObservation) {
			value.completeRelease()
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := perfmetrics.NewRegistry()
			previous := perfmetrics.Default
			perfmetrics.Default = registry
			defer func() { perfmetrics.Default = previous }()
			observation := &selectorLeaseObservation{provider: account.ProviderBuild, stage: "first_window"}
			test.record(observation)
			assertSegmentedMetric(t, registry.CollectAndReset(), "selector_segmented_active_upstream_total", "first_window", test.outcome, 1)
		})
	}
}

func newSegmentedActiveTestSelector(count int, limiter repository.ConcurrencyLimiter, priorities map[uint64]int) *Selector {
	return newSegmentedActiveTestSelectorWithWait(count, limiter, priorities, 0)
}

func newSegmentedActiveTestSelectorWithWait(count int, limiter repository.ConcurrencyLimiter, priorities map[uint64]int, capacityWait time.Duration) *Selector {
	bases := make([]account.RoutingAccountBase, count)
	for index := range bases {
		id := uint64(index + 1)
		priority := 10
		if value, ok := priorities[id]; ok {
			priority = value
		}
		bases[index] = account.RoutingAccountBase{Credential: account.Credential{
			ID: id, Provider: account.ProviderBuild, AuthStatus: account.AuthStatusActive,
			Enabled: true, Priority: priority, MaxConcurrent: account.DefaultMaxConcurrent,
		}}
	}
	repository := &layeredAccountRepository{bases: bases, overlays: map[string]account.RoutingOverlaySnapshot{"model": {}}}
	return NewSelector(repository, limiter, memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute, capacityWait)
}

func activeSegmentedCursorCount(selector *Selector) uint64 {
	var total uint64
	for index := range selector.segmentedState.activeCursors {
		total += selector.segmentedState.activeCursors[index].Load()
	}
	return total
}

func assertSegmentedMetric(t *testing.T, samples []perfmetrics.Sample, name, stage, outcome string, total int64) {
	t.Helper()
	for _, sample := range samples {
		if sample.Name == name && sample.Labels.Stage == stage && sample.Labels.Outcome == outcome {
			if sample.Total != total {
				t.Fatalf("metric %s/%s/%s total = %d, want %d", name, stage, outcome, sample.Total, total)
			}
			return
		}
	}
	t.Fatalf("metric %s/%s/%s not found in %#v", name, stage, outcome, samples)
}

type segmentedSelectiveLimiter struct {
	mu         sync.Mutex
	saturated  map[string]bool
	batchSizes []int
}

func newSegmentedSelectiveLimiter() *segmentedSelectiveLimiter {
	return &segmentedSelectiveLimiter{saturated: make(map[string]bool)}
}

func (l *segmentedSelectiveLimiter) SetSaturated(accountID uint64, value bool) {
	l.mu.Lock()
	l.saturated[repository.AccountConcurrencyKey(accountID)] = value
	l.mu.Unlock()
}

func (l *segmentedSelectiveLimiter) Acquire(_ context.Context, key string, _ int) (func(), bool, error) {
	l.mu.Lock()
	saturated := l.saturated[key]
	l.mu.Unlock()
	if saturated {
		return nil, false, nil
	}
	return func() {}, true, nil
}

func (l *segmentedSelectiveLimiter) Current(_ context.Context, key string) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.saturated[key] {
		return account.DefaultMaxConcurrent, nil
	}
	return 0, nil
}

func (l *segmentedSelectiveLimiter) CurrentMany(_ context.Context, keys []string) (map[string]int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.batchSizes = append(l.batchSizes, len(keys))
	result := make(map[string]int, len(keys))
	for _, key := range keys {
		if l.saturated[key] {
			result[key] = account.DefaultMaxConcurrent
		}
	}
	return result, nil
}

func (l *segmentedSelectiveLimiter) BatchSizes() []int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]int(nil), l.batchSizes...)
}
