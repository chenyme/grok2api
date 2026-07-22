package neterror

import (
	"errors"
	"net"
	"strings"
)

const responseHeaderTimeoutMarker = "timeout awaiting response headers"

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
