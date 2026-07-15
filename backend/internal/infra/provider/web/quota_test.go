package web

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

const capturedWeeklyCreditsHex = "00000000630a610d0000304112001a00220c089abbccd2061080f2d1fc012a0c089ab0f1d2061080f2d1fc013a07080515000020413a070804150000803f3a020802421e0802120c089abbccd2061080f2d1fc011a0c089ab0f1d2061080f2d1fc01580162006801800000000f677270632d7374617475733a300d0a"

func TestParseCapturedWeeklyCreditsResponse(t *testing.T) {
	body, err := hex.DecodeString(capturedWeeklyCreditsHex)
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

func TestSyncQuotaFetchesWeeklyOnlyAfterPaidTierIsConfirmed(t *testing.T) {
	weeklyBody, err := hex.DecodeString(capturedWeeklyCreditsHex)
	if err != nil {
		t.Fatal(err)
	}
	var weeklyCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/grok_api_v2.GrokBuildBilling/GetGrokCreditsConfig":
			weeklyCalls.Add(1)
			writer.Header().Set("Content-Type", "application/grpc-web+proto")
			_, _ = writer.Write(weeklyBody)
		case "/rest/rate-limits":
			var payload struct {
				ModelName string `json:"modelName"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Errorf("quota payload: %v", err)
			}
			total := map[string]int{"auto": 50, "fast": 140}[payload.ModelName]
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"windowSizeSeconds": 7200, "remainingQueries": total, "totalQueries": total,
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("test-sso")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{
		BaseURL: server.URL, StatsigMode: "manual", StatsigManualValue: "test-signature",
	}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)
	snapshot, err := adapter.SyncQuota(context.Background(), account.Credential{ID: 2, WebTier: account.WebTierAuto, EncryptedAccessToken: encrypted})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Tier != account.WebTierSuper || len(snapshot.Windows) != 1 || snapshot.Windows[0].Mode != weeklyQuotaMode || weeklyCalls.Load() != 1 {
		t.Fatalf("snapshot = %#v, weekly calls = %d", snapshot, weeklyCalls.Load())
	}
}

func TestSyncQuotaStopsAfterFirstUnauthorizedMode(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()

	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("expired-sso")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{
		BaseURL: server.URL, StatsigMode: "manual", StatsigManualValue: "test-signature",
	}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)
	_, err = adapter.SyncQuota(context.Background(), account.Credential{ID: 3, WebTier: account.WebTierAuto, EncryptedAccessToken: encrypted})
	if !errors.Is(err, provider.ErrUnauthorized) {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("unauthorized credential made %d quota requests", calls.Load())
	}
}

func TestInferWebTierFromUpstreamQuota(t *testing.T) {
	tests := []struct {
		name    string
		windows []account.QuotaWindow
		want    account.WebTier
		known   bool
	}{
		{name: "current basic", windows: []account.QuotaWindow{{Mode: "auto", Total: 7}, {Mode: "fast", Total: 30}}, want: account.WebTierBasic, known: true},
		{name: "legacy basic", windows: []account.QuotaWindow{{Mode: "auto", Total: 20}}, want: account.WebTierBasic, known: true},
		{name: "super", windows: []account.QuotaWindow{{Mode: "auto", Total: 50}, {Mode: "fast", Total: 140}}, want: account.WebTierSuper, known: true},
		{name: "heavy", windows: []account.QuotaWindow{{Mode: "auto", Total: 150}, {Mode: "fast", Total: 400}}, want: account.WebTierHeavy, known: true},
		{name: "heavy mode", windows: []account.QuotaWindow{{Mode: "heavy", Total: 20}}, want: account.WebTierHeavy, known: true},
		{name: "conflicting signal uses lower tier", windows: []account.QuotaWindow{{Mode: "auto", Total: 50}, {Mode: "fast", Total: 30}}, want: account.WebTierBasic, known: true},
		{name: "unknown", windows: []account.QuotaWindow{{Mode: "auto", Total: 9}, {Mode: "fast", Total: 31}}, want: account.WebTierAuto, known: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, known := inferWebTierFromQuota(test.windows)
			if got != test.want || known != test.known {
				t.Fatalf("tier = %q, known = %v, want %q/%v", got, known, test.want, test.known)
			}
		})
	}
}

func TestResolveWebTierUsesFreshWebQuotaOverStoredTier(t *testing.T) {
	basicWindows := []account.QuotaWindow{{Mode: "auto", Total: 7}, {Mode: "fast", Total: 30}}
	for _, stored := range []account.WebTier{account.WebTierAuto, account.WebTierBasic, account.WebTierSuper, account.WebTierHeavy} {
		tier, useWeekly := resolveWebTierFromQuota(stored, basicWindows, true)
		if tier != account.WebTierBasic || useWeekly {
			t.Fatalf("stored %q resolved to %q, weekly=%v", stored, tier, useWeekly)
		}
	}

	tier, useWeekly := resolveWebTierFromQuota(account.WebTierBasic, []account.QuotaWindow{{Mode: "auto", Total: 50}}, true)
	if tier != account.WebTierSuper || !useWeekly {
		t.Fatalf("super snapshot resolved to %q, weekly=%v", tier, useWeekly)
	}

	tier, useWeekly = resolveWebTierFromQuota(account.WebTierHeavy, nil, true)
	if tier != account.WebTierHeavy || !useWeekly {
		t.Fatalf("heavy weekly fallback resolved to %q, weekly=%v", tier, useWeekly)
	}

	tier, useWeekly = resolveWebTierFromQuota(account.WebTierSuper, []account.QuotaWindow{{Mode: "auto", Total: 9}}, true)
	if tier != account.WebTierAuto || useWeekly {
		t.Fatalf("unknown snapshot resolved to %q, weekly=%v", tier, useWeekly)
	}

	tier, useWeekly = resolveWebTierFromQuota(account.WebTierSuper, nil, true)
	if tier != account.WebTierSuper || !useWeekly {
		t.Fatalf("super weekly fallback resolved to %q, weekly=%v", tier, useWeekly)
	}

	tier, useWeekly = resolveWebTierFromQuota(account.WebTierBasic, nil, true)
	if tier != account.WebTierBasic || useWeekly {
		t.Fatalf("basic should not be promoted when modes unavailable: got %q, weekly=%v", tier, useWeekly)
	}

	tier, useWeekly = resolveWebTierFromQuota(account.WebTierAuto, nil, true)
	if tier != account.WebTierAuto || useWeekly {
		t.Fatalf("auto should not be promoted when modes unavailable: got %q, weekly=%v", tier, useWeekly)
	}
}

func TestSyncQuotaCorrectsStoredSuperFromFreshWebQuota(t *testing.T) {
	var weeklyCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/grok_api_v2.GrokBuildBilling/GetGrokCreditsConfig":
			weeklyCalls.Add(1)
			http.Error(writer, "not available", http.StatusNotFound)
		case "/rest/rate-limits":
			var payload struct {
				ModelName string `json:"modelName"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Errorf("quota payload: %v", err)
			}
			total := 0
			switch payload.ModelName {
			case "auto":
				total = 7
			case "fast":
				total = 30
			default:
				http.Error(writer, "unsupported mode", http.StatusBadRequest)
				return
			}
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"windowSizeSeconds": 7200, "remainingQueries": total, "totalQueries": total,
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("test-sso")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{
		BaseURL: server.URL, StatsigMode: "manual", StatsigManualValue: "test-signature",
	}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)
	snapshot, err := adapter.SyncQuota(context.Background(), account.Credential{
		ID: 1, WebTier: account.WebTierSuper, EncryptedAccessToken: encrypted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Tier != account.WebTierBasic || len(snapshot.Windows) != 2 || snapshot.Windows[0].Mode != "auto" || snapshot.Windows[0].Total != 7 || snapshot.Windows[1].Mode != "fast" || snapshot.Windows[1].Total != 30 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if weeklyCalls.Load() != 0 {
		t.Fatalf("basic account probed weekly endpoint %d times", weeklyCalls.Load())
	}
}
