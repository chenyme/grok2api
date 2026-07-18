package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/reasoningreplay"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestForwardResponseMatchesGrokBuildHeadersAndPreservesReasoning(t *testing.T) {
	var captured map[string]any
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer access-token" || r.Header.Get("X-XAI-Token-Auth") != "xai-grok-cli" || r.Header.Get("x-authenticateresponse") != "authenticate-response" || r.Header.Get("x-grok-client-version") != "0.2.102" || r.Header.Get("x-grok-client-identifier") != "grok-shell" || r.Header.Get("x-grok-client-mode") != "headless" || r.Header.Get("User-Agent") != "grok-shell/0.2.102 (linux; x86_64)" {
			t.Fatalf("headers = %#v", r.Header)
		}
		requestID := r.Header.Get("x-grok-req-id")
		sessionID := r.Header.Get("x-grok-session-id")
		expectedSessionID, err := grokSessionID("isolated-key")
		if err != nil {
			t.Fatal(err)
		}
		requestUUID, requestErr := uuid.Parse(requestID)
		agentUUID, agentErr := uuid.Parse(r.Header.Get("x-grok-agent-id"))
		if requestErr != nil || requestUUID.Version() != uuid.Version(4) || agentErr != nil || agentUUID.Version() != uuid.Version(4) || sessionID != expectedSessionID || r.Header.Get("x-grok-conv-id") != sessionID {
			t.Fatalf("client identity headers = %#v", r.Header)
		}
		for _, legacy := range []string{"x-grok-client-surface", "x-grok-client-name", "x-grok-conversation-id", "x-grok-session-id-legacy", "x-grok-request-id"} {
			if r.Header.Get(legacy) != "" {
				t.Fatalf("legacy header %s = %q", legacy, r.Header.Get(legacy))
			}
		}
		if r.Header.Get("x-grok-user-id") != "user-123" || r.Header.Get("x-userid") != "" || r.Header.Get("Accept-Encoding") != "gzip" || len(r.Header.Get("traceparent")) != 55 {
			t.Fatalf("protocol headers = %#v", r.Header)
		}
		if _, ok := r.Header["Tracestate"]; ok {
			t.Fatalf("tracestate = %#v", r.Header["Tracestate"])
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"resp_1","object":"response"}`)),
			Request:    r,
		}, nil
	})

	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{BaseURL: "https://cli-chat-proxy.grok.com/v1", ClientVersion: "0.2.102", ClientIdentifier: "grok-shell", TokenAuth: "xai-grok-cli", UserAgent: "grok-shell/0.2.102 (linux; x86_64)"}, cipher)
	adapter.http.Transport = transport
	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: account.Credential{ID: 7, UserID: "user-123", EncryptedAccessToken: encrypted}, Method: http.MethodPost, Path: "/responses",
		Model: "grok-4.5", PromptCacheKey: "isolated-key", NormalizeBody: true,
		Body: []byte(`{"model":"public","prompt_cache_key":"client-key","input":[{"type":"reasoning","id":"rs_1","encrypted_content":"cipher"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	input := captured["input"].([]any)
	if captured["model"] != "grok-4.5" || captured["prompt_cache_key"] != "isolated-key" || len(input) != 1 || input[0].(map[string]any)["type"] != "reasoning" || input[0].(map[string]any)["encrypted_content"] != "cipher" {
		t.Fatalf("captured = %#v", captured)
	}
}

func TestForwardResponseReplaysReasoningAcrossMessagesTurns(t *testing.T) {
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}

	rawEncrypted := make([]byte, 64)
	for index := range rawEncrypted {
		rawEncrypted[index] = byte(index)
	}
	replayEncrypted := base64.RawStdEncoding.EncodeToString(rawEncrypted)
	requestCount := 0
	adapter := NewAdapter(Config{BaseURL: "https://cli-chat-proxy.grok.com/v1"}, cipher)
	adapter.SetReasoningReplay(reasoningreplay.New(
		memory.NewReasoningReplayStore(16),
		reasoningreplay.Config{Enabled: true, TTL: time.Hour},
		nil,
	))
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestCount++
		var payload struct {
			Input []map[string]any `json:"input"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if requestCount == 2 {
			if len(payload.Input) != 4 || payload.Input[0]["role"] != "user" || payload.Input[1]["type"] != "reasoning" || payload.Input[1]["encrypted_content"] != replayEncrypted || payload.Input[2]["role"] != "assistant" || payload.Input[3]["role"] != "user" {
				t.Fatalf("second turn replay order = %#v", payload.Input)
			}
		}
		body := `{"id":"resp_2","model":"grok-4.5","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"second"}]}]}`
		if requestCount == 1 {
			body = `{"id":"resp_1","model":"grok-4.5","status":"completed","output":[{"type":"reasoning","encrypted_content":"` + replayEncrypted + `"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first"}]}]}`
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})

	credential := account.Credential{Provider: account.ProviderBuild, EncryptedAccessToken: encrypted}
	first, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: credential, Method: http.MethodPost, Path: "/responses", Model: "grok-4.5",
		NormalizeBody: true, Operation: conversation.OperationMessages, PromptCacheKey: "messages-replay-key",
		Body: []byte(`{"model":"public","max_tokens":128,"messages":[{"role":"user","content":"first"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(first.Body); err != nil {
		t.Fatal(err)
	}
	if err := first.Body.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: credential, Method: http.MethodPost, Path: "/responses", Model: "grok-4.5",
		NormalizeBody: true, Operation: conversation.OperationMessages, PromptCacheKey: "messages-replay-key",
		Body: []byte(`{"model":"public","max_tokens":128,"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"first"},{"role":"user","content":"second"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Body.Close()
	if _, err := io.ReadAll(second.Body); err != nil {
		t.Fatal(err)
	}
	if requestCount != 2 {
		t.Fatalf("request count = %d", requestCount)
	}
}

func TestListModelsUsesOfficialMetadataHeaders(t *testing.T) {
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{BaseURL: "https://cli-chat-proxy.grok.com/v1", ClientVersion: "0.2.102", ClientIdentifier: "grok-shell", TokenAuth: "xai-grok-cli", UserAgent: "grok-shell/0.2.102 (linux; x86_64)"}, cipher)
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/v1/models" || request.Header.Get("Authorization") != "Bearer access-token" || request.Header.Get("X-XAI-Token-Auth") != "xai-grok-cli" || request.Header.Get("x-grok-client-version") != "0.2.102" || request.Header.Get("x-grok-client-identifier") != "grok-shell" || request.Header.Get("x-grok-client-mode") != "headless" || request.Header.Get("User-Agent") != "grok-shell/0.2.102 (linux; x86_64)" {
			t.Fatalf("headers = %#v", request.Header)
		}
		if request.Header.Get("x-userid") != "user-123" || request.Header.Get("x-email") != "user@example.com" || request.Header.Get("x-grok-user-id") != "" || request.Header.Get("x-authenticateresponse") != "" || request.Header.Get("x-grok-session-id") != "" {
			t.Fatalf("metadata headers = %#v", request.Header)
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"grok-4.5"}]}`)), Request: request}, nil
	})
	models, err := adapter.ListModels(context.Background(), account.Credential{UserID: "user-123", Email: "user@example.com", EncryptedAccessToken: encrypted})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0] != "grok-4.5" {
		t.Fatalf("models = %#v", models)
	}
}

func TestModelCatalogETagSignalsMissingOrChangedCatalogBaseline(t *testing.T) {
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}
	modelETag := `"catalog-v1"`
	responseETag := modelETag
	adapter := NewAdapter(Config{BaseURL: "https://cli-chat-proxy.grok.com/v1"}, cipher)
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		header := make(http.Header)
		body := `{"id":"resp_1","status":"completed","output":[]}`
		if request.URL.Path == "/v1/models" {
			header.Set("ETag", modelETag)
			body = `{"data":[{"id":"grok-4.5"}]}`
		} else {
			header.Set("x-models-etag", responseETag)
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})
	credential := account.Credential{ID: 42, EncryptedAccessToken: encrypted}
	if !adapter.modelCatalogChanged(43, `"catalog-v1"`) {
		t.Fatal("缺少进程内目录基线时应补一次账号模型同步")
	}
	if _, err := adapter.ListModels(context.Background(), credential); err != nil {
		t.Fatal(err)
	}
	forward := func() *provider.Response {
		response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
			Credential: credential, Method: http.MethodPost, Path: "/responses", Body: []byte(`{}`), Operation: conversation.OperationResponses,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		return response
	}
	if response := forward(); response.ModelCatalogChanged {
		t.Fatal("与最近模型同步相同的 ETag 不应触发刷新")
	}
	responseETag = `"catalog-v2"`
	if response := forward(); !response.ModelCatalogChanged {
		t.Fatal("推理响应报告新 ETag 时应触发账号模型刷新")
	}
	modelETag = responseETag
	if _, err := adapter.ListModels(context.Background(), credential); err != nil {
		t.Fatal(err)
	}
	if response := forward(); response.ModelCatalogChanged {
		t.Fatal("成功同步新目录后不应继续重复触发刷新")
	}
}

func TestGetBillingUsesCreditsAndLiveSubscriptionTier(t *testing.T) {
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{BaseURL: "https://cli-chat-proxy.grok.com/v1", ClientVersion: "0.2.102", ClientIdentifier: "grok-shell", TokenAuth: "xai-grok-cli", UserAgent: "grok-shell/0.2.102 (linux; x86_64)"}, cipher)
	calls := 0
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		var body string
		switch request.URL.Path {
		case "/v1/billing":
			if request.URL.Query().Get("format") != "credits" {
				t.Fatalf("billing request = %s", request.URL.String())
			}
			body = `{"config":{"creditUsagePercent":0,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-07-01T00:00:00Z","end":"2026-07-08T00:00:00Z"}}}`
		case "/v1/user":
			if request.URL.Query().Get("include") != "subscription" {
				t.Fatalf("subscription request = %s", request.URL.String())
			}
			body = `{"subscriptionTier":"SuperGrokPro"}`
		default:
			t.Fatalf("unexpected request = %s", request.URL.String())
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})
	billing, err := adapter.GetBilling(context.Background(), account.Credential{ID: 7, EncryptedAccessToken: encrypted})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || billing.AccountID != 7 || billing.CreditUsagePercent != 0 || billing.UsagePeriodType != "USAGE_PERIOD_TYPE_WEEKLY" || billing.PlanName != "SuperGrokPro" || !billing.IsPaid() || billing.SyncedAt.IsZero() {
		t.Fatalf("calls=%d billing=%#v", calls, billing)
	}
}

