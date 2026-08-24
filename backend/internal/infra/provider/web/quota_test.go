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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
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

func TestParseUnusedPreciseWeeklyCreditsResponse(t *testing.T) {
	body, err := hex.DecodeString("00000000480a4612001a00220c08c5d5d3d20610c0c7a1ee012a0c08c5caf8d20610c0c7a1ee01421e0802120c08c5d5d3d20610c0c7a1ee011a0c08c5caf8d20610c0c7a1ee01580162006801800000000f677270632d7374617475733a300d0a")
	if err != nil {
		t.Fatal(err)
	}
	window, err := parseWeeklyCreditsResponse(body, 42, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if window.Total != 10000 || window.Remaining != 10000 || window.UsagePercent != 0 || window.ResetAt == nil || window.ResetAt.Nanosecond() == 0 {
		t.Fatalf("window = %#v", window)
	}
}

func TestParseCoarseWeeklyCreditsResponseRemainsUnavailable(t *testing.T) {
	body, err := hex.DecodeString("00000000300a2e12001a0022060880a6b6d2062a0608809bdbd2064212080212060880a6b6d2061a0608809bdbd206580162006801800000000f677270632d7374617475733a300d0a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseWeeklyCreditsResponse(body, 42, time.Now().UTC()); err == nil {
		t.Fatal("expected coarse period without usage to be rejected")
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
		case "/rest/media/imagine/quota_info":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{
				"image":{"available":true,"windowSizeSeconds":64800},
				"imagePro":{"available":true,"windowSizeSeconds":64800},
				"imageEdit":{"available":true,"windowSizeSeconds":64800},
				"video":{"available":true,"windowSizeSeconds":64800},
				"video720p":{"available":true,"windowSizeSeconds":64800}
			}`))
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

func TestSyncQuotaFailsWhenPaidWeeklySnapshotIsUnavailable(t *testing.T) {
	var weeklyCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/rest/media/imagine/quota_info":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{
				"image":{"available":true,"windowSizeSeconds":64800},
				"imagePro":{"available":true,"windowSizeSeconds":64800},
				"imageEdit":{"available":true,"windowSizeSeconds":64800},
				"video":{"available":true,"windowSizeSeconds":64800},
				"video720p":{"available":true,"windowSizeSeconds":64800}
			}`))
		case "/grok_api_v2.GrokBuildBilling/GetGrokCreditsConfig":
			weeklyCalls.Add(1)
			http.Error(writer, "temporary weekly failure", http.StatusServiceUnavailable)
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
	if _, err := adapter.SyncQuota(context.Background(), account.Credential{ID: 2, WebTier: account.WebTierAuto, EncryptedAccessToken: encrypted}); err == nil {
		t.Fatal("expected the paid weekly failure to reject the partial snapshot")
	}
	if weeklyCalls.Load() != 1 {
		t.Fatalf("weekly calls = %d", weeklyCalls.Load())
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

func TestSyncQuotaBlockedForbiddenIsUnauthorized(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusForbidden)
		_, _ = writer.Write([]byte(`{"code":7,"message":"User is blocked [WKE=unauthorized:blocked-user]","details":[]}`))
	}))
	defer server.Close()

	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("blocked-sso")
	if err != nil {
		t.Fatal(err)
	}
	egressRepository := &recordingWebEgressRepository{node: egressdomain.Node{
		ID: 1, Name: "web", Scope: egressdomain.ScopeWeb, Enabled: true, Health: 1,
	}}
	adapter := NewAdapter(Config{
		BaseURL: server.URL, StatsigMode: "manual", StatsigManualValue: "test-signature",
	}, infraegress.NewManager(egressRepository, cipher), cipher, nil, nil)
	_, err = adapter.SyncQuota(context.Background(), account.Credential{ID: 4, WebTier: account.WebTierAuto, EncryptedAccessToken: encrypted})
	if !errors.Is(err, provider.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	// manual Statsig mode does not invalidate/retry; SyncQuota stops after the first unauthorized mode.
	if calls.Load() != 1 {
		t.Fatalf("blocked credential made %d quota requests, want 1", calls.Load())
	}
	if updates := egressRepository.UpdateCount(); updates != 0 {
		t.Fatalf("blocked account changed egress health %d times", updates)
	}
}

