package neterror

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"testing"
)

type timeoutError string

func (e timeoutError) Error() string { return string(e) }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestIsResponseHeaderTimeout(t *testing.T) {
	wrapped := &url.Error{Op: "Post", URL: "https://example.test/v1/responses", Err: timeoutError("http2: timeout awaiting response headers")}
	if !IsResponseHeaderTimeout(wrapped) {
		t.Fatal("HTTP/2 response-header timeout was not recognized")
	}
	for _, err := range []error{
		context.DeadlineExceeded,
		timeoutError("TLS handshake timeout"),
		errors.New("timeout awaiting response headers"),
	} {
		if IsResponseHeaderTimeout(err) {
			t.Fatalf("unexpected response-header timeout classification for %v", err)
		}
	}
}

func TestIsUpstreamStreamIdleTimeout(t *testing.T) {
	wrapped := &url.Error{Op: "Post", URL: "https://example.test/v1/responses", Err: ErrUpstreamStreamIdleTimeout}
	if !IsUpstreamStreamIdleTimeout(wrapped) || !IsBuildStreamIdleTimeout(wrapped) {
		t.Fatal("provider stream-idle timeout was not recognized through compatibility classifiers")
	}
	if IsUpstreamStreamIdleTimeout(context.DeadlineExceeded) {
		t.Fatal("generic context deadline was misclassified as stream-idle timeout")
	}
}

func TestIdleTimeoutErrorRetainsBodyProgress(t *testing.T) {
	empty := &IdleTimeoutError{}
	if !IsUpstreamStreamIdleTimeout(empty) || IdleTimeoutObservedData(empty) {
		t.Fatalf("empty idle classification = idle:%t observed:%t", IsUpstreamStreamIdleTimeout(empty), IdleTimeoutObservedData(empty))
	}
	partial := fmt.Errorf("read body: %w", &IdleTimeoutError{DataObserved: true})
	if !IsUpstreamStreamIdleTimeout(partial) || !IdleTimeoutObservedData(partial) {
		t.Fatalf("partial idle classification = idle:%t observed:%t", IsUpstreamStreamIdleTimeout(partial), IdleTimeoutObservedData(partial))
	}
}

func TestIsUpstreamResponseEmpty(t *testing.T) {
	if !IsUpstreamResponseEmpty(fmt.Errorf("read body: %w", ErrUpstreamResponseEmpty)) || IsUpstreamResponseEmpty(io.EOF) {
		t.Fatal("empty response sentinel classification failed")
	}
}

func TestIsClientRequestCancel(t *testing.T) {
	idleCtx, idleCancel := context.WithCancelCause(context.Background())
	idleCancel(ErrUpstreamStreamIdleTimeout)
	if IsClientRequestCancel(idleCtx, context.Canceled) {
		t.Fatal("idle timeout must not look like a client cancel")
	}
	if IsClientRequestCancel(context.Background(), ErrUpstreamStreamIdleTimeout) {
		t.Fatal("idle timeout error must not look like a client cancel")
	}
	plain, cancel := context.WithCancel(context.Background())
	cancel()
	if !IsClientRequestCancel(plain, io.EOF) {
		t.Fatal("canceled request context must be a client cancel")
	}
	if !IsClientRequestCancel(context.Background(), context.Canceled) {
		t.Fatal("context.Canceled must be a client cancel")
	}
	if IsClientRequestCancel(context.Background(), io.EOF) {
		t.Fatal("plain upstream EOF must not look like a client cancel")
	}
	internal, stop := context.WithCancelCause(context.Background())
	stop(errors.New("upstream stream first char timeout"))
	if IsClientRequestCancel(internal, context.Canceled) {
		t.Fatal("internal cancel cause must not look like a client cancel")
	}
}
