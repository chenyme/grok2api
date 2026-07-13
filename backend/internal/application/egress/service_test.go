package egress

import (
	"strings"
	"testing"
)

func TestSanitizeCloudflareCookiesDropsControlsAndNonCloudflareValues(t *testing.T) {
	value := SanitizeCloudflareCookies("CF_CLEARANCE=valid; __cf_bm=bad\r\nX-Leak: yes; sso=secret; cf_chl_test=ok")
	if value != "cf_clearance=valid; cf_chl_test=ok" {
		t.Fatalf("sanitized cookies = %q", value)
	}
	if strings.Contains(strings.ToLower(value), "sso") || strings.Contains(value, "\r") || strings.Contains(value, "\n") {
		t.Fatalf("unsafe cookie value = %q", value)
	}
}

func TestNormalizeProxyURLValidatesStructure(t *testing.T) {
	for _, raw := range []string{
		"http://user:password@127.0.0.1:8080", "https://proxy.example:8443",
		"socks4://127.0.0.1:1080", "socks4a://proxy.example:1080",
		"socks5://user:password@127.0.0.1:1080", "socks5h://user:password@proxy.example:1080",
	} {
		value, err := NormalizeProxyURL(raw)
		if err != nil || value == "" {
			t.Fatalf("valid proxy %q = %q, err = %v", raw, value, err)
		}
	}
	for _, invalid := range []string{"file:///tmp/proxy", "https://", "http://proxy.example/path", "http://proxy.example\r\nX-Leak: yes"} {
		if _, err := NormalizeProxyURL(invalid); err == nil {
			t.Fatalf("invalid proxy accepted: %q", invalid)
		}
	}
}
