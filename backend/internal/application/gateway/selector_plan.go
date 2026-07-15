package gateway

import (
	"container/heap"
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type candidateScore struct {
	index        int
	tier         int
	billingFresh bool
	inFlight     int
	remaining    float64
	lastSelected time.Time
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
	keys := make([]string, len(values))
	for index, candidate := range values {
		keys[index] = accountConcurrencyKey(candidate.Credential.ID)
	}
	var concurrencySnapshot map[string]int
	batchReader, batched := s.concurrency.(repository.ConcurrencySnapshotReader)
	if batched {
		var err error
		concurrencySnapshot, err = batchReader.CurrentMany(ctx, keys)
		if err != nil {
			return nil, fmt.Errorf("批量读取账号并发租约: %w", err)
		}
	}
	inFlight := make([]int, len(values))
	for index := range values {
		if batched {
			inFlight[index] = concurrencySnapshot[keys[index]]
			continue
		}
		current, err := s.concurrency.Current(ctx, keys[index])
		if err != nil {
			return nil, fmt.Errorf("读取账号并发租约: %w", err)
		}
		inFlight[index] = current
	}

	s.mu.Lock()
	scores := make([]candidateScore, len(values))
	for index, candidate := range values {
		score := candidateScore{
			index: index, tier: tierOrderRank(tierOrder, candidate.Credential.WebTier),
			inFlight: inFlight[index], lastSelected: s.lastSelectedAt[candidate.Credential.ID],
		}
		if candidate.Billing != nil {
			score.remaining = candidate.Billing.Remaining()
			score.billingFresh = now.Sub(candidate.Billing.SyncedAt) <= 30*time.Minute
		}
		scores[index] = score
	}
	s.mu.Unlock()

	plan := &candidatePlan{values: values, scores: scores}
	heap.Init(plan)
	return plan, nil
}

func accountConcurrencyKey(accountID uint64) string {
	return "account:" + strconv.FormatUint(accountID, 10)
}
