package web

import (
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestApplyFreeWebVideoDurationCap(t *testing.T) {
	free := &account.Billing{PlanName: "Free"}
	paid := &account.Billing{PlanName: "SuperGrok"}
	inferred := &account.Billing{SyncedAt: time.Now().UTC()}
	tests := []struct {
		name    string
		seconds int
		cap     int
		billing *account.Billing
		want    int
	}{
		{name: "free over cap", seconds: 10, cap: 6, billing: free, want: 6},
		{name: "free at cap", seconds: 6, cap: 6, billing: free, want: 6},
		{name: "free under cap", seconds: 4, cap: 6, billing: free, want: 4},
		{name: "paid not capped", seconds: 10, cap: 6, billing: paid, want: 10},
		{name: "unknown billing not capped", seconds: 10, cap: 6, billing: nil, want: 10},
		{name: "inferred free over cap", seconds: 8, cap: 6, billing: inferred, want: 6},
		{name: "zero cap uses default 6", seconds: 10, cap: 0, billing: free, want: 6},
		{name: "custom cap 8", seconds: 12, cap: 8, billing: free, want: 8},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := applyFreeWebVideoDurationCap(test.seconds, test.cap, test.billing)
			if got != test.want {
				t.Fatalf("got %d want %d", got, test.want)
			}
		})
	}
}
