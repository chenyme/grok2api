package buildtransport

import (
	"errors"
	"net/http"
	"time"

	"golang.org/x/net/http2"
)

const (
	// IdleConnTimeout stays below the CLI proxy's observed idle-close window so
	// an idle connection is retired before a later POST can reuse it.
	IdleConnTimeout = 30 * time.Second
	// HTTP2ReadIdleTimeout periodically probes an otherwise idle HTTP/2
	// connection. Go's default is zero, which leaves half-dead pooled
	// connections undetected until a request lands on them.
	HTTP2ReadIdleTimeout = 20 * time.Second
	HTTP2PingTimeout     = 10 * time.Second
)

// ConfigureHTTP2Health enables active PING health checks on a Build transport.
// It must be called after proxy and dialer options have been applied.
func ConfigureHTTP2Health(transport *http.Transport) (*http2.Transport, error) {
	if transport == nil {
		return nil, errors.New("Build HTTP transport is nil")
	}
	h2, err := http2.ConfigureTransports(transport)
	if err != nil {
		return nil, err
	}
	h2.ReadIdleTimeout = HTTP2ReadIdleTimeout
	h2.PingTimeout = HTTP2PingTimeout
	return h2, nil
}
