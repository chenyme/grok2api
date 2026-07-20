package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

type fallbackMarkerStub struct {
	calls atomic.Int32
	err   error
}

func (m *fallbackMarkerStub) MarkBuildAPIFallback(ctx context.Context, accountID uint64, enabled bool) error {
	m.calls.Add(1)
	return m.err
}

func TestForwardResponsePrimarySuccessDoesNotProbeFallback(t *testing.T) {
	adapter, encrypted := newFallbackTestAdapter(t)
	marker := &fallbackMarkerStub{}
	adapter.SetFallbackMarker(marker)
	var hits atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		hits.Add(1)
		if !strings.Contains(request.URL.Host, "primary.test") {
			t.Fatalf("unexpected host %s", request.URL.Host)
		}
		return jsonResponse(http.StatusOK, `{"id":"resp_1","output":[]}`, request), nil
	})
	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: account.Credential{ID: 9, EncryptedAccessToken: encrypted},
		Method:     http.MethodPost, Path: "/responses", Body: []byte(`{"model":"grok-4.5","input":"hi"}`),
		Model: "grok-4.5", NormalizeBody: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", hits.Load())
	}
	if marker.calls.Load() != 0 {
		t.Fatalf("marker calls = %d", marker.calls.Load())
	}
}

func TestForwardResponsePrimary403Fallback200Activates(t *testing.T) {
	adapter, encrypted := newFallbackTestAdapter(t)
	marker := &fallbackMarkerStub{}
	adapter.SetFallbackMarker(marker)
	var primaryHits, fallbackHits atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(request.URL.Host, "primary.test"):
			primaryHits.Add(1)
			return jsonResponse(http.StatusForbidden, `{"error":{"message":"forbidden"}}`, request), nil
		case strings.Contains(request.URL.Host, "xai.test"):
			fallbackHits.Add(1)
			return jsonResponse(http.StatusOK, `{"id":"resp_ok","output":[]}`, request), nil
		default:
			t.Fatalf("unexpected host %s", request.URL.Host)
			return nil, nil
		}
	})
	credential := account.Credential{ID: 11, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted}
	paidBilling := &account.Billing{MonthlyLimit: 100}
	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: credential, Billing: paidBilling, Method: http.MethodPost, Path: "/responses",
		Body: []byte(`{"model":"grok-4.5","input":"hi"}`), Model: "grok-4.5",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if !strings.Contains(response.UpstreamURL, "xai.test") {
		t.Fatalf("upstream = %s", response.UpstreamURL)
	}
	if primaryHits.Load() < 1 || fallbackHits.Load() != 1 {
		t.Fatalf("primary=%d fallback=%d", primaryHits.Load(), fallbackHits.Load())
	}
	if marker.calls.Load() != 1 {
		t.Fatalf("marker calls = %d", marker.calls.Load())
	}
}

func TestForwardResponseNonSuper403NeverProbesXAI(t *testing.T) {
	tests := []struct {
		name    string
		billing *account.Billing
	}{
		{name: "unknown", billing: nil},
		{name: "free", billing: &account.Billing{IsUnifiedBillingUser: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter, encrypted := newFallbackTestAdapter(t)
			marker := &fallbackMarkerStub{}
			adapter.SetFallbackMarker(marker)
			var primaryHits, fallbackHits atomic.Int32
			adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if strings.Contains(request.URL.Host, "xai.test") {
					fallbackHits.Add(1)
					t.Fatalf("non-Super account must never probe XAI")
				}
				primaryHits.Add(1)
				return jsonResponse(http.StatusForbidden, `{"error":{"message":"primary forbidden"}}`, request), nil
			})
			response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
				Credential: account.Credential{ID: 111, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted},
				Billing:    test.billing,
				Method:     http.MethodPost, Path: "/responses", Body: []byte(`{"model":"grok-4.5","input":"hi"}`), Model: "grok-4.5",
			})
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusForbidden || primaryHits.Load() != 1 || fallbackHits.Load() != 0 {
				t.Fatalf("status=%d primary=%d fallback=%d", response.StatusCode, primaryHits.Load(), fallbackHits.Load())
			}
			if marker.calls.Load() != 0 {
				t.Fatalf("mark calls = %d", marker.calls.Load())
			}
		})
	}
}

