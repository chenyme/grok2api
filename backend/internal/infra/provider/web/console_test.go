package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestConsoleCatalogModelsResolveToSharedTransport(t *testing.T) {
	cases := map[string]string{
		"grok-4.20-multi-agent-console": "",
		"grok-4.20-multi-agent-low":    "low",
		"grok-4.20-multi-agent-medium": "medium",
		"grok-4.20-multi-agent-high":   "high",
		"grok-4.20-multi-agent-xhigh":  "xhigh",
	}
	for model, effort := range cases {
		spec, ok := Resolve(model)
		if !ok {
			t.Fatalf("missing Console model %s", model)
		}
		if spec.Transport != consoleResponsesTransport || spec.ProtocolModel != "grok-4.20-multi-agent-0309" || spec.ReasoningEffort != effort {
			t.Fatalf("spec for %s = %#v", model, spec)
		}
	}
}

func TestBuildConsolePayloadAppliesDefaultsAndFixedEffort(t *testing.T) {
	body := []byte(`{"model":"grok-4.20-multi-agent-high","input":"hello","stream":false,"reasoning":{"effort":"low"}}`)
	spec, _ := Resolve("grok-4.20-multi-agent-high")
	data, streaming, err := buildConsolePayload(body, spec, false)
	if err != nil {
		t.Fatal(err)
	}
	if streaming {
		t.Fatal("non-stream request unexpectedly became streaming")
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["model"] != "grok-4.20-multi-agent-0309" || payload["max_output_tokens"] != float64(2_000_000) || payload["store"] != false {
		t.Fatalf("payload defaults = %#v", payload)
	}
	reasoning, _ := payload["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" {
		t.Fatalf("reasoning = %#v", reasoning)
	}
	tools, _ := payload["tools"].([]any)
	if len(tools) != 2 || payload["tool_choice"] != "auto" {
		t.Fatalf("tools = %#v choice=%#v", tools, payload["tool_choice"])
	}
	include, _ := payload["include"].([]any)
	if len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("include = %#v", include)
	}
}

func TestConsoleForwardUsesConsoleHeadersAndConvertsChatResponse(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/responses" {
			http.NotFound(writer, request)
			return
		}
		if request.Header.Get("Authorization") != "Bearer test-sso" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		cookie := request.Header.Get("Cookie")
		if !strings.Contains(cookie, "sso=test-sso") || !strings.Contains(cookie, "sso-rw=test-sso") {
			t.Errorf("cookie = %q", cookie)
		}
		if request.Header.Get("Origin") != "https://console.x.ai" || request.Header.Get("Referer") != "https://console.x.ai/" || request.Header.Get("x-cluster") == "" {
			t.Errorf("console headers = %#v", request.Header)
		}
		if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"id":"resp_console","object":"response","created_at":1,"model":"grok-4.20-multi-agent-0309","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"console works"}]}],"usage":{"input_tokens":2,"output_tokens":3}}`)
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
	adapter := NewAdapter(Config{ConsoleBaseURL: server.URL}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)
	body := []byte(`{"model":"grok-4.20-multi-agent-low","messages":[{"role":"user","content":"hello"}],"stream":false}`)
	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: account.Credential{ID: 7, EncryptedAccessToken: encrypted}, Method: http.MethodPost,
		Path: "/responses", Body: body, Model: "grok-4.20-multi-agent-low", Operation: "chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(data), "console works") || !strings.Contains(string(data), "chat.completion") {
		t.Fatalf("status=%d body=%s", response.StatusCode, data)
	}
	if received["model"] != "grok-4.20-multi-agent-0309" {
		t.Fatalf("upstream payload = %#v", received)
	}
}

func TestConsoleForwardConvertsStreamingMessagesResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"grok-4.20-multi-agent-0309\",\"status\":\"in_progress\"}}\n\n")
		_, _ = io.WriteString(writer, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
		_, _ = io.WriteString(writer, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"grok-4.20-multi-agent-0309\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
	}))
	defer server.Close()
	cipher, _ := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	encrypted, _ := cipher.Encrypt("test-sso")
	adapter := NewAdapter(Config{ConsoleBaseURL: server.URL}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)
	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: account.Credential{ID: 8, EncryptedAccessToken: encrypted}, Method: http.MethodPost,
		Path: "/responses", Body: []byte(`{"model":"grok-4.20-multi-agent-console","messages":[{"role":"user","content":"hello"}],"stream":true,"max_tokens":1024}`),
		Model: "grok-4.20-multi-agent-console", Operation: "messages", Streaming: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(data), "content_block_delta") || !strings.Contains(string(data), "hi") {
		t.Fatalf("messages stream = %s", data)
	}
}
