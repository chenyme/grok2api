package console

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestCatalogContainsAllConsoleModelsAndAliases(t *testing.T) {
	expected := map[string]string{
		"Console/grok-4.3":                     "grok-4.3",
		"Console/grok-4.20-0309":               "grok-4.20-0309",
		"Console/grok-4.20-0309-reasoning":     "grok-4.20-0309-reasoning",
		"Console/grok-4.20-0309-non-reasoning": "grok-4.20-0309-non-reasoning",
		"Console/grok-4.20-multi-agent-0309":   "grok-4.20-multi-agent-0309",
		"Console/grok-build-0.1":               "grok-build-0.1",
	}
	routes := Routes()
	if len(routes) != len(expected) {
		t.Fatalf("routes = %d, want %d", len(routes), len(expected))
	}
	for _, route := range routes {
		if route.Provider != account.ProviderConsole || route.Capability != modeldomain.CapabilityResponses || !route.Enabled {
			t.Fatalf("invalid route: %#v", route)
		}
		if expected[route.PublicID] != route.UpstreamModel {
			t.Fatalf("route %q = %q", route.PublicID, route.UpstreamModel)
		}
	}
	aliases := Aliases()
	if len(aliases) != 13 {
		t.Fatalf("aliases = %d, want 13", len(aliases))
	}
	registry := provider.NewRegistry(NewAdapter(Config{}, nil, nil))
	if registry.SupportsStoredResponses(account.ProviderConsole) {
		t.Fatal("console must not advertise stored Responses support")
	}
	for _, name := range []string{
		"grok-4.3-console", "grok-4.20-0309-console", "grok-4.20-0309-reasoning-console",
		"grok-4.20-0309-non-reasoning-console", "grok-4.20-multi-agent-console", "grok-build-console",
		"grok-4.3-low", "grok-4.3-medium", "grok-4.3-high",
		"grok-4.20-multi-agent-low", "grok-4.20-multi-agent-medium", "grok-4.20-multi-agent-high", "grok-4.20-multi-agent-xhigh",
	} {
		alias, ok := registry.ResolveModelAlias(name)
		if !ok {
			t.Fatalf("alias %q missing", name)
		}
		if !strings.HasPrefix(alias.PublicModel, "Console/") {
			t.Fatalf("alias %q targets non-canonical model %q", name, alias.PublicModel)
		}
	}
}