func TestForwardResponseNon403DoesNotProbeXAI(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			adapter, encrypted := newFallbackTestAdapter(t)
			marker := &fallbackMarkerStub{}
			adapter.SetFallbackMarker(marker)
			var fallbackHits atomic.Int32
			adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if strings.Contains(request.URL.Host, "xai.test") {
					fallbackHits.Add(1)
					t.Fatalf("non-403 must not probe XAI")
				}
				return jsonResponse(status, `{"error":"no"}`, request), nil
			})
			response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
				Credential: account.Credential{ID: 112, EncryptedAccessToken: encrypted},
				Method:     http.MethodPost, Path: "/responses", Body: []byte(`{"model":"grok-4.5","input":"hi"}`), Model: "grok-4.5",
			})
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != status || fallbackHits.Load() != 0 || marker.calls.Load() != 0 {
				t.Fatalf("status=%d fallback=%d marks=%d", response.StatusCode, fallbackHits.Load(), marker.calls.Load())
			}
		})
	}
}

func TestForwardResponseRouteModePrecedence(t *testing.T) {
	tests := []struct {
		name       string
		mode       account.BuildRouteMode
		botFlagged bool
		entitled   bool
		billing    *account.Billing
		wantHost   string
	}{
		{name: "entitled auto bot flagged", mode: account.BuildRouteAuto, botFlagged: true, entitled: true, wantHost: "xai.test"},
		{name: "entitled auto normal", mode: account.BuildRouteAuto, entitled: true, wantHost: "primary.test"},
		{name: "entitled force build overrides bot flag", mode: account.BuildRouteBuild, botFlagged: true, entitled: true, wantHost: "primary.test"},
		{name: "entitled force xai", mode: account.BuildRouteXAI, entitled: true, wantHost: "xai.test"},
		{name: "paid force xai", mode: account.BuildRouteXAI, billing: &account.Billing{PrepaidBalance: 1}, wantHost: "xai.test"},
		{name: "free auto bot flagged", mode: account.BuildRouteAuto, botFlagged: true, billing: &account.Billing{IsUnifiedBillingUser: true}, wantHost: "primary.test"},
		{name: "free force xai", mode: account.BuildRouteXAI, billing: &account.Billing{IsUnifiedBillingUser: true}, wantHost: "xai.test"},
		{name: "unknown auto bot flagged", mode: account.BuildRouteAuto, botFlagged: true, wantHost: "primary.test"},
		{name: "unknown force xai", mode: account.BuildRouteXAI, wantHost: "xai.test"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter, encrypted := newFallbackTestAdapter(t)
			if test.botFlagged {
				encrypted = encryptFallbackTestJWT(t, adapter, map[string]any{"bot_flag_source": 1})
			}
			var gotHost string
			adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				gotHost = request.URL.Host
				return jsonResponse(http.StatusOK, `{"id":"ok"}`, request), nil
			})
			response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
				Credential: account.Credential{ID: 113, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted, BuildRouteMode: test.mode, BuildSuperEntitled: test.entitled},
				Billing:    test.billing,
				Method:     http.MethodPost, Path: "/responses", Body: []byte(`{"model":"grok-4.5","input":"hi"}`), Model: "grok-4.5",
			})
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if gotHost != test.wantHost {
				t.Fatalf("host = %s, want %s", gotHost, test.wantHost)
			}
		})
	}
}

func TestForwardResponseForceBuildDoesNotFallbackOn403(t *testing.T) {
	adapter, encrypted := newFallbackTestAdapter(t)
	encrypted = encryptFallbackTestJWT(t, adapter, map[string]any{"bot_flag_source": 1})
	var primaryHits, fallbackHits atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if strings.Contains(request.URL.Host, "xai.test") {
			fallbackHits.Add(1)
			t.Fatalf("force Build must not use XAI")
		}
		primaryHits.Add(1)
		return jsonResponse(http.StatusForbidden, `{"error":"forbidden"}`, request), nil
	})
	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: account.Credential{ID: 114, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted, BuildRouteMode: account.BuildRouteBuild},
		Method:     http.MethodPost, Path: "/responses", Body: []byte(`{"model":"grok-4.5","input":"hi"}`), Model: "grok-4.5",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden || primaryHits.Load() != 1 || fallbackHits.Load() != 0 {
		t.Fatalf("status=%d primary=%d fallback=%d", response.StatusCode, primaryHits.Load(), fallbackHits.Load())
	}
}

