package memory

import (
	"container/heap"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

const maxQuotaRecoveryEvents = 100000

type quotaItem struct {
	value account.QuotaRecoveryEvent
	index int
}

type quotaHeap []*quotaItem

func (h quotaHeap) Len() int { return len(h) }
func (h quotaHeap) Less(i, j int) bool {
	if h[i].value.DueAt.Equal(h[j].value.DueAt) {
		return quotaEventKey(h[i].value) < quotaEventKey(h[j].value)
	}
	return h[i].value.DueAt.Before(h[j].value.DueAt)
}
func (h quotaHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i]; h[i].index, h[j].index = i, j }
func (h *quotaHeap) Push(value any) {
	item := value.(*quotaItem)
	item.index = len(*h)
	*h = append(*h, item)
}
func (h *quotaHeap) Pop() any {
	old := *h
	item := old[len(old)-1]
	item.index = -1
	*h = old[:len(old)-1]
	return item
}

type QuotaRecoveryQueue struct {
	mu    sync.Mutex
	heap  quotaHeap
	items map[string]*quotaItem
}

func NewQuotaRecoveryQueue() *QuotaRecoveryQueue {
	return &QuotaRecoveryQueue{items: make(map[string]*quotaItem)}
}

func (q *QuotaRecoveryQueue) ScheduleQuotaRecovery(_ context.Context, value account.QuotaRecoveryEvent) error {
	if value.AccountID == 0 || value.Mode == "" || value.DueAt.IsZero() {
		return fmt.Errorf("额度恢复事件无效")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	key := quotaEventKey(value)
	if existing := q.items[key]; existing != nil {
		value.ClaimToken = ""
		existing.value = value
		heap.Fix(&q.heap, existing.index)
		return nil
	}
	if len(q.items) >= maxQuotaRecoveryEvents {
		return fmt.Errorf("额度恢复队列已满")
	}
	item := &quotaItem{value: value}
	heap.Push(&q.heap, item)
	q.items[key] = item
	return nil
}

func (q *QuotaRecoveryQueue) EnsureQuotaRecovery(_ context.Context, value account.QuotaRecoveryEvent) error {
	if value.AccountID == 0 || value.Mode == "" || value.DueAt.IsZero() {
		return fmt.Errorf("额度恢复事件无效")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	key := quotaEventKey(value)
	if q.items[key] != nil {
		return nil
	}
	if len(q.items) >= maxQuotaRecoveryEvents {
		return fmt.Errorf("额度恢复队列已满")
	}
	item := &quotaItem{value: value}
	heap.Push(&q.heap, item)
	q.items[key] = item
	return nil
}

func (q *QuotaRecoveryQueue) ClaimDueQuotaRecoveries(_ context.Context, now time.Time, limit int, lease time.Duration) ([]account.QuotaRecoveryEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	values := make([]account.QuotaRecoveryEvent, 0, limit)
	for len(values) < limit && q.heap.Len() > 0 && !q.heap[0].value.DueAt.After(now) {
		item := heap.Pop(&q.heap).(*quotaItem)
		token, err := quotaClaimToken()
		if err != nil {
			heap.Push(&q.heap, item)
			return nil, err
		}
		item.value.DueAt = now.Add(lease)
		item.value.ClaimToken = token
		heap.Push(&q.heap, item)
		values = append(values, item.value)
	}
	return values, nil
}

func (q *QuotaRecoveryQueue) AckQuotaRecovery(_ context.Context, value account.QuotaRecoveryEvent) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	key := quotaEventKey(value)
	if item := q.items[key]; item != nil {
		if value.ClaimToken == "" || item.value.ClaimToken != value.ClaimToken {
			return repository.ErrConflict
		}
		heap.Remove(&q.heap, item.index)
		delete(q.items, key)
	}
	return nil
}

func (q *QuotaRecoveryQueue) RescheduleQuotaRecovery(_ context.Context, value account.QuotaRecoveryEvent) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	item := q.items[quotaEventKey(value)]
	if item == nil || value.ClaimToken == "" || item.value.ClaimToken != value.ClaimToken {
		return repository.ErrConflict
	}
	value.ClaimToken = ""
	item.value = value
	heap.Fix(&q.heap, item.index)
	return nil
}

func quotaEventKey(value account.QuotaRecoveryEvent) string {
	return fmt.Sprintf("%d:%s", value.AccountID, value.Mode)
}

func quotaClaimToken() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}
