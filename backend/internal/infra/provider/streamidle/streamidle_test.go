package streamidle

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

type contextReader struct{ ctx context.Context }

func (r contextReader) Read([]byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func (contextReader) Close() error { return nil }

func TestReadCloserCancelsBlockedReadWithSharedSentinel(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	wrapper := New(contextReader{ctx: ctx}, 20*time.Millisecond, cancel)
	defer wrapper.Close()

	_, err := wrapper.Read(make([]byte, 1))
	if !errors.Is(err, neterror.ErrUpstreamStreamIdleTimeout) {
		t.Fatalf("Read() error = %v, want ErrUpstreamStreamIdleTimeout", err)
	}
	if cause := context.Cause(ctx); !errors.Is(cause, neterror.ErrUpstreamStreamIdleTimeout) {
		t.Fatalf("context cause = %v, want ErrUpstreamStreamIdleTimeout", cause)
	}
	if !wrapper.TimedOut() {
		t.Fatal("TimedOut() = false after idle deadline")
	}
}

func TestReadCloserResetsDeadlineOnProgress(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	wrapper := New(io.NopCloser(&pacedReader{chunks: 3, gap: 10 * time.Millisecond}), 30*time.Millisecond, cancel)
	defer wrapper.Close()
	if _, err := io.Copy(io.Discard, wrapper); err != nil {
		t.Fatal(err)
	}
	if wrapper.TimedOut() || context.Cause(ctx) != nil {
		t.Fatalf("steady stream timed out: timedOut=%t cause=%v", wrapper.TimedOut(), context.Cause(ctx))
	}
}

type pacedReader struct {
	chunks int
	gap    time.Duration
}

func (r *pacedReader) Read(buffer []byte) (int, error) {
	if r.chunks == 0 {
		return 0, io.EOF
	}
	time.Sleep(r.gap)
	r.chunks--
	buffer[0] = 'x'
	return 1, nil
}