func TestForwardResponsePrimary403FallbackFailKeepsPrimaryError(t *testing.T) {
	adapter, encrypted := newFallbackTestAdapter(t)
	marker := &fallbackMarkerStub{}
	adapter.SetFallbackMarker(marker)
	var primaryHits, fallbackHits atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(request.URL.Host, "xai.test"):
			fallbackHits.Add(1)
			return jsonResponse(http.StatusBadGateway, `{"error":"xai down"}`, request), nil
		case strings.Contains(request.URL.Host, "primary.test"):
			primaryHits.Add(1)
			return jsonResponse(http.StatusForbidden, `{"error":{"message":"primary forbidden"}}`, request), nil
		default:
			t.Fatalf("unexpected host %s", request.URL.Host)
			return nil, nil
		}
	})
	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: account.Credential{ID: 12, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted, BuildSuperEntitled: true},
		Method:     http.MethodPost, Path: "/responses",
		Body: []byte(`{"model":"grok-4.5","input":"hi"}`), Model: "grok-4.5",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", response.StatusCode)
	}
	body, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(body), "primary forbidden") {
		t.Fatalf("body = %s", body)
	}
	// 主 403 缓冲回放：仅一次 primary POST + 一次 fallback，不得二次 primary。
	if primaryHits.Load() != 1 {
		t.Fatalf("primary hits = %d, want 1 (no replay)", primaryHits.Load())
	}
	if fallbackHits.Load() != 1 {
		t.Fatalf("fallback hits = %d, want 1", fallbackHits.Load())
	}
	if marker.calls.Load() != 0 {
		t.Fatalf("must not mark on fallback failure, calls=%d", marker.calls.Load())
	}
}

func TestForwardResponseMarkedAccountStillUsesBuildFirst(t *testing.T) {
	adapter, encrypted := newFallbackTestAdapter(t)
	marker := &fallbackMarkerStub{}
	adapter.SetFallbackMarker(marker)
	var primaryHits, fallbackHits atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(request.URL.Host, "primary.test"):
			primaryHits.Add(1)
			return jsonResponse(http.StatusOK, `{"id":"ok"}`, request), nil
		case strings.Contains(request.URL.Host, "xai.test"):
			fallbackHits.Add(1)
			t.Fatalf("marked account must not skip a successful Build request")
			return nil, nil
		default:
			t.Fatalf("unexpected host %s", request.URL.Host)
			return nil, nil
		}
	})
	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: account.Credential{ID: 13, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted, BuildAPIFallback: true},
		Method:     http.MethodPost, Path: "/responses",
		Body: []byte(`{"model":"grok-4.5","input":"hi"}`), Model: "grok-4.5",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if primaryHits.Load() != 1 || fallbackHits.Load() != 0 {
		t.Fatalf("primary=%d fallback=%d", primaryHits.Load(), fallbackHits.Load())
	}
	if marker.calls.Load() != 0 {
		t.Fatalf("already marked account should not re-mark")
	}
}

func TestForwardResponseMarkedAccountStillNeedsCurrentBuild403(t *testing.T) {
	adapter, encrypted := newFallbackTestAdapter(t)
	adapter.SetFallbackMarker(&fallbackMarkerStub{})
	var primaryHits, fallbackHits atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(request.URL.Host, "primary.test"):
			primaryHits.Add(1)
			return jsonResponse(http.StatusForbidden, `{"error":"forbidden"}`, request), nil
		case strings.Contains(request.URL.Host, "xai.test"):
			fallbackHits.Add(1)
			if request.Method != http.MethodPost || !strings.HasSuffix(request.URL.Path, "/responses/compact") {
				t.Fatalf("unexpected %s %s", request.Method, request.URL.Path)
			}
			return jsonResponse(http.StatusOK, `{"id":"compacted"}`, request), nil
		default:
			t.Fatalf("unexpected host %s", request.URL.Host)
			return nil, nil
		}
	})
	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: account.Credential{ID: 130, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted, BuildAPIFallback: true, BuildSuperEntitled: true},
		Method:     http.MethodPost, Path: "/responses/compact",
		Body: []byte(`{"model":"grok-4.5","response_id":"resp_1"}`), Model: "grok-4.5",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if primaryHits.Load() != 1 || fallbackHits.Load() != 1 {
		t.Fatalf("primary=%d fallback=%d", primaryHits.Load(), fallbackHits.Load())
	}
}

