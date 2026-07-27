package redis

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
	redisclient "github.com/redis/go-redis/v9"
)

func TestRetryConcurrencyReleases(t *testing.T) {
	registry := perfmetrics.NewRegistry()
	previous := perfmetrics.Default
	perfmetrics.Default = registry
	t.Cleanup(func() { perfmetrics.Default = previous })
	tests := []struct {
		name          string
		pipelineError error
		remaining     int
		outcomes      map[string]int64
	}{
		{name: "successful retry removes live and expired entries", outcomes: map[string]int64{"expired": 1, "recovered": 1}},
		{name: "failed retry retains only live entries", pipelineError: errors.New("Redis unavailable"), remaining: 1, outcomes: map[string]int64{"expired": 1, "retry_failed": 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := redisclient.NewClient(&redisclient.Options{Addr: "unused:6379"})
			client.AddHook(releasePipelineHook{err: test.pipelineError})
			defer client.Close()
			store := &Store{client: client}
			now := time.Now().UTC()
			pending := map[string]concurrencyReleaseRetry{
				"live":    {redisKey: "concurrency:1", token: "live", expiresAt: now.Add(time.Minute)},
				"expired": {redisKey: "concurrency:2", token: "expired", expiresAt: now.Add(-time.Minute)},
			}
			store.retryConcurrencyReleases(pending)
			if len(pending) != test.remaining {
				t.Fatalf("pending releases = %#v", pending)
			}
			if test.remaining == 1 {
				if _, ok := pending["live"]; !ok {
					t.Fatalf("live release was discarded: %#v", pending)
				}
			}
			assertConcurrencyReleaseMetrics(t, registry.CollectAndReset(), test.outcomes)
		})
	}
}

func TestEnqueueConcurrencyReleaseRetryMetrics(t *testing.T) {
	registry := perfmetrics.NewRegistry()
	previous := perfmetrics.Default
	perfmetrics.Default = registry
	t.Cleanup(func() { perfmetrics.Default = previous })
	store := &Store{
		concurrencyReleaseQueue: make(chan concurrencyReleaseRetry, 1),
		concurrencyReleaseStop:  make(chan struct{}),
	}
	value := concurrencyReleaseRetry{redisKey: "concurrency:1", token: "token", expiresAt: time.Now().Add(time.Minute)}
	store.enqueueConcurrencyReleaseRetry(value)
	store.enqueueConcurrencyReleaseRetry(value)
	close(store.concurrencyReleaseStop)
	store.enqueueConcurrencyReleaseRetry(value)
	assertConcurrencyReleaseMetrics(t, registry.CollectAndReset(), map[string]int64{
		"queued": 1, "dropped": 1, "shutdown": 1,
	})
}

func assertConcurrencyReleaseMetrics(t *testing.T, samples []perfmetrics.Sample, expected map[string]int64) {
	t.Helper()
	observed := make(map[string]int64)
	for _, sample := range samples {
		if sample.Name == "runtime_concurrency_release_total" {
			observed[sample.Labels.Outcome] += sample.Total
		}
	}
	for outcome, total := range expected {
		if observed[outcome] != total {
			t.Fatalf("metric %s = %d, want %d; samples=%#v", outcome, observed[outcome], total, samples)
		}
	}
}

type releasePipelineHook struct{ err error }

func (h releasePipelineHook) DialHook(next redisclient.DialHook) redisclient.DialHook {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		return next(ctx, network, address)
	}
}

func (h releasePipelineHook) ProcessHook(next redisclient.ProcessHook) redisclient.ProcessHook {
	return next
}

func (h releasePipelineHook) ProcessPipelineHook(_ redisclient.ProcessPipelineHook) redisclient.ProcessPipelineHook {
	return func(_ context.Context, commands []redisclient.Cmder) error {
		for _, command := range commands {
			command.SetErr(h.err)
			if h.err == nil {
				if value, ok := command.(*redisclient.IntCmd); ok {
					value.SetVal(1)
				}
			}
		}
		return h.err
	}
}
