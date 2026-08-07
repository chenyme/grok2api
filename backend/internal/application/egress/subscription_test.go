package egress

import (
	"context"
	"encoding/base64"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

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
