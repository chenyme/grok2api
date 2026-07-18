package web

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestSyncAccountIdentityUsesWebBrowserIdentity(t *testing.T) {
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
		if request.Header.Get("Accept") != "*/*" || request.Header.Get("Referer") != "http://"+request.Host+"/" || request.Header.Get("Sec-Fetch-Site") != "same-origin" {
			t.Errorf("browser headers = %#v", request.Header)
		}
		if request.Header.Get("Sec-Ch-Ua") == "" || request.Header.Get("Sec-Ch-Ua-Platform") != `"macOS"` {
			t.Errorf("client hints = %#v", request.Header)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"user":{"id":"user-1","email":"User@Example.com","teamId":"team-1"}}`))
	}))
	t.Cleanup(server.Close)
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	token, _ := cipher.Encrypt("test-sso")
	cookies, _ := cipher.Encrypt("cf_clearance=clear")
	adapter := NewAdapter(Config{BaseURL: server.URL}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)
	identity, err := adapter.SyncAccountIdentity(context.Background(), account.Credential{
		ID: 1, Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO,
		EncryptedAccessToken: token, EncryptedCloudflareCookie: cookies,
	})
	if err != nil {
		t.Fatal(err)
	}
	if identity.UserID != "user-1" || identity.Email != "User@Example.com" || identity.TeamID != "team-1" {
		t.Fatalf("identity = %#v", identity)
	}
}

func TestParseAccountIdentityRejectsMissingIdentity(t *testing.T) {
	t.Parallel()
	if _, err := parseAccountIdentity([]byte(`{"user":{"name":"anonymous"}}`)); err == nil {
		t.Fatal("expected missing identity error")
	}
}

func TestParseAccountIdentityAcceptsAuthenticatedSessionEnvelope(t *testing.T) {
	t.Parallel()
	identity, err := parseAccountIdentity([]byte(`{"status":"authenticated","session":{"userId":"user-1","email":"user@example.com","organizationId":"org-1"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if identity.UserID != "user-1" || identity.Email != "user@example.com" || identity.TeamID != "org-1" {
		t.Fatalf("identity = %#v", identity)
	}
}
