package console

import (
	"fmt"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// When Cloudflare blocks an egress IP every account behind it fails the same
// way, and per-account retries only prolong the block. The breaker pauses all
// grok.com dashboard traffic through that egress node with exponential
// cool-down and is closed by the first success.
const (
	egressBreakerBase = 2 * time.Minute
	egressBreakerMax  = 30 * time.Minute
)

type egressBreaker struct {
	mu    sync.Mutex
	now   func() time.Time
	nodes map[uint64]*egressBreakerState
}

type egressBreakerState struct {
	until    time.Time
	failures int
}

func newEgressBreaker() *egressBreaker {
	return &egressBreaker{now: time.Now, nodes: make(map[uint64]*egressBreakerState)}
}

// blocked returns the remaining cool-down for the node, or 0 when open.
func (b *egressBreaker) blocked(nodeID uint64) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	state := b.nodes[nodeID]
	if state == nil {
		return 0
	}
	return max(0, state.until.Sub(b.now()))
}

// trip records a Cloudflare rejection. It reports whether the breaker was
// newly opened so the caller logs once per cool-down, not once per account.
func (b *egressBreaker) trip(nodeID uint64) (time.Duration, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	state := b.nodes[nodeID]
	if state == nil {
		state = &egressBreakerState{}
		b.nodes[nodeID] = state
	}
	if state.until.After(now) {
		return state.until.Sub(now), false
	}
	state.failures++
	delay := egressBreakerBase << min(state.failures-1, 4)
	if delay > egressBreakerMax {
		delay = egressBreakerMax
	}
	state.until = now.Add(delay)
	return delay, true
}

func (b *egressBreaker) reset(nodeID uint64) {
	b.mu.Lock()
	delete(b.nodes, nodeID)
	b.mu.Unlock()
}

type egressBlockedError struct {
	retryAfter time.Duration
}

func (e *egressBlockedError) Error() string {
	return fmt.Sprintf("当前出口被 Cloudflare 拦截，Console 额度同步暂停 %s 后重试", e.retryAfter.Round(time.Second))
}

func (e *egressBlockedError) Unwrap() error                     { return provider.ErrEgressBlocked }
func (e *egressBlockedError) RetryAfterDuration() time.Duration { return e.retryAfter }
