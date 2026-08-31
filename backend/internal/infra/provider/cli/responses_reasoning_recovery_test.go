package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
			{"type":"compaction","id":"cmp_1","encrypted_content":"opaque-compaction"},
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
	if json.Unmarshal(downgraded, &payload) != nil || len(payload.Input) != 4 {
		t.Fatalf("downgraded = %s, len=%d", downgraded, len(payload.Input))
	}
	summary := payload.Input[0]
	if summary["type"] != "message" || summary["role"] != "assistant" {
		t.Fatalf("readable reasoning = %#v", summary)
	}
	summaryText, _ := summary["content"].(string)
	if !strings.Contains(summaryText, "readable") || strings.Contains(summaryText, "omitted") {
		t.Fatalf("readable reasoning text = %q", summaryText)
	}
	compaction := payload.Input[1]
	if compaction["type"] != "compaction" || compaction["encrypted_content"] != "opaque-compaction" {
		t.Fatalf("compaction item was rewritten: %#v", compaction)
	}
	if payload.Input[2]["encrypted_content"] != "message-value" {
		t.Fatalf("non-reasoning encrypted content changed: %#v", payload.Input[2])
	}
	if payload.Input[3]["role"] != "user" {
		t.Fatalf("user message changed: %#v", payload.Input[3])
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
	if len(response.RecoveredAttempts) != 1 || response.RecoveredAttempts[0].Stage != "reasoning_decode_rejected" {
		t.Fatalf("recovered attempts = %#v", response.RecoveredAttempts)
	}
	if attempt := response.RecoveredAttempts[0]; attempt.UpstreamURL != "https://build.test/v1/responses" || attempt.StartedAt.IsZero() {
		t.Fatalf("recovered provenance = %#v", attempt)
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
	if len(response.RecoveredAttempts) < 1 {
		t.Fatalf("recovered attempts = %#v", response.RecoveredAttempts)
	}
	var decode *provider.RecoveredAttempt
	for i := range response.RecoveredAttempts {
		if response.RecoveredAttempts[i].Stage == "reasoning_decode_rejected" {
			decode = &response.RecoveredAttempts[i]
			break
		}
	}
	if decode == nil || decode.Result != "recovered_encrypted_content_stripped" {
		t.Fatalf("recovered attempts = %#v", response.RecoveredAttempts)
	}
	if decode.Diagnostic.StatusCode != http.StatusBadRequest || !strings.Contains(string(decode.Diagnostic.Body), "compaction blob") {
		t.Fatalf("hidden 400 = %#v", decode.Diagnostic)
	}
	if decode.UpstreamURL != "https://xai.test/v1/responses" || decode.StartedAt.IsZero() {
		t.Fatalf("hidden recovery provenance = %#v", decode)
	}
	var primary *provider.RecoveredAttempt
	for i := range response.RecoveredAttempts {
		if response.RecoveredAttempts[i].Stage == "primary_plane_response" {
			primary = &response.RecoveredAttempts[i]
			break
		}
	}
	if primary == nil || primary.UpstreamURL != "https://build.test/v1/responses" || primary.Diagnostic.StatusCode != http.StatusForbidden {
		t.Fatalf("primary fallback provenance = %#v", response.RecoveredAttempts)
	}
}

func TestRecoverReasoningDecodeFailureLetsRecoveredForbiddenReachPlaneFallback(t *testing.T) {
	adapter, encrypted := newReasoningRecoveryTestAdapter(t)
	adapter.SetFallbackMarker(reasoningRecoveryFallbackMarker{})
	var calls atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch call := calls.Add(1); call {
		case 1:
			if request.URL.Host != "build.test" {
				t.Fatalf("primary host = %q", request.URL.Host)
			}
			return jsonHTTPResponse(request, http.StatusBadRequest, `{"error":"Could not decrypt the provided encrypted_content. Ensure the value is unmodified."}`), nil
		case 2:
			data, _ := io.ReadAll(request.Body)
			if request.URL.Host != "build.test" || strings.Contains(string(data), `"encrypted_content"`) {
				t.Fatalf("portable retry host=%q body=%s", request.URL.Host, data)
			}
			return jsonHTTPResponse(request, http.StatusForbidden, `{"error":"build denied"}`), nil
		case 3:
			if request.URL.Host != "xai.test" {
				t.Fatalf("fallback host = %q", request.URL.Host)
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
	if calls.Load() != 3 || response.StatusCode != http.StatusOK || response.RecoveredPrimaryFailure == nil || response.RecoveredPrimaryFailure.StatusCode != http.StatusForbidden {
		t.Fatalf("calls=%d status=%d recovered_primary=%#v", calls.Load(), response.StatusCode, response.RecoveredPrimaryFailure)
	}
}

func TestRecoverReasoningDecodeFailureResetsSessionWithoutOpaqueInput(t *testing.T) {
	adapter, encrypted := newReasoningRecoveryTestAdapter(t)
	var calls atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		call := calls.Add(1)
		data, _ := io.ReadAll(request.Body)
		switch call {
		case 1:
			if request.Header.Get("x-grok-session-id") == "" || !strings.Contains(string(data), `"prompt_cache_key":"session-1"`) {
				t.Fatalf("initial session request headers=%#v body=%s", request.Header, data)
			}
			return jsonHTTPResponse(request, http.StatusBadRequest, `{"error":"Could not decode the compaction blob. Ensure it is unmodified from the compact response."}`), nil
		case 2:
			if request.Header.Get("x-grok-session-id") != "" || request.Header.Get("x-grok-conv-id") != "" || strings.Contains(string(data), `"prompt_cache_key"`) {
				t.Fatalf("stateless recovery headers=%#v body=%s", request.Header, data)
			}
			return jsonHTTPResponse(request, http.StatusOK, `{"id":"resp_ok","status":"completed","output":[]}`), nil
		default:
			t.Fatalf("unexpected call %d", call)
			return nil, nil
		}
	})
	response, err := adapter.ForwardResponse(t.Context(), provider.ResponseResourceRequest{
		Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted},
		Method:     http.MethodPost, Path: "/responses", Model: "grok-4.5", PromptCacheKey: "session-1",
		Body: []byte(`{"model":"grok-4.5","input":[{"role":"user","content":"continue"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	warnings := response.Header.Get("X-Grok2API-Compatibility-Warnings")
	if calls.Load() != 2 || response.StatusCode != http.StatusOK || !strings.Contains(warnings, "reasoning_session_reset") || strings.Contains(warnings, "reasoning_encrypted_content_downgraded") {
		t.Fatalf("calls=%d status=%d warnings=%q", calls.Load(), response.StatusCode, warnings)
	}
	if len(response.RecoveredAttempts) != 1 || response.RecoveredAttempts[0].Result != "recovered_session_reset" {
		t.Fatalf("recovered attempts = %#v", response.RecoveredAttempts)
	}
}

func TestRecoverReasoningDecodeFailureEscalatesFromOpaqueStripToSessionReset(t *testing.T) {
	adapter, encrypted := newReasoningRecoveryTestAdapter(t)
	var calls atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		call := calls.Add(1)
		data, _ := io.ReadAll(request.Body)
		switch call {
		case 1:
			if !strings.Contains(string(data), `"encrypted_content":"opaque"`) || request.Header.Get("x-grok-session-id") == "" {
				t.Fatalf("initial body=%s headers=%#v", data, request.Header)
			}
		case 2:
			if strings.Contains(string(data), `"encrypted_content"`) || request.Header.Get("x-grok-session-id") == "" || !strings.Contains(string(data), `"prompt_cache_key":"session-1"`) {
				t.Fatalf("opaque downgrade body=%s headers=%#v", data, request.Header)
			}
		case 3:
			if strings.Contains(string(data), `"encrypted_content"`) || strings.Contains(string(data), `"prompt_cache_key"`) || request.Header.Get("x-grok-session-id") != "" {
				t.Fatalf("session reset body=%s headers=%#v", data, request.Header)
			}
			return jsonHTTPResponse(request, http.StatusOK, `{"id":"resp_ok","status":"completed","output":[]}`), nil
		default:
			t.Fatalf("unexpected call %d", call)
		}
		return jsonHTTPResponse(request, http.StatusBadRequest, `{"error":"Could not decode the compaction blob. Ensure it is unmodified from the compact response."}`), nil
	})
	response, err := adapter.ForwardResponse(t.Context(), provider.ResponseResourceRequest{
		Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted},
		Method:     http.MethodPost, Path: "/responses", Model: "grok-4.5", PromptCacheKey: "session-1",
		Body: []byte(`{"model":"grok-4.5","input":[{"type":"reasoning","summary":[],"encrypted_content":"opaque"},{"role":"user","content":"continue"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	warnings := response.Header.Get("X-Grok2API-Compatibility-Warnings")
	if calls.Load() != 3 || response.StatusCode != http.StatusOK || !strings.Contains(warnings, "reasoning_encrypted_content_downgraded") || !strings.Contains(warnings, "reasoning_session_reset") {
		t.Fatalf("calls=%d status=%d warnings=%q", calls.Load(), response.StatusCode, warnings)
	}
}

func TestRecoverReasoningDecodeFailurePreservesRateLimitAfterOpaqueStrip(t *testing.T) {
	adapter, encrypted := newReasoningRecoveryTestAdapter(t)
	var calls atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch calls.Add(1) {
		case 1:
			return jsonHTTPResponse(request, http.StatusBadRequest, `{"error":"Could not decode the compaction blob. Ensure it is unmodified from the compact response."}`), nil
		case 2:
			data, _ := io.ReadAll(request.Body)
			if strings.Contains(string(data), `"encrypted_content"`) {
				t.Fatalf("rate-limit recovery body still contains encrypted content: %s", data)
			}
			return jsonHTTPResponse(request, http.StatusTooManyRequests, `{"error":{"message":"rate limited"}}`), nil
		default:
			t.Fatalf("unexpected call %d", calls.Load())
			return nil, nil
		}
	})

	response, err := adapter.ForwardResponse(t.Context(), provider.ResponseResourceRequest{
		Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted},
		Method:     http.MethodPost, Path: "/responses", Model: "grok-4.5", PromptCacheKey: "session-1",
		Body: []byte(`{"model":"grok-4.5","input":[{"type":"reasoning","summary":[],"encrypted_content":"opaque"},{"role":"user","content":"continue"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	warnings := response.Header.Get("X-Grok2API-Compatibility-Warnings")
	if calls.Load() != 2 || response.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(body), "rate limited") || !strings.Contains(warnings, "reasoning_encrypted_content_downgraded") || strings.Contains(warnings, "reasoning_recovery_failed") {
		t.Fatalf("calls=%d status=%d warnings=%q body=%s", calls.Load(), response.StatusCode, warnings, body)
	}
}

func TestRecoverReasoningDecodeFailurePreservesRateLimitAfterSessionReset(t *testing.T) {
	adapter, encrypted := newReasoningRecoveryTestAdapter(t)
	var calls atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		call := calls.Add(1)
		data, _ := io.ReadAll(request.Body)
		switch call {
		case 1:
			if !strings.Contains(string(data), `"encrypted_content":"opaque"`) || request.Header.Get("x-grok-session-id") == "" {
				t.Fatalf("initial body=%s headers=%#v", data, request.Header)
			}
		case 2:
			if strings.Contains(string(data), `"encrypted_content"`) || request.Header.Get("x-grok-session-id") == "" || !strings.Contains(string(data), `"prompt_cache_key":"session-1"`) {
				t.Fatalf("opaque downgrade body=%s headers=%#v", data, request.Header)
			}
		case 3:
			if strings.Contains(string(data), `"encrypted_content"`) || strings.Contains(string(data), `"prompt_cache_key"`) || request.Header.Get("x-grok-session-id") != "" || request.Header.Get("x-grok-conv-id") != "" {
				t.Fatalf("session reset body=%s headers=%#v", data, request.Header)
			}
			response := jsonHTTPResponse(request, http.StatusTooManyRequests, `{"error":{"message":"rate limited after session reset"}}`)
			response.Header.Set("Retry-After", "17")
			return response, nil
		default:
			t.Fatalf("unexpected call %d", call)
			return nil, nil
		}
		return jsonHTTPResponse(request, http.StatusBadRequest, `{"error":"Could not decode the compaction blob. Ensure it is unmodified from the compact response."}`), nil
	})

	response, err := adapter.ForwardResponse(t.Context(), provider.ResponseResourceRequest{
		Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted},
		Method:     http.MethodPost, Path: "/responses", Model: "grok-4.5", PromptCacheKey: "session-1",
		Body: []byte(`{"model":"grok-4.5","input":[{"type":"reasoning","summary":[],"encrypted_content":"opaque"},{"role":"user","content":"continue"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	warnings := response.Header.Get("X-Grok2API-Compatibility-Warnings")
	if calls.Load() != 3 || response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Retry-After") != "17" || !strings.Contains(string(body), "rate limited after session reset") {
		t.Fatalf("calls=%d status=%d retry_after=%q body=%s", calls.Load(), response.StatusCode, response.Header.Get("Retry-After"), body)
	}
	if !strings.Contains(warnings, "reasoning_encrypted_content_downgraded") || !strings.Contains(warnings, "reasoning_session_reset") || strings.Contains(warnings, "reasoning_recovery_failed") {
		t.Fatalf("warnings=%q", warnings)
	}
}

func TestRecoverReasoningDecodeFailurePreservesServerErrorAfterSessionReset(t *testing.T) {
	adapter, encrypted := newReasoningRecoveryTestAdapter(t)
	var calls atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return jsonHTTPResponse(request, http.StatusBadRequest, `{"error":"Could not decode the compaction blob. Ensure it is unmodified from the compact response."}`), nil
		}
		return jsonHTTPResponse(request, http.StatusBadGateway, `{"error":"stateless retry failed upstream"}`), nil
	})
	response, err := adapter.ForwardResponse(t.Context(), provider.ResponseResourceRequest{
		Credential:     account.Credential{ID: 1, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted},
		Method:         http.MethodPost,
		Path:           "/responses",
		Model:          "grok-4.5",
		PromptCacheKey: "session-1",
		Body:           []byte(`{"model":"grok-4.5","input":[{"role":"user","content":"continue"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	warnings := response.Header.Get("X-Grok2API-Compatibility-Warnings")
	if calls.Load() != 2 || response.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "stateless retry failed upstream") {
		t.Fatalf("calls=%d status=%d body=%s", calls.Load(), response.StatusCode, body)
	}
	if response.ReasoningRecoveryFailed || !strings.Contains(warnings, "reasoning_session_reset") || strings.Contains(warnings, "reasoning_recovery_failed") {
		t.Fatalf("internal_failure=%t warnings=%q", response.ReasoningRecoveryFailed, warnings)
	}
}

// TestRecoverReasoningDecodeFailureWithMillionTokenScaleOpaqueReasoning keeps
// the legacy Build wording compatible with large reasoning replay payloads.
// The request has no compaction item, so the opaque reasoning remains eligible
// for same-account recovery even when Build calls it a compaction blob.
func TestRecoverReasoningDecodeFailureWithMillionTokenScaleOpaqueReasoning(t *testing.T) {
	adapter, encrypted := newReasoningRecoveryTestAdapter(t)
	opaqueReasoning := strings.Repeat("x", 4<<20)
	body, err := json.Marshal(map[string]any{
		"model": "grok-4.5",
		"input": []any{
			map[string]any{
				"type":              "reasoning",
				"summary":           []any{},
				"encrypted_content": opaqueReasoning,
			},
			map[string]any{"role": "user", "content": "压缩后继续执行"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		call := calls.Add(1)
		data, readErr := io.ReadAll(request.Body)
		if readErr != nil {
			t.Fatal(readErr)
		}
		switch call {
		case 1:
			if len(data) < 4<<20 || !strings.Contains(string(data), `"encrypted_content"`) {
				t.Fatalf("初始 1M 级 reasoning 请求不完整：size=%d", len(data))
			}
			return jsonHTTPResponse(request, http.StatusBadRequest, `{"error":"Could not decode the compaction blob. Ensure it is unmodified from the compact response."}`), nil
		case 2:
			if len(data) >= 4096 || strings.Contains(string(data), `"encrypted_content"`) || !strings.Contains(string(data), "压缩后继续执行") {
				t.Fatalf("恢复请求仍携带 opaque reasoning：size=%d", len(data))
			}
			return jsonHTTPResponse(request, http.StatusTooManyRequests, `{"error":{"message":"rate limited after reasoning recovery"}}`), nil
		default:
			t.Fatalf("unexpected call %d", call)
			return nil, nil
		}
	})

	response, err := adapter.ForwardResponse(t.Context(), provider.ResponseResourceRequest{
		Credential:     account.Credential{ID: 1, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted},
		Method:         http.MethodPost,
		Path:           "/responses",
		Model:          "grok-4.5",
		PromptCacheKey: "million-token-session",
		Body:           body,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	responseBody, _ := io.ReadAll(response.Body)
	warnings := response.Header.Get("X-Grok2API-Compatibility-Warnings")
	if calls.Load() != 2 || response.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(responseBody), "rate limited after reasoning recovery") || !strings.Contains(warnings, "reasoning_encrypted_content_downgraded") {
		t.Fatalf("calls=%d status=%d warnings=%q body=%s", calls.Load(), response.StatusCode, warnings, responseBody)
	}
}

// TestCompactionBlobDecodeFailureIsReturnedToClient covers Claude Code
// replaying a large opaque compact state that Build rejects. That 400 is
// surfaced; recovery must not strip the blob and retry.
func TestCompactionBlobDecodeFailureIsReturnedToClient(t *testing.T) {
	adapter, encrypted := newReasoningRecoveryTestAdapter(t)
	compactionBlob := strings.Repeat("x", 4<<20)
	body, err := json.Marshal(map[string]any{
		"model":                "grok-4.5",
		"previous_response_id": "resp_compacted",
		"input": []any{
			map[string]any{
				"type":              "compaction",
				"encrypted_content": compactionBlob,
			},
			map[string]any{"role": "user", "content": "压缩后继续执行"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		call := calls.Add(1)
		data, readErr := io.ReadAll(request.Body)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if call != 1 {
			t.Fatalf("unexpected recovery retry %d", call)
		}
		if len(data) < 4<<20 || !strings.Contains(string(data), `"encrypted_content"`) || !strings.Contains(string(data), `"previous_response_id":"resp_compacted"`) {
			t.Fatalf("compact blob was not forwarded: size=%d", len(data))
		}
		return jsonHTTPResponse(request, http.StatusBadRequest, `{"error":"Could not decode the compaction blob. Ensure it is unmodified from the compact response."}`), nil
	})

	response, err := adapter.ForwardResponse(t.Context(), provider.ResponseResourceRequest{
		Credential:     account.Credential{ID: 1, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted},
		Method:         http.MethodPost,
		Path:           "/responses",
		Model:          "grok-4.5",
		PromptCacheKey: "million-token-session",
		Body:           body,
		NormalizeBody:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	responseBody, _ := io.ReadAll(response.Body)
	warnings := response.Header.Get("X-Grok2API-Compatibility-Warnings")
	if calls.Load() != 1 || response.StatusCode != http.StatusBadRequest || !strings.Contains(string(responseBody), "Could not decode the compaction blob") || strings.Contains(warnings, "reasoning_encrypted_content_downgraded") {
		t.Fatalf("calls=%d status=%d warnings=%q body=%s", calls.Load(), response.StatusCode, warnings, responseBody)
	}
	if response.ReasoningRecoveryFailed || len(response.RecoveredAttempts) != 0 {
		t.Fatalf("surfaced compaction 400 must remain the sole final attempt: %#v", response.RecoveredAttempts)
	}
}

func TestRecoverReasoningDecodeFailureDoesNotResetStoredResponseChain(t *testing.T) {
	adapter, encrypted := newReasoningRecoveryTestAdapter(t)
	var calls atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonHTTPResponse(request, http.StatusBadRequest, `{"error":"Could not decode the compaction blob. Ensure it is unmodified from the compact response."}`), nil
	})
	response, err := adapter.ForwardResponse(t.Context(), provider.ResponseResourceRequest{
		Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted},
		Method:     http.MethodPost, Path: "/responses", Model: "grok-4.5", PromptCacheKey: "session-1",
		Body: []byte(`{"model":"grok-4.5","previous_response_id":"resp_1","input":[{"role":"user","content":"continue"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if calls.Load() != 1 || response.StatusCode != http.StatusBadRequest || !strings.Contains(response.Header.Get("X-Grok2API-Compatibility-Warnings"), "reasoning_recovery_failed") {
		t.Fatalf("calls=%d status=%d warnings=%q", calls.Load(), response.StatusCode, response.Header.Get("X-Grok2API-Compatibility-Warnings"))
	}
	if !response.ReasoningRecoveryFailed {
		t.Fatal("exhausted same-account recovery must set the internal gateway retry hint")
	}
	if len(response.RecoveredAttempts) != 0 {
		t.Fatalf("unattempted recovery must not duplicate the final 400: %#v", response.RecoveredAttempts)
	}
}

func TestRecoverReasoningDecodeFailureReturnsNon400RetryResponse(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			adapter, encrypted := newReasoningRecoveryTestAdapter(t)
			var calls atomic.Int32
			adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if calls.Add(1) == 1 {
					return jsonHTTPResponse(request, http.StatusBadRequest, `{"error":"Could not decode the compaction blob. Ensure it is unmodified from the compact response."}`), nil
				}
				return jsonHTTPResponse(request, status, `{"error":"recovery retry response"}`), nil
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
			if calls.Load() != 2 || response.StatusCode != status || !strings.Contains(string(data), "recovery retry response") {
				t.Fatalf("calls=%d status=%d headers=%#v body=%s", calls.Load(), response.StatusCode, response.Header, data)
			}
			if response.ReasoningRecoveryFailed || strings.Contains(response.Header.Get("X-Grok2API-Compatibility-Warnings"), "reasoning_recovery_failed") {
				t.Fatalf("non-400 retry was mislabeled as recovery failure: %#v", response)
			}
			if len(response.RecoveredAttempts) != 1 {
				t.Fatalf("recovered attempts = %#v", response.RecoveredAttempts)
			}
			attempt := response.RecoveredAttempts[0]
			if attempt.Stage != "reasoning_decode_rejected" || attempt.Result != "replaced_by_retry_response" || attempt.Diagnostic.StatusCode != http.StatusBadRequest || attempt.UpstreamURL != "https://build.test/v1/responses" || attempt.StartedAt.IsZero() {
				t.Fatalf("hidden initial 400 = %#v", attempt)
			}
		})
	}
}

func TestRecoverReasoningDecodeFailurePropagatesRetryTransportError(t *testing.T) {
	adapter, encrypted := newReasoningRecoveryTestAdapter(t)
	wantErr := errors.New("recovery transport failed")
	var calls atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return jsonHTTPResponse(request, http.StatusBadRequest, `{"error":"Could not decode the compaction blob. Ensure it is unmodified from the compact response."}`), nil
		}
		return nil, wantErr
	})
	response, err := adapter.ForwardResponse(t.Context(), provider.ResponseResourceRequest{
		Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted},
		Method:     http.MethodPost, Path: "/responses", Model: "grok-4.5",
		Body: []byte(`{"model":"grok-4.5","input":[{"type":"reasoning","summary":[],"encrypted_content":"opaque"}]}`),
	})
	if response != nil || !errors.Is(err, wantErr) || calls.Load() != 2 {
		t.Fatalf("response=%#v err=%v calls=%d", response, err, calls.Load())
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
		ClientVersion: "0.2.110", ClientIdentifier: "grok-shell", TokenAuth: "xai-grok-cli",
		UserAgent: "grok-shell/0.2.110 (linux; x86_64)",
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

func TestRecoverReasoningDecodeFailureKeepsReadableSummary(t *testing.T) {
	adapter, encrypted := newReasoningRecoveryTestAdapter(t)
	var calls atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		call := calls.Add(1)
		data, _ := io.ReadAll(request.Body)
		if call == 1 {
			if !strings.Contains(string(data), `"encrypted_content":"opaque"`) {
				t.Fatalf("first body = %s", data)
			}
			return jsonHTTPResponse(request, http.StatusBadRequest, `{"error":"Could not decrypt the provided encrypted_content. Ensure the value is unmodified."}`), nil
		}
		if strings.Contains(string(data), `"encrypted_content"`) || strings.Contains(string(data), `"type":"reasoning"`) {
			t.Fatalf("retry still has opaque reasoning: %s", data)
		}
		if !strings.Contains(string(data), "do not touch Y") {
			t.Fatalf("retry dropped readable summary: %s", data)
		}
		return jsonHTTPResponse(request, http.StatusOK, `{"id":"resp_ok","status":"completed","output":[]}`), nil
	})
	response, err := adapter.ForwardResponse(t.Context(), provider.ResponseResourceRequest{
		Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted},
		Method:     http.MethodPost, Path: "/responses", Model: "grok-4.5", PromptCacheKey: "session-1",
		Body: []byte(`{"model":"grok-4.5","input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"do not touch Y"}],"encrypted_content":"opaque"},{"role":"user","content":"continue"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if calls.Load() != 2 || response.StatusCode != http.StatusOK || !strings.Contains(response.Header.Get("X-Grok2API-Compatibility-Warnings"), "reasoning_encrypted_content_downgraded") {
		t.Fatalf("calls=%d status=%d warnings=%q", calls.Load(), response.StatusCode, response.Header.Get("X-Grok2API-Compatibility-Warnings"))
	}
}

func TestRecoverReasoningDecodeFailureContinuesToSessionResetWhenStripRewords400(t *testing.T) {
	adapter, encrypted := newReasoningRecoveryTestAdapter(t)
	var calls atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		call := calls.Add(1)
		data, _ := io.ReadAll(request.Body)
		switch call {
		case 1:
			return jsonHTTPResponse(request, http.StatusBadRequest, `{"error":"Could not decrypt the provided encrypted_content. Ensure the value is unmodified."}`), nil
		case 2:
			if strings.Contains(string(data), `"encrypted_content"`) || request.Header.Get("x-grok-session-id") == "" {
				t.Fatalf("opaque downgrade body=%s headers=%#v", data, request.Header)
			}
			return jsonHTTPResponse(request, http.StatusBadRequest, `{"error":"invalid request history"}`), nil
		case 3:
			if strings.Contains(string(data), `"encrypted_content"`) || strings.Contains(string(data), `"prompt_cache_key"`) || request.Header.Get("x-grok-session-id") != "" {
				t.Fatalf("session reset body=%s headers=%#v", data, request.Header)
			}
			return jsonHTTPResponse(request, http.StatusOK, `{"id":"resp_ok","status":"completed","output":[]}`), nil
		default:
			t.Fatalf("unexpected call %d", call)
			return nil, nil
		}
	})
	response, err := adapter.ForwardResponse(t.Context(), provider.ResponseResourceRequest{
		Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted},
		Method:     http.MethodPost, Path: "/responses", Model: "grok-4.5", PromptCacheKey: "session-1",
		Body: []byte(`{"model":"grok-4.5","input":[{"type":"reasoning","summary":[],"encrypted_content":"opaque"},{"role":"user","content":"continue"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	warnings := response.Header.Get("X-Grok2API-Compatibility-Warnings")
	if calls.Load() != 3 || response.StatusCode != http.StatusOK || !strings.Contains(warnings, "reasoning_encrypted_content_downgraded") || !strings.Contains(warnings, "reasoning_session_reset") {
		t.Fatalf("calls=%d status=%d warnings=%q", calls.Load(), response.StatusCode, warnings)
	}
}

func TestRecoverReasoningDecodeFailureKeepsLegacyCompactionMarkerWithoutCompactionInput(t *testing.T) {
	adapter, encrypted := newReasoningRecoveryTestAdapter(t)
	var calls atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		call := calls.Add(1)
		if call == 1 {
			return jsonHTTPResponse(request, http.StatusBadRequest, `{"code":"invalid-argument","error":"Could not decode the compaction blob. Ensure it is unmodified from the compact response."}`), nil
		}
		return jsonHTTPResponse(request, http.StatusOK, `{"id":"resp_ok","status":"completed","output":[]}`), nil
	})

	body := []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"hello"}]}`)
	resp, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: account.Credential{ID: 131, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted},
		Method:     http.MethodPost, Path: "/responses", Model: "grok-4.6", PromptCacheKey: "session-compaction-test",
		NormalizeBody: true, Operation: conversation.OperationChat, Body: body,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if calls.Load() != 2 || resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("X-Grok2API-Compatibility-Warnings"), "reasoning_session_reset") {
		t.Fatalf("calls=%d status=%d warnings=%q", calls.Load(), resp.StatusCode, resp.Header.Get("X-Grok2API-Compatibility-Warnings"))
	}
}
