package requestmeta

import (
	"context"
	"net"
	"strings"
)

type clientIPContextKey struct{}

// NormalizeClientIP returns a canonical IPv4 or IPv6 address without a port.
// Invalid values are discarded so forwarded header contents never reach audits.
func NormalizeClientIP(value string) string {
	ip := net.ParseIP(strings.TrimSpace(value))
	if ip == nil {
		return ""
	}
	return ip.String()
}

// WithClientIP attaches a normalized request-network identifier to ctx.
func WithClientIP(ctx context.Context, value string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, clientIPContextKey{}, NormalizeClientIP(value))
}

// ClientIP returns the normalized caller address captured at the HTTP boundary.
func ClientIP(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(clientIPContextKey{}).(string)
	return NormalizeClientIP(value)
}
