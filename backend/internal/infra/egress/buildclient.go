package egress

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	_ "github.com/bdandy/go-socks4"
	xproxy "golang.org/x/net/proxy"
)

// newBuildClient keeps Grok Build on the standard Go HTTP/TLS stack used by
// the official CLI-facing transport. Browser TLS impersonation is reserved for
// Grok Web, where the browser fingerprint and User-Agent belong together.
func newBuildClient(proxyURL string) (*http.Client, error) {
	direct := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           direct.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   128,
		MaxConnsPerHost:       256,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	if strings.TrimSpace(proxyURL) != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("解析 Grok Build 出口代理: %w", err)
		}
		switch strings.ToLower(parsed.Scheme) {
		case "http", "https":
			transport.Proxy = http.ProxyURL(parsed)
		case "socks4", "socks4a", "socks5", "socks5h":
			dialer, err := xproxy.FromURL(parsed, direct)
			if err != nil {
				return nil, fmt.Errorf("创建 Grok Build SOCKS 代理: %w", err)
			}
			transport.DialContext = dialContext(dialer)
		default:
			return nil, fmt.Errorf("Grok Build 不支持代理协议 %q", parsed.Scheme)
		}
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

func dialContext(dialer xproxy.Dialer) func(context.Context, string, string) (net.Conn, error) {
	if contextual, ok := dialer.(xproxy.ContextDialer); ok {
		return contextual.DialContext
	}
	type result struct {
		connection net.Conn
		err        error
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		completed := make(chan result, 1)
		go func() {
			connection, err := dialer.Dial(network, address)
			completed <- result{connection: connection, err: err}
		}()
		select {
		case value := <-completed:
			return value.connection, value.err
		case <-ctx.Done():
			go func() {
				value := <-completed
				if value.connection != nil {
					_ = value.connection.Close()
				}
			}()
			return nil, ctx.Err()
		}
	}
}
