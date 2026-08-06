package cli

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

// blockingReader never returns from Read on its own; it only unblocks when the
// test signals it (via the release channel) or the context is cancelled. This
// mirrors how a Go HTTP transport body unblocks when the request context fires.
type blockingReader struct {
	release chan struct{}
	err     error
}

func (r *blockingReader) Read(p []byte) (int, error) {
	<-r.release
	return 0, r.err
}

func (r *blockingReader) Close() error { return nil }

// staticReader returns its bytes and error immediately, once.
type staticReader struct {
	data []byte
	err  error
	once bool
}

func (r *staticReader) Read(p []byte) (int, error) {
	if r.once {
		return 0, io.EOF
	}
	r.once = true
	n := copy(p, r.data)
	return n, r.err
}

func (r *staticReader) Close() error { return nil }

// TestIdleTimeoutReadCloserNormalRead verifies that a body producing data
// before the idle window elapses completes without aborting.
func TestIdleTimeoutReadCloserNormalRead(t *testing.T) {
	body := &staticReader{data: []byte("hello"), err: io.EOF}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	wrapper := newIdleTimeoutReadCloser(body, time.Hour, cancel)

	buffer := make([]byte, 32)
	n, err := wrapper.Read(buffer)
	if n != 5 || string(buffer[:n]) != "hello" {
		t.Fatalf("Read() = %d %q, want 5 \"hello\"", n, buffer[:n])
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Read() error = %v, want io.EOF", err)
	}
	if wrapper.timedOut.Load() {
		t.Fatal("timedOut should be false on a normal EOF")
	}
	if ctx.Err() != nil {
		t.Fatal("context should not be cancelled on a normal read")
	}
}

// TestIdleTimeoutReadCloserIdleAbort verifies that a silent body is aborted
// with ErrBuildStreamIdleTimeout once the idle window elapses, and that the
// request context is cancelled with the same cause.
func TestIdleTimeoutReadCloserIdleAbort(t *testing.T) {
	release := make(chan struct{})
	// The reader returns context.Canceled once unblocked, mirroring how a Go
	// transport body surfaces a cancelled request context.
	body := &blockingReader{release: release, err: context.Canceled}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	wrapper := newIdleTimeoutReadCloser(body, 20*time.Millisecond, cancel)

	// Wait for the idle timer to fire and cancel the context.
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("idle timer did not fire within 1s")
	}
	if cause := context.Cause(ctx); !errors.Is(cause, neterror.ErrBuildStreamIdleTimeout) {
		t.Fatalf("context cause = %v, want ErrBuildStreamIdleTimeout", cause)
	}
	if !wrapper.timedOut.Load() {
		t.Fatal("timedOut should be true after idle window")
	}

	// Unblock the blocked Read; the wrapper must surface the sentinel error
	// (not the raw context.Canceled) because timedOut is set.
	go close(release)
	buffer := make([]byte, 32)
	n, err := wrapper.Read(buffer)
	if n != 0 {
		t.Fatalf("Read() n = %d, want 0", n)
	}
	if !errors.Is(err, neterror.ErrBuildStreamIdleTimeout) {
		t.Fatalf("Read() error = %v, want ErrBuildStreamIdleTimeout", err)
	}
}

// TestIdleTimeoutReadCloserResetOnData verifies that each successful Read
// resets the idle deadline so a slow-but-steady stream is never interrupted.
func TestIdleTimeoutReadCloserResetOnData(t *testing.T) {
	body := &feedReader{chunks: [][]byte{[]byte("chunk-1"), []byte("chunk-2"), []byte("chunk-3")}, finalErr: io.EOF}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	// Idle is shorter than the total read time but longer than each gap.
	wrapper := newIdleTimeoutReadCloser(body, 60*time.Millisecond, cancel)

	for _, expected := range []string{"chunk-1", "chunk-2", "chunk-3"} {
		buffer := make([]byte, 32)
		n, err := wrapper.Read(buffer)
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("Read() error = %v, want nil or io.EOF", err)
		}
		if string(buffer[:n]) != expected {
			t.Fatalf("Read() = %q, want %q", buffer[:n], expected)
		}
		// Gap between chunks, well within the idle window.
		time.Sleep(30 * time.Millisecond)
	}
	if wrapper.timedOut.Load() {
		t.Fatal("timedOut should be false for a steady stream")
	}
	if ctx.Err() != nil {
		t.Fatal("context should not be cancelled for a steady stream")
	}
}

// TestIdleTimeoutReadCloserCloseStopsTimer verifies that Close stops the idle
// timer, preventing it from firing and cancelling the context after release.
func TestIdleTimeoutReadCloserClose(t *testing.T) {
	release := make(chan struct{})
	body := &blockingReader{release: release}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	wrapper := newIdleTimeoutReadCloser(body, 20*time.Millisecond, cancel)

	if err := wrapper.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	// Close calls cancel(nil) which cancels the context; that is expected.
	// The key invariant is that the timer did not independently fire with the
	// sentinel cause before Close stopped it. We verify the cause is nil (from
	// Close's cancel(nil)), not the idle sentinel.
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		t.Fatalf("context cause = %v, want nil or context.Canceled from Close", cause)
	}
	if wrapper.timedOut.Load() {
		t.Fatal("timedOut should be false when Close stops the timer in time")
	}
	close(release)
}

// feedReader returns chunks one at a time, then finalErr.
type feedReader struct {
	chunks   [][]byte
	finalErr error
	index    int
	mu       sync.Mutex
}

func (r *feedReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.index >= len(r.chunks) {
		return 0, r.finalErr
	}
	chunk := r.chunks[r.index]
	r.index++
	n := copy(p, chunk)
	return n, nil
}

func (r *feedReader) Close() error { return nil }
