package middleware

import (
	"errors"
	"net/http"
	"testing"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
)

func TestClientRuntimeStoreFailureUsesServiceUnavailable(t *testing.T) {
	err := errors.Join(clientkeyapp.ErrRuntimeUnavailable, errors.New("redis unavailable"))
	if status := clientErrorStatus(err); status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", status)
	}
	if code := clientErrorCode(err); code != "runtime_store_unavailable" {
		t.Fatalf("code = %q", code)
	}
	if message := clientErrorMessage(err); message == err.Error() {
		t.Fatal("runtime implementation detail leaked to client")
	}
}

func TestBearerTokenAcceptsCaseInsensitiveSchemeAndWhitespace(t *testing.T) {
	token, ok := bearerToken("  bearer\tsecret-token  ")
	if !ok || token != "secret-token" {
		t.Fatalf("token = %q, ok = %v", token, ok)
	}
	for _, value := range []string{"", "Bearer", "Basic token", "Bearer token extra"} {
		if _, ok := bearerToken(value); ok {
			t.Fatalf("header %q unexpectedly accepted", value)
		}
	}
}