func TestNormalizeAccountModelCapabilitiesSuperAddsVideo15(t *testing.T) {
	adapter := &Adapter{}
	// Super / paid：主 Build 仅返回 grok-4.5 时也必须补齐 1.5。
	got := adapter.NormalizeAccountModelCapabilities([]string{"grok-4.5", "  ", "grok-4.5"}, &account.Billing{MonthlyLimit: 100})
	if len(got) != 2 || got[0] != "grok-4.5" || got[1] != buildVideoModel {
		t.Fatalf("super primary catalog = %#v", got)
	}
	// Super + fallback 目录已含 1.5：幂等去重，其它模型不变。
	got = adapter.NormalizeAccountModelCapabilities(
		[]string{"grok-4.5", buildVideoModel, "grok-code-fast-1", buildVideoModel},
		&account.Billing{OnDemandCap: 10},
	)
	if len(got) != 3 || got[0] != "grok-4.5" || got[1] != buildVideoModel || got[2] != "grok-code-fast-1" {
		t.Fatalf("super fallback catalog = %#v", got)
	}
	// Free：即使目录暴露 1.5 也必须移除。
	got = adapter.NormalizeAccountModelCapabilities([]string{"grok-4.5", buildVideoModel}, &account.Billing{Used: 1, PlanName: "free"})
	if len(got) != 1 || got[0] != "grok-4.5" {
		t.Fatalf("free catalog = %#v", got)
	}
	// Unknown（无 Billing）：与 Free 相同，移除 1.5。
	got = adapter.NormalizeAccountModelCapabilities([]string{buildVideoModel, "grok-4.5"}, nil)
	if len(got) != 1 || got[0] != "grok-4.5" {
		t.Fatalf("unknown catalog = %#v", got)
	}
	// 不得依赖 BuildAPIFallback；空目录 + Super 仅补 1.5。
	got = adapter.NormalizeAccountModelCapabilities(nil, &account.Billing{PlanName: "SuperGrok", CreditUsagePercent: 1})
	if len(got) != 1 || got[0] != buildVideoModel {
		t.Fatalf("super empty catalog = %#v", got)
	}
}

