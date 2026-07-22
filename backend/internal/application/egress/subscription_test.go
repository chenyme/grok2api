package egress

import (
	"encoding/base64"
	"net/netip"
	"strings"
	"testing"
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
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "169.254.10.1", "100.64.0.1", "::1", "fc00::1"} {
		if isPublicAddress(netip.MustParseAddr(raw)) {
			t.Fatalf("non-public address accepted: %s", raw)
		}
	}
	if !isPublicAddress(netip.MustParseAddr("1.1.1.1")) {
		t.Fatal("public address rejected")
	}
}
