package clientkey

import (
	"reflect"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestAccountScopeParsingAndNormalization(t *testing.T) {
	if _, valid := ParseProviderScopeValues(nil); valid {
		t.Fatal("an explicitly empty provider scope must be rejected")
	}
	if _, valid := ParseTierScopeValues(nil); valid {
		t.Fatal("an explicitly empty tier scope must be rejected")
	}
	providers, valid := ParseProviderScopeValues([]string{"grok_build", "grok_web"})
	if !valid || providers != ProviderScopeBuild|ProviderScopeWeb {
		t.Fatalf("provider scope = %d, valid = %v", providers, valid)
	}
	if _, valid := ParseProviderScopeValues([]string{"all", "grok_web"}); valid {
		t.Fatal("all mixed with a provider must be rejected")
	}
	tiers, valid := ParseTierScopeValues([]string{"free", "super"})
	if !valid || tiers != TierScopeFree|TierScopeSuper {
		t.Fatalf("tier scope = %d, valid = %v", tiers, valid)
	}
	if tiers == TierScopeAll {
		t.Fatal("free + super must continue excluding unknown tiers")
	}
	if _, valid := ParseTierScopeValues([]string{"all", "free"}); valid {
		t.Fatal("all mixed with a tier must be rejected")
	}
	if values := ProviderScopeAll.Values(); !reflect.DeepEqual(values, []string{"all"}) {
		t.Fatalf("all provider values = %#v", values)
	}
	if values := tiers.Values(); !reflect.DeepEqual(values, []string{"free", "super"}) {
		t.Fatalf("tier values = %#v", values)
	}
}

func TestAccountScopeAllowsProviderAndTierIntersection(t *testing.T) {
	scope := AccountScope{Providers: ProviderScopeBuild | ProviderScopeConsole, Tiers: TierScopeFree}
	if !scope.AllowsAccount(account.ProviderBuild, AccountTierFree) {
		t.Fatal("Build Free should be allowed")
	}
	if scope.AllowsAccount(account.ProviderBuild, AccountTierSuper) {
		t.Fatal("Build Super should be excluded")
	}
	if scope.AllowsAccount(account.ProviderWeb, AccountTierFree) {
		t.Fatal("Web should be excluded by provider scope")
	}
	if !scope.AllowsAccount(account.ProviderConsole, AccountTierUnknown) {
		t.Fatal("Console should ignore tier restrictions")
	}
	if (AccountScope{Providers: ProviderScopeBuild, Tiers: TierScopeFree | TierScopeSuper}).AllowsAccount(account.ProviderBuild, AccountTierUnknown) {
		t.Fatal("Free + Super should exclude unknown Build accounts")
	}
}
