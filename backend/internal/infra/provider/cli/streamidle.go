package cli

import (
	"context"
	"io"
	"time"

	providerstreamidle "github.com/chenyme/grok2api/backend/internal/infra/provider/streamidle"
)

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

type idleTimeoutReadCloser = providerstreamidle.ReadCloser

func newIdleTimeoutReadCloser(body io.ReadCloser, idle time.Duration, cancel context.CancelCauseFunc) *idleTimeoutReadCloser {
	return providerstreamidle.New(body, idle, cancel)
}
