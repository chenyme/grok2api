package egress

import (
	"errors"
	"strings"
	"testing"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
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

func TestNormalizeProxyURLAllowsAccountPlaceholderOnlyInUsername(t *testing.T) {
	value, err := NormalizeProxyURL("socks5h://Default.{account}:token@resin:2260")
	if err != nil {
		t.Fatal(err)
	}
	if value != "socks5h://Default.%7Baccount%7D:token@resin:2260" && value != "socks5h://Default.{account}:token@resin:2260" {
		t.Fatalf("normalized Resin proxy = %q", value)
	}
	if !strings.Contains(value, ProxyAccountPlaceholder) {
		t.Fatalf("account placeholder was lost: %q", value)
	}
	for _, invalid := range []string{
		"socks5h://user:token@{account}:2260",
		"socks5h://user:{account}@resin:2260",
		"socks5h://{account}:{account}@resin:2260",
		"socks5h://grok2api_account_placeholder:token@{account}:2260",
	} {
		if _, err := NormalizeProxyURL(invalid); err == nil {
			t.Fatalf("invalid account placeholder accepted: %q", invalid)
		}
	}
}

func TestServiceRejectsRemovedAllScope(t *testing.T) {
	service := &Service{}
	_, err := service.applyInput(domain.Node{}, Input{Name: "legacy", Scope: domain.Scope("all"), Enabled: true}, true)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("all scope error = %v", err)
	}
}

func TestBuildNodeAlwaysUsesProviderUserAgent(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(nil, cipher, "web-agent", "console-agent")
	value, err := service.applyInput(domain.Node{UserAgent: "legacy-build-agent"}, Input{
		Name: "build", Scope: domain.ScopeBuild, Enabled: true, UserAgent: "custom-build-agent",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if value.UserAgent != "" || publicNode(value).UserAgent != "" {
		t.Fatalf("build node userAgent = %q", value.UserAgent)
	}
	if defaults := service.DefaultUserAgents(); defaults[string(domain.ScopeBuild)] != "" || defaults[string(domain.ScopeWeb)] != "web-agent" || defaults[string(domain.ScopeConsole)] != "console-agent" {
		t.Fatalf("default user agents = %#v", defaults)
	}
}

func TestConsoleNodeUsesConsoleDefaultUserAgent(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(nil, cipher, "web-agent", "console-agent")
	value, err := service.applyInput(domain.Node{}, Input{Name: "console", Scope: domain.ScopeConsole, Enabled: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	if value.UserAgent != "console-agent" {
		t.Fatalf("console node userAgent = %q", value.UserAgent)
	}
}