func TestSyncQuotaBlockedForbiddenSkipsStatsigRetryInURLMode(t *testing.T) {
	var rateLimitCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// URL mode may also probe the base URL for Statsig meta; only count quota endpoint.
		if request.URL.Path == "/rest/rate-limits" {
			rateLimitCalls.Add(1)
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusForbidden)
			_, _ = writer.Write([]byte(`{"code":7,"message":"User is blocked [WKE=unauthorized:blocked-user]","details":[]}`))
			return
		}
		http.NotFound(writer, request)
	}))
	defer server.Close()

	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("blocked-sso-url")
	if err != nil {
		t.Fatal(err)
	}
	// URL mode would invalidate Statsig and retry unless blocked body is classified first.
	adapter := NewAdapter(Config{
		BaseURL: server.URL, StatsigMode: "url", StatsigSignerURL: "https://signer.example/sign",
	}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)
	_, err = adapter.SyncQuota(context.Background(), account.Credential{ID: 6, WebTier: account.WebTierAuto, EncryptedAccessToken: encrypted})
	if !errors.Is(err, provider.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if rateLimitCalls.Load() != 1 {
		t.Fatalf("blocked credential made %d rate-limits requests, want 1 (no Statsig retry)", rateLimitCalls.Load())
	}
}

func TestSyncQuotaGenericForbiddenIsNotUnauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusForbidden)
		_, _ = writer.Write([]byte(`{"message":"temporary rejection"}`))
	}))
	defer server.Close()

	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("generic-sso")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{
		BaseURL: server.URL, StatsigMode: "manual", StatsigManualValue: "test-signature",
	}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)
	_, err = adapter.SyncQuota(context.Background(), account.Credential{ID: 5, WebTier: account.WebTierAuto, EncryptedAccessToken: encrypted})
	if err == nil || errors.Is(err, provider.ErrUnauthorized) {
		t.Fatalf("err = %v, want generic forbidden error", err)
	}
}

func TestSyncWeeklyCreditsBlockedForbiddenIsUnauthorized(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusForbidden)
		_, _ = writer.Write([]byte(`{"code":"unauthorized:blocked-user","error":"User is blocked"}`))
	}))
	defer server.Close()

	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("blocked-weekly-sso")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{
		BaseURL: server.URL, StatsigMode: "manual", StatsigManualValue: "test-signature",
	}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)
	_, err = adapter.SyncQuotaMode(context.Background(), account.Credential{ID: 7, EncryptedAccessToken: encrypted}, weeklyQuotaMode)
	if !errors.Is(err, provider.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("blocked weekly credential made %d requests, want 1", calls.Load())
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

type recordingWebEgressRepository struct {
	mu      sync.Mutex
	node    egressdomain.Node
	updates int
}

func (r *recordingWebEgressRepository) ListEgressNodes(_ context.Context, scope egressdomain.Scope, _ repository.SortQuery) ([]egressdomain.Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.node.Scope != scope {
		return nil, nil
	}
	return []egressdomain.Node{r.node}, nil
}

func (r *recordingWebEgressRepository) GetEgressNode(context.Context, uint64) (egressdomain.Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.node, nil
}

func (r *recordingWebEgressRepository) CreateEgressNode(context.Context, egressdomain.Node) (egressdomain.Node, error) {
	return egressdomain.Node{}, errors.New("unsupported")
}

func (r *recordingWebEgressRepository) UpdateEgressNode(_ context.Context, value egressdomain.Node) (egressdomain.Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.node = value
	r.updates++
	return value, nil
}

func (r *recordingWebEgressRepository) DeleteEgressNode(context.Context, uint64) error {
	return errors.New("unsupported")
}

func (r *recordingWebEgressRepository) UpdateCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.updates
}

func TestSyncQuotaCorrectsStoredSuperFromFreshWebQuota(t *testing.T) {
	var weeklyCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/rest/media/imagine/quota_info":
			writeEmptyImagineQuota(writer)
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

func writeEmptyImagineQuota(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "application/json")
	_, _ = writer.Write([]byte(`{"image":null,"imageEdit":null,"imagePro":null,"video":null,"video720p":null}`))
}

