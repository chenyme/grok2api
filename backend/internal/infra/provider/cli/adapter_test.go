package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
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
		if r.Header.Get("Authorization") != "Bearer access-token" || r.Header.Get("x-grok-client-version") != "0.2.99" || r.Header.Get("x-grok-client-identifier") != "grok-shell" || r.Header.Get("User-Agent") != "grok-shell/0.2.99 (linux; x86_64)" || r.Header.Get("x-grok-conv-id") != "isolated-key" {
			t.Fatalf("headers = %#v", r.Header)
		}
		requestID := r.Header.Get("x-grok-req-id")
		sessionID := r.Header.Get("x-grok-session-id")
		if r.Header.Get("x-grok-client-surface") != "tui" || r.Header.Get("x-grok-client-name") != "grok-shell" || len(r.Header.Get("x-grok-agent-id")) != 32 || len(sessionID) != 36 {
			t.Fatalf("client identity headers = %#v", r.Header)
		}
		if r.Header.Get("x-grok-conversation-id") != "isolated-key" || len(requestID) != 32 || r.Header.Get("x-grok-request-id") != requestID || r.Header.Get("x-grok-session-id-legacy") != sessionID {
			t.Fatalf("request identity headers = %#v", r.Header)
		}
		if r.Header.Get("x-userid") != "user-123" || r.Header.Get("Accept-Encoding") != "gzip" || len(r.Header.Get("traceparent")) != 55 {
			t.Fatalf("protocol headers = %#v", r.Header)
		}
		if values, ok := r.Header["Tracestate"]; !ok || len(values) != 1 || values[0] != "" {
			t.Fatalf("tracestate = %#v", values)
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
	adapter := NewAdapter(Config{BaseURL: "https://api.x.ai/v1", ClientVersion: "0.2.99", ClientIdentifier: "grok-shell", TokenAuth: "xai-grok-cli", UserAgent: "grok-shell/0.2.99 (linux; x86_64)"}, cipher)
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
	if captured["model"] != "grok-4.5" || captured["prompt_cache_key"] != "isolated-key" || len(input) != 1 || input[0].(map[string]any)["encrypted_content"] != "cipher" {
		t.Fatalf("captured = %#v", captured)
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
	adapter := NewAdapter(Config{BaseURL: "https://cli-chat-proxy.grok.com/v1", ClientVersion: "0.2.99", ClientIdentifier: "grok-shell", TokenAuth: "xai-grok-cli", UserAgent: "grok-shell/0.2.99 (linux; x86_64)"}, cipher)
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

func TestForwardResponseRejectsHostedToolSearchBeforeUpstream(t *testing.T) {
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
		t.Fatalf("不支持的服务端 Tool Search 不应请求上游: %s", request.URL)
		return nil, nil
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
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", response.StatusCode)
	}
	var payload struct {
		Error struct {
			Type  string `json:"type"`
			Param string `json:"param"`
			Code  string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error.Type != "invalid_request_error" || payload.Error.Param != "tools[0].execution" || payload.Error.Code != "unsupported_parameter" {
		t.Fatalf("error = %#v", payload.Error)
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
		if request.Header.Get("x-grok-conv-id") != "messages-cache-key" {
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
		if payload["prompt_cache_key"] != "chat-cache-key" || request.Header.Get("x-grok-conv-id") != "chat-cache-key" {
			t.Fatalf("prompt cache request: payload=%#v headers=%#v", payload, request.Header)
		}
		return &http.Response{
			StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": []string{"application/json"}},
			Body:    io.NopCloser(strings.NewReader(`{"id":"resp_1","model":"grok-4.5","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)),
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
}