func TestForwardResponseMarkedAccountStoredResourceStaysPrimary(t *testing.T) {
	adapter, encrypted := newFallbackTestAdapter(t)
	marker := &fallbackMarkerStub{}
	adapter.SetFallbackMarker(marker)
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/responses/resp_1"},
		{http.MethodDelete, "/responses/resp_1"},
	} {
		t.Run(tc.method, func(t *testing.T) {
			var primaryHits, fallbackHits atomic.Int32
			adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				switch {
				case strings.Contains(request.URL.Host, "primary.test"):
					primaryHits.Add(1)
					return jsonResponse(http.StatusOK, `{"id":"resp_1"}`, request), nil
				case strings.Contains(request.URL.Host, "xai.test"):
					fallbackHits.Add(1)
					t.Fatalf("stored resource must not hit XAI")
					return nil, nil
				default:
					t.Fatalf("unexpected host %s", request.URL.Host)
					return nil, nil
				}
			})
			response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
				Credential: account.Credential{ID: 131, EncryptedAccessToken: encrypted, BuildAPIFallback: true},
				Method:     tc.method, Path: tc.path,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if primaryHits.Load() != 1 || fallbackHits.Load() != 0 {
				t.Fatalf("primary=%d fallback=%d", primaryHits.Load(), fallbackHits.Load())
			}
			if marker.calls.Load() != 0 {
				t.Fatalf("marker calls = %d", marker.calls.Load())
			}
		})
	}
}

func TestForwardResponseUnmarkedStoredResource403DoesNotProbe(t *testing.T) {
	adapter, encrypted := newFallbackTestAdapter(t)
	marker := &fallbackMarkerStub{}
	adapter.SetFallbackMarker(marker)
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/responses/resp_1"},
		{http.MethodDelete, "/responses/resp_1"},
	} {
		t.Run(tc.method, func(t *testing.T) {
			var primaryHits, fallbackHits atomic.Int32
			adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				switch {
				case strings.Contains(request.URL.Host, "primary.test"):
					primaryHits.Add(1)
					return jsonResponse(http.StatusForbidden, `{"error":{"message":"primary forbidden"}}`, request), nil
				case strings.Contains(request.URL.Host, "xai.test"):
					fallbackHits.Add(1)
					return jsonResponse(http.StatusOK, `{"id":"should-not"}`, request), nil
				default:
					t.Fatalf("unexpected host %s", request.URL.Host)
					return nil, nil
				}
			})
			response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
				Credential: account.Credential{ID: 132, EncryptedAccessToken: encrypted},
				Method:     tc.method, Path: tc.path,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", response.StatusCode)
			}
			if primaryHits.Load() != 1 || fallbackHits.Load() != 0 {
				t.Fatalf("primary=%d fallback=%d", primaryHits.Load(), fallbackHits.Load())
			}
			if marker.calls.Load() != 0 {
				t.Fatalf("must not mark, calls=%d", marker.calls.Load())
			}
		})
	}
}

func TestGetBillingAlwaysPrimaryEvenWhenMarked(t *testing.T) {
	adapter, encrypted := newFallbackTestAdapter(t)
	marker := &fallbackMarkerStub{}
	adapter.SetFallbackMarker(marker)
	var primaryHits, fallbackHits atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(request.URL.Host, "primary.test"):
			primaryHits.Add(1)
			if strings.HasSuffix(request.URL.Path, "/user") {
				return jsonResponse(http.StatusNotFound, `{"error":"subscription unavailable"}`, request), nil
			}
			if !strings.Contains(request.URL.Path, "/billing") {
				t.Fatalf("path = %s", request.URL.Path)
			}
			// 上游 v3.0.2 Billing 仅请求 format=credits，且始终主地址。
			if request.URL.RawQuery != "format=credits" {
				t.Fatalf("query = %q, want format=credits", request.URL.RawQuery)
			}
			return jsonResponse(http.StatusOK, `{"config":{"onDemandCap":{"val":10},"onDemandUsed":{"val":1},"monthlyLimit":{"val":100},"used":{"val":5}}}`, request), nil
		case strings.Contains(request.URL.Host, "xai.test"):
			fallbackHits.Add(1)
			t.Fatalf("billing must never hit XAI")
			return nil, nil
		default:
			t.Fatalf("unexpected host %s", request.URL.Host)
			return nil, nil
		}
	})
	billing, err := adapter.GetBilling(context.Background(), account.Credential{
		ID: 140, EncryptedAccessToken: encrypted, BuildAPIFallback: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if billing.OnDemandCap != 10 || billing.OnDemandUsed != 1 {
		t.Fatalf("billing = %+v", billing)
	}
	if primaryHits.Load() != 2 || fallbackHits.Load() != 0 {
		t.Fatalf("primary=%d fallback=%d", primaryHits.Load(), fallbackHits.Load())
	}
	if marker.calls.Load() != 0 {
		t.Fatalf("marker calls = %d", marker.calls.Load())
	}
}

func TestGetBillingPrimary403NeverProbesXAI(t *testing.T) {
	adapter, encrypted := newFallbackTestAdapter(t)
	marker := &fallbackMarkerStub{}
	adapter.SetFallbackMarker(marker)
	var primaryHits, fallbackHits atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(request.URL.Host, "primary.test"):
			primaryHits.Add(1)
			return jsonResponse(http.StatusForbidden, `{"error":"billing forbidden"}`, request), nil
		case strings.Contains(request.URL.Host, "xai.test"):
			fallbackHits.Add(1)
			return jsonResponse(http.StatusOK, `{"config":{"monthlyLimit":{"val":1}}}`, request), nil
		default:
			t.Fatalf("unexpected host %s", request.URL.Host)
			return nil, nil
		}
	})
	_, err := adapter.GetBilling(context.Background(), account.Credential{
		ID: 141, EncryptedAccessToken: encrypted, BuildAPIFallback: false,
	})
	if err == nil {
		t.Fatal("expected billing error")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v", err)
	}
	if primaryHits.Load() != 1 || fallbackHits.Load() != 0 {
		t.Fatalf("primary=%d fallback=%d", primaryHits.Load(), fallbackHits.Load())
	}
	if marker.calls.Load() != 0 {
		t.Fatalf("must not mark from billing 403, calls=%d", marker.calls.Load())
	}
}

