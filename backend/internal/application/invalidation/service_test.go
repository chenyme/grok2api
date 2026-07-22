package invalidation

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type testBus struct {
	mu         sync.Mutex
	published  []repository.InvalidationEvent
	events     []repository.InvalidationEvent
	publishErr error
}

func (b *testBus) PublishInvalidation(_ context.Context, event repository.InvalidationEvent) error {
	b.mu.Lock()
	b.published = append(b.published, event)
	b.mu.Unlock()
	return b.publishErr
}

func (b *testBus) ListenInvalidations(ctx context.Context, handler func(context.Context, repository.InvalidationEvent) error) error {
	for _, event := range b.events {
		if err := handler(ctx, event); err != nil {
			return err
		}
	}
	return nil
}

func TestNotifyAlwaysAppliesLocalInvalidation(t *testing.T) {
	var applied []repository.InvalidationEvent
	service := NewService(nil, "local", func(event repository.InvalidationEvent) {
		applied = append(applied, event)
	}, slog.Default())
	service.Notify(context.Background(), repository.InvalidationEvent{
		Kind: repository.InvalidationAccountStateChanged, Provider: account.ProviderBuild,
	})
	if len(applied) != 1 || applied[0].SourceInstance != "local" || applied[0].PublishedAt.IsZero() {
		t.Fatalf("local invalidation = %#v", applied)
	}
}

func TestNotifyAppliesLocallyWhenRemoteQueueIsFull(t *testing.T) {
	var applied int
	service := NewService(&testBus{}, "local", func(repository.InvalidationEvent) { applied++ }, slog.Default())
	for range cap(service.queue) {
		service.queue <- repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged}
	}
	service.Notify(context.Background(), repository.InvalidationEvent{Kind: repository.InvalidationAccountBillingChanged})
	if applied != 1 || service.dropped.Load() != 1 {
		t.Fatalf("applied=%d dropped=%d", applied, service.dropped.Load())
	}
}

func TestRunSubscriberIgnoresInvalidAndSameSourceEvents(t *testing.T) {
	bus := &testBus{events: []repository.InvalidationEvent{
		{Kind: "unknown", SourceInstance: "remote"},
		{Kind: repository.InvalidationAccountStateChanged, Provider: account.ProviderBuild, SourceInstance: "local"},
		{Kind: repository.InvalidationAccountQuotaChanged, Provider: account.ProviderWeb, SourceInstance: "remote", Revision: 2},
	}}
	var applied []repository.InvalidationEvent
	service := NewService(bus, "local", func(event repository.InvalidationEvent) {
		applied = append(applied, event)
	}, slog.Default())
	if err := service.RunSubscriber(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || applied[0].Kind != repository.InvalidationAccountQuotaChanged {
		t.Fatalf("applied events = %#v", applied)
	}
}

func TestRunPublisherDoesNotStopAfterPublishFailure(t *testing.T) {
	bus := &testBus{publishErr: errors.New("redis unavailable")}
	service := NewService(bus, "local", nil, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.RunPublisher(ctx) }()
	service.Notify(context.Background(), repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged})
	deadline := time.Now().Add(time.Second)
	for service.failures.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if service.failures.Load() != 1 {
		t.Fatalf("publish failures = %d", service.failures.Load())
	}
}
