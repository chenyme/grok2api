package sessionidentity

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestParseBlockedSessionIsUnauthorized(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte(`{"status":"blocked"}`))
	if !errors.Is(err, provider.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}

func TestParseBlockedSessionWithIdentityFieldsIsUnauthorized(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte(`{"status":"blocked","userId":"user-1","email":"a@example.com","session":{"userId":"user-1","email":"a@example.com"}}`))
	if !errors.Is(err, provider.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized even when identity fields are present", err)
	}
}

func TestParseUnauthenticatedSessionIsUnauthorized(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte(`{"status":"unauthenticated"}`))
	if !errors.Is(err, provider.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}

func TestParseUnauthenticatedSessionWithIdentityFieldsIsUnauthorized(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte(`{"status":"unauthenticated","userId":"user-1","email":"a@example.com"}`))
	if !errors.Is(err, provider.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized even when identity fields are present", err)
	}
}

func TestParseMissingIdentityWithoutStatusStaysGeneric(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte(`{}`))
	if err == nil || errors.Is(err, provider.ErrUnauthorized) {
		t.Fatalf("err = %v, want generic missing-identity error", err)
	}
}

func TestParseAuthenticatedSession(t *testing.T) {
	t.Parallel()
	identity, err := Parse([]byte(`{
		"status":"authenticated",
		"session":{"userId":"user-1","email":"a@example.com","organizationId":"org-1"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if identity.UserID != "user-1" || identity.Email != "a@example.com" || identity.TeamID != "org-1" {
		t.Fatalf("identity = %#v", identity)
	}
}

type sessionEgressRepoStub struct {
	mu    sync.Mutex
	nodes map[uint64]domainegress.Node
}

func (r *sessionEgressRepoStub) ListEgressNodes(_ context.Context, scope domainegress.Scope, _ repository.SortQuery) ([]domainegress.Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]domainegress.Node, 0, len(r.nodes))
	for _, node := range r.nodes {
		if scope == "" || node.Scope == scope {
			result = append(result, node)
		}
	}
	return result, nil
}

func (r *sessionEgressRepoStub) GetEgressNode(_ context.Context, id uint64) (domainegress.Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	node, ok := r.nodes[id]
	if !ok {
		return domainegress.Node{}, repository.ErrNotFound
	}
	return node, nil
}

func (r *sessionEgressRepoStub) CreateEgressNode(context.Context, domainegress.Node) (domainegress.Node, error) {
	return domainegress.Node{}, errors.New("unsupported")
}

func (r *sessionEgressRepoStub) UpdateEgressNode(_ context.Context, node domainegress.Node) (domainegress.Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nodes[node.ID] = node
	return node, nil
}

func (r *sessionEgressRepoStub) DeleteEgressNode(context.Context, uint64) error {
	return errors.New("unsupported")
}

func TestFetchSelectsScopeByProviderAndReportsLeaseScope(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/auth/session" {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"status":"authenticated","session":{"userId":"u-scope","email":"scope@example.com"}}`)
	}))
	defer origin.Close()

	var connsMu sync.Mutex
	var hijackedConns []net.Conn
	t.Cleanup(func() {
		connsMu.Lock()
		for _, conn := range hijackedConns {
			_ = conn.Close()
		}
		connsMu.Unlock()
	})
	proxyServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodConnect {
			http.Error(writer, "connect required", http.StatusMethodNotAllowed)
			return
		}
		upstream, err := net.DialTimeout("tcp", request.Host, 2*time.Second)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadGateway)
			return
		}
		hijacker, ok := writer.(http.Hijacker)
		if !ok {
			_ = upstream.Close()
			http.Error(writer, "hijack unsupported", http.StatusInternalServerError)
			return
		}
		clientConn, _, err := hijacker.Hijack()
		if err != nil {
			_ = upstream.Close()
			return
		}
		connsMu.Lock()
		hijackedConns = append(hijackedConns, clientConn, upstream)
		connsMu.Unlock()
		_, _ = io.WriteString(clientConn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go func() {
			defer upstream.Close()
			defer clientConn.Close()
			_, _ = io.Copy(upstream, clientConn)
		}()
		go func() {
			defer upstream.Close()
			defer clientConn.Close()
			_, _ = io.Copy(clientConn, upstream)
		}()
	}))
	defer proxyServer.Close()

	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encryptedProxy, err := cipher.Encrypt(proxyServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	encryptedSSO, err := cipher.Encrypt("test-sso")
	if err != nil {
		t.Fatal(err)
	}
	repo := &sessionEgressRepoStub{nodes: map[uint64]domainegress.Node{
		1: {ID: 1, Name: "web-node", Scope: domainegress.ScopeWeb, Enabled: true, Health: 1, EncryptedProxyURL: encryptedProxy},
		2: {ID: 2, Name: "console-node", Scope: domainegress.ScopeConsole, Enabled: true, Health: 1, EncryptedProxyURL: encryptedProxy},
	}}
	manager := infraegress.NewManager(repo, cipher)

	ctxConsole, traceConsole := infraegress.WithTrace(context.Background())
	identity, err := Fetch(ctxConsole, origin.URL, account.Credential{
		ID: 10, Provider: account.ProviderConsole, AuthType: account.AuthTypeSSO, EncryptedAccessToken: encryptedSSO, EgressNodeID: 2,
	}, manager, cipher)
	if err != nil || identity.UserID != "u-scope" {
		t.Fatalf("console Fetch = %#v, %v", identity, err)
	}
	if sel, ok := traceConsole.Selection(domainegress.ScopeConsole); !ok || sel.NodeID != 2 {
		t.Fatalf("console selection = %#v, ok=%v", sel, ok)
	}

	ctxWeb, traceWeb := infraegress.WithTrace(context.Background())
	identity, err = Fetch(ctxWeb, origin.URL, account.Credential{
		ID: 11, Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, EncryptedAccessToken: encryptedSSO, EgressNodeID: 1,
	}, manager, cipher)
	if err != nil || identity.UserID != "u-scope" {
		t.Fatalf("web Fetch = %#v, %v", identity, err)
	}
	if sel, ok := traceWeb.Selection(domainegress.ScopeWeb); !ok || sel.NodeID != 1 {
		t.Fatalf("web selection = %#v, ok=%v", sel, ok)
	}
}
