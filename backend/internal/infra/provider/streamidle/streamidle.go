package streamidle

import (
	"context"
	"io"
	"sync/atomic"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

// ReadCloser enforces an inactivity deadline over one streaming response body.
// It cancels the owning request context so HTTP/1.1 and HTTP/2 transports can
// unblock an in-flight Read without applying a connection-wide deadline.
type ReadCloser struct {
	io.ReadCloser
	idle     time.Duration
	timer    *time.Timer
	cancel   context.CancelCauseFunc
	timedOut atomic.Bool
}

func New(body io.ReadCloser, idle time.Duration, cancel context.CancelCauseFunc) *ReadCloser {
	wrapper := &ReadCloser{ReadCloser: body, idle: idle, cancel: cancel}
	wrapper.timer = time.AfterFunc(idle, func() {
		wrapper.timedOut.Store(true)
		cancel(neterror.ErrUpstreamStreamIdleTimeout)
	})
	return wrapper
}

func (r *ReadCloser) Read(buffer []byte) (int, error) {
	n, err := r.ReadCloser.Read(buffer)
	if n > 0 {
		r.timer.Reset(r.idle)
	}
	if err != nil && r.timedOut.Load() {
		return n, neterror.ErrUpstreamStreamIdleTimeout
	}
	return n, err
}

func (r *ReadCloser) Close() error {
	r.timer.Stop()
	r.cancel(nil)
	return r.ReadCloser.Close()
}

// TimedOut exposes immutable timeout state for lifecycle tests and diagnostics.
func (r *ReadCloser) TimedOut() bool { return r.timedOut.Load() }
