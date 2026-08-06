package neterror

import (
	"errors"
	"net"
	"strings"
)

const responseHeaderTimeoutMarker = "timeout awaiting response headers"

// ErrBuildStreamIdleTimeout is the cause attached to a request context when a
// Grok Build streaming response is aborted because no data arrived within the
// configured idle window.
var ErrBuildStreamIdleTimeout = errors.New("grok build stream idle timeout")

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
	return errors.Is(err, ErrBuildStreamIdleTimeout)
}
