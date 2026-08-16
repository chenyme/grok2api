package console

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	fhttptest "github.com/bogdanfinn/fhttp/httptest"
	"github.com/bogdanfinn/websocket"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	providerstreamidle "github.com/chenyme/grok2api/backend/internal/infra/provider/streamidle"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/neterror"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/golang-jwt/jwt/v5"
)

func TestCatalogContainsAllConsoleModelsAndAliases(t *testing.T) {
	type routeKey struct {
		publicID   string
		capability modeldomain.Capability
	}
	expected := map[routeKey]string{
		{publicID: "Console/grok-4.3", capability: modeldomain.CapabilityResponses}:                     "grok-4.3",
		{publicID: "Console/grok-4.20-0309-reasoning", capability: modeldomain.CapabilityResponses}:     "grok-4.20-0309-reasoning",
		{publicID: "Console/grok-4.20-0309-non-reasoning", capability: modeldomain.CapabilityResponses}: "grok-4.20-0309-non-reasoning",
		{publicID: "Console/grok-4.20-multi-agent-0309", capability: modeldomain.CapabilityResponses}:   "grok-4.20-multi-agent-0309",
		{publicID: "Console/grok-4.5", capability: modeldomain.CapabilityResponses}:                     "grok-4.5",
		{publicID: "Console/grok-build-0.1", capability: modeldomain.CapabilityResponses}:               "grok-build-0.1",
		{publicID: "Console/grok-imagine-image", capability: modeldomain.CapabilityImage}:               "grok-imagine-image",
		{publicID: "Console/grok-imagine-image", capability: modeldomain.CapabilityImageEdit}:           "grok-imagine-image",
		{publicID: "Console/grok-imagine-image-quality", capability: modeldomain.CapabilityImage}:       "grok-imagine-image-quality",
		{publicID: "Console/grok-imagine-image-quality", capability: modeldomain.CapabilityImageEdit}:   "grok-imagine-image-quality",
		{publicID: "Console/grok-imagine-image-2.0", capability: modeldomain.CapabilityImage}:           "grok-imagine-image-2.0",
		{publicID: "Console/grok-imagine-image-2.0", capability: modeldomain.CapabilityImageEdit}:       "grok-imagine-image-2.0",
		{publicID: "Console/grok-imagine-video", capability: modeldomain.CapabilityVideo}:               "grok-imagine-video",
		{publicID: "Console/grok-imagine-video-1.5", capability: modeldomain.CapabilityVideo}:           "grok-imagine-video-1.5",
		{publicID: "Console/grok-voice-latest", capability: modeldomain.CapabilityRealtime}:             "grok-voice-latest",
		{publicID: "Console/grok-voice-latest", capability: modeldomain.CapabilityTTS}:                  "grok-voice-latest",
		{publicID: "Console/grok-voice-think-fast-2.0", capability: modeldomain.CapabilityRealtime}:     "grok-voice-think-fast-2.0",
		{publicID: "Console/grok-voice-think-fast-2.0", capability: modeldomain.CapabilityTTS}:          "grok-voice-think-fast-2.0",
		{publicID: "Console/grok-voice-think-fast-1.0", capability: modeldomain.CapabilityRealtime}:     "grok-voice-think-fast-1.0",
		{publicID: "Console/grok-voice-think-fast-1.0", capability: modeldomain.CapabilityTTS}:          "grok-voice-think-fast-1.0",
		{publicID: "Console/grok-stt", capability: modeldomain.CapabilitySTT}:                           "grok-stt",
	}
	routes := Routes()
	if len(routes) != len(expected) {
		t.Fatalf("routes = %d, want %d", len(routes), len(expected))
	}
	for _, route := range routes {
		if route.Provider != account.ProviderConsole || !route.Enabled {
			t.Fatalf("invalid route: %#v", route)
		}
		if expected[routeKey{publicID: route.PublicID, capability: route.Capability}] != route.UpstreamModel {
			t.Fatalf("route %q = %q", route.PublicID, route.UpstreamModel)
		}
	}
	aliases := Aliases()
	if len(aliases) != 14 {
		t.Fatalf("aliases = %d, want 14", len(aliases))
	}
	registry := provider.NewRegistry(NewAdapter(Config{}, nil, nil, nil))
	if registry.SupportsStoredResponses(account.ProviderConsole) {
		t.Fatal("console must not advertise stored Responses support")
	}
	for _, name := range []string{
		"grok-imagine-image-quality-2.0",
		"grok-4.3-console", "grok-4.20-0309-reasoning-console",
		"grok-4.20-0309-non-reasoning-console", "grok-4.20-multi-agent-console", "grok-build-console",
		"grok-4.3-low", "grok-4.3-medium", "grok-4.3-high",
		"grok-4.5-console",
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
	adapter := NewAdapter(Config{}, nil, nil, nil)
	for model, want := range map[string]string{
		"grok-4.5": QuotaMode, "grok-imagine-image-quality": QuotaModeImage, "grok-imagine-image-2.0": QuotaModeImage,
		"grok-imagine-image": QuotaModeImage, "grok-imagine-video": QuotaModeVideo, "grok-imagine-video-1.5": QuotaModeVideo,
		"grok-voice-latest": QuotaMode, "grok-voice-think-fast-2.0": QuotaMode, "grok-voice-think-fast-1.0": QuotaMode, "grok-stt": QuotaMode,
	} {
		if got := adapter.QuotaMode(model); got != want {
			t.Fatalf("QuotaMode(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestConsoleVoiceErrorIsSanitizedAndPreservesRetryMetadata(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": {"17"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"Authorization: Bearer secret-token"}}`)),
	}
	err := consoleVoiceResponseError(response)
	if err == nil {
		t.Fatal("expected voice upstream error")
	}
	if strings.Contains(strings.ToLower(err.Error()), "authorization") || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("voice error exposed sensitive upstream body: %q", err)
	}
	if status, ok := provider.ErrorHTTPStatus(err); !ok || status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, ok = %v", status, ok)
	}
	if retryAfter := provider.ErrorRetryAfter(err); retryAfter != 17*time.Second {
		t.Fatalf("retry after = %s", retryAfter)
	}
	if message, ok := provider.ErrorPublicMessage(err); !ok || strings.Contains(message, "secret-token") {
		t.Fatalf("public message = %q, ok = %v", message, ok)
	}
}

func TestConsoleVoiceErrorSanitizesCredentialAliases(t *testing.T) {
	for _, body := range []string{
		`{"error":{"message":"access_token=secret"}}`,
		`{"error":{"message":"refresh-token=secret"}}`,
		`{"error":{"message":"api_key=secret"}}`,
		`{"error":{"message":"Bearer secret"}}`,
		`{"error":{"message":"cf_clearance=secret"}}`,
	} {
		err := newConsoleMediaUpstreamError(http.StatusBadGateway, []byte(body), 0)
		message, ok := provider.ErrorPublicMessage(err)
		if !ok || message == "" || strings.Contains(strings.ToLower(message), "secret") {
			t.Fatalf("body %q produced public message %q, ok = %v", body, message, ok)
		}
	}
}

func TestConsoleVoiceDPoPRejectionIsRequestScoped(t *testing.T) {
	err := newConsoleMediaUpstreamError(http.StatusForbidden, []byte(`{"error":{"code":"unauthorized:dpop-required"}}`), 0)
	if !provider.IsRequestScopedError(err) {
		t.Fatalf("DPoP rejection was not request-scoped: %v", err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "authorization") {
		t.Fatalf("DPoP error exposed authorization details: %q", err)
	}
}

func TestVoiceWebSocketProofEndpointUsesHTTPUpgradeURI(t *testing.T) {
	got, err := voiceWebSocketProofEndpoint("wss://console.x.ai/v1/realtime?model=grok-voice-latest")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://console.x.ai/v1/realtime?model=grok-voice-latest" {
		t.Fatalf("proof endpoint = %q", got)
	}
}

func TestVoiceWebSocketRefreshesDPoPOnceAfterUnauthorized(t *testing.T) {
	var tokenRequests atomic.Int32
	var websocketRequests atomic.Int32
	server := fhttptest.NewServer(fhttp.HandlerFunc(func(writer fhttp.ResponseWriter, request *fhttp.Request) {
		switch request.URL.Path {
		case "/v1/dpop/token":
			tokenRequests.Add(1)
			var payload struct {
				JWK dpopJWK `json:"jwk"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Errorf("decode DPoP token request: %v", err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			thumbprint, err := dpopJWKThumbprint(payload.JWK)
			if err != nil {
				t.Errorf("DPoP thumbprint: %v", err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			header, _ := json.Marshal(map[string]any{"alg": "HS256", "typ": "JWT"})
			claims, _ := json.Marshal(map[string]any{
				"sub": "test-user", "iat": time.Now().UTC().Unix(), "exp": time.Now().UTC().Add(5 * time.Minute).Unix(),
				"cnf": map[string]any{"jkt": thumbprint}, "token_use": "dpop-bound",
			})
			accessToken := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims) + ".dGVzdA"
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(map[string]any{"access_token": accessToken, "token_type": "DPoP", "expires_in": 300})
		case "/v1/realtime":
			current := websocketRequests.Add(1)
			proof, _, err := jwt.NewParser().ParseUnverified(request.Header.Get("DPoP"), jwt.MapClaims{})
			if err != nil {
				t.Errorf("parse DPoP proof: %v", err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			claims := proof.Claims.(jwt.MapClaims)
			wantHTU := "http://" + request.Host + request.URL.EscapedPath()
			if claims["htu"] != wantHTU || claims["htm"] != http.MethodGet {
				t.Errorf("websocket DPoP binding = %#v, want htu=%q", claims, wantHTU)
			}
			if current == 1 {
				writer.WriteHeader(http.StatusUnauthorized)
				return
			}
			connection, err := (&websocket.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(writer, request, nil)
			if err != nil {
				t.Errorf("upgrade voice websocket: %v", err)
				return
			}
			_ = connection.Close()
		default:
			fhttp.NotFound(writer, request)
		}
	}))
	defer server.Close()

	adapter, credential := newConsoleTestAdapter(t, server.URL)
	connection, cleanup, err := adapter.DialVoiceWebSocket(context.Background(), provider.VoiceWebSocketRequest{
		Credential: credential, Path: "/realtime", Model: "grok-voice-latest",
	})
	if err != nil {
		t.Fatal(err)
	}
	if connection != nil {
		_ = connection.Close()
	}
	if cleanup != nil {
		cleanup()
	}
	if tokenRequests.Load() != 2 || websocketRequests.Load() != 2 {
		t.Fatalf("requests token=%d websocket=%d", tokenRequests.Load(), websocketRequests.Load())
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
	adapter := NewAdapter(Config{SessionBaseURL: server.URL}, infraegress.NewManager(consoleEgressRepositoryStub{}, cipher), cipher, nil)
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
	if len(tools) != 2 || toolIdentity(tools[0]) != "web_search" || toolIdentity(tools[1]) != "function:lookup" {
		t.Fatalf("tools = %#v", tools)
	}
	webSearch, _ := tools[0].(map[string]any)
	if webSearch["custom"] != nil || webSearch["enable_image_understanding"] != true {
		t.Fatalf("web_search = %#v", webSearch)
	}
	stateless, err := normalizeRequest([]byte(`{"model":"grok-4.3","store":true,"previous_response_id":"resp_1","service_tier":"priority","prompt_cache_key":"cache_1","input":"hello"}`), spec)
	if err != nil {
		t.Fatal(err)
	}
	var statelessPayload map[string]any
	if json.Unmarshal(stateless, &statelessPayload) != nil || statelessPayload["store"] != false || statelessPayload["previous_response_id"] != nil || statelessPayload["service_tier"] != nil || statelessPayload["prompt_cache_key"] != nil {
		t.Fatalf("stateless payload = %#v", statelessPayload)
	}
}

func TestNormalizeRequestForwardsXSearchTimeRangeAndImageSearch(t *testing.T) {
	spec, ok := Resolve("grok-4.3")
	if !ok {
		t.Fatal("grok-4.3 missing")
	}

	t.Run("forwards enable_image_search on web_search", func(t *testing.T) {
		body, err := normalizeRequest([]byte(`{
			"model":"grok-4.3",
			"tools":[{"type":"web_search","enable_image_search":true,"custom":true}]
		}`), spec)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		tools, _ := payload["tools"].([]any)
		if len(tools) != 1 {
			t.Fatalf("tools = %#v", tools)
		}
		webSearch, _ := tools[0].(map[string]any)
		if webSearch["type"] != "web_search" || webSearch["enable_image_understanding"] != true {
			t.Fatalf("web_search defaults = %#v", webSearch)
		}
		if webSearch["enable_image_search"] != true {
			t.Fatalf("enable_image_search not forwarded: %#v", webSearch)
		}
		if webSearch["custom"] != nil {
			t.Fatalf("unknown field custom should be stripped: %#v", webSearch)
		}
	})

	t.Run("omits enable_image_search when client does not set it", func(t *testing.T) {
		body, err := normalizeRequest([]byte(`{
			"model":"grok-4.3",
			"tools":[{"type":"web_search"}]
		}`), spec)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		webSearch, _ := payload["tools"].([]any)[0].(map[string]any)
		if _, exists := webSearch["enable_image_search"]; exists {
			t.Fatalf("enable_image_search should be absent by default: %#v", webSearch)
		}
	})

	t.Run("forwards valid x_search from_date and to_date", func(t *testing.T) {
		body, err := normalizeRequest([]byte(`{
			"model":"grok-4.3",
			"tools":[{"type":"x_search","from_date":"2026-07-01","to_date":"2026-07-23","noise":1}]
		}`), spec)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		xSearch, _ := payload["tools"].([]any)[0].(map[string]any)
		if xSearch["type"] != "x_search" || xSearch["enable_video_understanding"] != true {
			t.Fatalf("x_search defaults = %#v", xSearch)
		}
		if xSearch["from_date"] != "2026-07-01" || xSearch["to_date"] != "2026-07-23" {
			t.Fatalf("date bounds not forwarded: %#v", xSearch)
		}
		if xSearch["noise"] != nil {
			t.Fatalf("unknown field noise should be stripped: %#v", xSearch)
		}
	})

	t.Run("drops invalid date formats", func(t *testing.T) {
		body, err := normalizeRequest([]byte(`{
			"model":"grok-4.3",
			"tools":[{"type":"x_search","from_date":"2026-7-01","to_date":"2026-02-30"}]
		}`), spec)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		xSearch, _ := payload["tools"].([]any)[0].(map[string]any)
		if xSearch["from_date"] != nil || xSearch["to_date"] != nil {
			t.Fatalf("invalid dates should be dropped: %#v", xSearch)
		}
		if xSearch["type"] != "x_search" || xSearch["enable_video_understanding"] != true {
			t.Fatalf("x_search defaults should remain: %#v", xSearch)
		}
	})

	t.Run("drops inverted date range", func(t *testing.T) {
		body, err := normalizeRequest([]byte(`{
			"model":"grok-4.3",
			"tools":[{"type":"x_search","from_date":"2026-07-24","to_date":"2026-07-23"}]
		}`), spec)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		xSearch, _ := payload["tools"].([]any)[0].(map[string]any)
		if xSearch["from_date"] != nil || xSearch["to_date"] != nil {
			t.Fatalf("inverted range should drop both bounds: %#v", xSearch)
		}
	})

	t.Run("keeps only from_date when to_date absent", func(t *testing.T) {
		body, err := normalizeRequest([]byte(`{
			"model":"grok-4.3",
			"tools":[{"type":"x_search","from_date":"2026-08-01"}]
		}`), spec)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		xSearch, _ := payload["tools"].([]any)[0].(map[string]any)
		if xSearch["from_date"] != "2026-08-01" || xSearch["to_date"] != nil {
			t.Fatalf("single bound = %#v", xSearch)
		}
	})
}

func TestNormalizeRequestDoesNotInjectToolsForConsoleCatalog(t *testing.T) {
	for _, spec := range catalog {
		t.Run(spec.PublicID, func(t *testing.T) {
			body, err := normalizeRequest([]byte(`{"model":"public","input":"hello","tool_choice":"required"}`), spec)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			if payload["tools"] != nil || payload["tool_choice"] != nil {
				t.Fatalf("Console must not inject tools: %#v", payload)
			}
		})
	}
}

func TestNormalizeRequestPreservesMultiAgentDefaultsWithoutInjectingTools(t *testing.T) {
	spec, ok := Resolve("grok-4.20-multi-agent-0309")
	if !ok {
		t.Fatal("grok-4.20-multi-agent-0309 missing")
	}
	body, err := normalizeRequest([]byte(`{
		"model":"grok-4.20-multi-agent-0309",
		"input":[{"role":"system","content":"hello"},{"role":"user","content":[{"type":"input_text","text":"news"}]}],
		"stream":true
	}`), spec)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["max_output_tokens"] != float64(1_000_000) || payload["reasoning"] != nil || payload["store"] != false {
		t.Fatalf("multi-agent defaults = %#v", payload)
	}
	include, _ := payload["include"].([]any)
	if len(include) != 1 || include[0] != "reasoning.encrypted_content" || payload["tools"] != nil || payload["tool_choice"] != nil {
		t.Fatalf("multi-agent compatibility = %#v", payload)
	}
	explicit, err := normalizeRequest([]byte(`{"model":"grok-4.20-multi-agent-0309","input":"hello","reasoning":{"effort":"xhigh"}}`), spec)
	if err != nil {
		t.Fatal(err)
	}
	payload = nil
	if json.Unmarshal(explicit, &payload) != nil || payload["reasoning"].(map[string]any)["effort"] != "xhigh" {
		t.Fatalf("explicit multi-agent effort = %#v", payload)
	}
}

func TestNormalizeRequestAppliesConsoleCompatibilityBoundary(t *testing.T) {
	spec, ok := Resolve("grok-4.20-0309-non-reasoning")
	if !ok {
		t.Fatal("grok-4.20-0309-non-reasoning missing")
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
	if len(tools) != 1 || toolIdentity(tools[0]) != "web_search" {
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

func TestNormalizeRequestStripsUnsupportedGrok420ReasoningEffort(t *testing.T) {
	spec, ok := Resolve("grok-4.20-0309-reasoning")
	if !ok {
		t.Fatal("grok-4.20-0309-reasoning missing")
	}
	if !spec.SupportsReasoning || spec.SupportsReasoningEffort {
		t.Fatalf("fixed reasoning capability = %#v", spec)
	}
	body, err := normalizeRequest([]byte(`{
		"model":"grok-4.20-0309-reasoning",
		"input":"hello",
		"reasoning":{"effort":"low","summary":"auto"}
	}`), spec)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	reasoning, _ := payload["reasoning"].(map[string]any)
	if reasoning["effort"] != nil || reasoning["summary"] != "auto" {
		t.Fatalf("reasoning = %#v", reasoning)
	}

	effortOnly, err := normalizeRequest([]byte(`{
		"model":"grok-4.20-0309-reasoning",
		"input":"hello",
		"reasoning":{"effort":"none"}
	}`), spec)
	if err != nil {
		t.Fatal(err)
	}
	payload = nil
	if json.Unmarshal(effortOnly, &payload) != nil || payload["reasoning"] != nil {
		t.Fatalf("effort-only reasoning must be removed: %#v", payload)
	}

	withoutEffort, err := normalizeRequest([]byte(`{
		"model":"grok-4.20-0309-reasoning",
		"input":"hello"
	}`), spec)
	if err != nil {
		t.Fatal(err)
	}
	payload = nil
	if err := json.Unmarshal(withoutEffort, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["reasoning"] != nil {
		t.Fatalf("base model request should retain the upstream default: %#v", payload)
	}
}

func TestGrok420FixedReasoningStripsEffortAfterProtocolConversion(t *testing.T) {
	spec, ok := Resolve("grok-4.20-0309-reasoning")
	if !ok {
		t.Fatal("grok-4.20-0309-reasoning missing")
	}
	tests := []struct {
		name      string
		operation string
		body      string
	}{
		{
			name:      "chat completions reasoning_effort",
			operation: conversation.OperationChat,
			body:      `{"model":"public","messages":[{"role":"user","content":"hello"}],"reasoning_effort":"high"}`,
		},
		{
			name:      "anthropic adaptive thinking effort",
			operation: conversation.OperationMessages,
			body:      `{"model":"public","max_tokens":1024,"messages":[{"role":"user","content":"hello"}],"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			converted, err := conversation.ConvertRequest([]byte(test.body), spec.UpstreamModel, test.operation)
			if err != nil {
				t.Fatal(err)
			}
			normalized, err := normalizeRequest(converted, spec)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err := json.Unmarshal(normalized, &payload); err != nil {
				t.Fatal(err)
			}
			reasoning, _ := payload["reasoning"].(map[string]any)
			if reasoning["effort"] != nil {
				t.Fatalf("unsupported effort reached Console upstream: %s", normalized)
			}
		})
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

// 「[」为 JSON 保留前缀：顶层裸数组必须走 JSON 解析，不得落入纯文本路径静默导入。
func TestConsoleImportAcceptsBareArray(t *testing.T) {
	values, err := parseImportedCredentials([]byte(`[{"name":"console-a","sso_token":"token-a","cloudflare_cookies":"cf_clearance=abc"},{"sso_token":"token-b"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0].Name != "console-a" || values[0].AccessToken != "token-a" || values[1].AccessToken != "token-b" {
		t.Fatalf("bare array values = %#v", values)
	}
	if values[0].CloudflareCookies != "cf_clearance=abc" {
		t.Fatalf("cloudflare cookies = %q", values[0].CloudflareCookies)
	}
}

func TestConsoleImportBareArrayErrors(t *testing.T) {
	// 空数组：JSON 路径解析出 0 个账号，而不是被当成空文本导入。
	if _, err := parseImportedCredentials([]byte("[]")); err == nil || !strings.Contains(err.Error(), "没有 Grok Console 账号") {
		t.Fatalf("empty array error = %v", err)
	}
	// null 元素：归一化阶段带账号序号报错。
	if _, err := parseImportedCredentials([]byte(`[{"sso_token":"token-a"},null]`)); err == nil || !strings.Contains(err.Error(), "第 2 个账号缺少 sso_token") {
		t.Fatalf("null element error = %v", err)
	}
	// 非法 [ 开头输入：明确 JSON 报错，禁止静默当纯文本导入。
	if _, err := parseImportedCredentials([]byte("[not-json")); err == nil || !strings.Contains(err.Error(), "JSON") {
		t.Fatalf("malformed array error = %v", err)
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
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveTestDPoPToken(t, writer, request) {
			return
		}
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

func TestAdapterDoesNotPenalizeEgressForBlockedAccount(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantUpdates int
	}{
		{name: "blocked account", body: `{"code":"unauthorized:blocked-user","error":"User is blocked"}`, wantUpdates: 0},
		{name: "anti-bot rejection", body: `{"error":{"code":7,"message":"Request rejected by anti-bot rules."}}`, wantUpdates: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if serveTestDPoPToken(t, writer, request) {
					return
				}
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(writer, test.body)
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
			repository := &recordingConsoleEgressRepository{node: egressdomain.Node{
				ID: 1, Name: "console", Scope: egressdomain.ScopeConsole, Enabled: true, Health: 1,
			}}
			adapter := NewAdapter(Config{BaseURL: server.URL, TimeoutSeconds: 5}, infraegress.NewManager(repository, cipher), cipher, nil)
			credential := account.Credential{ID: 1, Provider: account.ProviderConsole, AuthType: account.AuthTypeSSO, EncryptedAccessToken: encrypted}
			response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
				Credential: credential, Method: http.MethodPost, Path: "/responses", Model: "grok-4.3",
				Operation: conversation.OperationResponses, NormalizeBody: true, Body: []byte(`{"model":"grok-4.3","input":"hello"}`),
			})
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if response.StatusCode != http.StatusForbidden || string(body) != test.body {
				t.Fatalf("status=%d body=%s", response.StatusCode, body)
			}
			if updates := repository.UpdateCount(); updates != test.wantUpdates {
				t.Fatalf("egress updates = %d, want %d", updates, test.wantUpdates)
			}
		})
	}
}

func TestShouldInvalidateConsoleClearance(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "dpop protocol rejection", body: `{"code":"unauthorized:dpop-required","error":"DPoP proof required"}`, want: false},
		{name: "blocked account", body: `{"code":"unauthorized:blocked-user","error":"User is blocked"}`, want: false},
		{name: "anti-bot rejection", body: `{"error":{"code":7,"message":"Request rejected by anti-bot rules."}}`, want: true},
		{name: "generic forbidden", body: `forbidden`, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldInvalidateConsoleClearance([]byte(test.body)); got != test.want {
				t.Fatalf("shouldInvalidateConsoleClearance() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestAdapterForwardsConsoleHeadersAndNormalizedBody(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveTestDPoPToken(t, writer, request) {
			return
		}
		if request.URL.Path != "/v1/responses" || request.Method != http.MethodPost {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if !strings.HasPrefix(request.Header.Get("Authorization"), "DPoP ") || request.Header.Get("DPoP") == "" || request.Header.Get("x-cluster") != "https://us-east-1.api.x.ai" || request.Header.Get("Accept") != "*/*" || request.Header.Get("Priority") != "u=1, i" {
			t.Errorf("headers = %#v", request.Header)
		}
		verifyTestDPoPProof(t, request)
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

func TestAdapterScopesStreamIdleTimeoutToConsoleTextStreams(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveTestDPoPToken(t, writer, request) {
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()

	for _, streaming := range []bool{false, true} {
		name := "non_streaming"
		if streaming {
			name = "streaming"
		}
		t.Run(name, func(t *testing.T) {
			adapter, credential := newConsoleTestAdapter(t, server.URL)
			adapter.UpdateConfig(Config{BaseURL: server.URL, TimeoutSeconds: 5, StreamIdleTimeoutSeconds: 1})
			response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
				Credential: credential, Method: http.MethodPost, Path: "/responses", Model: "grok-4.3",
				Operation: conversation.OperationResponses, Streaming: streaming, NormalizeBody: true,
				Body: []byte(`{"model":"grok-4.3","input":"hello"}`),
			})
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			released, ok := response.Body.(*releaseBody)
			if !ok {
				t.Fatalf("response body = %T, want *releaseBody", response.Body)
			}
			_, wrapped := released.ReadCloser.(*providerstreamidle.ReadCloser)
			if wrapped != streaming {
				t.Fatalf("stream-idle wrapper present = %t, streaming = %t", wrapped, streaming)
			}
		})
	}
}

func TestConsoleStreamingReadReturnsIdleTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveTestDPoPToken(t, writer, request) {
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		writer.(http.Flusher).Flush()
		<-request.Context().Done()
	}))
	defer server.Close()

	adapter, credential := newConsoleTestAdapter(t, server.URL)
	adapter.UpdateConfig(Config{BaseURL: server.URL, TimeoutSeconds: 5, StreamIdleTimeoutSeconds: 1})
	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: credential, Method: http.MethodPost, Path: "/responses", Model: "grok-4.3",
		Operation: conversation.OperationResponses, Streaming: true, NormalizeBody: true,
		Body: []byte(`{"model":"grok-4.3","input":"hello","stream":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); !errors.Is(err, neterror.ErrUpstreamStreamIdleTimeout) {
		t.Fatalf("body read error = %v, want ErrUpstreamStreamIdleTimeout", err)
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
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveTestDPoPToken(t, writer, request) {
			return
		}
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

func TestSyncQuotaUsesDPoPUsageQuotas(t *testing.T) {
	var tokenRequests atomic.Int32
	var usageRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/dpop/token" {
			tokenRequests.Add(1)
			serveTestDPoPToken(t, writer, request)
			return
		}
		if request.URL.Path != "/v1/usage" || request.Method != http.MethodGet {
			http.NotFound(writer, request)
			return
		}
		usageRequests.Add(1)
		verifyTestDPoPProof(t, request)
		if request.Header.Get("x-cluster") != "" {
			t.Errorf("usage x-cluster = %q", request.Header.Get("x-cluster"))
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"quotas":[{"kind":"chat","limit":10,"used":1,"remaining":9,"last_consumed_at":1785895737},{"kind":"image","limit":5,"used":0,"remaining":5},{"kind":"video","limit":2,"used":0,"remaining":2}]}`)
	}))
	defer server.Close()
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	for range 2 {
		snapshot, err := adapter.SyncQuota(context.Background(), credential)
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Windows) != 3 {
			t.Fatalf("quota windows = %#v", snapshot.Windows)
		}
		want := []struct {
			mode                         string
			remaining, total, windowSecs int
		}{
			{mode: QuotaMode, remaining: 9, total: 10, windowSecs: 24 * 60 * 60},
			{mode: QuotaModeImage, remaining: 5, total: 5},
			{mode: QuotaModeVideo, remaining: 2, total: 2},
		}
		for index, expected := range want {
			window := snapshot.Windows[index]
			if window.Mode != expected.mode || window.Remaining != expected.remaining || window.Total != expected.total || window.Source != account.QuotaSourceUpstream || window.ResetAt != nil || window.WindowSeconds != expected.windowSecs {
				t.Fatalf("quota[%d] = %#v", index, window)
			}
		}
	}
	if tokenRequests.Load() != 1 || usageRequests.Load() != 2 {
		t.Fatalf("requests token=%d usage=%d", tokenRequests.Load(), usageRequests.Load())
	}
}

func TestSyncQuotaPredictsChatRecoveryAfter24Hours(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/dpop/token" {
			serveTestDPoPToken(t, writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"quotas":[{"kind":"chat","limit":10,"used":10,"remaining":0},{"kind":"image","limit":5,"used":0,"remaining":5},{"kind":"video","limit":2,"used":0,"remaining":2}]}`)
	}))
	defer server.Close()
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	startedAt := time.Now().UTC()
	snapshot, err := adapter.SyncQuota(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	chat := snapshot.Windows[0]
	if chat.ResetAt == nil || chat.WindowSeconds != 24*60*60 {
		t.Fatalf("chat recovery = %#v", chat)
	}
	want := startedAt.Add(24 * time.Hour)
	if chat.ResetAt.Before(want) || chat.ResetAt.After(time.Now().UTC().Add(24*time.Hour)) {
		t.Fatalf("predicted recovery = %s, want around %s", chat.ResetAt, want)
	}
}

func TestSyncQuotaModeSelectsConsoleMediaWindow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/dpop/token" {
			serveTestDPoPToken(t, writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"quotas":[{"kind":"chat","limit":10,"used":1,"remaining":9},{"kind":"image","limit":5,"used":2,"remaining":3},{"kind":"video","limit":2,"used":0,"remaining":2}]}`)
	}))
	defer server.Close()
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	window, err := adapter.SyncQuotaMode(context.Background(), credential, QuotaModeImage)
	if err != nil {
		t.Fatal(err)
	}
	if window.Mode != QuotaModeImage || window.Remaining != 3 || window.Total != 5 || window.UsagePercent != 40 {
		t.Fatalf("image quota = %#v", window)
	}
}

func TestSyncQuotaRejectsPartialUsageSnapshot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/dpop/token" {
			serveTestDPoPToken(t, writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"quotas":[{"kind":"chat","limit":10,"used":1,"remaining":9}]}`)
	}))
	defer server.Close()
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	if _, err := adapter.SyncQuota(context.Background(), credential); err == nil || !strings.Contains(err.Error(), "image") {
		t.Fatalf("partial usage err = %v", err)
	}
}

func TestAdapterRefreshesDPoPSessionOnceAfterUnauthorized(t *testing.T) {
	var tokenRequests atomic.Int32
	var responseRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/dpop/token" {
			tokenRequests.Add(1)
			serveTestDPoPToken(t, writer, request)
			return
		}
		currentResponse := responseRequests.Add(1)
		verifyTestDPoPProof(t, request)
		if currentResponse == 1 {
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(writer, `{"error":"expired dpop token"}`)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"id":"resp_refreshed","object":"response","status":"completed","output":[]}`)
	}))
	defer server.Close()
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: credential, Method: http.MethodPost, Path: "/responses", Model: "grok-4.3",
		Operation: conversation.OperationResponses, NormalizeBody: true, Body: []byte(`{"model":"grok-4.3","input":"hello"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || tokenRequests.Load() != 2 || responseRequests.Load() != 2 {
		t.Fatalf("status=%d token=%d responses=%d", response.StatusCode, tokenRequests.Load(), responseRequests.Load())
	}
}

func TestAdapterCoalescesConcurrentDPoPTokenExchange(t *testing.T) {
	const workers = 16
	var tokenRequests atomic.Int32
	var responseRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/dpop/token" {
			tokenRequests.Add(1)
			time.Sleep(20 * time.Millisecond)
			serveTestDPoPToken(t, writer, request)
			return
		}
		responseRequests.Add(1)
		verifyTestDPoPProof(t, request)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"id":"resp_concurrent","object":"response","status":"completed","output":[]}`)
	}))
	defer server.Close()
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	start := make(chan struct{})
	errorsChannel := make(chan error, workers)
	var group sync.WaitGroup
	group.Add(workers)
	for range workers {
		go func() {
			defer group.Done()
			<-start
			response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
				Credential: credential, Method: http.MethodPost, Path: "/responses", Model: "grok-4.3",
				Operation: conversation.OperationResponses, NormalizeBody: true, Body: []byte(`{"model":"grok-4.3","input":"hello"}`),
			})
			if err == nil {
				_, err = io.Copy(io.Discard, response.Body)
				closeErr := response.Body.Close()
				if err == nil {
					err = closeErr
				}
			}
			errorsChannel <- err
		}()
	}
	close(start)
	group.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatal(err)
		}
	}
	if tokenRequests.Load() != 1 || responseRequests.Load() != workers {
		t.Fatalf("requests token=%d responses=%d", tokenRequests.Load(), responseRequests.Load())
	}
}