func TestGrokSessionIDFollowsConversationIdentity(t *testing.T) {
	explicit := "019f6b02-5bae-7cf3-b26e-73e85c861749"
	if value, err := grokSessionID(explicit); err != nil || value != explicit {
		t.Fatalf("explicit session = %q, %v", value, err)
	}
	first, err := grokSessionID("client-conversation")
	if err != nil {
		t.Fatal(err)
	}
	second, err := grokSessionID("client-conversation")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := uuid.Parse(first)
	if err != nil || parsed.Version() != uuid.Version(8) || first != second {
		t.Fatalf("derived sessions = %q %q, %v", first, second, err)
	}
	generated, err := grokSessionID("")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err = uuid.Parse(generated)
	if err != nil || parsed.Version() != uuid.Version(7) {
		t.Fatalf("generated session = %q, %v", generated, err)
	}
}

func TestInferenceIdentityIsConversationScopedNotAccountScoped(t *testing.T) {
	adapter := NewAdapter(Config{ClientVersion: "0.2.102", ClientIdentifier: "grok-shell", TokenAuth: "xai-grok-cli", UserAgent: "grok-shell/0.2.102 (linux; x86_64)"}, nil)
	build := func(accountID uint64, conversation string) http.Header {
		request := httptest.NewRequest(http.MethodPost, "https://cli-chat-proxy.grok.com/v1/responses", nil)
		if err := adapter.applyHeaders(request, account.Credential{ID: accountID}, "token", "grok-4.5", conversation, true); err != nil {
			t.Fatal(err)
		}
		return request.Header
	}
	first := build(1, "conversation-a")
	second := build(2, "conversation-a")
	third := build(1, "conversation-b")
	if first.Get("x-grok-agent-id") != second.Get("x-grok-agent-id") || first.Get("x-grok-session-id") != second.Get("x-grok-session-id") {
		t.Fatalf("same conversation identity changed across accounts: first=%#v second=%#v", first, second)
	}
	if first.Get("x-grok-req-id") == second.Get("x-grok-req-id") {
		t.Fatalf("request ID was reused: %q", first.Get("x-grok-req-id"))
	}
	if first.Get("x-grok-session-id") == third.Get("x-grok-session-id") {
		t.Fatalf("different conversations shared session ID: %q", first.Get("x-grok-session-id"))
	}
}

