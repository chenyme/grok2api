package egress

import (
	"errors"
	"strings"
	"testing"
	"time"

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
	service := NewService(nil, cipher, "browser-agent")
	value, err := service.applyInput(domain.Node{UserAgent: "legacy-build-agent"}, Input{
		Name: "build", Scope: domain.ScopeBuild, Enabled: true, UserAgent: "custom-build-agent",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if value.UserAgent != "" || service.publicNode(value).UserAgent != "" {
		t.Fatalf("build node userAgent = %q", value.UserAgent)
	}
	if defaults := service.DefaultUserAgents(); defaults[string(domain.ScopeBuild)] != "" || defaults[string(domain.ScopeWeb)] != "browser-agent" || defaults[string(domain.ScopeConsole)] != "browser-agent" {
		t.Fatalf("default user agents = %#v", defaults)
	}
}

func TestConsoleNodeUsesBrowserDefaultUserAgent(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(nil, cipher, "browser-agent")
	value, err := service.applyInput(domain.Node{}, Input{Name: "console", Scope: domain.ScopeConsole, Enabled: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	if value.UserAgent != "browser-agent" {
		t.Fatalf("console node userAgent = %q", value.UserAgent)
	}
}

func TestPublicNodeReportsAccountBoundProxy(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encryptedProxy, err := cipher.Encrypt("socks5h://Default.{account}:token@resin:2260")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(nil, cipher, "browser-agent")
	cooldown := time.Now().UTC().Add(time.Minute)
	public := service.publicNode(domain.Node{
		Scope: domain.ScopeWeb, EncryptedProxyURL: encryptedProxy, Health: 0.2,
		FailureCount: 3, CooldownUntil: &cooldown, LastError: "legacy failure",
	})
	if !public.AccountBoundProxy {
		t.Fatal("Resin proxy was not reported as account-bound")
	}
	if !public.ProxyPool {
		t.Fatal("account-bound proxy was not reported as a proxy pool")
	}
	if public.Health != 1 || public.FailureCount != 0 || public.CooldownUntil != nil || public.LastError != "" {
		t.Fatalf("proxy pool exposed obsolete node health: %#v", public)
	}
	if service.publicNode(domain.Node{Scope: domain.ScopeWeb}).AccountBoundProxy {
		t.Fatal("direct node was reported as account-bound")
	}
}

func TestApplyInputResetsHealthOnlyWhenEgressConfigurationChanges(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(nil, cipher, "browser-agent")
	cooldown := time.Now().UTC().Add(time.Minute)
	base := domain.Node{
		Name: "node", Scope: domain.ScopeWeb, Enabled: true, Health: 0.2,
		FailureCount: 4, CooldownUntil: &cooldown, LastError: "transport error",
	}

	renamed, err := service.applyInput(base, Input{Name: "renamed", Scope: domain.ScopeWeb, Enabled: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Health != base.Health || renamed.FailureCount != base.FailureCount || renamed.CooldownUntil == nil || renamed.LastError != base.LastError {
		t.Fatalf("name-only edit reset health: %#v", renamed)
	}
	legacyPool := base
	legacyPool.ProxyPool = true
	legacyPool.EncryptedProxyURL, err = cipher.Encrypt("socks5h://proxy.example:1080")
	if err != nil {
		t.Fatal(err)
	}
	preserved, err := service.applyInput(legacyPool, Input{Name: "renamed", Scope: domain.ScopeWeb, Enabled: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !preserved.ProxyPool {
		t.Fatal("an update without proxyPool disabled the existing mode")
	}

	proxyURL := "socks5h://proxy.example:1080"
	proxyPool := true
	changed, err := service.applyInput(base, Input{Name: "node", Scope: domain.ScopeWeb, Enabled: true, ProxyPool: &proxyPool, ProxyURL: &proxyURL}, false)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Health != 1 || changed.FailureCount != 0 || changed.CooldownUntil != nil || changed.LastError != "" || !changed.ProxyPool {
		t.Fatalf("egress configuration did not reset health: %#v", changed)
	}
}

func TestProxyPoolRequiresConfiguredProxy(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(nil, cipher, "browser-agent")
	proxyPool := true
	_, err = service.applyInput(domain.Node{}, Input{
		Name: "pool", Scope: domain.ScopeBuild, Enabled: true, ProxyPool: &proxyPool,
	}, true)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("proxy pool without a proxy error = %v", err)
	}
}
