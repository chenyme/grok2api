package web

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	fhttp "github.com/bogdanfinn/fhttp"
	fhttptest "github.com/bogdanfinn/fhttp/httptest"
	"github.com/bogdanfinn/websocket"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestIsClearanceRefreshableMediaError(t *testing.T) {
	tests := []struct {
		name string
		body string
		code int
		want bool
	}{
		{name: "empty challenge response", code: http.StatusForbidden, want: true},
		{name: "cloudflare html", code: http.StatusForbidden, body: "<!doctype html><title>Just a moment...</title>", want: true},
		{name: "structured moderation response", code: http.StatusForbidden, body: `{"error":{"code":"content-moderated","message":"rejected"}}`, want: false},
		{name: "server failure", code: http.StatusBadGateway, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := newWebMediaUpstreamError(test.code, []byte(test.body), false)
			if got := isClearanceRefreshableMediaError(err); got != test.want {
				t.Fatalf("refreshable=%v, want %v (kind=%q challenge=%v)", got, test.want, err.bodyKind, err.cloudflareChallenge)
			}
		})
	}
}

func TestWebMediaUpstreamErrorProviderResponseIsBounded(t *testing.T) {
	err := newWebMediaUpstreamError(http.StatusForbidden, nil, false)
	response := err.providerResponse()
	if response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("response=%#v", response)
	}
	body, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(body), "upstream_forbidden") || !strings.Contains(string(body), "Grok Web") {
		t.Fatalf("body=%s", body)
	}
}

func TestGenerateWSImageReacquiresAfterChallengeHandshake(t *testing.T) {
	var handshakes atomic.Int32
	server := fhttptest.NewServer(fhttp.HandlerFunc(func(writer fhttp.ResponseWriter, request *fhttp.Request) {
		if request.URL.Path != "/ws/imagine/listen" {
			fhttp.NotFound(writer, request)
			return
		}
		if handshakes.Add(1) == 1 {
			writer.Header().Set("Content-Type", "text/html")
			writer.WriteHeader(http.StatusForbidden)
			_, _ = writer.Write([]byte("<!doctype html><title>Just a moment...</title>"))
			return
		}
		connection, err := (&websocket.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(writer, request, nil)
		if err != nil {
			t.Errorf("upgrade Imagine WebSocket: %v", err)
			return
		}
		defer connection.Close()
		for range 2 {
			var message map[string]any
			if err := connection.ReadJSON(&message); err != nil {
				t.Errorf("read Imagine request: %v", err)
				return
			}
		}
		_ = connection.WriteJSON(map[string]any{
			"type": "image", "id": "image-1", "blob": "aW1hZ2U=", "percentage_complete": 100, "grid_index": 0,
		})
		_ = connection.WriteJSON(map[string]any{
			"type": "json", "id": "image-1", "current_status": "completed", "moderated": false, "order": 0,
		})
	}))
	defer server.Close()

	adapter, credential := testMediaAdapter(t, server.URL)
	response, err := adapter.GenerateImage(context.Background(), provider.ImageGenerationRequest{
		Credential: credential, Model: "grok-imagine-image-quality", Prompt: "draw a teapot", Count: 1, ResponseFormat: "b64_json",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(response.Body)
	if readErr != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), `"b64_json"`) {
		t.Fatalf("status=%d body=%s err=%v", response.StatusCode, body, readErr)
	}
	if got := handshakes.Load(); got != 2 {
		t.Fatalf("handshakes=%d, want 2", got)
	}
}

func TestGenerateLiteImageReacquiresAfterChallengeHandshake(t *testing.T) {
	var handshakes atomic.Int32
	server := fhttptest.NewServer(fhttp.HandlerFunc(func(writer fhttp.ResponseWriter, request *fhttp.Request) {
		if request.URL.Path != "/ws/mgw/" {
			fhttp.NotFound(writer, request)
			return
		}
		if handshakes.Add(1) == 1 {
			writer.Header().Set("Content-Type", "text/html")
			writer.WriteHeader(http.StatusForbidden)
			_, _ = writer.Write([]byte("<!doctype html><title>Just a moment...</title>"))
			return
		}
		connection, err := (&websocket.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(writer, request, nil)
		if err != nil {
			t.Errorf("upgrade Gateway WebSocket: %v", err)
			return
		}
		defer connection.Close()
		var initial map[string]any
		if err := connection.ReadJSON(&initial); err != nil {
			t.Errorf("read Gateway session: %v", err)
			return
		}
		event, _ := initial["event"].(map[string]any)
		eventID, _ := event["event_id"].(string)
		_ = connection.WriteJSON(map[string]any{
			"session_id": "session-1", "event": map[string]any{"type": "session.created", "client_event_id": eventID},
		})
		_ = connection.WriteJSON(map[string]any{
			"session_id": "session-1", "event": map[string]any{"type": "conversation.attached", "conversation": map[string]any{"id": "session-1"}},
		})
		for range 2 {
			var message map[string]any
			if err := connection.ReadJSON(&message); err != nil {
				t.Errorf("read Gateway turn: %v", err)
				return
			}
		}
		_ = connection.WriteJSON(map[string]any{
			"session_id": "session-1",
			"event": map[string]any{
				"type": "response.grok.output",
				"output": map[string]any{"card_attachment": map[string]any{"jsonData": map[string]any{
					"id": "card-1", "image_chunk": map[string]any{"progress": 100, "imageUrl": "users/test/generated/image.jpg", "moderated": false},
				}}},
			},
		})
	}))
	defer server.Close()

	adapter, credential := testMediaAdapter(t, server.URL)
	credential.UserID = "497f19f8-49d4-458a-bee4-43ec3dcaf8ca"
	spec, ok := Resolve("grok-imagine-image")
	if !ok {
		t.Fatal("missing Lite image model")
	}
	rawURL, err := adapter.generateLiteImageURL(context.Background(), credential, spec, "draw a teapot")
	if err != nil {
		t.Fatal(err)
	}
	if rawURL != "https://assets.grok.com/users/test/generated/image.jpg" {
		t.Fatalf("url=%q", rawURL)
	}
	if got := handshakes.Load(); got != 2 {
		t.Fatalf("handshakes=%d, want 2", got)
	}
}

func testMediaAdapter(t *testing.T, baseURL string) (*Adapter, account.Credential) {
	t.Helper()
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("test-sso")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{BaseURL: baseURL, StatsigMode: "manual", ChatTimeoutSeconds: 5}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, imageAssetStoreStub{})
	credential := account.Credential{ID: 1, Provider: account.ProviderWeb, EncryptedAccessToken: encrypted}
	return adapter, credential
}
