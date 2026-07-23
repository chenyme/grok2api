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

func TestSyncAccountIdentityUsesWebSessionWithConsoleCredential(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/auth/session" || request.Method != http.MethodGet {
			http.NotFound(writer, request)
			return
		}
		if request.Header.Get("User-Agent") != infraegress.DefaultUserAgent {
			t.Errorf("user agent = %q", request.Header.Get("User-Agent"))
		}
		if request.Header.Get("Cookie") != "sso=test-sso; sso-rw=test-sso; cf_clearance=clear" {
			t.Errorf("cookie = %q", request.Header.Get("Cookie"))
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"user":{"sub":"console-user","email":"console@example.com","teamId":"team-1"}}`))
	}))
	t.Cleanup(server.Close)
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	token, _ := cipher.Encrypt("test-sso")
	cookies, _ := cipher.Encrypt("cf_clearance=clear")
	adapter := NewAdapter(Config{SessionBaseURL: server.URL}, infraegress.NewManager(consoleEgressRepositoryStub{}, cipher), cipher)
	identity, err := adapter.SyncAccountIdentity(context.Background(), account.Credential{
		ID: 1, Provider: account.ProviderConsole, AuthType: account.AuthTypeSSO,
		EncryptedAccessToken: token, EncryptedCloudflareCookie: cookies,
	})
	if err != nil {
		t.Fatal(err)
	}
	if identity.UserID != "console-user" || identity.Email != "console@example.com" || identity.TeamID != "team-1" {
		t.Fatalf("identity = %#v", identity)
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
	if reasoning["effort"] != "xhigh" {
		t.Fatalf("reasoning = %#v", reasoning)
	}
	include, _ := payload["include"].([]any)
	if len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("include = %#v", include)
	}
	tools, _ := payload["tools"].([]any)
	if len(tools) != 3 || toolIdentity(tools[0]) != "web_search" || toolIdentity(tools[1]) != "x_search" || toolIdentity(tools[2]) != "function:lookup" {
		t.Fatalf("tools = %#v", tools)
	}
	webSearch, _ := tools[0].(map[string]any)
	if webSearch["custom"] != nil || webSearch["enable_image_understanding"] != true {
		t.Fatalf("web_search = %#v", webSearch)
	}
	stateless, err := normalizeRequest([]byte(`{"model":"grok-4.3","store":true,"previous_response_id":"resp_1","service_tier":"priority","input":"hello"}`), spec)
	if err != nil {
		t.Fatal(err)
	}
	var statelessPayload map[string]any
	if json.Unmarshal(stateless, &statelessPayload) != nil || statelessPayload["store"] != false || statelessPayload["previous_response_id"] != nil || statelessPayload["service_tier"] != nil {
		t.Fatalf("stateless payload = %#v", statelessPayload)
	}
}

func TestNormalizeRequestAppliesConsoleCompatibilityBoundary(t *testing.T) {
	spec, ok := Resolve("grok-4.20-0309")
	if !ok {
		t.Fatal("grok-4.20-0309 missing")
	}
	body, err := normalizeRequest([]byte(`{
		"model":"public",
		"response_format":{"type":"json_schema","json_schema":{"name":"answer","strict":true,"schema":{"type":"object"}}},
		"input":[
			{"type":"reasoning","content":[{"text":"prior thought"}]},
			{"type":"message","role":"user","content":[
				{"type":"output_text","text":"hello"},
				{"type":"image_url","image_url":{"url":"https://example.com/image.png"}}
			]}
		],
		"tools":[
			{"type":"namespace","name":"crm","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]},
			{"type":"web_search","external_web_access":true}
		],
		"tool_choice":"required"
	}`), spec)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["response_format"] != nil || payload["reasoning"] != nil || payload["tool_choice"] != "auto" {
		t.Fatalf("payload boundary = %#v", payload)
	}
	include, _ := payload["include"].([]any)
	if len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("include = %#v", include)
	}
	text, _ := payload["text"].(map[string]any)
	format, _ := text["format"].(map[string]any)
	if format["type"] != "json_schema" || format["name"] != "answer" || format["json_schema"] != nil {
		t.Fatalf("text.format = %#v", format)
	}
	input, _ := payload["input"].([]any)
	reasoning := input[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if reasoning["type"] != "reasoning_text" {
		t.Fatalf("reasoning content = %#v", reasoning)
	}
	parts := input[1].(map[string]any)["content"].([]any)
	if parts[0].(map[string]any)["type"] != "input_text" || parts[1].(map[string]any)["type"] != "input_image" || parts[1].(map[string]any)["image_url"] != "https://example.com/image.png" {
		t.Fatalf("message parts = %#v", parts)
	}
	tools, _ := payload["tools"].([]any)
	if len(tools) != 2 || toolIdentity(tools[0]) != "web_search" || toolIdentity(tools[1]) != "x_search" {
		t.Fatalf("sanitized tools = %#v", tools)
	}
	if tools[0].(map[string]any)["external_web_access"] != nil {
		t.Fatalf("unsupported web search controls leaked: %#v", tools[0])
	}
}

func TestNormalizeReasoningPreservesReferenceEfforts(t *testing.T) {
	for input, want := range map[string]string{
		"none": "none", "minimal": "low", "low": "low", "medium": "medium",
		"high": "high", "xhigh": "xhigh", "max": "xhigh",
	} {
		if got := normalizeEffort(input); got != want {
			t.Fatalf("normalizeEffort(%q) = %q, want %q", input, got, want)
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
	values, err = parseImportedCredentials([]byte(`{"provider":"grok_console","accounts":[{"name":"console-a","sso_token":"token-a","cloudflare_cookies":"cf_clearance=abc"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].Provider != account.ProviderConsole || values[0].AuthType != account.AuthTypeSSO || values[0].Name != "console-a" || values[0].AccessToken != "token-a" {
		t.Fatalf("json values = %#v", values)
	}
	if values[0].CloudflareCookies != "cf_clearance=abc" {
		t.Fatalf("cloudflare cookies = %q", values[0].CloudflareCookies)
	}
}

func TestConsoleImportAcceptsJSONLines(t *testing.T) {
	data := []byte("\xef\xbb\xbf{\"name\":\"first\",\"sso_token\":\"token-one\",\"email\":\"one@example.com\"}\r\n\r\n" +
		"{\"name\":\"second\",\"token\":\"token-two\",\"user_id\":\"user-two\"}\r\n")
	values, err := parseImportedCredentials(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0].AccessToken != "token-one" || values[0].Email != "one@example.com" || values[1].AccessToken != "token-two" || values[1].UserID != "user-two" {
		t.Fatalf("credentials = %#v", values)
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

func TestNormalizeRateLimitResponsePrefersRetryAfterHeader(t *testing.T) {
	response := &http.Response{
		Header: http.Header{"Retry-After": {"17"}},
		Body:   io.NopCloser(strings.NewReader("Too many requests for team 00000000-0000-0000-0000-000000000013 and model grok-4.20-multi-agent-0309. Requests per Second (actual/limit): 2/2")),
	}
	_, metadata, err := normalizeRateLimitResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	if metadata == nil || metadata.RetryAfter != 17*time.Second {
		t.Fatalf("metadata = %#v", metadata)
	}
}

func TestParseConsoleRateLimitMetadataRPS(t *testing.T) {
	metadata := parseConsoleRateLimitMetadata([]byte(`{"code":"resource-exhausted","error":"Too many requests for team 00000000-0000-0000-0000-000000000013 and model grok-4.3. Your team's rate limit is — Requests per Second (actual/limit): 2/2."}`))
	if metadata == nil {
		t.Fatal("metadata is nil")
	}
	if metadata.Scope != provider.RateLimitScopeRPS || metadata.Actual != 2 || metadata.Limit != 2 || metadata.RetryAfter != 2*time.Second {
		t.Fatalf("metadata = %#v", metadata)
	}
	if metadata.TeamID != "00000000-0000-0000-0000-000000000013" || metadata.Model != "grok-4.3" {
		t.Fatalf("team/model = %q/%q", metadata.TeamID, metadata.Model)
	}
}

func TestParseConsoleRateLimitMetadataRPM(t *testing.T) {
	body := []byte(`{"error":{"message":"Too many requests for team 00000000-0000-0000-0000-000000000013 and model grok-4.20-multi-agent-0309. Requests per Minute (actual/limit): 101/60. Resets in: 3m 4s"}}`)
	metadata := parseConsoleRateLimitMetadata(body)
	if metadata == nil {
		t.Fatal("metadata is nil")
	}
	if metadata.Scope != provider.RateLimitScopeRPM || metadata.Actual != 101 || metadata.Limit != 60 || metadata.RetryAfter != 3*time.Minute+4*time.Second {
		t.Fatalf("metadata = %#v", metadata)
	}
}

func TestParseConsoleRateLimitMetadataOrdinary429(t *testing.T) {
	if metadata := parseConsoleRateLimitMetadata([]byte(`Rate limit reached. Resets in: 1h`)); metadata != nil {
		t.Fatalf("metadata = %#v", metadata)
	}
}

func TestParseConsoleRateLimitMetadataExtractsTeamAndModel(t *testing.T) {
	metadata := parseConsoleRateLimitMetadata([]byte(`{"message":"Too many requests for team 00000000-0000-0000-0000-000000000013 and model grok-4.20-multi-agent-0309. Requests per Second (actual/limit): 3/2. Resets in: 1s"}`))
	if metadata == nil {
		t.Fatal("metadata is nil")
	}
	if metadata.TeamID != "00000000-0000-0000-0000-000000000013" || metadata.Model != "grok-4.20-multi-agent-0309" {
		t.Fatalf("team/model = %q/%q", metadata.TeamID, metadata.Model)
	}
	if metadata.RetryAfter != 2*time.Second {
		t.Fatalf("retry after = %s", metadata.RetryAfter)
	}
}

func TestAdapterAttachesConsoleRateLimitMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(writer, "Too many requests for team 00000000-0000-0000-0000-000000000013 and model grok-4.20-multi-agent-0309. Requests per Second (actual/limit): 2/2")
	}))
	defer server.Close()
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: credential, Method: http.MethodPost, Path: "/responses", Model: "grok-4.20-multi-agent-0309",
		Operation: "responses", NormalizeBody: true, Body: []byte(`{"model":"grok-4.20-multi-agent-0309","input":"hello"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.RateLimit == nil {
		t.Fatal("rate limit metadata is nil")
	}
	if response.RateLimit.Scope != provider.RateLimitScopeRPS || response.RateLimit.TeamID != "00000000-0000-0000-0000-000000000013" || response.RateLimit.Model != "grok-4.20-multi-agent-0309" {
		t.Fatalf("rate limit metadata = %#v", response.RateLimit)
	}
	if response.Header.Get("Retry-After") != "2" {
		t.Fatalf("retry-after = %q", response.Header.Get("Retry-After"))
	}
}

func TestAdapterForwardsConsoleHeadersAndNormalizedBody(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/responses" || request.Method != http.MethodPost {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer anonymous" || request.Header.Get("x-cluster") != "https://us-east-1.api.x.ai" || request.Header.Get("Accept") != "*/*" || request.Header.Get("Priority") != "u=1, i" {
			t.Errorf("headers = %#v", request.Header)
		}
		if request.Header.Get("User-Agent") != infraegress.DefaultUserAgent {
			t.Errorf("user-agent = %q", request.Header.Get("User-Agent"))
		}
		if request.Header.Get("Sec-Ch-Ua") != `"Google Chrome";v="146", "Chromium";v="146", "Not(A:Brand";v="24"` ||
			request.Header.Get("Sec-Ch-Ua-Mobile") != "?0" || request.Header.Get("Sec-Ch-Ua-Platform") != `"macOS"` ||
			request.Header.Get("Sec-Ch-Ua-Arch") != "x86" || request.Header.Get("Sec-Ch-Ua-Bitness") != "64" {
			t.Errorf("client hints = %#v", request.Header)
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

func TestApplyChromiumClientHintsSkipsNonChromiumUserAgent(t *testing.T) {
	header := make(http.Header)
	applyChromiumClientHints(header, "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Version/18.0 Safari/605.1.15")
	for name := range header {
		if strings.HasPrefix(http.CanonicalHeaderKey(name), "Sec-Ch-Ua") {
			t.Fatalf("unexpected client hint %q", name)
		}
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
	adapter := NewAdapter(Config{BaseURL: baseURL, TimeoutSeconds: 5}, infraegress.NewManager(consoleEgressRepositoryStub{}, cipher), cipher)
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
