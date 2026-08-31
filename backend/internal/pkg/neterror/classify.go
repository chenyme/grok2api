package neterror

import (
	"context"
	"errors"
	"net"
	"strings"
)

const responseHeaderTimeoutMarker = "timeout awaiting response headers"

// ErrUpstreamStreamIdleTimeout is attached to a request context when a
// provider streaming response is aborted because no data arrived within the
// configured idle window.
var ErrUpstreamStreamIdleTimeout = errors.New("upstream stream idle timeout")

// ErrUpstreamResponseEmpty identifies a successful upstream response whose
// body reached EOF before producing any bytes. It is separate from malformed
// JSON and client cancellation so callers may apply the empty-response health
// policy without penalizing ordinary request aborts.
var ErrUpstreamResponseEmpty = errors.New("upstream response body is empty")

// ErrBuildStreamIdleTimeout is retained as a compatibility alias for callers
// introduced before stream-idle protection became provider-neutral.
var ErrBuildStreamIdleTimeout = ErrUpstreamStreamIdleTimeout

// IsResponseHeaderTimeout identifies the HTTP/1.1 and HTTP/2 timeout values
// returned by the Go transport while waiting for the first response headers.
func IsResponseHeaderTimeout(err error) bool {
	if err == nil {
		return false
	}
	var networkError net.Error
	if !errors.As(err, &networkError) || !networkError.Timeout() {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), responseHeaderTimeoutMarker)
}

// IsBuildStreamIdleTimeout reports whether err is (or wraps) the sentinel
// raised when a Grok Build streaming response is aborted for going idle.
func IsBuildStreamIdleTimeout(err error) bool {
	return errors.Is(err, ErrUpstreamStreamIdleTimeout)
}

// IsUpstreamStreamIdleTimeout reports whether err is (or wraps) the shared
// provider stream-idle timeout sentinel.
func IsUpstreamStreamIdleTimeout(err error) bool {
	return errors.Is(err, ErrUpstreamStreamIdleTimeout)
}

// IsUpstreamResponseEmpty reports whether a successful upstream response
// completed without a response body.
func IsUpstreamResponseEmpty(err error) bool {
	return errors.Is(err, ErrUpstreamResponseEmpty)
}

// IdleTimeoutError retains whether any response bytes arrived before an idle
// deadline. A zero-byte idle may use the long account cooldown; a partial
// response should receive only the ordinary transient failure penalty.
type IdleTimeoutError struct {
	DataObserved bool
}

func (e *IdleTimeoutError) Error() string { return ErrUpstreamStreamIdleTimeout.Error() }
func (e *IdleTimeoutError) Unwrap() error { return ErrUpstreamStreamIdleTimeout }

// IdleTimeoutObservedData returns true only for an idle timeout that records
// response-body progress before the deadline.
func IdleTimeoutObservedData(err error) bool {
	var idle *IdleTimeoutError
	return errors.As(err, &idle) && idle.DataObserved
}

// IsClientRequestCancel reports a real client disconnect.
// Internal aborts (idle timeout, first-char timeout, …) cancel the same
// context with a cause other than context.Canceled and must not be treated
// as the client hanging up. Pre-header client cancel still maps to 499
// request_canceled; a 2xx stream copy uses client_stream_interrupted.
func IsClientRequestCancel(ctx context.Context, err error) bool {
	if IsUpstreamStreamIdleTimeout(err) {
		return false
	}
	if ctx != nil {
		cause := context.Cause(ctx)
		if IsUpstreamStreamIdleTimeout(cause) {
			return false
		}
		if ctx.Err() != nil {
			if cause != nil && !errors.Is(cause, context.Canceled) {
				return false
			}
			return true
		}
	}
	return errors.Is(err, context.Canceled)
}
