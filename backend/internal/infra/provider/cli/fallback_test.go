package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	calls       atomic.Int32
	policyCalls atomic.Int32
	err         error
	policyErr   error
	denied      bool
}

func (m *fallbackMarkerStub) CanUseBuildAPIFallback(ctx context.Context, accountID uint64) (bool, error) {
	m.policyCalls.Add(1)
	if m.policyErr != nil {
		return false, m.policyErr
	}
	return !m.denied, nil
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
	credential := account.Credential{ID: 11, EncryptedAccessToken: encrypted}
	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: credential, Method: http.MethodPost, Path: "/responses",
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

func TestForwardResponseFree403NeverUsesXAI(t *testing.T) {
	for _, tc := range []struct {
		name   string
		marked bool
		marker *fallbackMarkerStub
	}{
		{name: "unmarked free", marker: &fallbackMarkerStub{denied: true}},
		{name: "marked free", marked: true, marker: &fallbackMarkerStub{denied: true}},
		{name: "policy lookup failure", marker: &fallbackMarkerStub{policyErr: context.DeadlineExceeded}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter, encrypted := newFallbackTestAdapter(t)
			adapter.SetFallbackMarker(tc.marker)
			var primaryHits, fallbackHits atomic.Int32
			adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if strings.Contains(request.URL.Host, "xai.test") {
					fallbackHits.Add(1)
					t.Fatalf("Free account must never hit XAI")
				}
				primaryHits.Add(1)
				return jsonResponse(http.StatusForbidden, `{"error":{"message":"primary forbidden"}}`, request), nil
			})
			response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
				Credential: account.Credential{ID: 111, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted, BuildAPIFallback: tc.marked},
				Method:     http.MethodPost, Path: "/responses", Body: []byte(`{"model":"grok-4.5","input":"hi"}`), Model: "grok-4.5",
			})
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusForbidden || primaryHits.Load() != 1 || fallbackHits.Load() != 0 {
				t.Fatalf("status=%d primary=%d fallback=%d", response.StatusCode, primaryHits.Load(), fallbackHits.Load())
			}
			if tc.marker.policyCalls.Load() != 1 || tc.marker.calls.Load() != 0 {
				t.Fatalf("policy=%d mark=%d", tc.marker.policyCalls.Load(), tc.marker.calls.Load())
			}
		})
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
		Credential: account.Credential{ID: 12, EncryptedAccessToken: encrypted},
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

func TestForwardResponseMarkedAccountCreateUsesFallbackDirectly(t *testing.T) {
	adapter, encrypted := newFallbackTestAdapter(t)
	marker := &fallbackMarkerStub{}
	adapter.SetFallbackMarker(marker)
	var primaryHits, fallbackHits atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(request.URL.Host, "primary.test"):
			primaryHits.Add(1)
			t.Fatalf("marked create must not hit primary, path=%s", request.URL.Path)
			return nil, nil
		case strings.Contains(request.URL.Host, "xai.test"):
			fallbackHits.Add(1)
			if request.Method != http.MethodPost || !strings.HasSuffix(request.URL.Path, "/responses") {
				t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
			}
			return jsonResponse(http.StatusOK, `{"id":"ok"}`, request), nil
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
	if primaryHits.Load() != 0 || fallbackHits.Load() != 1 {
		t.Fatalf("primary=%d fallback=%d", primaryHits.Load(), fallbackHits.Load())
	}
	if marker.calls.Load() != 0 {
		t.Fatalf("already marked account should not re-mark")
	}
}

func TestForwardResponseMarkedAccountCompactUsesFallbackDirectly(t *testing.T) {
	adapter, encrypted := newFallbackTestAdapter(t)
	adapter.SetFallbackMarker(&fallbackMarkerStub{})
	var primaryHits, fallbackHits atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(request.URL.Host, "primary.test"):
			primaryHits.Add(1)
			t.Fatalf("marked compact must not hit primary")
			return nil, nil
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
		Credential: account.Credential{ID: 130, EncryptedAccessToken: encrypted, BuildAPIFallback: true},
		Method:     http.MethodPost, Path: "/responses/compact",
		Body: []byte(`{"model":"grok-4.5","response_id":"resp_1"}`), Model: "grok-4.5",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if primaryHits.Load() != 0 || fallbackHits.Load() != 1 {
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
		{http.MethodGet, "/models"},
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

func TestListModelsSuperUsesUnifiedXAICatalog(t *testing.T) {
	for _, marked := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallback_marked=%t", marked), func(t *testing.T) {
			adapter, encrypted := newFallbackTestAdapter(t)
			marker := &fallbackMarkerStub{}
			adapter.SetFallbackMarker(marker)
			var primaryHits, fallbackHits atomic.Int32
			adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if strings.Contains(request.URL.Host, "primary.test") {
					primaryHits.Add(1)
					t.Fatalf("Super model catalog must not depend on the primary Build endpoint")
				}
				fallbackHits.Add(1)
				return jsonResponse(http.StatusOK, `{"data":[{"id":"grok-4.5"},{"id":"grok-imagine-video"}]}`, request), nil
			})
			models, err := adapter.ListModels(context.Background(), account.Credential{ID: 14, EncryptedAccessToken: encrypted, BuildAPIFallback: marked})
			if err != nil {
				t.Fatal(err)
			}
			if len(models) != 2 || models[0] != "grok-4.5" || models[1] != "grok-imagine-video" {
				t.Fatalf("models = %#v", models)
			}
			if primaryHits.Load() != 0 || fallbackHits.Load() != 1 || marker.policyCalls.Load() != 1 {
				t.Fatalf("primary=%d fallback=%d policy=%d", primaryHits.Load(), fallbackHits.Load(), marker.policyCalls.Load())
			}
			if marker.calls.Load() != 0 {
				t.Fatalf("catalog sync must not change fallback state, calls=%d", marker.calls.Load())
			}
		})
	}
}