func TestIsXAIInferenceFallbackCapable(t *testing.T) {
	capable := []struct{ method, path string }{
		{http.MethodPost, "/responses"},
		{http.MethodPost, "/responses/compact"},
		{http.MethodPost, "responses"},
		{http.MethodPost, "/videos/generations"},
		{http.MethodGet, "/videos/job_1"},
	}
	for _, tc := range capable {
		if !isXAIInferenceFallbackCapable(tc.method, tc.path) {
			t.Fatalf("want capable: %s %s", tc.method, tc.path)
		}
	}
	notCapable := []struct{ method, path string }{
		{http.MethodGet, "/models"},
		{http.MethodGet, "/responses/resp_1"},
		{http.MethodDelete, "/responses/resp_1"},
		{http.MethodGet, "/billing"},
		{http.MethodGet, "/billing?format=credits"},
		{http.MethodPost, "/unknown"},
		{http.MethodGet, "/responses"},
	}
	for _, tc := range notCapable {
		if isXAIInferenceFallbackCapable(tc.method, tc.path) {
			t.Fatalf("want primary-only: %s %s", tc.method, tc.path)
		}
	}
}

func TestListModelsAlwaysUsesBuildPrimary(t *testing.T) {
	for _, marked := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallback_marked=%t", marked), func(t *testing.T) {
			adapter, encrypted := newFallbackTestAdapter(t)
			marker := &fallbackMarkerStub{}
			adapter.SetFallbackMarker(marker)
			var primaryHits, fallbackHits atomic.Int32
			adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if strings.Contains(request.URL.Host, "xai.test") {
					fallbackHits.Add(1)
					t.Fatalf("model catalog must never hit XAI")
				}
				primaryHits.Add(1)
				return jsonResponse(http.StatusOK, `{"data":[{"id":"grok-4.5"}]}`, request), nil
			})
			models, err := adapter.ListModels(context.Background(), account.Credential{
				ID: 14, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted,
				BuildAPIFallback: marked, BuildSuperEntitled: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(models) != 1 || models[0] != "grok-4.5" {
				t.Fatalf("models = %#v", models)
			}
			if primaryHits.Load() != 1 || fallbackHits.Load() != 0 || marker.calls.Load() != 0 {
				t.Fatalf("primary=%d fallback=%d marks=%d", primaryHits.Load(), fallbackHits.Load(), marker.calls.Load())
			}
		})
	}
}

func TestListModels403NeverProbesXAI(t *testing.T) {
	adapter, encrypted := newFallbackTestAdapter(t)
	marker := &fallbackMarkerStub{}
	adapter.SetFallbackMarker(marker)
	var primaryHits, fallbackHits atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if strings.Contains(request.URL.Host, "xai.test") {
			fallbackHits.Add(1)
			t.Fatalf("model catalog 403 must not probe XAI")
		}
		primaryHits.Add(1)
		return jsonResponse(http.StatusForbidden, `{}`, request), nil
	})
	_, err := adapter.ListModels(context.Background(), account.Credential{
		ID: 114, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted, BuildAPIFallback: true, BuildSuperEntitled: true,
	})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v", err)
	}
	if primaryHits.Load() != 1 || fallbackHits.Load() != 0 || marker.calls.Load() != 0 {
		t.Fatalf("primary=%d fallback=%d marks=%d", primaryHits.Load(), fallbackHits.Load(), marker.calls.Load())
	}
}