func TestDecodeImagineQuotaSnapshotMatchesObservedProtocol(t *testing.T) {
	now := time.Date(2026, 8, 17, 4, 0, 0, 0, time.UTC)
	videoResetAt := time.Date(2026, 8, 18, 4, 47, 49, 902572219, time.UTC)
	body := []byte(`{
		"image": null,
		"imageEdit": null,
		"imagePro": {"available":true,"remainingQueries":2,"windowSizeSeconds":86400},
		"video": null,
		"video720p": {"available":true,"remainingQueries":0,"windowSizeSeconds":86400,"nextAvailableAt":"2026-08-18T04:47:49.902572219Z"}
	}`)
	windows, err := decodeImagineQuotaSnapshot(body, 42, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 2 {
		t.Fatalf("windows = %#v", windows)
	}
	wantResetAt := now.Add(24 * time.Hour)
	if windows[0].Mode != account.QuotaModeWebImagePro || windows[0].Remaining != 2 || windows[0].Total != 0 || windows[0].ResetAt == nil || !windows[0].ResetAt.Equal(wantResetAt) {
		t.Fatalf("image_pro = %#v", windows[0])
	}
	if windows[1].Mode != account.QuotaModeWebVideo720p || windows[1].Remaining != 0 || windows[1].Total != 0 || windows[1].ResetAt == nil || !windows[1].ResetAt.Equal(videoResetAt) {
		t.Fatalf("video_720p = %#v", windows[1])
	}
}

func TestDecodeImagineQuotaSnapshotPrefersUpstreamNextAvailableAt(t *testing.T) {
	now := time.Date(2026, 8, 13, 8, 0, 0, 0, time.UTC)
	upstreamResetAt := now.Add(37 * time.Minute)
	body := []byte(`{
		"image":null,"imageEdit":null,"video":null,"video720p":null,
		"imagePro":{"available":true,"remainingQueries":4,"windowSizeSeconds":86400,"nextAvailableAt":"` + upstreamResetAt.Format(time.RFC3339) + `"}
	}`)
	windows, err := decodeImagineQuotaSnapshot(body, 42, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 1 || windows[0].ResetAt == nil || !windows[0].ResetAt.Equal(upstreamResetAt) {
		t.Fatalf("windows = %#v", windows)
	}
}

func TestDecodeImagineQuotaSnapshotRequiresCompleteResponse(t *testing.T) {
	now := time.Now().UTC()
	_, err := decodeImagineQuotaSnapshot([]byte(`{"image":null,"imageEdit":null,"imagePro":null,"video":null}`), 42, now)
	if err == nil || !strings.Contains(err.Error(), "video720p") {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeImagineQuotaSnapshotAcceptsExplicitUnavailableProduct(t *testing.T) {
	now := time.Now().UTC()
	windows, err := decodeImagineQuotaSnapshot([]byte(`{
		"image":null,"imageEdit":{"available":false},"imagePro":null,"video":null,"video720p":null
	}`), 42, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 1 || windows[0].Mode != account.QuotaModeWebImageEdit || windows[0].Remaining != 0 || windows[0].ResetAt == nil {
		t.Fatalf("windows = %#v", windows)
	}
}

func TestDecodeImagineQuotaSnapshotAcceptsAvailabilityOnlySharedWeeklyProducts(t *testing.T) {
	now := time.Now().UTC()
	windows, err := decodeImagineQuotaSnapshot([]byte(`{
		"image":{"available":true,"windowSizeSeconds":64800},
		"imagePro":{"available":true,"windowSizeSeconds":64800},
		"imageEdit":{"available":true,"windowSizeSeconds":64800},
		"video":{"available":true,"windowSizeSeconds":64800},
		"video720p":{"available":true,"windowSizeSeconds":64800}
	}`), 42, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 0 {
		t.Fatalf("availability-only products must use the shared weekly pool, got %#v", windows)
	}
}

func TestDecodeImagineQuotaSnapshotRequiresWindowForIndependentCounter(t *testing.T) {
	now := time.Now().UTC()
	_, err := decodeImagineQuotaSnapshot([]byte(`{
		"image":null,"imageEdit":null,
		"imagePro":{"available":true,"remainingQueries":2},
		"video":null,"video720p":null
	}`), 42, now)
	if err == nil || !strings.Contains(err.Error(), "imagePro") {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeImagineQuotaSnapshotRequiresWindowForAvailabilityOnlyProduct(t *testing.T) {
	now := time.Now().UTC()
	_, err := decodeImagineQuotaSnapshot([]byte(`{
		"image":null,"imageEdit":null,
		"imagePro":{"available":true},
		"video":null,"video720p":null
	}`), 42, now)
	if err == nil || !strings.Contains(err.Error(), "imagePro") {
		t.Fatalf("err = %v", err)
	}
}
