package batch

import (
	"context"
	"fmt"
	"math/rand/v2"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

// PanicError 表示任务发生 panic；堆栈只用于服务端诊断，不应直接返回给客户端。
type PanicError struct {
	Value any
	Stack []byte
}

func (e *PanicError) Error() string {
	return fmt.Sprintf("批量任务 panic: %v", e.Value)
}

// Pool 在多个批量操作之间共享并发容量，避免并发请求叠加突破上游保护阈值。
type Pool struct {
	mu      sync.Mutex
	limit   int
	slots   int
	changed chan struct{}
	parent  *Pool
	shared  LeaseLimiter
	key     string
	active  atomic.Int64
	queued  atomic.Int64
	peak    atomic.Int64
	jitter  atomic.Int64
}

type LeaseLimiter interface {
	Acquire(ctx context.Context, key string, limit int) (release func(), acquired bool, err error)
}

type PoolSnapshot struct {
	Limit  int
	Active int
	Queued int
	Peak   int
}

func NewPool(limit int) *Pool {
	if limit < 1 {
		limit = 1
	}
	return &Pool{limit: limit, changed: make(chan struct{})}
}

func NewSharedPool(limit int, limiter LeaseLimiter, key string) *Pool {
	pool := NewPool(limit)
	pool.shared = limiter
	pool.key = key
	return pool
}

// NewChildPool 创建分类并发池；任务先取得分类容量，再进入父级全局池。
func NewChildPool(limit int, parent *Pool) *Pool {
	pool := NewPool(limit)
	pool.parent = parent
	return pool
}

// NewSharedChildPool 创建同时受分类集群租约和父级总容量约束的并发池。
func NewSharedChildPool(limit int, limiter LeaseLimiter, key string, parent *Pool) *Pool {
	pool := NewSharedPool(limit, limiter, key)
	pool.parent = parent
	return pool
}

// UpdateLimit 热更新并发上限；降低上限不会中断正在执行的任务。
func (p *Pool) UpdateLimit(limit int) {
	if p == nil {
		return
	}
	if limit < 1 {
		limit = 1
	}
	p.mu.Lock()
	p.limit = limit
	p.signalLocked()
	p.mu.Unlock()
	p.peak.Store(p.active.Load())
}

// UpdateJitter 热更新任务进入并发池前的随机延迟上限；零表示关闭。
func (p *Pool) UpdateJitter(maximum time.Duration) {
	if p == nil {
		return
	}
	if maximum < 0 {
		maximum = 0
	}
	p.jitter.Store(int64(maximum))
}

// Do 等待共享执行容量并隔离任务 panic，调用方仍通过 context 控制排队和执行生命周期。
func (p *Pool) Do(ctx context.Context, work func(context.Context) error) (err error) {
	if p == nil {
		return invoke(ctx, work)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.waitJitter(ctx); err != nil {
		return err
	}
	p.queued.Add(1)
	started := false
	defer func() {
		if !started {
			p.queued.Add(-1)
		}
	}()
	if err := p.acquireSlot(ctx); err != nil {
		return err
	}
	defer p.releaseSlot()
	var releaseShared func()
	if p.shared != nil {
		for {
			release, acquired, acquireErr := p.shared.Acquire(ctx, p.key, p.Limit())
			if acquireErr != nil {
				return acquireErr
			}
			if acquired {
				releaseShared = release
				break
			}
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	defer func() {
		if releaseShared != nil {
			releaseShared()
		}
	}()
	run := func(workCtx context.Context) error {
		started = true
		p.queued.Add(-1)
		p.begin()
		defer p.end()
		return invoke(workCtx, work)
	}
	if p.parent != nil {
		return p.parent.Do(ctx, run)
	}
	return run(ctx)
}

func (p *Pool) waitJitter(ctx context.Context) error {
	maximum := p.jitter.Load()
	if maximum <= 0 || p.Limit() <= 1 {
		return nil
	}
	delay := time.Duration(rand.Int64N(maximum))
	if delay == 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (p *Pool) acquireSlot(ctx context.Context) error {
	for {
		p.mu.Lock()
		if p.slots < p.limit {
			p.slots++
			p.mu.Unlock()
			return nil
		}
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (p *Pool) releaseSlot() {
	p.mu.Lock()
	p.slots--
	p.signalLocked()
	p.mu.Unlock()
}

func (p *Pool) signalLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

func (p *Pool) begin() {
	current := p.active.Add(1)
	for {
		peak := p.peak.Load()
		if current <= peak || p.peak.CompareAndSwap(peak, current) {
			break
		}
	}
}

func (p *Pool) end() {
	p.active.Add(-1)
}

// Limit 返回当前并发上限。
func (p *Pool) Limit() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.limit
}

func (p *Pool) Snapshot() PoolSnapshot {
	if p == nil {
		return PoolSnapshot{}
	}
	return PoolSnapshot{Limit: p.Limit(), Active: int(p.active.Load()), Queued: int(p.queued.Load()), Peak: int(p.peak.Load())}
}

// Do 隔离单个任务 panic，适用于长驻 Worker 和后台任务监督器。
func Do(ctx context.Context, work func(context.Context) error) error {
	return invoke(ctx, work)
}

type Options struct {
	Workers   int
	QueueSize int
	Pool      *Pool
}

type Result[T any] struct {
	Value     T
	Err       error
	Completed bool
}

type Summary struct {
	Total     int
	Submitted int
	Completed int
	Succeeded int
	Failed    int
	Panicked  int
	Canceled  bool
	Duration  time.Duration
}

type indexedItem[T any] struct {
	index int
	value T
}

// Map 以稳定输入顺序返回结果；单项失败和 panic 不会中断其他已提交任务。
func Map[T, R any](ctx context.Context, items []T, options Options, work func(context.Context, T) (R, error)) ([]Result[R], Summary, error) {
	return MapObserved(ctx, items, options, work, nil)
}

// MapObserved 在任务释放共享容量后通知结果观察者，适合连接下游有界流水线。
func MapObserved[T, R any](ctx context.Context, items []T, options Options, work func(context.Context, T) (R, error), observe func(index int, result Result[R])) ([]Result[R], Summary, error) {
	startedAt := time.Now()
	results := make([]Result[R], len(items))
	summary := Summary{Total: len(items)}
	if len(items) == 0 {
		summary.Duration = time.Since(startedAt)
		return results, summary, nil
	}
	workers := options.Workers
	if workers < 1 {
		workers = 1
	}
	workers = min(workers, len(items))
	queueSize := options.QueueSize
	if queueSize < 1 {
		queueSize = workers * 2
	}
	queueSize = min(queueSize, len(items))
	jobs := make(chan indexedItem[T], queueSize)
	var wait sync.WaitGroup
	wait.Add(workers)
	for range workers {
		go func() {
			defer wait.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job, ok := <-jobs:
					if !ok {
						return
					}
					var value R
					err := options.Pool.Do(ctx, func(workCtx context.Context) error {
						var workErr error
						value, workErr = work(workCtx, job.value)
						return workErr
					})
					execution := Result[R]{Value: value, Err: err, Completed: true}
					if observe != nil {
						observeErr := invoke(ctx, func(context.Context) error {
							observe(job.index, execution)
							return nil
						})
						if execution.Err == nil && observeErr != nil {
							execution.Err = observeErr
						}
					}
					results[job.index] = execution
				}
			}
		}()
	}

sendLoop:
	for index, item := range items {
		if ctx.Err() != nil {
			break
		}
		select {
		case jobs <- indexedItem[T]{index: index, value: item}:
			summary.Submitted++
		case <-ctx.Done():
			break sendLoop
		}
	}
	close(jobs)
	wait.Wait()
	for _, result := range results {
		if !result.Completed {
			continue
		}
		summary.Completed++
		if result.Err == nil {
			summary.Succeeded++
			continue
		}
		summary.Failed++
		if _, ok := result.Err.(*PanicError); ok {
			summary.Panicked++
		}
	}
	summary.Canceled = ctx.Err() != nil
	summary.Duration = time.Since(startedAt)
	return results, summary, ctx.Err()
}

// Run 执行只关心成功或失败的批量任务。
func Run[T any](ctx context.Context, items []T, options Options, work func(context.Context, T) error) ([]Result[struct{}], Summary, error) {
	return Map(ctx, items, options, func(workCtx context.Context, item T) (struct{}, error) {
		return struct{}{}, work(workCtx, item)
	})
}

func invoke(ctx context.Context, work func(context.Context) error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = &PanicError{Value: recovered, Stack: debug.Stack()}
		}
	}()
	return work(ctx)
}
