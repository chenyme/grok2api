package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestStripReasoningEncryptedContentPreservesOnlyPortableHistory(t *testing.T) {
	body := []byte(`{
		"input":[
			{"type":"reasoning","id":"rs_empty","status":"completed","summary":[],"encrypted_content":"opaque-empty"},
			{"type":"reasoning","summary":[{"type":"summary_text","text":""}],"encrypted_content":"opaque-blank"},
			{"type":"reasoning","id":"rs_summary","status":"completed","summary":[{"type":"summary_text","text":"readable"}],"encrypted_content":"opaque-summary"},
			{"type":"message","role":"assistant","content":"answer","encrypted_content":"message-value"},
			{"type":"message","role":"user","content":"continue"}
		]
	}`)
	downgraded, changed := stripReasoningEncryptedContent(body)
	if !changed {
		t.Fatal("expected encrypted reasoning downgrade")
	}
	var payload struct {
		Input []map[string]any `json:"input"`
	}
	if json.Unmarshal(downgraded, &payload) != nil || len(payload.Input) != 3 {
		t.Fatalf("downgraded = %s", downgraded)
	}
	reasoning := payload.Input[0]
	if reasoning["type"] != "reasoning" || reasoning["id"] != nil || reasoning["status"] != nil || reasoning["encrypted_content"] != nil {
		t.Fatalf("reasoning = %#v", reasoning)
	}
	if payload.Input[1]["encrypted_content"] != "message-value" {
		t.Fatalf("non-reasoning encrypted content changed: %#v", payload.Input[1])
	}
}

func TestRecoverReasoningDecodeFailureRetriesSameUpstreamOnce(t *testing.T) {
	adapter, encrypted := newReasoningRecoveryTestAdapter(t)
	var calls atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		call := calls.Add(1)
		data, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if request.URL.String() != "https://build.test/v1/responses" || request.Header.Get("Authorization") != "Bearer access-token" {
			t.Fatalf("request = %s headers=%#v", request.URL, request.Header)
		}
		if call == 1 {
			if request.Header.Get("Idempotency-Key") != "original-id" {
				t.Fatalf("first idempotency key = %q", request.Header.Get("Idempotency-Key"))
			}
			if !strings.Contains(string(data), `"encrypted_content":"opaque"`) || !strings.Contains(string(data), `"summary":[]`) {
				t.Fatalf("first body = %s", data)
			}
			return jsonHTTPResponse(request, http.StatusBadRequest, `{"error":"Could not decrypt the provided encrypted_content. Ensure the value is unmodified."}`), nil
		}
		if request.Header.Get("Idempotency-Key") == "" || request.Header.Get("Idempotency-Key") == "original-id" {
			t.Fatalf("retry idempotency key = %q", request.Header.Get("Idempotency-Key"))
		}
		var retryPayload struct {
			Input []map[string]any `json:"input"`
		}
		if json.Unmarshal(data, &retryPayload) != nil {
			t.Fatalf("retry body = %s", data)
		}
		for _, item := range retryPayload.Input {
			if item["type"] == "reasoning" || item["encrypted_content"] != nil {
				t.Fatalf("retry input = %#v", retryPayload.Input)
			}
		}
		return jsonHTTPResponse(request, http.StatusOK, `{"id":"resp_ok","status":"completed","output":[]}`), nil
	})

	response, err := adapter.ForwardResponse(t.Context(), provider.ResponseResourceRequest{
		Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted},
		Method:     http.MethodPost, Path: "/responses", Model: "grok-4.5", PromptCacheKey: "session-1",
		IdempotencyID: "original-id",
		Body:          []byte(`{"model":"public","max_tokens":1024,"thinking":{"type":"enabled","budget_tokens":512},"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"opaque"}]},{"role":"user","content":"continue"}]}`),
		NormalizeBody: true, Operation: conversation.OperationMessages,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if calls.Load() != 2 || response.StatusCode != http.StatusOK || !strings.Contains(response.Header.Get("X-Grok2API-Compatibility-Warnings"), "reasoning_encrypted_content_downgraded") {
		t.Fatalf("calls=%d status=%d headers=%#v", calls.Load(), response.StatusCode, response.Header)
	}
	data, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(data), `"type":"message"`) {
		t.Fatalf("converted response = %s", data)
	}
}