func TestGenerateVideoFallbackInjectsUploadURL(t *testing.T) {
	adapter, encrypted := newFallbackTestAdapter(t)
	marker := &fallbackMarkerStub{}
	adapter.SetFallbackMarker(marker)
	issuer := &uploadIssuerStub{url: "https://public.example/v1/media/uploads/aabb", assetID: "vid_test"}
	adapter.SetVideoUploadIssuer(issuer)
	var createPayload map[string]any
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if strings.Contains(request.URL.Host, "xai.test") && request.Header.Get("User-Agent") != "xai-grok-build/0.2.99" {
			t.Fatalf("XAI video user agent = %q", request.Header.Get("User-Agent"))
		}
		if request.Method == http.MethodPost {
			if strings.Contains(request.URL.Host, "primary.test") {
				return jsonResponse(http.StatusForbidden, `{"error":"forbidden"}`, request), nil
			}
			if request.Header.Get("x-grok-model-override") != xaiVideoModel {
				t.Fatalf("XAI model override = %q", request.Header.Get("x-grok-model-override"))
			}
			_ = json.NewDecoder(request.Body).Decode(&createPayload)
			return jsonResponse(http.StatusOK, `{"request_id":"job_1"}`, request), nil
		}
		return jsonResponse(http.StatusOK, `{"status":"done"}`, request), nil
	})
	result, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: account.Credential{ID: 15, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted, BuildSuperEntitled: true},
		JobID:      "video_job_1", Prompt: "waves", Duration: 6, Resolution: "720p", ReferenceURLs: []string{"https://cdn.example.com/first.png"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.AssetID != "vid_test" {
		t.Fatalf("asset = %s", result.AssetID)
	}
	output, _ := createPayload["output"].(map[string]any)
	if output["upload_url"] != issuer.url {
		t.Fatalf("payload = %#v", createPayload)
	}
	image, _ := createPayload["image"].(map[string]any)
	if createPayload["model"] != xaiVideoModel || image["url"] != "https://cdn.example.com/first.png" || image["image_url"] != nil {
		t.Fatalf("XAI fallback payload = %#v", createPayload)
	}
	if marker.calls.Load() != 1 {
		t.Fatalf("marker = %d", marker.calls.Load())
	}
}

func TestGenerateVideoMarkedStillUsesBuildFirst(t *testing.T) {
	adapter, encrypted := newFallbackTestAdapter(t)
	marker := &fallbackMarkerStub{}
	adapter.SetFallbackMarker(marker)
	var primaryHits, fallbackHits atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if strings.Contains(request.URL.Host, "xai.test") {
			fallbackHits.Add(1)
			t.Fatalf("marked video must not skip a successful Build request")
		}
		primaryHits.Add(1)
		if request.Method == http.MethodPost {
			return jsonResponse(http.StatusOK, `{"request_id":"job_marked"}`, request), nil
		}
		return jsonResponse(http.StatusOK, `{"status":"done","url":"https://assets.grok.com/video.mp4"}`, request), nil
	})
	result, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: account.Credential{ID: 115, EncryptedAccessToken: encrypted, BuildAPIFallback: true},
		JobID:      "video_marked", Prompt: "waves", Duration: 6, Resolution: "720p",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.URL != "https://assets.grok.com/video.mp4" {
		t.Fatalf("result = %#v", result)
	}
	if primaryHits.Load() < 2 || fallbackHits.Load() != 0 || marker.calls.Load() != 0 {
		t.Fatalf("primary=%d fallback=%d marks=%d", primaryHits.Load(), fallbackHits.Load(), marker.calls.Load())
	}
}