func TestAdapterCoalescesConcurrentDPoPRefreshAfterUnauthorized(t *testing.T) {
	const workers = 16
	var tokenRequests atomic.Int32
	var responseRequests atomic.Int32
	var initialRequests atomic.Int32
	var releaseInitial sync.Once
	initialReady := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/dpop/token" {
			tokenRequests.Add(1)
			time.Sleep(20 * time.Millisecond)
			serveTestDPoPToken(t, writer, request)
			return
		}
		responseRequests.Add(1)
		verifyTestDPoPProof(t, request)
		if current := initialRequests.Add(1); current <= workers {
			if current == workers {
				releaseInitial.Do(func() { close(initialReady) })
			}
			<-initialReady
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(writer, `{"error":"expired dpop token"}`)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"id":"resp_refreshed","object":"response","status":"completed","output":[]}`)
	}))
	defer server.Close()
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	start := make(chan struct{})
	errorsChannel := make(chan error, workers)
	var group sync.WaitGroup
	group.Add(workers)
	for range workers {
		go func() {
			defer group.Done()
			<-start
			response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
				Credential: credential, Method: http.MethodPost, Path: "/responses", Model: "grok-4.3",
				Operation: conversation.OperationResponses, NormalizeBody: true, Body: []byte(`{"model":"grok-4.3","input":"hello"}`),
			})
			if err == nil {
				_, err = io.Copy(io.Discard, response.Body)
				closeErr := response.Body.Close()
				if err == nil {
					err = closeErr
				}
			}
			errorsChannel <- err
		}()
	}
	close(start)
	group.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatal(err)
		}
	}
	if tokenRequests.Load() != 2 || responseRequests.Load() != workers*2 {
		t.Fatalf("requests token=%d responses=%d", tokenRequests.Load(), responseRequests.Load())
	}
}

func TestDPoPSessionCacheUsesBoundedLRUEviction(t *testing.T) {
	manager := newDPoPSessionManager()
	now := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	for index := range dpopSessionCacheLimit {
		manager.store(strconv.Itoa(index), dpopSession{accessToken: strconv.Itoa(index), expiresAt: now.Add(time.Minute)})
	}
	if _, ok := manager.cached("0"); !ok {
		t.Fatal("failed to touch oldest cache entry")
	}
	manager.store("new", dpopSession{accessToken: "new", expiresAt: now.Add(time.Minute)})
	if len(manager.sessions) != dpopSessionCacheLimit {
		t.Fatalf("cache size = %d", len(manager.sessions))
	}
	if _, ok := manager.cached("0"); !ok {
		t.Fatal("recently used entry was evicted")
	}
	if _, ok := manager.cached("1"); ok {
		t.Fatal("least recently used entry was retained")
	}
}

func TestConsoleImageGenerationForwardsStandardDPoPRequest(t *testing.T) {
	imageBytes := []byte("\x89PNG\r\n\x1a\nconsole-image")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveTestDPoPToken(t, writer, request) {
			return
		}
		if request.URL.Path == "/generated.png" && request.Method == http.MethodGet {
			if request.Header.Get("Authorization") != "" || request.Header.Get("DPoP") != "" || request.Header.Get("Cookie") != "" {
				t.Errorf("asset request leaked credentials: %#v", request.Header)
			}
			writer.Header().Set("Content-Type", "image/png")
			_, _ = writer.Write(imageBytes)
			return
		}
		if request.URL.Path != "/v1/images/generations" || request.Method != http.MethodPost {
			http.NotFound(writer, request)
			return
		}
		verifyTestDPoPProof(t, request)
		if request.Header.Get("x-cluster") != "" {
			t.Errorf("image x-cluster = %q", request.Header.Get("x-cluster"))
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload["model"] != "grok-imagine-image-quality" || payload["prompt"] != "draw" || payload["n"] != float64(2) || payload["aspect_ratio"] != "3:2" || payload["resolution"] != "2k" || payload["response_format"] != "url" {
			t.Errorf("image payload = %#v", payload)
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"created": 123, "data": []any{map[string]any{"url": "http://" + request.Host + "/generated.png", "revised_prompt": "drawn"}}})
	}))
	t.Cleanup(server.Close)
	store := &consoleImageAssetStoreStub{}
	adapter, credential := newConsoleTestAdapterWithAssets(t, server.URL, store)
	ctx, trace := infraegress.WithTrace(context.Background())
	response, err := adapter.GenerateImage(ctx, provider.ImageGenerationRequest{
		Credential: credential, Model: "grok-imagine-image-quality", Prompt: "draw", Count: 2,
		Size: "1536x1024", Resolution: "2k", ResponseFormat: "url",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.QuotaUnits != 2 {
		t.Fatalf("image response = %#v", response)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Created int64 `json:"created"`
		Data    []struct {
			URL           string `json:"url"`
			MIMEType      string `json:"mime_type"`
			RevisedPrompt string `json:"revised_prompt"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if result.Created != 123 || len(result.Data) != 1 || result.Data[0].URL != "https://local.example/v1/media/images/console-1" || result.Data[0].MIMEType != "image/png" || result.Data[0].RevisedPrompt != "drawn" {
		t.Fatalf("localized image response = %s", body)
	}
	if saved := store.Saved(); len(saved) != 1 || !bytes.Equal(saved[0], imageBytes) {
		t.Fatalf("saved images = %#v", saved)
	}
	if selection, ok := trace.Selection(egressdomain.ScopeConsoleAsset); !ok || selection.Scope != egressdomain.ScopeConsoleAsset {
		t.Fatalf("Console image asset selection = %#v, ok=%v", selection, ok)
	}
}

