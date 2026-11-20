package web

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	fhttptest "github.com/bogdanfinn/fhttp/httptest"
	"github.com/bogdanfinn/websocket"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

func TestWebNonStreamingControlFramesDoNotResetIdle(t *testing.T) {
	server := fhttptest.NewServer(fhttp.HandlerFunc(func(writer fhttp.ResponseWriter, request *fhttp.Request) {
		connection, err := (&websocket.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		if _, _, err := connection.ReadMessage(); err != nil {
			return
		}
		for range 7 {
			if err := connection.WriteJSON(map[string]any{"event": map[string]any{"type": "pong"}}); err != nil {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		if err := connection.WriteJSON(map[string]any{"event": map[string]any{
			"type": "response.done", "response": map[string]any{"status": "completed", "id": "response-test"},
		}}); err != nil {
			return
		}
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
	adapter := NewAdapter(
		Config{BaseURL: server.URL, ChatTimeoutSeconds: 5, StreamIdleTimeoutSeconds: 1},
		infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil,
	)
	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: account.Credential{
			ID: 1, Provider: account.ProviderWeb, UserID: "497f19f8-49d4-458a-bee4-43ec3dcaf8ca", EncryptedAccessToken: encrypted,
		},
		Method: http.MethodPost, Path: "/v1/chat/completions", Model: "grok-chat-fast", Operation: conversation.OperationChat,
		Body: []byte(`{"model":"grok-chat-fast","messages":[{"role":"user","content":"test"}],"stream":false}`),
	})
	if response != nil {
		defer response.Body.Close()
	}
	if !errors.Is(err, neterror.ErrUpstreamStreamIdleTimeout) {
		t.Fatalf("control-only stream error = %v, want upstream idle timeout", err)
	}
	if neterror.IdleTimeoutObservedData(err) {
		t.Fatal("control frames must not count as generated output")
	}
}

type closeObservedSource struct {
	io.ReadCloser
	closed chan struct{}
	once   sync.Once
}

func (source *closeObservedSource) Close() error {
	var closeErr error
	source.once.Do(func() {
		closeErr = source.ReadCloser.Close()
		close(source.closed)
	})
	return closeErr
}

func TestWebStreamCloseReleasesBlockedUpstream(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	source := &closeObservedSource{ReadCloser: reader, closed: make(chan struct{})}
	defer source.Close()
	adapter := &Adapter{egress: infraegress.NewManager(egressRepositoryStub{}, nil)}
	body := adapter.streamOpenAIResponse(
		context.Background(), source, &infraegress.Lease{}, account.Credential{},
		"response-test", "grok-chat-fast", "chat", "test", nil, toolConfiguration{},
		true, conversation.ResponseOptions{},
	)
	if _, err := body.Read(make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-source.closed:
	case <-time.After(time.Second):
		t.Fatal("closing the response did not close its blocked upstream")
	}
}
