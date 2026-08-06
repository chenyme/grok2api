package cli

import (
	"context"
	"io"
	"sync/atomic"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

// errBuildStreamIdleTimeout aliases the neterror sentinel so the body wrapper
// can attach it as the cancel cause when a Grok Build stream goes idle.
var errBuildStreamIdleTimeout = neterror.ErrBuildStreamIdleTimeout

// idleCancelContext wraps a context.CancelCauseFunc-aware context and carries
// the configured idle duration plus the cancel function so the body wrapper
// (which only receives the context) can arm an idle timer.
type idleCancelContext struct {
	context.Context
	idle   time.Duration
	cancel context.CancelCauseFunc
}

func withIdleCancel(ctx context.Context, idle time.Duration, cancel context.CancelCauseFunc) context.Context {
	return &idleCancelContext{Context: ctx, idle: idle, cancel: cancel}
}

func idleCancelFrom(ctx context.Context) (time.Duration, context.CancelCauseFunc) {
	if value, ok := ctx.(*idleCancelContext); ok {
		return value.idle, value.cancel
	}
	return 0, nil
}

// idleTimeoutReadCloser wraps a streaming response body and aborts the read
// when no bytes arrive within the idle window. Each successful Read resets the
// deadline, so a slow-but-steady stream is never interrupted; only a fully
// silent connection is aborted.
//
// On timeout the underlying request context is cancelled (via cancel) so the
// Go transport unblocks the in-flight Read, and the next Read returns
// errBuildStreamIdleTimeout so callers can distinguish the abort from a
// transport error or clean EOF.
type idleTimeoutReadCloser struct {
	io.ReadCloser
	idle     time.Duration
	timer    *time.Timer
	cancel   context.CancelCauseFunc
	timedOut atomic.Bool
}

func newIdleTimeoutReadCloser(body io.ReadCloser, idle time.Duration, cancel context.CancelCauseFunc) *idleTimeoutReadCloser {
	wrapper := &idleTimeoutReadCloser{ReadCloser: body, idle: idle, cancel: cancel}
	wrapper.timer = time.AfterFunc(idle, func() {
		wrapper.timedOut.Store(true)
		cancel(errBuildStreamIdleTimeout)
	})
	return wrapper
}

func (r *idleTimeoutReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.timer.Reset(r.idle)
	}
	if err != nil && r.timedOut.Load() {
		return n, errBuildStreamIdleTimeout
	}
	return n, err
}

func (r *idleTimeoutReadCloser) Close() error {
	r.timer.Stop()
	r.cancel(nil)
	return r.ReadCloser.Close()
}
