package cli

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestProbeResponsesMapsHTTPStatus(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/responses" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Fatalf("authorization = %q", got)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("content-type = %q", r.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"model":"grok-4.5"`) || !strings.Contains(string(body), `"store":false`) {
			t.Fatalf("body = %s", body)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp_1"}`))
	}))
	t.Cleanup(server.Close)

	adapter := NewAdapter(Config{
		BaseURL: server.URL, ClientVersion: "0.2.99", ClientIdentifier: "grok-shell",
		TokenAuth: "xai-grok-cli", UserAgent: "grok-shell/0.2.99",
	}, mustProbeCipher(t))

	status, err := adapter.ProbeResponses(context.Background(), account.Credential{ID: 1, UserID: "user-1"}, "access-token")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
}

func TestProbeResponsesNetworkError(t *testing.T) {
	t.Parallel()
	adapter := NewAdapter(Config{BaseURL: "http://127.0.0.1:1"}, mustProbeCipher(t))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	status, err := adapter.ProbeResponses(ctx, account.Credential{ID: 2}, "access-token")
	if err == nil {
		t.Fatal("expected network error")
	}
	if status != 0 {
		t.Fatalf("status = %d", status)
	}
}

func TestProbeResponsesEmptyToken(t *testing.T) {
	t.Parallel()
	adapter := NewAdapter(Config{BaseURL: "https://example.invalid"}, mustProbeCipher(t))
	status, err := adapter.ProbeResponses(context.Background(), account.Credential{ID: 3}, "  ")
	if err == nil || status != 0 {
		t.Fatalf("status=%d err=%v", status, err)
	}
}

func mustProbeCipher(t *testing.T) *security.Cipher {
	t.Helper()
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return cipher
}
