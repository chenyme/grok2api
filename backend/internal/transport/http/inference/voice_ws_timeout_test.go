package inference

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestWatchRequestTimeoutClosesQuietSessionOnDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	var mu sync.Mutex
	errorCode := ""
	var once sync.Once
	closed := make(chan struct{})
	closeAll := func() {
		once.Do(func() { close(closed) })
	}

	go watchRequestTimeout(ctx, &mu, &errorCode, closeAll)

	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("watchRequestTimeout did not call closeAll after the context deadline elapsed")
	}

	mu.Lock()
	got := errorCode
	mu.Unlock()
	if got != "request_timeout" {
		t.Fatalf("errorCode = %q, want %q", got, "request_timeout")
	}
}

func TestWatchRequestTimeoutDoesNotOverwriteAnExistingOutcome(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	var mu sync.Mutex
	errorCode := "client_stream_interrupted"
	closeAllCalled := make(chan struct{})
	closeAll := func() { close(closeAllCalled) }

	go watchRequestTimeout(ctx, &mu, &errorCode, closeAll)

	select {
	case <-closeAllCalled:
	case <-time.After(time.Second):
		t.Fatal("watchRequestTimeout did not call closeAll after the context deadline elapsed")
	}

	mu.Lock()
	got := errorCode
	mu.Unlock()
	if got != "client_stream_interrupted" {
		t.Fatalf("errorCode = %q, want the pre-existing outcome preserved", got)
	}
}

func TestWatchRequestTimeoutDistinguishesRequestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var mu sync.Mutex
	errorCode := ""
	closed := make(chan struct{})
	go watchRequestTimeout(ctx, &mu, &errorCode, func() { close(closed) })
	cancel()

	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("watchRequestTimeout did not close the canceled session")
	}

	mu.Lock()
	got := errorCode
	mu.Unlock()
	if got != "request_canceled" {
		t.Fatalf("errorCode = %q, want request_canceled", got)
	}
}