func TestConsoleImageEditForwardsMultipleImages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveTestDPoPToken(t, writer, request) {
			return
		}
		if request.URL.Path != "/v1/images/edits" {
			http.NotFound(writer, request)
			return
		}
		verifyTestDPoPProof(t, request)
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		images, _ := payload["images"].([]any)
		if len(images) != 2 || payload["image"] != nil || payload["model"] != "grok-imagine-image" || payload["n"] != float64(2) {
			t.Errorf("edit payload = %#v", payload)
		}
		if payload["response_format"] != "b64_json" {
			t.Errorf("response_format = %#v", payload["response_format"])
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"data":[{"b64_json":"aW1hZ2U=","revised_prompt":"merged"}]}`))
	}))
	t.Cleanup(server.Close)
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	response, err := adapter.EditImage(context.Background(), provider.ImageEditRequest{
		Credential: credential, Model: "grok-imagine-image", Prompt: "merge", Count: 2,
		ImageURLs: []string{"https://example.com/a.png", "data:image/png;base64,AAAA"}, Resolution: "1k", ResponseFormat: "b64_json",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.QuotaUnits != 2 {
		t.Fatalf("edit response = %#v", response)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"b64_json":"aW1hZ2U="`)) {
		t.Fatalf("b64 response = %s", body)
	}
}