func TestListModelsSuperAcceptsPrimaryWhenXAICatalogIsForbidden(t *testing.T) {
	adapter, encrypted := newFallbackTestAdapter(t)
	marker := &fallbackMarkerStub{}
	adapter.SetFallbackMarker(marker)
	var primaryHits, fallbackHits atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if strings.Contains(request.URL.Host, "xai.test") {
			fallbackHits.Add(1)
			return jsonResponse(http.StatusForbidden, `{}`, request), nil
		}
		primaryHits.Add(1)
		return jsonResponse(http.StatusOK, `{"data":[{"id":"grok-4.5"}]}`, request), nil
	})
	models, err := adapter.ListModels(context.Background(), account.Credential{ID: 15, EncryptedAccessToken: encrypted})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0] != "grok-4.5" {
		t.Fatalf("models = %#v", models)
	}
	if primaryHits.Load() != 1 || fallbackHits.Load() != 1 || marker.policyCalls.Load() != 1 || marker.calls.Load() != 0 {
		t.Fatalf("primary=%d fallback=%d policy=%d marks=%d", primaryHits.Load(), fallbackHits.Load(), marker.policyCalls.Load(), marker.calls.Load())
	}
}

func TestListModelsFree403NeverUsesXAI(t *testing.T) {
	adapter, encrypted := newFallbackTestAdapter(t)
	marker := &fallbackMarkerStub{denied: true}
	adapter.SetFallbackMarker(marker)
	var primaryHits, fallbackHits atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if strings.Contains(request.URL.Host, "xai.test") {
			fallbackHits.Add(1)
			t.Fatalf("Free account models must never hit XAI")
		}
		primaryHits.Add(1)
		return jsonResponse(http.StatusForbidden, `{}`, request), nil
	})
	_, err := adapter.ListModels(context.Background(), account.Credential{ID: 114, EncryptedAccessToken: encrypted, BuildAPIFallback: true})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v", err)
	}
	if primaryHits.Load() != 1 || fallbackHits.Load() != 0 || marker.policyCalls.Load() != 1 {
		t.Fatalf("primary=%d fallback=%d policy=%d", primaryHits.Load(), fallbackHits.Load(), marker.policyCalls.Load())
	}
}

func TestListModelsPolicyFailureDoesNotUsePartialPrimaryCatalog(t *testing.T) {
	adapter, encrypted := newFallbackTestAdapter(t)
	marker := &fallbackMarkerStub{policyErr: context.DeadlineExceeded}
	adapter.SetFallbackMarker(marker)
	var hits atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		hits.Add(1)
		t.Fatalf("policy failure must preserve the previous catalog without an upstream request")
		return nil, nil
	})
	_, err := adapter.ListModels(context.Background(), account.Credential{ID: 115, EncryptedAccessToken: encrypted})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if hits.Load() != 0 || marker.policyCalls.Load() != 1 {
		t.Fatalf("hits=%d policy=%d", hits.Load(), marker.policyCalls.Load())
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
		if request.Method == http.MethodPost {
			if strings.Contains(request.URL.Host, "primary.test") {
				return jsonResponse(http.StatusForbidden, `{"error":"forbidden"}`, request), nil
			}
			_ = json.NewDecoder(request.Body).Decode(&createPayload)
			return jsonResponse(http.StatusOK, `{"request_id":"job_1"}`, request), nil
		}
		return jsonResponse(http.StatusOK, `{"status":"done"}`, request), nil
	})
	result, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: account.Credential{ID: 15, EncryptedAccessToken: encrypted},
		JobID:      "video_job_1", Prompt: "waves", Duration: 6, Resolution: "720p",
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
	if marker.calls.Load() != 1 {
		t.Fatalf("marker = %d", marker.calls.Load())
	}
}

func TestGenerateVideoFree403NeverUsesXAI(t *testing.T) {
	adapter, encrypted := newFallbackTestAdapter(t)
	marker := &fallbackMarkerStub{denied: true}
	adapter.SetFallbackMarker(marker)
	var primaryHits, fallbackHits atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if strings.Contains(request.URL.Host, "xai.test") {
			fallbackHits.Add(1)
			t.Fatalf("Free account video must never hit XAI")
		}
		primaryHits.Add(1)
		return jsonResponse(http.StatusForbidden, `{"error":"forbidden"}`, request), nil
	})
	_, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: account.Credential{ID: 115, EncryptedAccessToken: encrypted, BuildAPIFallback: true},
		JobID:      "video_free", Prompt: "waves", Duration: 6, Resolution: "720p",
	})
	if err == nil {
		t.Fatal("expected primary 403")
	}
	status, ok := provider.ErrorHTTPStatus(err)
	if !ok || status != http.StatusForbidden {
		t.Fatalf("status=%d ok=%v err=%v", status, ok, err)
	}
	if primaryHits.Load() != 1 || fallbackHits.Load() != 0 || marker.policyCalls.Load() != 1 {
		t.Fatalf("primary=%d fallback=%d policy=%d", primaryHits.Load(), fallbackHits.Load(), marker.policyCalls.Load())
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
	cred := account.Credential{ID: 16, EncryptedAccessToken: encrypted, BuildAPIFallback: false}
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

func (m *trackingFallbackMarker) CanUseBuildAPIFallback(ctx context.Context, accountID uint64) (bool, error) {
	return true, nil
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
		Credential: account.Credential{ID: 99, EncryptedAccessToken: encrypted},
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
