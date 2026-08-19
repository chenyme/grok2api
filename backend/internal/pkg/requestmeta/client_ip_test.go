package requestmeta

import (
	"context"
	"testing"
)

func TestClientIPContextNormalizesAndRejectsInvalidValues(t *testing.T) {
	ctx := WithClientIP(context.Background(), "  ::ffff:192.0.2.10  ")
	if got := ClientIP(ctx); got != "192.0.2.10" {
		t.Fatalf("client IP = %q", got)
	}
	if got := ClientIP(WithClientIP(ctx, "not-an-ip")); got != "" {
		t.Fatalf("invalid value was retained: %q", got)
	}
}