func TestConsoleImage20ForwardsQualityAndRejectsItForLegacyModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveTestDPoPToken(t, writer, request) {
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload["model"] != "grok-imagine-image-2.0" || payload["quality"] != "low" || payload["resolution"] != "2k" {
			t.Errorf("Image 2.0 payload = %#v", payload)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"data":[{"b64_json":"aW1hZ2U="}]}`))
	}))
	t.Cleanup(server.Close)
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	response, err := adapter.GenerateImage(context.Background(), provider.ImageGenerationRequest{
		Credential: credential, Model: "grok-imagine-image-2.0", Prompt: "draw", Count: 1,
		Resolution: "2k", Quality: "low", ResponseFormat: "b64_json",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	legacy, err := adapter.GenerateImage(context.Background(), provider.ImageGenerationRequest{
		Credential: credential, Model: "grok-imagine-image", Prompt: "draw", Count: 1, Quality: "low",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Body.Close()
	if legacy.StatusCode != http.StatusBadRequest {
		t.Fatalf("legacy quality response = %#v", legacy)
	}
}

func TestConsoleVideoCreatesAndPollsStandardResources(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveTestDPoPToken(t, writer, request) {
			return
		}
		verifyTestDPoPProof(t, request)
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1/videos/generations":
			var payload map[string]any
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if payload["model"] != "grok-imagine-video" || payload["duration"] != float64(6) || payload["resolution"] != "720p" {
				t.Errorf("video payload = %#v", payload)
			}
			_, _ = writer.Write([]byte(`{"request_id":"upstream-video-1"}`))
		case request.Method == http.MethodGet && request.URL.Path == "/v1/videos/upstream-video-1":
			_, _ = writer.Write([]byte(`{"status":"done","progress":100,"video":{"url":"https://vidgen.x.ai/result.mp4"}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	progress := 0
	result, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: credential, Prompt: "animate", Duration: 6, AspectRatio: "16:9", Resolution: "720p",
		Progress: func(value int) { progress = value },
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.URL != "https://vidgen.x.ai/result.mp4" || result.ContentType != "video/mp4" || progress != 99 {
		t.Fatalf("video result = %#v, progress = %d", result, progress)
	}
}

func TestConsoleVideoCreateFailureStagesPreserveRetrySafety(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		wantStage provider.VideoStage
	}{
		{name: "malformed successful response", status: http.StatusOK, body: `{"status":"queued"}`, wantStage: provider.VideoStageSubmitted},
		{name: "explicit rate limit rejection", status: http.StatusTooManyRequests, body: `{"error":"rate limited"}`, wantStage: provider.VideoStageCreate},
		{name: "server failure result unknown", status: http.StatusInternalServerError, body: `{"error":"failed"}`, wantStage: provider.VideoStageSubmitted},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if serveTestDPoPToken(t, writer, request) {
					return
				}
				verifyTestDPoPProof(t, request)
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(test.status)
				_, _ = io.WriteString(writer, test.body)
			}))
			t.Cleanup(server.Close)
			adapter, credential := newConsoleTestAdapter(t, server.URL)
			_, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
				Credential: credential, Model: "grok-imagine-video", Prompt: "test", Duration: 5, Resolution: "720p",
			})
			stage, ok := provider.VideoErrorStage(err)
			if !ok || stage != test.wantStage {
				t.Fatalf("stage = %q, ok=%t, err=%v; want %q", stage, ok, err, test.wantStage)
			}
		})
	}
}

