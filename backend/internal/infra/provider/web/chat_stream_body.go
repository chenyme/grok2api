package web

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
)

type gatewayStreamBody struct {
	io.ReadCloser
	upstream     io.Closer
	once         sync.Once
	upstreamOnce sync.Once
	closeErr     error
	upstreamErr  error
	closed       atomic.Bool
	done         chan struct{}
}

func (body *gatewayStreamBody) Close() error {
	body.once.Do(func() {
		body.closed.Store(true)
		body.closeErr = errors.Join(body.ReadCloser.Close(), body.closeUpstream())
	})
	return body.closeErr
}

func (body *gatewayStreamBody) upstreamError(ctx context.Context, err error) error {
	if body.closed.Load() || errors.Is(ctx.Err(), context.Canceled) {
		return context.Canceled
	}
	return err
}

func (body *gatewayStreamBody) closeUpstream() error {
	body.upstreamOnce.Do(func() {
		body.upstreamErr = body.upstream.Close()
	})
	return body.upstreamErr
}