func TestNormalizeRequestAppliesConsoleContract(t *testing.T) {
	spec, ok := Resolve("grok-4.3")
	if !ok {
		t.Fatal("grok-4.3 missing")
	}
	body, err := normalizeRequest([]byte(`{
		"model":"grok-4.3",
		"metadata":{"private":"value"},
		"reasoning":{"effort":"xhigh"},
		"tools":[{"type":"web_search","custom":true},{"type":"function","name":"lookup","parameters":{"type":"object"}}]
	}`), spec)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["model"] != "grok-4.3" || payload["store"] != false || payload["metadata"] != nil {
		t.Fatalf("payload = %#v", payload)
	}
	if payload["max_output_tokens"] != float64(1_000_000) {
		t.Fatalf("max_output_tokens = %#v", payload["max_output_tokens"])
	}
	reasoning, _ := payload["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" {
		t.Fatalf("reasoning = %#v", reasoning)
	}
	tools, _ := payload["tools"].([]any)
	if len(tools) != 3 || toolIdentity(tools[0]) != "web_search" || toolIdentity(tools[1]) != "x_search" || toolIdentity(tools[2]) != "function:lookup" {
		t.Fatalf("tools = %#v", tools)
	}
	for _, body := range []string{
		`{"model":"grok-4.3","store":true,"input":"hello"}`,
		`{"model":"grok-4.3","previous_response_id":"resp_1","input":"hello"}`,
	} {
		if _, err := normalizeRequest([]byte(body), spec); err == nil {
			t.Fatalf("expected stateless validation error for %s", body)
		}
	}
}

func TestConsoleImportAcceptsJSONPlainTextAndCookieFormat(t *testing.T) {
	values, err := parseImportedCredentials([]byte("sso=token-one; sso-rw=token-one\ntoken-two\ntoken-two\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0].AccessToken != "token-one" || values[1].AccessToken != "token-two" {
		t.Fatalf("plain values = %#v", values)
	}
	values, err = parseImportedCredentials([]byte(`{"provider":"grok_console","accounts":[{"name":"console-a","sso_token":"token-a"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].Provider != account.ProviderConsole || values[0].AuthType != account.AuthTypeSSO || values[0].Name != "console-a" || values[0].AccessToken != "token-a" {
		t.Fatalf("json values = %#v", values)
	}
}

func TestConsoleRetryAfterParsesCompoundDuration(t *testing.T) {
	if value := consoleRetryAfter([]byte(`Rate limit reached. Resets in: 1h 2m 3s`)); value != time.Hour+2*time.Minute+3*time.Second {
		t.Fatalf("retry after = %s", value)
	}
	if value := consoleRetryAfter([]byte(`ordinary error`)); value != 0 {
		t.Fatalf("ordinary retry after = %s", value)
	}
}

func TestAdapterForwardsConsoleHeadersAndNormalizedBody(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/responses" || request.Method != http.MethodPost {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer anonymous" || request.Header.Get("x-cluster") != "https://us-east-1.api.x.ai" {
			t.Errorf("headers = %#v", request.Header)
		}
		cookie := request.Header.Get("Cookie")
		if !strings.Contains(cookie, "sso=test-sso") || !strings.Contains(cookie, "sso-rw=test-sso") {
			t.Errorf("cookie = %q", cookie)
		}
		if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"id":"resp_console","object":"response","status":"completed","output":[]}`)
	}))
	defer server.Close()

	adapter, credential := newConsoleTestAdapter(t, server.URL)
	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: credential,
		Method:     http.MethodPost, Path: "/responses", Model: "grok-4.3", Operation: "responses", NormalizeBody: true,
		Body: []byte(`{"model":"grok-4.3","input":"hello","metadata":{"drop":true}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !bytes.Contains(data, []byte(`"resp_console"`)) {
		t.Fatalf("status=%d body=%s", response.StatusCode, data)
	}
	if received["model"] != "grok-4.3" || received["store"] != false || received["metadata"] != nil {
		t.Fatalf("received = %#v", received)
	}
}

func TestAdapterPreservesConversationRateLimitStatusAndProtocol(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(writer, "Rate limit reached. Resets in: 1h 2m 3s")
	}))
	defer server.Close()
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	tests := []struct {
		operation string
		body      string
	}{
		{operation: conversation.OperationChat, body: `{"model":"grok-4.3","messages":[{"role":"user","content":"hello"}],"stream":true}`},
		{operation: conversation.OperationMessages, body: `{"model":"grok-4.3","max_tokens":64,"messages":[{"role":"user","content":"hello"}],"stream":true}`},
	}
	for _, test := range tests {
		t.Run(test.operation, func(t *testing.T) {
			response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
				Credential: credential, Method: http.MethodPost, Path: "/responses", Model: "grok-4.3",
				Operation: test.operation, NormalizeBody: true, Streaming: true, Body: []byte(test.body),
			})
			if err != nil {
				t.Fatal(err)
			}
			data, readErr := io.ReadAll(response.Body)
			if readErr != nil {
				t.Fatal(readErr)
			}
			_ = response.Body.Close()
			if response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Retry-After") != "3723" {
				t.Fatalf("status=%d retry-after=%q body=%s", response.StatusCode, response.Header.Get("Retry-After"), data)
			}
			var payload map[string]any
			if err := json.Unmarshal(data, &payload); err != nil {
				t.Fatalf("invalid compatible error JSON: %v, body=%s", err, data)
			}
			if test.operation == conversation.OperationMessages && payload["type"] != "error" {
				t.Fatalf("messages error = %#v", payload)
			}
			errorObject, _ := payload["error"].(map[string]any)
			if errorObject["type"] != "rate_limit_error" || !strings.Contains(errorObject["message"].(string), "Rate limit reached") {
				t.Fatalf("compatible error = %#v", payload)
			}
		})
	}
}

func newConsoleTestAdapter(t *testing.T, baseURL string) (*Adapter, account.Credential) {
	t.Helper()
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("test-sso")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{BaseURL: baseURL, UserAgent: "console-test", TimeoutSeconds: 5}, infraegress.NewManager(consoleEgressRepositoryStub{}, cipher), cipher)
	credential := account.Credential{ID: 1, Provider: account.ProviderConsole, AuthType: account.AuthTypeSSO, EncryptedAccessToken: encrypted}
	return adapter, credential
}

type consoleEgressRepositoryStub struct{}

func (consoleEgressRepositoryStub) ListEgressNodes(context.Context, egressdomain.Scope, repository.SortQuery) ([]egressdomain.Node, error) {
	return nil, nil
}

func (consoleEgressRepositoryStub) GetEgressNode(context.Context, uint64) (egressdomain.Node, error) {
	return egressdomain.Node{}, errors.New("not found")
}

func (consoleEgressRepositoryStub) CreateEgressNode(context.Context, egressdomain.Node) (egressdomain.Node, error) {
	return egressdomain.Node{}, errors.New("unsupported")
}

func (consoleEgressRepositoryStub) UpdateEgressNode(context.Context, egressdomain.Node) (egressdomain.Node, error) {
	return egressdomain.Node{}, errors.New("unsupported")
}

func (consoleEgressRepositoryStub) DeleteEgressNode(context.Context, uint64) error {
	return errors.New("unsupported")
}