func TestRecoverReasoningDecodeFailureDoesNotRetryOtherBadRequests(t *testing.T) {
	adapter, encrypted := newReasoningRecoveryTestAdapter(t)
	var calls atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonHTTPResponse(request, http.StatusBadRequest, `{"error":{"message":"unrelated invalid request"}}`), nil
	})
	response, err := adapter.ForwardResponse(t.Context(), provider.ResponseResourceRequest{
		Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted},
		Method:     http.MethodPost, Path: "/responses", Model: "grok-4.5",
		Body: []byte(`{"model":"grok-4.5","input":[{"type":"reasoning","summary":[],"encrypted_content":"opaque"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if calls.Load() != 1 || response.StatusCode != http.StatusBadRequest {
		t.Fatalf("calls=%d status=%d", calls.Load(), response.StatusCode)
	}
}

func TestRecoverReasoningDecodeFailureStaysOnXAIFallbackPlane(t *testing.T) {
	adapter, encrypted := newReasoningRecoveryTestAdapter(t)
	adapter.SetFallbackMarker(reasoningRecoveryFallbackMarker{})
	var calls atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch call := calls.Add(1); call {
		case 1:
			if request.URL.Host != "build.test" {
				t.Fatalf("primary host = %q", request.URL.Host)
			}
			return jsonHTTPResponse(request, http.StatusForbidden, `{"error":"build denied"}`), nil
		case 2:
			if request.URL.Host != "xai.test" {
				t.Fatalf("fallback host = %q", request.URL.Host)
			}
			return jsonHTTPResponse(request, http.StatusBadRequest, `{"error":"Could not decode the compaction blob. Ensure it is unmodified from the compact response."}`), nil
		case 3:
			data, _ := io.ReadAll(request.Body)
			if request.URL.Host != "xai.test" || strings.Contains(string(data), `"type":"reasoning"`) {
				t.Fatalf("recovery host=%q body=%s", request.URL.Host, data)
			}
			return jsonHTTPResponse(request, http.StatusOK, `{"id":"resp_ok","status":"completed","output":[]}`), nil
		default:
			t.Fatalf("unexpected call %d", call)
			return nil, nil
		}
	})
	response, err := adapter.ForwardResponse(t.Context(), provider.ResponseResourceRequest{
		Credential: account.Credential{
			ID: 1, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted,
			BuildRouteMode: account.BuildRouteAuto, BuildSuperEntitled: true,
		},
		Method: http.MethodPost, Path: "/responses", Model: "grok-4.5",
		Body: []byte(`{"model":"grok-4.5","input":[{"type":"reasoning","summary":[],"encrypted_content":"opaque"},{"role":"user","content":"continue"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if calls.Load() != 3 || response.StatusCode != http.StatusOK || !strings.Contains(response.Header.Get("X-Grok2API-Compatibility-Warnings"), "reasoning_encrypted_content_downgraded") {
		t.Fatalf("calls=%d status=%d headers=%#v", calls.Load(), response.StatusCode, response.Header)
	}
}

func TestRecoverReasoningDecodeFailurePreservesOriginalWhenRetryFails(t *testing.T) {
	adapter, encrypted := newReasoningRecoveryTestAdapter(t)
	var calls atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return jsonHTTPResponse(request, http.StatusBadRequest, `{"error":"Could not decode the compaction blob. Ensure it is unmodified from the compact response."}`), nil
		}
		return jsonHTTPResponse(request, http.StatusServiceUnavailable, `{"error":"temporary failure"}`), nil
	})
	response, err := adapter.ForwardResponse(t.Context(), provider.ResponseResourceRequest{
		Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted},
		Method:     http.MethodPost, Path: "/responses", Model: "grok-4.5",
		Body: []byte(`{"model":"grok-4.5","input":[{"type":"reasoning","summary":[],"encrypted_content":"opaque"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	if calls.Load() != 2 || response.StatusCode != http.StatusBadRequest || !strings.Contains(string(data), "Could not decode") || response.Header.Get("X-Grok2API-Compatibility-Warnings") != "" {
		t.Fatalf("calls=%d status=%d headers=%#v body=%s", calls.Load(), response.StatusCode, response.Header, data)
	}
}

func newReasoningRecoveryTestAdapter(t *testing.T) (*Adapter, string) {
	t.Helper()
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}
	return NewAdapter(Config{
		BaseURL: "https://build.test/v1", FallbackBaseURL: "https://xai.test/v1",
		ClientVersion: "0.2.106", ClientIdentifier: "grok-shell", TokenAuth: "xai-grok-cli",
		UserAgent: "grok-shell/0.2.106 (linux; x86_64)",
	}, cipher), encrypted
}

func jsonHTTPResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Status: http.StatusText(status), Request: request,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   io.NopCloser(strings.NewReader(body)),
	}
}

type reasoningRecoveryFallbackMarker struct{}

func (reasoningRecoveryFallbackMarker) MarkBuildAPIFallback(context.Context, uint64, bool) error {
	return nil
}
