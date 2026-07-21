package gateway

import (
	"container/heap"
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type candidateScore struct {
	index           int
	tier            int
	preferFreeBuild bool
	billingFresh    bool
	inFlight        int
	remaining       float64
	lastSelected    time.Time
}

// candidatePlan 使用线性建堆保留完整路由优先级，并允许 claim 失败后按顺序取下一账号。
type candidatePlan struct {
	values []account.RoutingCandidate
	scores []candidateScore
}

func (p *candidatePlan) Len() int { return len(p.scores) }

func (p *candidatePlan) Less(left, right int) bool {
	return candidateScoreBetter(p.values, p.scores[left], p.scores[right])
}

func (p *candidatePlan) Swap(left, right int) {
	p.scores[left], p.scores[right] = p.scores[right], p.scores[left]
}

func (p *candidatePlan) Push(value any) {
	p.scores = append(p.scores, value.(candidateScore))
}

func (p *candidatePlan) Pop() any {
	last := len(p.scores) - 1
	value := p.scores[last]
	p.scores = p.scores[:last]
	return value
}

func (p *candidatePlan) Next() (account.RoutingCandidate, bool) {
	if p == nil || p.Len() == 0 {
		return account.RoutingCandidate{}, false
	}
	score := heap.Pop(p).(candidateScore)
	return p.values[score.index], true
}

func candidateScoreBetter(values []account.RoutingCandidate, leftScore, rightScore candidateScore) bool {
	leftCandidate, rightCandidate := values[leftScore.index], values[rightScore.index]
	left, right := leftCandidate.Credential, rightCandidate.Credential
	if leftCandidate.SupportsModel != rightCandidate.SupportsModel {
		return leftCandidate.SupportsModel
	}
	if leftCandidate.ModelCapabilityKnown != rightCandidate.ModelCapabilityKnown {
		return leftCandidate.ModelCapabilityKnown
	}
	if leftScore.preferFreeBuild != rightScore.preferFreeBuild {
		return leftScore.preferFreeBuild
	}
	if leftScore.tier != rightScore.tier {
		return leftScore.tier < rightScore.tier
	}
	if left.Priority != right.Priority {
		return left.Priority > right.Priority
	}
	if leftScore.billingFresh != rightScore.billingFresh {
		return leftScore.billingFresh
	}
	if leftScore.inFlight != rightScore.inFlight {
		return leftScore.inFlight < rightScore.inFlight
	}
	if leftScore.remaining != rightScore.remaining {
		return leftScore.remaining > rightScore.remaining
	}
	if !leftScore.lastSelected.Equal(rightScore.lastSelected) {
		return leftScore.lastSelected.Before(rightScore.lastSelected)
	}
	return left.ID < right.ID
}

// planCandidates 批量读取动态并发状态，并以 O(n) 建堆生成保持原比较规则的候选计划。
func (s *Selector) planCandidates(ctx context.Context, values []account.RoutingCandidate, now time.Time, tierOrder []account.WebTier) (*candidatePlan, error) {
	return s.planCandidateIndexes(ctx, values, nil, now, tierOrder)
}

// planCandidateIndexes 在不可变候选快照上按下标规划，避免过滤阶段复制完整账号结构。
// indexes 为 nil 时表示使用 values 的全部元素。
func (s *Selector) planCandidateIndexes(ctx context.Context, values []account.RoutingCandidate, indexes []int, now time.Time, tierOrder []account.WebTier) (*candidatePlan, error) {
	length := len(indexes)
	if indexes == nil {
		length = len(values)
	}
	keys := make([]string, length)
	for position := range length {
		index := position
		if indexes != nil {
			index = indexes[position]
		}
		keys[position] = accountConcurrencyKey(values[index].Credential.ID)
	}
	concurrencySnapshot, err := s.loadConcurrencySnapshot(ctx, keys)
	if err != nil {
		return nil, err
	}
	inFlight := make([]int, length)
	for position := range length {
		inFlight[position] = concurrencySnapshot[keys[position]]
	}

	s.mu.Lock()
	scores := make([]candidateScore, length)
	for position := range length {
		index := position
		if indexes != nil {
			index = indexes[position]
		}
		candidate := values[index]
		score := candidateScore{
			index: index, tier: tierOrderRank(tierOrder, candidate.Credential.WebTier),
			preferFreeBuild: s.preferFreeBuild && candidate.IsKnownFreeBuild(),
			inFlight:        inFlight[position], lastSelected: s.lastSelectedAt[candidate.Credential.ID],
		}
		if candidate.Billing != nil {
			score.remaining = candidate.Billing.Remaining()
			score.billingFresh = now.Sub(candidate.Billing.SyncedAt) <= 30*time.Minute
		}
		scores[position] = score
	}
	s.mu.Unlock()

	plan := &candidatePlan{values: values, scores: scores}
	heap.Init(plan)
	return plan, nil
}

// loadConcurrencySnapshot 在极短窗口内合并相同候选池的并发快照读取。
// 快照只参与排序，最终容量仍由原子 Acquire 校验，因此陈旧快照不会突破账号并发上限。
func (s *Selector) loadConcurrencySnapshot(ctx context.Context, keys []string) (map[string]int, error) {
	cacheKey := concurrencySnapshotKey(keys)
	load := func() (map[string]int, error) {
		values := make(map[string]int, len(keys))
		if batchReader, ok := s.concurrency.(repository.ConcurrencySnapshotReader); ok {
			var err error
			values, err = batchReader.CurrentMany(ctx, keys)
			if err != nil {
				return nil, fmt.Errorf("批量读取账号并发租约: %w", err)
			}
		} else {
			for _, key := range keys {
				current, err := s.concurrency.Current(ctx, key)
				if err != nil {
					return nil, fmt.Errorf("读取账号并发租约: %w", err)
				}
				values[key] = current
			}
		}
		return values, nil
	}
	// 仅测试中的手工 Selector 可能没有初始化缓存，保持最小兼容回退。
	if s.concurrencySnapshots == nil {
		return load()
	}
	return s.concurrencySnapshots.Load(ctx, cacheKey, time.Now(), load)
}

func concurrencySnapshotKey(keys []string) [32]byte {
	hash := sha256.New()
	separator := []byte{0}
	for _, key := range keys {
		_, _ = hash.Write([]byte(key))
		_, _ = hash.Write(separator)
	}
	var result [32]byte
	copy(result[:], hash.Sum(nil))
	return result
}

func accountConcurrencyKey(accountID uint64) string {
	return repository.AccountConcurrencyKey(accountID)
}