func TestForwardResponseSupportsResourceMethodsAndQuery(t *testing.T) {
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{BaseURL: "https://cli-chat-proxy.grok.com/v1", ClientVersion: "0.2.102", ClientIdentifier: "grok-shell", TokenAuth: "xai-grok-cli", UserAgent: "grok-shell/0.2.102 (linux; x86_64)"}, cipher)
	methods := []string{http.MethodGet, http.MethodDelete}
	next := 0
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != methods[next] || request.URL.Path != "/v1/responses/resp_1" || request.URL.RawQuery != "include=reasoning.encrypted_content" {
			t.Fatalf("request = %s %s", request.Method, request.URL.RequestURI())
		}
		if request.Header.Get("Accept") != "application/json" || request.Header.Get("Content-Type") != "" {
			t.Fatalf("headers = %#v", request.Header)
		}
		if request.Body != nil {
			t.Fatal("resource request unexpectedly gained a body")
		}
		next++
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"resp_1"}`)), Request: request}, nil
	})

	for _, method := range methods {
		response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
			Credential:     account.Credential{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, EncryptedAccessToken: encrypted},
			Method:         method,
			Path:           "/responses/resp_1?include=reasoning.encrypted_content",
			PromptCacheKey: "resource-cache-key",
		})
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
	}
	if next != len(methods) {
		t.Fatalf("requests = %d", next)
	}
}

func TestForwardResponseDecodesExplicitGzipResponse(t *testing.T) {
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write([]byte(`{"id":"resp_gzip"}`)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{BaseURL: "https://cli-chat-proxy.grok.com/v1"}, cipher)
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Accept-Encoding") != "gzip" {
			t.Fatalf("Accept-Encoding = %q", request.Header.Get("Accept-Encoding"))
		}
		return &http.Response{
			StatusCode: http.StatusOK, Status: "200 OK",
			Header: http.Header{"Content-Encoding": []string{"gzip"}, "Content-Length": []string{"999"}},
			Body:   io.NopCloser(bytes.NewReader(compressed.Bytes())), Request: request,
		}, nil
	})
	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: account.Credential{ID: 8, EncryptedAccessToken: encrypted}, Method: http.MethodPost, Path: "/responses",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"id":"resp_gzip"}` || response.Header.Get("Content-Encoding") != "" || response.Header.Get("Content-Length") != "" {
		t.Fatalf("body=%q headers=%#v", body, response.Header)
	}
}

