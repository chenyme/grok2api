package batch

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSharedPoolBoundsConcurrentExecutors(t *testing.T) {
	pool := NewPool(3)
	var active atomic.Int64
	var peak atomic.Int64
	work := func(context.Context, int) error {
		current := active.Add(1)
		for {
			value := peak.Load()
			if current <= value || peak.CompareAndSwap(value, current) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		active.Add(-1)
		return nil
	}
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, summary, err := Run(context.Background(), make([]int, 10), Options{Workers: 10, Pool: pool}, work)
			if err != nil || summary.Succeeded != 10 {
				t.Errorf("summary = %#v, err = %v", summary, err)
			}
		}()
	}
	wait.Wait()
	if peak.Load() != 3 || pool.Snapshot().Peak != 3 {
		t.Fatalf("peak = %d, pool = %#v", peak.Load(), pool.Snapshot())
	}
}

type leaseLimiterStub struct {
	mu      sync.Mutex
	current map[string]int
}

func (s *leaseLimiterStub) Acquire(_ context.Context, key string, limit int) (func(), bool, error) {
	s.mu.Lock()
	if s.current == nil {
		s.current = make(map[string]int)
	}
	if s.current[key] >= limit {
		s.mu.Unlock()
		return nil, false, nil
	}
	s.current[key]++
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		s.current[key]--
		s.mu.Unlock()
	}, true, nil
}

func TestDistributedLeaseBoundsSeparateProcessPools(t *testing.T) {
	limiter := &leaseLimiterStub{}
	pools := []*Pool{NewSharedPool(2, limiter, "bulk"), NewSharedPool(2, limiter, "bulk")}
	var active atomic.Int64
	var peak atomic.Int64
	var wait sync.WaitGroup
	for _, pool := range pools {
		pool := pool
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, _, err := Run(context.Background(), make([]int, 4), Options{Workers: 2, Pool: pool}, func(context.Context, int) error {
				current := active.Add(1)
				for {
					value := peak.Load()
					if current <= value || peak.CompareAndSwap(value, current) {
						break
					}
				}
				time.Sleep(5 * time.Millisecond)
				active.Add(-1)
				return nil
			})
			if err != nil {
				t.Errorf("run: %v", err)
			}
		}()
	}
	wait.Wait()
	if peak.Load() != 2 {
		t.Fatalf("distributed peak = %d", peak.Load())
	}
}

func TestSharedChildPoolBoundsCategoryAcrossProcesses(t *testing.T) {
	limiter := &leaseLimiterStub{}
	parents := []*Pool{NewSharedPool(3, limiter, "global"), NewSharedPool(3, limiter, "global")}
	children := []*Pool{
		NewSharedChildPool(1, limiter, "sync", parents[0]),
		NewSharedChildPool(1, limiter, "sync", parents[1]),
	}
	var active atomic.Int64
	var peak atomic.Int64
	var wait sync.WaitGroup
	for _, pool := range children {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, _, err := Run(context.Background(), make([]int, 2), Options{Workers: 2, Pool: pool}, func(context.Context, int) error {
				current := active.Add(1)
				for {
					value := peak.Load()
					if current <= value || peak.CompareAndSwap(value, current) {
						break
					}
				}
				time.Sleep(5 * time.Millisecond)
				active.Add(-1)
				return nil
			})
			if err != nil {
				t.Errorf("run: %v", err)
			}
		}()
	}
	wait.Wait()
	if peak.Load() != 1 {
		t.Fatalf("category peak = %d", peak.Load())
	}
}

func TestMapIsolatesFailureAndPanic(t *testing.T) {
	results, summary, err := Map(context.Background(), []int{1, 2, 3}, Options{Workers: 3}, func(_ context.Context, value int) (int, error) {
		switch value {
		case 2:
			return 0, errors.New("failed")
		case 3:
			panic("broken")
		default:
			return value * 2, nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Value != 2 || results[0].Err != nil || results[1].Err == nil {
		t.Fatalf("results = %#v", results)
	}
	var panicErr *PanicError
	if !errors.As(results[2].Err, &panicErr) || len(panicErr.Stack) == 0 {
		t.Fatalf("panic result = %#v", results[2])
	}
	if summary.Succeeded != 1 || summary.Failed != 2 || summary.Panicked != 1 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestMapObservedReleasesPoolBeforeStartingDownstreamWork(t *testing.T) {
	pool := NewPool(1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var downstreamErr error
	_, summary, err := MapObserved(ctx, []int{1}, Options{Workers: 1, Pool: pool}, func(context.Context, int) (int, error) {
		return 42, nil
	}, func(_ int, result Result[int]) {
		if result.Err != nil {
			downstreamErr = result.Err
			return
		}
		downstreamErr = pool.Do(ctx, func(context.Context) error { return nil })
	})
	if err != nil || downstreamErr != nil || summary.Succeeded != 1 {
		t.Fatalf("summary = %#v, err = %v, downstream = %v", summary, err, downstreamErr)
	}
}

func TestMapStopsSubmittingAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	_, summary, err := Run(ctx, make([]int, 100), Options{Workers: 2, QueueSize: 1}, func(context.Context, int) error {
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || !summary.Canceled || summary.Submitted >= summary.Total {
		t.Fatalf("summary = %#v, err = %v", summary, err)
	}
}

func TestPoolHotResizeAndChildLimit(t *testing.T) {
	global := NewPool(3)
	child := NewChildPool(1, global)
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	done := make(chan error, 3)
	for range 3 {
		go func() {
			done <- child.Do(context.Background(), func(context.Context) error {
				started <- struct{}{}
				<-release
				return nil
			})
		}()
	}
	<-started
	select {
	case <-started:
		t.Fatal("child exceeded its initial limit")
	case <-time.After(20 * time.Millisecond):
	}
	child.UpdateLimit(3)
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("waiting work did not observe the increased limit")
		}
	}
	child.UpdateLimit(1)
	close(release)
	for range 3 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if child.Snapshot().Limit != 1 || child.Snapshot().Peak != 3 || global.Snapshot().Peak != 3 {
		t.Fatalf("child = %#v, global = %#v", child.Snapshot(), global.Snapshot())
	}
}

func TestPoolJitterHonorsCancellationBeforeWork(t *testing.T) {
	pool := NewPool(2)
	pool.UpdateJitter(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := pool.Do(ctx, func(context.Context) error {
		called = true
		return nil
	})
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("err = %v, called = %t", err, called)
	}
}

func TestPoolJitterIsSkippedForSerialExecution(t *testing.T) {
	pool := NewPool(1)
	pool.UpdateJitter(time.Hour)
	startedAt := time.Now()
	if err := pool.Do(context.Background(), func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(startedAt); elapsed > 100*time.Millisecond {
		t.Fatalf("serial execution was delayed by %s", elapsed)
	}
}