func TestConsoleVideoPostsReferenceImages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveTestDPoPToken(t, writer, request) {
			return
		}
		verifyTestDPoPProof(t, request)
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1/videos/generations":
			var payload map[string]any
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if _, exists := payload["image"]; exists {
				t.Errorf("multi-reference payload must not include image: %#v", payload)
			}
			references, ok := payload["reference_images"].([]any)
			if !ok || len(references) != 3 {
				t.Errorf("reference_images = %#v", payload["reference_images"])
			} else {
				first, _ := references[0].(map[string]any)
				third, _ := references[2].(map[string]any)
				if first["url"] != "https://example.com/a.png" || third["url"] != "https://example.com/c.png" {
					t.Errorf("reference_images = %#v", references)
				}
			}
			_, _ = writer.Write([]byte(`{"request_id":"upstream-video-ref-1"}`))
		case request.Method == http.MethodGet && request.URL.Path == "/v1/videos/upstream-video-ref-1":
			_, _ = writer.Write([]byte(`{"status":"done","progress":100,"video":{"url":"https://vidgen.x.ai/result-ref.mp4"}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	result, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: credential, Prompt: "animate", Duration: 6, AspectRatio: "16:9", Resolution: "720p",
		ReferenceURLs: []string{
			"https://example.com/a.png",
			"https://example.com/b.png",
			"https://example.com/c.png",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.URL != "https://vidgen.x.ai/result-ref.mp4" || result.ContentType != "video/mp4" {
		t.Fatalf("video result = %#v", result)
	}
}

func TestConsoleVideoPostsSingleReferenceImage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveTestDPoPToken(t, writer, request) {
			return
		}
		verifyTestDPoPProof(t, request)
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1/videos/generations":
			var payload map[string]any
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if _, exists := payload["image"]; exists {
				t.Errorf("single reference must not coerce to image: %#v", payload)
			}
			references, ok := payload["reference_images"].([]any)
			if !ok || len(references) != 1 {
				t.Errorf("reference_images = %#v", payload["reference_images"])
			} else {
				first, _ := references[0].(map[string]any)
				if first["url"] != "https://example.com/ref-only.png" {
					t.Errorf("reference_images = %#v", references)
				}
			}
			_, _ = writer.Write([]byte(`{"request_id":"upstream-video-ref-single"}`))
		case request.Method == http.MethodGet && request.URL.Path == "/v1/videos/upstream-video-ref-single":
			_, _ = writer.Write([]byte(`{"status":"done","progress":100,"video":{"url":"https://vidgen.x.ai/result-ref-single.mp4"}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	result, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: credential, Prompt: "animate", Duration: 6, AspectRatio: "16:9", Resolution: "720p",
		ReferenceURLs: []string{"https://example.com/ref-only.png"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.URL != "https://vidgen.x.ai/result-ref-single.mp4" {
		t.Fatalf("video result = %#v", result)
	}
}

func TestConsoleVideoRejectsImageWithReferences(t *testing.T) {
	adapter, credential := newConsoleTestAdapter(t, "https://console.example")
	_, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: credential, Prompt: "animate", Duration: 6, AspectRatio: "16:9", Resolution: "720p",
		ImageURL:      "https://example.com/first.png",
		ReferenceURLs: []string{"https://example.com/ref.png"},
	})
	if err == nil || !strings.Contains(err.Error(), "不能与") {
		t.Fatalf("error = %v", err)
	}
}

func TestConsoleVideoPostsReferenceAudios(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveTestDPoPToken(t, writer, request) {
			return
		}
		verifyTestDPoPProof(t, request)
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1/videos/generations":
			var payload map[string]any
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if _, exists := payload["image"]; exists {
				t.Errorf("image = %#v", payload["image"])
			}
			references, ok := payload["reference_images"].([]any)
			if !ok || len(references) != 1 {
				t.Errorf("reference_images = %#v", payload["reference_images"])
			} else {
				first, _ := references[0].(map[string]any)
				if first["url"] != "https://example.com/person.png" {
					t.Errorf("reference_images = %#v", references)
				}
			}
			audios, ok := payload["reference_audios"].([]any)
			if !ok || len(audios) != 1 {
				t.Errorf("reference_audios = %#v", payload["reference_audios"])
			} else if first, _ := audios[0].(map[string]any); first["voice_id"] != "eve" {
				t.Errorf("reference_audios = %#v", audios)
			}
			_, _ = writer.Write([]byte(`{"request_id":"upstream-video-ref-audio"}`))
		case request.Method == http.MethodGet && request.URL.Path == "/v1/videos/upstream-video-ref-audio":
			_, _ = writer.Write([]byte(`{"status":"done","progress":100,"video":{"url":"https://vidgen.x.ai/result-ref-audio.mp4","duration":8}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	result, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: credential, Prompt: "speak", Duration: 8, AspectRatio: "9:16", Resolution: "720p",
		ReferenceURLs:   []string{"https://example.com/person.png"},
		ReferenceAudios: []string{"eve"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.URL != "https://vidgen.x.ai/result-ref-audio.mp4" {
		t.Fatalf("video result = %#v", result)
	}
}

// Measured upstream ceiling: 8 references answer 400 "Too many reference images:
// 8. Maximum allowed is 7." on both grok-imagine-video and grok-imagine-video-1.5.
func TestConsoleVideoRejectsTooManyReferenceImages(t *testing.T) {
	adapter, credential := newConsoleTestAdapter(t, "https://console.example")
	references := make([]string, provider.ConsoleVideoMaxReferenceImages+1)
	for i := range references {
		references[i] = "https://example.com/" + strings.Repeat("x", i+1) + ".png"
	}
	_, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: credential, Prompt: "animate", Duration: 6, ReferenceURLs: references,
	})
	if err == nil || !strings.Contains(err.Error(), "最多支持") {
		t.Fatalf("error = %v", err)
	}
}

func TestConsoleVideoRejectsTooManyCombinedImages(t *testing.T) {
	adapter, credential := newConsoleTestAdapter(t, "https://console.example")
	references := make([]string, provider.ConsoleVideoMaxReferenceImages)
	for i := range references {
		references[i] = "https://example.com/" + strings.Repeat("y", i+1) + ".png"
	}
	_, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: credential, Prompt: "animate", Duration: 6,
		ImageURL: "https://example.com/first.png", ReferenceURLs: references,
	})
	if err == nil || !(strings.Contains(err.Error(), "不能与") || strings.Contains(err.Error(), "最多支持")) {
		t.Fatalf("error = %v", err)
	}
}

// Measured: grok-imagine-video with reference_images and duration=15 answers
// 400 "Duration 15s exceeds the maximum allowed for reference-to-video, which is
// 10s." The image field (image-to-video) and grok-imagine-video-1.5 both keep 15s.
func TestConsoleVideoRejectsLongReferenceDurationOnBaseModel(t *testing.T) {
	adapter, credential := newConsoleTestAdapter(t, "https://console.example")
	_, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: credential, Prompt: "animate", Duration: 15,
		ReferenceURLs: []string{"https://example.com/ref.png"},
	})
	if err == nil || !strings.Contains(err.Error(), "最长 10 秒") {
		t.Fatalf("error = %v", err)
	}
}

// The same 15s request must stay allowed for grok-imagine-video-1.5.
func TestConsoleVideo15AllowsLongReferenceDuration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveTestDPoPToken(t, writer, request) {
			return
		}
		verifyTestDPoPProof(t, request)
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1/videos/generations":
			_, _ = writer.Write([]byte(`{"request_id":"upstream-video-15s"}`))
		case request.Method == http.MethodGet && request.URL.Path == "/v1/videos/upstream-video-15s":
			_, _ = writer.Write([]byte(`{"status":"done","progress":100,"video":{"url":"https://vidgen.x.ai/result-15s.mp4"}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	result, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: credential, Model: "grok-imagine-video-1.5", Prompt: "animate", Duration: 15,
		ReferenceURLs: []string{"https://example.com/ref.png"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.URL != "https://vidgen.x.ai/result-15s.mp4" {
		t.Fatalf("video result = %#v", result)
	}
}

func TestConsoleVideoUsesImagineVideo15Model(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveTestDPoPToken(t, writer, request) {
			return
		}
		verifyTestDPoPProof(t, request)
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1/videos/generations":
			var payload map[string]any
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if payload["model"] != "grok-imagine-video-1.5" || payload["resolution"] != "1080p" {
				t.Errorf("video payload = %#v", payload)
			}
			_, _ = writer.Write([]byte(`{"request_id":"upstream-video-15"}`))
		case request.Method == http.MethodGet && request.URL.Path == "/v1/videos/upstream-video-15":
			_, _ = writer.Write([]byte(`{"status":"done","progress":100,"video":{"url":"https://vidgen.x.ai/result-15.mp4"}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	result, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: credential, Model: "grok-imagine-video-1.5", Prompt: "animate", Duration: 6, AspectRatio: "16:9", Resolution: "1080p",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.URL != "https://vidgen.x.ai/result-15.mp4" {
		t.Fatalf("video result = %#v", result)
	}
}

func TestConsoleVideo15Rejects1080pReferenceMode(t *testing.T) {
	adapter, credential := newConsoleTestAdapter(t, "https://console.example")
	_, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: credential, Model: "grok-imagine-video-1.5", Prompt: "animate", Duration: 6, Resolution: "1080p",
		ReferenceURLs: []string{"https://example.com/reference.png"},
	})
	if err == nil || !strings.Contains(err.Error(), "最高支持 720p") {
		t.Fatalf("error = %v", err)
	}
}

func TestParseConsoleVideoStatusRejectsUnknownState(t *testing.T) {
	if _, _, err := parseConsoleVideoStatus([]byte(`{"status":"mystery"}`), nil); err == nil || !strings.Contains(err.Error(), "状态无效") {
		t.Fatalf("unknown status error = %v", err)
	}
}

func serveTestDPoPToken(t *testing.T, writer http.ResponseWriter, request *http.Request) bool {
	t.Helper()
	if request.URL.Path != "/v1/dpop/token" {
		return false
	}
	if request.Method != http.MethodPost {
		t.Errorf("DPoP token method = %s", request.Method)
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return true
	}
	if request.Header.Get("Authorization") != "" || request.Header.Get("DPoP") != "" {
		t.Errorf("DPoP token request unexpectedly authenticated: %#v", request.Header)
	}
	if request.Header.Get("x-cluster") != "" {
		t.Errorf("DPoP token x-cluster = %q", request.Header.Get("x-cluster"))
	}
	if !strings.Contains(request.Header.Get("Cookie"), "sso=test-sso") {
		t.Errorf("DPoP token cookie = %q", request.Header.Get("Cookie"))
	}
	var payload struct {
		JWK dpopJWK `json:"jwk"`
	}
	if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
		t.Errorf("decode DPoP token request: %v", err)
		writer.WriteHeader(http.StatusBadRequest)
		return true
	}
	thumbprint, err := dpopJWKThumbprint(payload.JWK)
	if err != nil {
		t.Errorf("DPoP thumbprint: %v", err)
		writer.WriteHeader(http.StatusBadRequest)
		return true
	}
	header, _ := json.Marshal(map[string]any{"alg": "HS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{
		"sub": "test-user", "iat": time.Now().UTC().Unix(), "exp": time.Now().UTC().Add(5 * time.Minute).Unix(),
		"cnf": map[string]any{"jkt": thumbprint}, "token_use": "dpop-bound",
	})
	accessToken := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims) + ".dGVzdA"
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{"access_token": accessToken, "token_type": "DPoP", "expires_in": 300})
	return true
}

func verifyTestDPoPProof(t *testing.T, request *http.Request) {
	t.Helper()
	authorization := strings.TrimSpace(request.Header.Get("Authorization"))
	accessToken := strings.TrimSpace(strings.TrimPrefix(authorization, "DPoP "))
	proofValue := strings.TrimSpace(request.Header.Get("DPoP"))
	parsed, err := jwt.Parse(proofValue, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodES256 || token.Header["typ"] != "dpop+jwt" {
			return nil, fmt.Errorf("unexpected DPoP header: %#v", token.Header)
		}
		encoded, err := json.Marshal(token.Header["jwk"])
		if err != nil {
			return nil, err
		}
		var jwk dpopJWK
		if err := json.Unmarshal(encoded, &jwk); err != nil {
			return nil, err
		}
		xBytes, err := base64.RawURLEncoding.DecodeString(jwk.X)
		if err != nil {
			return nil, err
		}
		yBytes, err := base64.RawURLEncoding.DecodeString(jwk.Y)
		if err != nil {
			return nil, err
		}
		publicKey := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(xBytes), Y: new(big.Int).SetBytes(yBytes)}
		if !publicKey.Curve.IsOnCurve(publicKey.X, publicKey.Y) {
			return nil, errors.New("DPoP JWK point is not on P-256")
		}
		return publicKey, nil
	}, jwt.WithValidMethods([]string{"ES256"}))
	if err != nil || !parsed.Valid {
		t.Fatalf("invalid DPoP proof: valid=%v err=%v", parsed != nil && parsed.Valid, err)
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		t.Fatalf("DPoP claims = %#v", parsed.Claims)
	}
	wantHTU := "http://" + request.Host + request.URL.EscapedPath()
	if claims["htm"] != request.Method || claims["htu"] != wantHTU || strings.TrimSpace(fmt.Sprint(claims["jti"])) == "" {
		t.Fatalf("DPoP request binding = %#v", claims)
	}
	digest := sha256.Sum256([]byte(accessToken))
	if claims["ath"] != base64.RawURLEncoding.EncodeToString(digest[:]) {
		t.Fatalf("DPoP ath = %#v", claims["ath"])
	}
	iat, ok := claims["iat"].(float64)
	if !ok || time.Since(time.Unix(int64(iat), 0)) > time.Minute {
		t.Fatalf("DPoP iat = %#v", claims["iat"])
	}
}

func newConsoleTestAdapter(t *testing.T, baseURL string) (*Adapter, account.Credential) {
	return newConsoleTestAdapterWithAssets(t, baseURL, nil)
}

func newConsoleTestAdapterWithAssets(t *testing.T, baseURL string, assets provider.ImageAssetStore) (*Adapter, account.Credential) {
	t.Helper()
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("test-sso")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{BaseURL: baseURL, TimeoutSeconds: 5}, infraegress.NewManager(consoleEgressRepositoryStub{}, cipher), cipher, assets)
	credential := account.Credential{ID: 1, Provider: account.ProviderConsole, AuthType: account.AuthTypeSSO, EncryptedAccessToken: encrypted}
	return adapter, credential
}

type consoleImageAssetStoreStub struct {
	mu    sync.Mutex
	saved [][]byte
}

func (s *consoleImageAssetStoreStub) SaveImage(_ context.Context, data []byte) (mediadomain.Asset, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved = append(s.saved, bytes.Clone(data))
	return mediadomain.Asset{ID: fmt.Sprintf("console-%d", len(s.saved)), MIMEType: "image/png"}, nil
}

func (*consoleImageAssetStoreStub) PublicImageURL(id string) string {
	return "https://local.example/v1/media/images/" + id
}

func (s *consoleImageAssetStoreStub) Saved() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([][]byte, len(s.saved))
	for index := range s.saved {
		result[index] = bytes.Clone(s.saved[index])
	}
	return result
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

type recordingConsoleEgressRepository struct {
	mu      sync.Mutex
	node    egressdomain.Node
	updates int
}

func (r *recordingConsoleEgressRepository) ListEgressNodes(_ context.Context, scope egressdomain.Scope, _ repository.SortQuery) ([]egressdomain.Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.node.Scope != scope {
		return nil, nil
	}
	return []egressdomain.Node{r.node}, nil
}

func (r *recordingConsoleEgressRepository) GetEgressNode(context.Context, uint64) (egressdomain.Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.node, nil
}

func (r *recordingConsoleEgressRepository) CreateEgressNode(context.Context, egressdomain.Node) (egressdomain.Node, error) {
	return egressdomain.Node{}, errors.New("unsupported")
}

func (r *recordingConsoleEgressRepository) UpdateEgressNode(_ context.Context, value egressdomain.Node) (egressdomain.Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.node = value
	r.updates++
	return value, nil
}

func (r *recordingConsoleEgressRepository) DeleteEgressNode(context.Context, uint64) error {
	return errors.New("unsupported")
}

func (r *recordingConsoleEgressRepository) UpdateCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.updates
}

func TestConsoleVideoEditPostsVideoAndPolls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveTestDPoPToken(t, writer, request) {
			return
		}
		verifyTestDPoPProof(t, request)
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1/videos/edits":
			var payload map[string]any
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if payload["model"] != "grok-imagine-video" || payload["prompt"] != "add necklace" {
				t.Errorf("edit payload = %#v", payload)
			}
			if _, exists := payload["duration"]; exists {
				t.Errorf("edit payload must not include duration: %#v", payload)
			}
			video, ok := payload["video"].(map[string]any)
			if !ok || video["url"] != "https://example.com/source.mp4" {
				t.Errorf("edit video = %#v", payload["video"])
			}
			_, _ = writer.Write([]byte(`{"request_id":"upstream-video-edit-1"}`))
		case request.Method == http.MethodGet && request.URL.Path == "/v1/videos/upstream-video-edit-1":
			_, _ = writer.Write([]byte(`{"status":"done","progress":100,"video":{"url":"https://vidgen.x.ai/result-edit.mp4"}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	result, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: credential, Model: "grok-imagine-video", Operation: provider.VideoOperationEdit,
		Prompt: "add necklace", VideoURL: "https://example.com/source.mp4",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.URL != "https://vidgen.x.ai/result-edit.mp4" {
		t.Fatalf("edit result = %#v", result)
	}
}

func TestConsoleVideoExtendPostsVideoAndPolls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveTestDPoPToken(t, writer, request) {
			return
		}
		verifyTestDPoPProof(t, request)
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1/videos/extensions":
			var payload map[string]any
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if payload["model"] != "grok-imagine-video" || payload["duration"] != float64(4) || payload["prompt"] != "pan left" {
				t.Errorf("extend payload = %#v", payload)
			}
			video, ok := payload["video"].(map[string]any)
			if !ok || video["url"] != "https://example.com/source.mp4" {
				t.Errorf("extend video = %#v", payload["video"])
			}
			_, _ = writer.Write([]byte(`{"request_id":"upstream-video-extend-1"}`))
		case request.Method == http.MethodGet && request.URL.Path == "/v1/videos/upstream-video-extend-1":
			_, _ = writer.Write([]byte(`{"status":"done","progress":100,"video":{"url":"https://vidgen.x.ai/result-extend.mp4"}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	result, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: credential, Model: "grok-imagine-video", Operation: provider.VideoOperationExtend,
		Prompt: "pan left", Duration: 4, VideoURL: "https://example.com/source.mp4",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.URL != "https://vidgen.x.ai/result-extend.mp4" {
		t.Fatalf("extend result = %#v", result)
	}
}

func TestConsoleVideoEditRejectsNonImagineVideoModel(t *testing.T) {
	adapter, credential := newConsoleTestAdapter(t, "http://127.0.0.1")
	_, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: credential, Model: "grok-imagine-video-1.5", Operation: provider.VideoOperationEdit,
		Prompt: "x", VideoURL: "https://example.com/source.mp4",
	})
	if err == nil || !strings.Contains(err.Error(), "grok-imagine-video") {
		t.Fatalf("err = %v", err)
	}
}
