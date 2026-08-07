package neterror

import (
	"errors"
	"net"
	"strings"
)

const responseHeaderTimeoutMarker = "timeout awaiting response headers"

// ErrUpstreamStreamIdleTimeout is attached to a request context when a
// provider streaming response is aborted because no data arrived within the
// configured idle window.
var ErrUpstreamStreamIdleTimeout = errors.New("upstream stream idle timeout")

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
