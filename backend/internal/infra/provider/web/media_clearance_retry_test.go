package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	var solverCalls atomic.Int32
	server := fhttptest.NewServer(fhttp.HandlerFunc(func(writer fhttp.ResponseWriter, request *fhttp.Request) {
		if request.URL.Path == "/v1" {
			writeTestClearanceSolution(t, writer, solverCalls.Add(1))
			return
		}
		if request.URL.Path != "/ws/imagine/listen" {
			fhttp.NotFound(writer, request)
			return
		}
		handshake := handshakes.Add(1)
		if !strings.Contains(request.Header.Get("Cookie"), fmt.Sprintf("cf_clearance=clearance-%d", handshake)) {
			t.Errorf("handshake %d did not use refreshed Clearance: %q", handshake, request.Header.Get("Cookie"))
		}
		if handshake == 1 {
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
	enableTestClearance(adapter, server.URL)
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
	if got := solverCalls.Load(); got != 2 {
		t.Fatalf("solver calls=%d, want 2", got)
	}
}

func TestGenerateLiteImageReacquiresAfterChallengeHandshake(t *testing.T) {
	var handshakes atomic.Int32
	var solverCalls atomic.Int32
	server := fhttptest.NewServer(fhttp.HandlerFunc(func(writer fhttp.ResponseWriter, request *fhttp.Request) {
		if request.URL.Path == "/v1" {
			writeTestClearanceSolution(t, writer, solverCalls.Add(1))
			return
		}
		if request.URL.Path != "/ws/mgw/" {
			fhttp.NotFound(writer, request)
			return
		}
		handshake := handshakes.Add(1)
		if !strings.Contains(request.Header.Get("Cookie"), fmt.Sprintf("cf_clearance=clearance-%d", handshake)) {
			t.Errorf("handshake %d did not use refreshed Clearance: %q", handshake, request.Header.Get("Cookie"))
		}
		if handshake == 1 {
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
	enableTestClearance(adapter, server.URL)
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
	if got := solverCalls.Load(); got != 2 {
		t.Fatalf("solver calls=%d, want 2", got)
	}
}

func TestStructuredImageForbiddenDoesNotInvalidateClearance(t *testing.T) {
	var handshakes atomic.Int32
	var solverCalls atomic.Int32
	server := fhttptest.NewServer(fhttp.HandlerFunc(func(writer fhttp.ResponseWriter, request *fhttp.Request) {
		switch request.URL.Path {
		case "/v1":
			writeTestClearanceSolution(t, writer, solverCalls.Add(1))
		case "/ws/imagine/listen":
			handshakes.Add(1)
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusForbidden)
			_, _ = writer.Write([]byte(`{"error":{"code":"content-moderated","message":"rejected"}}`))
		default:
			fhttp.NotFound(writer, request)
		}
	}))
	defer server.Close()

	adapter, credential := testMediaAdapter(t, server.URL)
	enableTestClearance(adapter, server.URL)
	for range 2 {
		response, err := adapter.GenerateImage(context.Background(), provider.ImageGenerationRequest{
			Credential: credential, Model: "grok-imagine-image-quality", Prompt: "draw a teapot", Count: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusForbidden || !json.Valid(body) {
			t.Fatalf("status=%d body=%s err=%v", response.StatusCode, body, readErr)
		}
	}
	if got := handshakes.Load(); got != 2 {
		t.Fatalf("handshakes=%d, want one per request", got)
	}
	if got := solverCalls.Load(); got != 1 {
		t.Fatalf("structured 403 refreshed Clearance: solver calls=%d, want 1", got)
	}
}

func TestLiteChallengeRetryExhaustionReturnsNormalizedJSON(t *testing.T) {
	var handshakes atomic.Int32
	var solverCalls atomic.Int32
	server := fhttptest.NewServer(fhttp.HandlerFunc(func(writer fhttp.ResponseWriter, request *fhttp.Request) {
		switch request.URL.Path {
		case "/v1":
			writeTestClearanceSolution(t, writer, solverCalls.Add(1))
		case "/ws/mgw/":
			handshakes.Add(1)
			writer.Header().Set("Content-Type", "text/html")
			writer.WriteHeader(http.StatusForbidden)
			_, _ = writer.Write([]byte("<!doctype html><title>Just a moment...</title>"))
		default:
			fhttp.NotFound(writer, request)
		}
	}))
	defer server.Close()

	adapter, credential := testMediaAdapter(t, server.URL)
	enableTestClearance(adapter, server.URL)
	credential.UserID = "497f19f8-49d4-458a-bee4-43ec3dcaf8ca"
	response, err := adapter.GenerateImage(context.Background(), provider.ImageGenerationRequest{
		Credential: credential, Model: "grok-imagine-image", Prompt: "draw a teapot", Count: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusForbidden || !json.Valid(body) {
		t.Fatalf("status=%d body=%s err=%v", response.StatusCode, body, readErr)
	}
	if strings.Contains(strings.ToLower(string(body)), "<!doctype") || !strings.Contains(string(body), "upstream_forbidden") {
		t.Fatalf("challenge body was not normalized: %s", body)
	}
	if handshakes.Load() != 2 || solverCalls.Load() != 2 {
		t.Fatalf("handshakes=%d solver calls=%d, want 2/2", handshakes.Load(), solverCalls.Load())
	}
}

func TestImageEditReplaysWholeFlowWithRefreshedClearance(t *testing.T) {
	for _, challengeStage := range []string{"upload", "media_post", "generation"} {
		t.Run(challengeStage, func(t *testing.T) {
			var solverCalls atomic.Int32
			var uploadCalls atomic.Int32
			var mediaPostCalls atomic.Int32
			var generationCalls atomic.Int32
			server := fhttptest.NewServer(fhttp.HandlerFunc(func(writer fhttp.ResponseWriter, request *fhttp.Request) {
				switch request.URL.Path {
				case "/v1":
					writeTestClearanceSolution(t, writer, solverCalls.Add(1))
					return
				case "/http/upload-file-v2/direct":
					call := uploadCalls.Add(1)
					assertTestClearanceCookie(t, request, solverCalls.Load())
					if challengeStage == "upload" && call == 1 {
						writeTestChallenge(writer)
						return
					}
					writer.Header().Set("Content-Type", "application/json")
					_, _ = writer.Write([]byte(`{"uploadId":"upload-1","fileMetadata":{"fileMetadataId":"file-1","fileUri":"users/test/reference/content"}}`))
					return
				case "/rest/media/post/create":
					call := mediaPostCalls.Add(1)
					assertTestClearanceCookie(t, request, solverCalls.Load())
					if challengeStage == "media_post" && call == 1 {
						writeTestChallenge(writer)
						return
					}
					writer.Header().Set("Content-Type", "application/json")
					_, _ = writer.Write([]byte(`{"post":{"id":"post-1"}}`))
					return
				case "/rest/app-chat/conversations/new":
					call := generationCalls.Add(1)
					assertTestClearanceCookie(t, request, solverCalls.Load())
					if challengeStage == "generation" && call == 1 {
						writeTestChallenge(writer)
						return
					}
					writer.Header().Set("Content-Type", "text/event-stream")
					_, _ = writer.Write([]byte("data: {}\n\n"))
					return
				default:
					fhttp.NotFound(writer, request)
				}
			}))
			defer server.Close()

			adapter, credential := testMediaAdapter(t, server.URL)
			enableTestClearance(adapter, server.URL)
			response, err := adapter.EditImage(context.Background(), provider.ImageEditRequest{
				Credential: credential,
				ImageURLs:  []string{"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="},
				Prompt:     "turn it blue",
				Count:      1,
				Resolution: "1k",
				Streaming:  true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if response == nil || response.StatusCode != http.StatusOK {
				t.Fatalf("response=%#v", response)
			}
			_ = response.Body.Close()

			wantUpload, wantMediaPost, wantGeneration := int32(2), int32(1), int32(1)
			switch challengeStage {
			case "media_post":
				wantMediaPost = 2
			case "generation":
				wantMediaPost = 2
				wantGeneration = 2
			}
			if solverCalls.Load() != 2 || uploadCalls.Load() != wantUpload || mediaPostCalls.Load() != wantMediaPost || generationCalls.Load() != wantGeneration {
				t.Fatalf("solver/upload/post/generation=%d/%d/%d/%d, want 2/%d/%d/%d",
					solverCalls.Load(), uploadCalls.Load(), mediaPostCalls.Load(), generationCalls.Load(), wantUpload, wantMediaPost, wantGeneration)
			}
		})
	}
}

func TestImageEditStructuredForbiddenDoesNotRetryOrRefresh(t *testing.T) {
	for _, rejectedStage := range []string{"upload", "generation"} {
		t.Run(rejectedStage, func(t *testing.T) {
			var solverCalls atomic.Int32
			var uploadCalls atomic.Int32
			var mediaPostCalls atomic.Int32
			var generationCalls atomic.Int32
			server := fhttptest.NewServer(fhttp.HandlerFunc(func(writer fhttp.ResponseWriter, request *fhttp.Request) {
				switch request.URL.Path {
				case "/v1":
					writeTestClearanceSolution(t, writer, solverCalls.Add(1))
				case "/http/upload-file-v2/direct":
					uploadCalls.Add(1)
					assertTestClearanceCookie(t, request, solverCalls.Load())
					if rejectedStage == "upload" {
						writeTestPolicyForbidden(writer)
						return
					}
					writer.Header().Set("Content-Type", "application/json")
					_, _ = writer.Write([]byte(`{"uploadId":"upload-1","fileMetadata":{"fileMetadataId":"file-1","fileUri":"users/test/reference/content"}}`))
				case "/rest/media/post/create":
					mediaPostCalls.Add(1)
					assertTestClearanceCookie(t, request, solverCalls.Load())
					writer.Header().Set("Content-Type", "application/json")
					_, _ = writer.Write([]byte(`{"post":{"id":"post-1"}}`))
				case "/rest/app-chat/conversations/new":
					generationCalls.Add(1)
					assertTestClearanceCookie(t, request, solverCalls.Load())
					writeTestPolicyForbidden(writer)
				default:
					fhttp.NotFound(writer, request)
				}
			}))
			defer server.Close()

			adapter, credential := testMediaAdapter(t, server.URL)
			enableTestClearance(adapter, server.URL)
			for range 2 {
				response, err := adapter.EditImage(context.Background(), provider.ImageEditRequest{
					Credential: credential,
					ImageURLs:  []string{"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="},
					Prompt:     "turn it blue",
					Count:      1,
					Resolution: "1k",
				})
				if err != nil {
					t.Fatal(err)
				}
				body, readErr := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if readErr != nil || response.StatusCode != http.StatusForbidden || !json.Valid(body) {
					t.Fatalf("status=%d body=%s err=%v", response.StatusCode, body, readErr)
				}
			}

			if solverCalls.Load() != 1 {
				t.Fatalf("structured 403 refreshed Clearance: solver calls=%d, want 1", solverCalls.Load())
			}
			if uploadCalls.Load() != 2 {
				t.Fatalf("upload calls=%d, want 2", uploadCalls.Load())
			}
			if rejectedStage == "upload" {
				if mediaPostCalls.Load() != 0 || generationCalls.Load() != 0 {
					t.Fatalf("post/generation calls=%d/%d, want 0/0", mediaPostCalls.Load(), generationCalls.Load())
				}
			} else if mediaPostCalls.Load() != 2 || generationCalls.Load() != 2 {
				t.Fatalf("post/generation calls=%d/%d, want 2/2", mediaPostCalls.Load(), generationCalls.Load())
			}
		})
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
	adapter := NewAdapter(Config{BaseURL: baseURL, StatsigMode: "manual", ChatTimeoutSeconds: 5, ImageTimeoutSeconds: 5, MaxInputImageBytes: 1 << 20}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, imageAssetStoreStub{})
	credential := account.Credential{ID: 1, Provider: account.ProviderWeb, EncryptedAccessToken: encrypted}
	return adapter, credential
}

func enableTestClearance(adapter *Adapter, solverURL string) {
	adapter.egress.UpdateClearanceConfig(infraegress.ClearanceConfig{
		Mode: "flaresolverr", FlareSolverrURL: solverURL, TargetURL: "https://grok.com", Timeout: time.Second, RefreshInterval: time.Hour,
	})
}

func writeTestClearanceSolution(t *testing.T, writer fhttp.ResponseWriter, sequence int32) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(map[string]any{
		"status": "ok",
		"solution": map[string]any{
			"userAgent": "Mozilla/5.0 Chrome/146.0.0.0 Safari/537.36",
			"cookies":   []any{map[string]any{"name": "cf_clearance", "value": fmt.Sprintf("clearance-%d", sequence)}},
		},
	}); err != nil {
		t.Errorf("write Clearance solution: %v", err)
	}
}

func assertTestClearanceCookie(t *testing.T, request *fhttp.Request, sequence int32) {
	t.Helper()
	want := fmt.Sprintf("cf_clearance=clearance-%d", sequence)
	if !strings.Contains(request.Header.Get("Cookie"), want) {
		t.Errorf("request %s did not use %s: %q", request.URL.Path, want, request.Header.Get("Cookie"))
	}
}

func writeTestChallenge(writer fhttp.ResponseWriter) {
	writer.Header().Set("Content-Type", "text/html")
	writer.WriteHeader(http.StatusForbidden)
	_, _ = writer.Write([]byte("<!doctype html><title>Just a moment...</title>"))
}

func writeTestPolicyForbidden(writer fhttp.ResponseWriter) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusForbidden)
	_, _ = writer.Write([]byte(`{"error":{"code":"content-moderated","message":"rejected"}}`))
}