func TestGenerateVideoAutoBotFlaggedUsesXAIDirectly(t *testing.T) {
	adapter, _ := newFallbackTestAdapter(t)
	encrypted := encryptFallbackTestJWT(t, adapter, map[string]any{"bot_flag_source": 1})
	issuer := &uploadIssuerStub{url: "https://public.example/v1/media/uploads/bot1", assetID: "vid_bot"}
	adapter.SetVideoUploadIssuer(issuer)
	var primaryHits, fallbackHits atomic.Int32
	var createPayload map[string]any
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if strings.Contains(request.URL.Host, "primary.test") {
			primaryHits.Add(1)
			t.Fatalf("bot-flagged auto video must default to XAI")
		}
		fallbackHits.Add(1)
		if request.Header.Get("User-Agent") != "xai-grok-build/0.2.99" {
			t.Fatalf("XAI video user agent = %q", request.Header.Get("User-Agent"))
		}
		if request.Method == http.MethodPost {
			if request.Header.Get("x-grok-model-override") != xaiVideoModel {
				t.Fatalf("XAI model override = %q", request.Header.Get("x-grok-model-override"))
			}
			if err := json.NewDecoder(request.Body).Decode(&createPayload); err != nil {
				t.Fatal(err)
			}
			return jsonResponse(http.StatusOK, `{"request_id":"job_bot"}`, request), nil
		}
		return jsonResponse(http.StatusOK, `{"status":"done"}`, request), nil
	})
	result, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: account.Credential{ID: 116, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted, BuildRouteMode: account.BuildRouteAuto, BuildSuperEntitled: true},
		JobID:      "video_bot", Prompt: "waves", Duration: 6, Resolution: "720p", ReferenceURLs: []string{"https://cdn.example.com/bot.png"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.AssetID != "vid_bot" || primaryHits.Load() != 0 || fallbackHits.Load() < 2 {
		t.Fatalf("result=%#v primary=%d fallback=%d", result, primaryHits.Load(), fallbackHits.Load())
	}
	image, _ := createPayload["image"].(map[string]any)
	if createPayload["model"] != xaiVideoModel || image["url"] != "https://cdn.example.com/bot.png" || image["image_url"] != nil {
		t.Fatalf("XAI direct payload = %#v", createPayload)
	}
}

func TestGenerateVideoNonSuperAutoNeverUsesXAI(t *testing.T) {
	tests := []struct {
		name       string
		mode       account.BuildRouteMode
		botFlagged bool
		billing    *account.Billing
	}{
		{name: "free auto bot flagged", mode: account.BuildRouteAuto, botFlagged: true, billing: &account.Billing{IsUnifiedBillingUser: true}},
		{name: "unknown auto bot flagged", mode: account.BuildRouteAuto, botFlagged: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter, encrypted := newFallbackTestAdapter(t)
			if test.botFlagged {
				encrypted = encryptFallbackTestJWT(t, adapter, map[string]any{"bot_flag_source": 1})
			}
			var primaryHits, fallbackHits atomic.Int32
			adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if strings.Contains(request.URL.Host, "xai.test") {
					fallbackHits.Add(1)
					t.Fatalf("non-Super video must never use XAI")
				}
				primaryHits.Add(1)
				return jsonResponse(http.StatusForbidden, `{"error":"forbidden"}`, request), nil
			})
			_, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
				Credential: account.Credential{ID: 117, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted, BuildRouteMode: test.mode},
				Billing:    test.billing,
				JobID:      "video_non_super", Prompt: "waves", Duration: 6, Resolution: "720p",
			})
			status, ok := provider.ErrorHTTPStatus(err)
			if !ok || status != http.StatusForbidden {
				t.Fatalf("err=%v status=%d ok=%t", err, status, ok)
			}
			if primaryHits.Load() != 1 || fallbackHits.Load() != 0 {
				t.Fatalf("primary=%d fallback=%d", primaryHits.Load(), fallbackHits.Load())
			}
		})
	}
}

func TestGenerateVideoForceXAIOverridesNonSuper(t *testing.T) {
	tests := []struct {
		name    string
		billing *account.Billing
	}{
		{name: "free", billing: &account.Billing{IsUnifiedBillingUser: true}},
		{name: "unknown"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter, encrypted := newFallbackTestAdapter(t)
			adapter.SetVideoUploadIssuer(&uploadIssuerStub{url: "https://public.example/v1/media/uploads/forced", assetID: "vid_forced"})
			var primaryHits, fallbackHits atomic.Int32
			adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if strings.Contains(request.URL.Host, "primary.test") {
					primaryHits.Add(1)
					t.Fatalf("force XAI must override account tier")
				}
				fallbackHits.Add(1)
				if request.Method == http.MethodPost {
					return jsonResponse(http.StatusOK, `{"request_id":"job_forced"}`, request), nil
				}
				return jsonResponse(http.StatusOK, `{"status":"done"}`, request), nil
			})
			result, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
				Credential: account.Credential{ID: 118, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted, BuildRouteMode: account.BuildRouteXAI},
				Billing:    test.billing,
				JobID:      "video_forced", Prompt: "waves", Duration: 6, Resolution: "720p",
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.AssetID != "vid_forced" || primaryHits.Load() != 0 || fallbackHits.Load() < 2 {
				t.Fatalf("result=%#v primary=%d fallback=%d", result, primaryHits.Load(), fallbackHits.Load())
			}
		})
	}
}

