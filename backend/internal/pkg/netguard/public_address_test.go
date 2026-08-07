package netguard

import (
	"net/netip"
	"testing"
)

func TestIsPublicAddress(t *testing.T) {
	for _, raw := range []string{
		"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1", "0.0.0.1",
		"192.0.2.1", "198.18.0.1", "240.0.0.1", "::1", "fd00::1", "2001:db8::1",
		"64:ff9b::127.0.0.1", "64:ff9b:1::1", "100::1", "2002:7f00:1::", "3fff::1", "5f00::1",
	} {
		if IsPublicAddress(netip.MustParseAddr(raw)) {
			t.Errorf("address %s was allowed", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !IsPublicAddress(netip.MustParseAddr(raw)) {
			t.Errorf("public address %s was blocked", raw)
		}
	}
}
