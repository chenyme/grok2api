package account

import (
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestPreserveActiveQuotaWindowsUntilReset(t *testing.T) {
	now := time.Now().UTC()
	future := now.Add(time.Hour)
	past := now.Add(-time.Second)
	incoming := []accountdomain.QuotaWindow{{Mode: "console", Remaining: 20, Total: 20}}

	active := preserveActiveQuotaWindows([]accountdomain.QuotaWindow{{Mode: "console", Remaining: 7, Total: 20, ResetAt: &future}}, incoming, now)
	if len(active) != 1 || active[0].Remaining != 7 {
		t.Fatalf("active window = %#v", active)
	}

	expired := preserveActiveQuotaWindows([]accountdomain.QuotaWindow{{Mode: "console", Remaining: 0, Total: 20, ResetAt: &past}}, incoming, now)
	if len(expired) != 1 || expired[0].Remaining != 20 {
		t.Fatalf("expired window = %#v", expired)
	}
}