func TestGenerateVideoFallbackMalformedJobIDDoesNotActivate(t *testing.T) {
	adapter, encrypted := newFallbackTestAdapter(t)
	// 追踪本地置位：activateBuildAPIFallback 在写库前会先把 credential.BuildAPIFallback 设为 true。
	tracking := &trackingFallbackMarker{}
	adapter.SetFallbackMarker(tracking)
	issuer := &uploadIssuerStub{url: "https://public.example/v1/media/uploads/ccdd", assetID: "vid_bad"}
	adapter.SetVideoUploadIssuer(issuer)
	var primaryHits, fallbackHits atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost {
			t.Fatalf("unexpected method %s", request.Method)
		}
		if strings.Contains(request.URL.Host, "primary.test") {
			primaryHits.Add(1)
			return jsonResponse(http.StatusForbidden, `{"error":"forbidden"}`, request), nil
		}
		fallbackHits.Add(1)
		// 2xx 但缺少 request_id / id：不得激活降级。
		return jsonResponse(http.StatusOK, `{"status":"queued","message":"accepted"}`, request), nil
	})
	cred := account.Credential{ID: 16, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted, BuildAPIFallback: false, BuildSuperEntitled: true}
	_, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: cred, JobID: "video_job_bad", Prompt: "waves", Duration: 6, Resolution: "720p",
	})
	if err == nil {
		t.Fatal("expected parse error for missing job id")
	}
	if !strings.Contains(err.Error(), "request_id") {
		t.Fatalf("err = %v", err)
	}
	if primaryHits.Load() != 1 || fallbackHits.Load() != 1 {
		t.Fatalf("primary=%d fallback=%d", primaryHits.Load(), fallbackHits.Load())
	}
	if tracking.calls.Load() != 0 {
		t.Fatalf("must not persist fallback mark, calls=%d", tracking.calls.Load())
	}
	if tracking.activateSeen.Load() {
		t.Fatal("must not call activate/local-set path on malformed create response")
	}
	if cred.BuildAPIFallback {
		t.Fatal("caller credential must remain unmarked")
	}
}

// trackingFallbackMarker 记录 Mark 调用；activate 路径必定会调用 Mark（marker 非 nil）。
type trackingFallbackMarker struct {
	calls        atomic.Int32
	activateSeen atomic.Bool
	err          error
}

func (m *trackingFallbackMarker) MarkBuildAPIFallback(ctx context.Context, accountID uint64, enabled bool) error {
	m.calls.Add(1)
	if enabled {
		m.activateSeen.Store(true)
	}
	return m.err
}

type uploadIssuerStub struct {
	url, assetID string
	waitCalls    atomic.Int32
}

func (u *uploadIssuerStub) IssueVideoUpload(ctx context.Context, jobID string) (string, string, error) {
	return u.url, u.assetID, nil
}

func (u *uploadIssuerStub) WaitVideoUpload(ctx context.Context, assetID string) (string, error) {
	u.waitCalls.Add(1)
	return "video/mp4", nil
}

func newFallbackTestAdapter(t *testing.T) (*Adapter, string) {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{
		BaseURL: "https://primary.test/v1", FallbackBaseURL: "https://xai.test/v1",
		ClientVersion: "0.2.99", ClientIdentifier: "grok-shell", TokenAuth: "xai-grok-cli",
		UserAgent: "test-agent",
	}, cipher)
	return adapter, encrypted
}

func encryptFallbackTestJWT(t *testing.T, adapter *Adapter, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	token := "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
	encrypted, err := adapter.cipher.Encrypt(token)
	if err != nil {
		t.Fatal(err)
	}
	return encrypted
}

func TestFallbackMarkerFailureStillReturnsSuccess(t *testing.T) {
	adapter, encrypted := newFallbackTestAdapter(t)
	marker := &fallbackMarkerStub{err: context.DeadlineExceeded}
	adapter.SetFallbackMarker(marker)
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if strings.Contains(request.URL.Host, "primary.test") {
			return jsonResponse(http.StatusForbidden, `{}`, request), nil
		}
		return jsonResponse(http.StatusOK, `{"id":"ok"}`, request), nil
	})
	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: account.Credential{ID: 99, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted, BuildSuperEntitled: true},
		Method:     http.MethodPost, Path: "/responses", Body: []byte(`{"model":"x","input":"y"}`), Model: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if marker.calls.Load() != 1 {
		t.Fatalf("marker should still be attempted")
	}
}
