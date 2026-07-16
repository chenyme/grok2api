package web

import (
	"context"
	"net/netip"
	"testing"
)

type rebindingImageResolver struct {
	calls int
}

func (r *rebindingImageResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	r.calls++
	if r.calls == 1 {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
	return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
}

func TestRemoteImageTargetPinsFirstValidatedResolution(t *testing.T) {
	resolver := &rebindingImageResolver{}
	target, err := validateRemoteImageURLWithResolver(context.Background(), "https://images.example.test/photo.png?size=large", resolver)
	if err != nil {
		t.Fatal(err)
	}
	if resolver.calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolver.calls)
	}
	if target.originalURL.Host != "images.example.test" || target.fetchURL.Host != "93.184.216.34:443" {
		t.Fatalf("target = %#v", target)
	}
	if target.hostHeader != "images.example.test" || target.serverName != "images.example.test" {
		t.Fatalf("host=%q serverName=%q", target.hostHeader, target.serverName)
	}
	if target.fetchURL.Path != "/photo.png" || target.fetchURL.RawQuery != "size=large" {
		t.Fatalf("fetch URL = %s", target.fetchURL)
	}
}

func TestRemoteImageTargetRejectsAnyPrivateResolution(t *testing.T) {
	resolver := staticImageResolver{addresses: []netip.Addr{
		netip.MustParseAddr("93.184.216.34"),
		netip.MustParseAddr("10.0.0.8"),
	}}
	if _, err := validateRemoteImageURLWithResolver(context.Background(), "https://images.example.test/photo.png", resolver); err == nil {
		t.Fatal("mixed public and private DNS result was accepted")
	}
}

type staticImageResolver struct {
	addresses []netip.Addr
}

func (r staticImageResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return append([]netip.Addr(nil), r.addresses...), nil
}
