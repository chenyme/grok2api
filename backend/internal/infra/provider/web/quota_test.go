package web

import (
	"encoding/hex"
	"math"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestParseCapturedWeeklyCreditsResponse(t *testing.T) {
	body, err := hex.DecodeString("00000000630a610d0000304112001a00220c089abbccd2061080f2d1fc012a0c089ab0f1d2061080f2d1fc013a07080515000020413a070804150000803f3a020802421e0802120c089abbccd2061080f2d1fc011a0c089ab0f1d2061080f2d1fc01580162006801800000000f677270632d7374617475733a300d0a")
	if err != nil {
		t.Fatal(err)
	}
	syncedAt := time.Date(2026, 7, 12, 14, 0, 0, 0, time.UTC)
	window, err := parseWeeklyCreditsResponse(body, 42, syncedAt)
	if err != nil {
		t.Fatal(err)
	}
	if window.AccountID != 42 || window.Mode != weeklyQuotaMode || window.Total != 10000 || window.Remaining != 8900 || window.WindowSeconds != 7*24*60*60 {
		t.Fatalf("window = %#v", window)
	}
	if math.Abs(window.UsagePercent-11) > 0.001 || window.ResetAt == nil || window.ResetAt.Unix() != 1784436762 {
		t.Fatalf("usage/reset = %#v", window)
	}
	if len(window.Breakdown) != 3 || window.Breakdown[0].ProductCode != account.QuotaProductImagine || window.Breakdown[0].UsagePercent != 10 || window.Breakdown[1].ProductCode != account.QuotaProductChat || window.Breakdown[1].UsagePercent != 1 || window.Breakdown[2].ProductCode != account.QuotaProductBuild || window.Breakdown[2].UsagePercent != 0 {
		t.Fatalf("breakdown = %#v", window.Breakdown)
	}
}