func TestForwardResponseDowngradesServerToolSearchBeforeUpstream(t *testing.T) {
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{BaseURL: "https://cli-chat-proxy.grok.com/v1"}, cipher)
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if _, exists := payload["tools"]; exists {
			t.Fatalf("server tool_search 未从上游请求移除: %#v", payload)
		}
		return &http.Response{
			StatusCode: http.StatusOK, Status: "200 OK",
			Header:  http.Header{"Content-Type": []string{"application/json"}},
			Body:    io.NopCloser(strings.NewReader(`{"id":"resp_search"}`)),
			Request: request,
		}, nil
	})

	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: account.Credential{EncryptedAccessToken: encrypted},
		Method:     http.MethodPost, Path: "/responses", Model: "grok-4.5",
		NormalizeBody: true, Operation: conversation.OperationResponses,
		Body: []byte(`{"model":"public","input":"hello","tools":[{"type":"tool_search"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if !strings.Contains(response.Header.Get("X-Grok2API-Compatibility-Warnings"), "server_tool_search_eager_loaded") {
		t.Fatalf("compatibility warnings = %q", response.Header.Get("X-Grok2API-Compatibility-Warnings"))
	}
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["id"] != "resp_search" {
		t.Fatalf("response = %#v", payload)
	}
}

func TestForwardResponseRestoresNamespaceResponse(t *testing.T) {
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{BaseURL: "https://cli-chat-proxy.grok.com/v1"}, cipher)
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		tools := payload["tools"].([]any)
		if len(tools) != 1 || tools[0].(map[string]any)["name"] != "crm__lookup" {
			t.Fatalf("上游 tools = %#v", tools)
		}
		return &http.Response{
			StatusCode: http.StatusOK, Status: "200 OK",
			Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
				"id":"resp_1","object":"response",
				"tools":[{"type":"function","name":"crm__lookup"}],
				"output":[{"type":"function_call","call_id":"call_1","name":"crm__lookup","arguments":"{}"}]
			}`)),
			Request: request,
		}, nil
	})

	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: account.Credential{EncryptedAccessToken: encrypted},
		Method:     http.MethodPost, Path: "/responses", Model: "grok-4.5",
		NormalizeBody: true, Operation: conversation.OperationResponses,
		Body: []byte(`{
			"model":"public","input":"lookup",
			"tools":[{"type":"namespace","name":"crm","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}]
		}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.Header.Get("X-Grok2API-Compatibility-Warnings") != "namespace_flattened" {
		t.Fatalf("compatibility warnings = %q", response.Header.Get("X-Grok2API-Compatibility-Warnings"))
	}
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	call := payload["output"].([]any)[0].(map[string]any)
	if call["name"] != "lookup" || call["namespace"] != "crm" {
		t.Fatalf("下游 function_call = %#v", call)
	}
	tools := payload["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["type"] != "namespace" {
		t.Fatalf("下游 tools = %#v", tools)
	}
}

func TestForwardResponsePreservesClaudeCodeMessagesOptions(t *testing.T) {
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{BaseURL: "https://cli-chat-proxy.grok.com/v1"}, cipher)
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["instructions"] != "legacy system" || payload["store"] != false || payload["reasoning"].(map[string]any)["effort"] != "high" || payload["prompt_cache_key"] != "messages-cache-key" {
			t.Fatalf("upstream payload = %#v", payload)
		}
		expectedSessionID, err := grokSessionID("messages-cache-key")
		if err != nil {
			t.Fatal(err)
		}
		if request.Header.Get("x-grok-conv-id") != expectedSessionID || request.Header.Get("x-grok-session-id") != expectedSessionID {
			t.Fatalf("prompt cache headers = %#v", request.Header)
		}
		return &http.Response{
			StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
				"id":"resp_1","model":"grok-4.5","status":"completed",
				"output":[
					{"type":"reasoning","summary":[{"type":"summary_text","text":"thought"}],"encrypted_content":"signature"},
					{"type":"message","content":[{"type":"output_text","text":"ABCSTOPXYZ"}]}
				]
			}`)),
			Request: request,
		}, nil
	})

	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: account.Credential{EncryptedAccessToken: encrypted},
		Method:     http.MethodPost, Path: "/responses", Model: "grok-4.5", NormalizeBody: true, PromptCacheKey: "messages-cache-key",
		Operation: conversation.OperationMessages,
		Body: []byte(`{
			"model":"public","max_tokens":256,"stop_sequences":["STOP"],
			"thinking":{"type":"enabled","budget_tokens":20000},
			"messages":[{"role":"system","content":"legacy system"},{"role":"user","content":"hello"}]
		}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	content := payload["content"].([]any)
	if payload["stop_reason"] != "stop_sequence" || payload["stop_sequence"] != "STOP" || content[0].(map[string]any)["type"] != "thinking" || content[1].(map[string]any)["text"] != "ABC" {
		t.Fatalf("messages response = %#v", payload)
	}
}

func TestForwardResponseMapsClaudeCodeWebSearchEndToEnd(t *testing.T) {
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{BaseURL: "https://cli-chat-proxy.grok.com/v1"}, cipher)
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		tools, _ := payload["tools"].([]any)
		if len(tools) != 1 || tools[0].(map[string]any)["type"] != "web_search" || payload["tool_choice"] != "required" {
			t.Fatalf("upstream web search payload = %#v", payload)
		}
		domains := tools[0].(map[string]any)["filters"].(map[string]any)["allowed_domains"].([]any)
		if len(domains) != 1 || domains[0] != "doc.rust-lang.org" {
			t.Fatalf("upstream web search filters = %#v", tools[0])
		}
		return &http.Response{
			StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
				"id":"resp_search","model":"grok-4.5","status":"completed",
				"output":[
					{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"rust tutorials","sources":[{"url":"https://doc.rust-lang.org"}]}},
					{"type":"message","content":[{"type":"output_text","text":"Here you go.","annotations":[{"type":"url_citation","url":"https://doc.rust-lang.org","title":"The Rust Book"}]}]}
				],
					"usage":{"input_tokens":7,"output_tokens":5,"total_tokens":12,"cost_in_usd_ticks":12000,"context_details":{"input_tokens":6,"output_tokens":4}}
			}`)),
			Request: request,
		}, nil
	})

	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: account.Credential{EncryptedAccessToken: encrypted},
		Method:     http.MethodPost, Path: "/responses", Model: "grok-4.5", NormalizeBody: true,
		Operation: conversation.OperationMessages,
		Body: []byte(`{
			"model":"public","max_tokens":256,
			"messages":[{"role":"user","content":"Perform a web search for the query: rust tutorials"}],
			"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":8,"allowed_domains":["doc.rust-lang.org"]}],
			"tool_choice":{"type":"tool","name":"web_search"}
		}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	content := payload["content"].([]any)
	if len(content) != 3 || content[0].(map[string]any)["type"] != "server_tool_use" || content[1].(map[string]any)["type"] != "web_search_tool_result" || content[2].(map[string]any)["text"] != "Here you go." {
		t.Fatalf("messages web search response = %#v", payload)
	}
	use := content[0].(map[string]any)
	if use["input"].(map[string]any)["query"] != "rust tutorials" || content[1].(map[string]any)["tool_use_id"] != use["id"] {
		t.Fatalf("web search block linkage = %#v", content)
	}
	hits := content[1].(map[string]any)["content"].([]any)
	if len(hits) != 1 || hits[0].(map[string]any)["title"] != "The Rust Book" {
		t.Fatalf("web search hits = %#v", hits)
	}
	serverUsage := payload["usage"].(map[string]any)["server_tool_use"].(map[string]any)
	if serverUsage["web_search_requests"] != float64(1) || payload["stop_reason"] != "end_turn" {
		t.Fatalf("messages web search usage = %#v", payload)
	}
	usage := payload["usage"].(map[string]any)
	if usage["cost_in_usd_ticks"] != float64(12000) || usage["context_details"].(map[string]any)["input_tokens"] != float64(6) {
		t.Fatalf("messages upstream usage = %#v", usage)
	}
}

func TestForwardResponseInjectsPromptCacheKeyAfterChatConversion(t *testing.T) {
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{BaseURL: "https://cli-chat-proxy.grok.com/v1"}, cipher)
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		expectedSessionID, err := grokSessionID("chat-cache-key")
		if err != nil {
			t.Fatal(err)
		}
		if payload["prompt_cache_key"] != "chat-cache-key" || request.Header.Get("x-grok-conv-id") != expectedSessionID || request.Header.Get("x-grok-session-id") != expectedSessionID {
			t.Fatalf("prompt cache request: payload=%#v headers=%#v", payload, request.Header)
		}
		return &http.Response{
			StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
				"id":"resp_1","model":"grok-4.5","status":"completed",
				"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],
				"usage":{"input_tokens":11,"output_tokens":2,"cost_in_usd_ticks":7000,"context_details":{"input_tokens":10,"output_tokens":2}}
			}`)),
			Request: request,
		}, nil
	})

	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: account.Credential{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, EncryptedAccessToken: encrypted},
		Method:     http.MethodPost, Path: "/responses", Model: "grok-4.5", NormalizeBody: true,
		Operation: conversation.OperationChat, PromptCacheKey: "chat-cache-key",
		Body: []byte(`{"model":"public","messages":[{"role":"user","content":"hello"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	usage := payload["usage"].(map[string]any)
	if payload["object"] != "chat.completion" || usage["prompt_tokens"] != float64(11) || usage["cost_in_usd_ticks"] != float64(7000) || usage["context_details"].(map[string]any)["input_tokens"] != float64(10) {
		t.Fatalf("chat response = %#v", payload)
	}
}
