package egress

import (
	"context"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

type subscriptionSyncRepositoryStub struct {
	OperationsRepository
	mu    sync.Mutex
	nodes map[uint64][]domain.Node
}

func (r *subscriptionSyncRepositoryStub) UpsertEgressNodesFromSource(_ context.Context, sourceID uint64, nodes []domain.Node) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nodes[sourceID] = append([]domain.Node(nil), nodes...)
	return len(nodes), nil
}

func (r *subscriptionSyncRepositoryStub) UpdateEgressSourceSync(context.Context, uint64, time.Time, time.Time, int, string) error {
	return nil
}

func (r *subscriptionSyncRepositoryStub) nodesFor(sourceID uint64) []domain.Node {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domain.Node(nil), r.nodes[sourceID]...)
}

func TestParseProxySubscriptionAcceptsPlainAndBase64Lists(t *testing.T) {
	plain, skipped, err := parseProxySubscription(strings.Join([]string{
		"# proxy list",
		"http://user:pass@one.example:8080",
		"socks5h://two.example:1080",
		"http://user:pass@one.example:8080",
		"not a proxy",
	}, "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != 2 || skipped != 2 {
		t.Fatalf("plain entries=%d skipped=%d", len(plain), skipped)
	}
	for _, entry := range plain {
		if entry.ProxyURL == "" || len(entry.Key) != 64 {
			t.Fatalf("unsafe parsed entry: %#v", entry)
		}
	}

	encodedInput := base64.RawStdEncoding.EncodeToString([]byte("https://three.example:8443\nsocks4a://four.example:1080\n"))
	encoded, encodedSkipped, err := parseProxySubscription(encodedInput)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != 2 || encodedSkipped != 0 {
		t.Fatalf("base64 entries=%d skipped=%d", len(encoded), encodedSkipped)
	}
}

func TestParseProxySubscriptionRejectsNoUsableEntries(t *testing.T) {
	if _, _, err := parseProxySubscription("# only comments\nfile:///tmp/proxies\n"); err == nil {
		t.Fatal("invalid proxy subscription was accepted")
	}
}

func TestParseProxySubscriptionImportsSupportedTunnelSchemes(t *testing.T) {
	vmess := "vmess://" + base64.RawStdEncoding.EncodeToString([]byte(`{"v":"2","ps":"node","add":"proxy.example","port":"443","id":"123e4567-e89b-12d3-a456-426614174000","aid":"0","scy":"auto","net":"tcp"}`))
	entries, skipped, err := parseProxySubscription(strings.Join([]string{
		"http://proxy.example:8080",
		"trojan://password@proxy.example:443#remark",
		"vless://123e4567-e89b-12d3-a456-426614174000@proxy.example:443?encryption=none#remark",
		"ss://YWVzLTEyOC1nY206c2VjcmV0@proxy.example:8388#remark",
		vmess,
		"hysteria2://proxy.example:443",
		"tuic://user:pass@proxy.example:443",
	}, "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 5 || skipped != 2 || entries[0].ProxyURL != "http://proxy.example:8080" {
		t.Fatalf("entries=%#v skipped=%d", entries, skipped)
	}
	for _, entry := range entries[1:] {
		if strings.Contains(entry.ProxyURL, "#") {
			t.Fatalf("subscription remark was retained: %q", entry.ProxyURL)
		}
	}
}

func TestIsPublicAddressRejectsNonPublicRanges(t *testing.T) {
	for _, raw := range []string{
		"0.0.0.1", "10.0.0.1", "100.64.0.1", "127.0.0.1", "169.254.10.1",
		"192.0.0.1", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "240.0.0.1",
		"::1", "fc00::1", "2001:db8::1", "::ffff:127.0.0.1",
	} {
		if isPublicAddress(netip.MustParseAddr(raw)) {
			t.Fatalf("non-public address accepted: %s", raw)
		}
	}
	if !isPublicAddress(netip.MustParseAddr("1.1.1.1")) {
		t.Fatal("public address rejected")
	}
}

func TestValidatePublicSubscriptionTargetRejectsPrivateAddresses(t *testing.T) {
	for _, value := range []string{
		"http://127.0.0.1/subscription",
		"http://10.0.0.1/subscription",
		"http://169.254.169.254/latest/meta-data",
		"http://[::1]/subscription",
	} {
		if err := validatePublicSubscriptionTarget(context.Background(), value); err == nil {
			t.Fatalf("private subscription target accepted: %s", value)
		}
	}
	for _, value := range []string{"https://1.1.1.1/subscription", "https://[2606:4700:4700::1111]/subscription"} {
		if err := validatePublicSubscriptionTarget(context.Background(), value); err != nil {
			t.Fatalf("public subscription target rejected: %s: %v", value, err)
		}
	}
}

func TestSubscriptionProxyForwardDialerBoundsHandshakeConnection(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	dialer := &subscriptionProxyForwardDialer{timeout: 20 * time.Millisecond}
	connection, err := dialer.withDeadline(client, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	started := time.Now()
	buffer := make([]byte, 1)
	_, err = connection.Read(buffer)
	if err == nil {
		t.Fatal("connection without peer data did not reach its handshake deadline")
	}
	if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatalf("deadline error=%v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("handshake deadline took %s", elapsed)
	}
}

func TestSubscriptionTransportSupportsConfiguredProxyProtocols(t *testing.T) {
	for _, proxyURL := range []string{
		"http://127.0.0.1:8080",
		"https://127.0.0.1:8443",
		"socks4://127.0.0.1:1080",
		"socks4a://127.0.0.1:1080",
		"socks5://127.0.0.1:1080",
		"socks5h://127.0.0.1:1080",
	} {
		transport, err := subscriptionTransport(proxyURL)
		if err != nil {
			t.Fatalf("proxy %s: %v", proxyURL, err)
		}
		transport.CloseIdleConnections()
	}
}

func TestSubscriptionFetchProxyRejectsCorruptSourceSecret(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{cipher: cipher}
	if _, err := service.subscriptionFetchProxy(domain.SubscriptionSource{EncryptedProxyURL: "not-ciphertext"}); err == nil {
		t.Fatal("corrupt per-source subscription proxy was accepted")
	}
}

func TestSyncSourceUsesItsOwnFetchProxy(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	type proxyFixture struct {
		hits      atomic.Int32
		server    *httptest.Server
		nodeProxy string
	}
	newProxy := func(nodeProxy string) *proxyFixture {
		fixture := &proxyFixture{nodeProxy: nodeProxy}
		fixture.server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			fixture.hits.Add(1)
			if request.URL.Host != "1.1.1.1" {
				response.WriteHeader(http.StatusBadGateway)
				return
			}
			_, _ = response.Write([]byte(fixture.nodeProxy + "\n"))
		}))
		t.Cleanup(fixture.server.Close)
		return fixture
	}
	firstProxy := newProxy("socks5h://first-node.example:1080")
	secondProxy := newProxy("http://second-node.example:8080")

	encryptedSourceURL, err := cipher.Encrypt("http://1.1.1.1/subscription")
	if err != nil {
		t.Fatal(err)
	}
	repository := &subscriptionSyncRepositoryStub{nodes: make(map[uint64][]domain.Node)}
	service := &Service{cipher: cipher}
	for id, fixture := range map[uint64]*proxyFixture{1: firstProxy, 2: secondProxy} {
		encryptedProxyURL, encryptErr := cipher.Encrypt(fixture.server.URL)
		if encryptErr != nil {
			t.Fatal(encryptErr)
		}
		_, syncErr := service.syncSource(context.Background(), repository, domain.SubscriptionSource{
			ID: id, Name: "source", Scope: domain.ScopeBuild, Enabled: true,
			EncryptedURL: encryptedSourceURL, EncryptedProxyURL: encryptedProxyURL, RefreshIntervalSeconds: 900,
		})
		if syncErr != nil {
			t.Fatalf("sync source %d: %v", id, syncErr)
		}
	}

	if firstProxy.hits.Load() != 1 || secondProxy.hits.Load() != 1 {
		t.Fatalf("proxy hits: first=%d second=%d", firstProxy.hits.Load(), secondProxy.hits.Load())
	}
	for id, expected := range map[uint64]string{1: firstProxy.nodeProxy, 2: secondProxy.nodeProxy} {
		nodes := repository.nodesFor(id)
		if len(nodes) != 1 {
			t.Fatalf("source %d imported %d nodes", id, len(nodes))
		}
		actual, decryptErr := cipher.Decrypt(nodes[0].EncryptedProxyURL)
		if decryptErr != nil {
			t.Fatal(decryptErr)
		}
		if actual != expected {
			t.Fatalf("source %d imported proxy %q, want %q", id, actual, expected)
		}
	}
}
