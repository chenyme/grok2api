package cli

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestShouldReportEgressFailure(t *testing.T) {
	canceledContext, cancel := context.WithCancel(context.Background())
	cancel()
	deadlineContext, deadlineCancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer deadlineCancel()

	tests := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{name: "no error", ctx: context.Background(), err: nil, want: false},
		{name: "canceled request context", ctx: canceledContext, err: errors.New("connection reset"), want: false},
		{name: "expired request context", ctx: deadlineContext, err: context.DeadlineExceeded, want: false},
		{name: "canceled transport", ctx: context.Background(), err: context.Canceled, want: false},
		{name: "wrapped canceled transport", ctx: context.Background(), err: fmt.Errorf("round trip: %w", context.Canceled), want: false},
		{name: "connection failure", ctx: context.Background(), err: errors.New("connection refused"), want: true},
		{name: "independent transport timeout", ctx: context.Background(), err: context.DeadlineExceeded, want: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldReportEgressFailure(test.ctx, test.err); got != test.want {
				t.Fatalf("shouldReportEgressFailure() = %v, want %v", got, test.want)
			}
		})
	}
}
