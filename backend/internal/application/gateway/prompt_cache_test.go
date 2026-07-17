package gateway

import (
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestResolveBuildSessionIdentityIsStableAndTenantIsolated(t *testing.T) {
	base := resolveBuildSessionIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "session-1")
	if len(base.upstreamID) != 36 || len(base.affinityKey) != 64 || base != resolveBuildSessionIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "session-1") {
		t.Fatalf("unstable identity = %#v", base)
	}
	for name, value := range map[string]buildSessionIdentity{
		"client":   resolveBuildSessionIdentity(8, accountdomain.ProviderBuild, "grok-4.5", "", "session-1"),
		"provider": resolveBuildSessionIdentity(7, accountdomain.ProviderConsole, "grok-4.5", "", "session-1"),
		"session":  resolveBuildSessionIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "session-2"),
	} {
		if value == base {
			t.Fatalf("%s was not isolated: %#v", name, value)
		}
	}
}

func TestResolveBuildSessionIdentitySeparatesAffinityFromUpstreamSession(t *testing.T) {
	first := resolveBuildSessionIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "session-1")
	otherModel := resolveBuildSessionIdentity(7, accountdomain.ProviderBuild, "grok-4.3", "", "session-1")
	if first.upstreamID == "" || first.upstreamID != otherModel.upstreamID {
		t.Fatalf("upstream session changed with model: first=%#v other=%#v", first, otherModel)
	}
	if first.affinityKey == otherModel.affinityKey {
		t.Fatalf("model-specific account affinity was not isolated: %#v", first)
	}
}

func TestResolveBuildSessionIdentityPrefersExplicitKey(t *testing.T) {
	first := resolveBuildSessionIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "client-key", "session-1")
	second := resolveBuildSessionIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "client-key", "session-2")
	if first.upstreamID == "" || first != second {
		t.Fatalf("explicit key did not take precedence: first=%#v second=%#v", first, second)
	}
	if value := resolveBuildSessionIdentity(0, accountdomain.ProviderBuild, "grok-4.5", "client-key", ""); value != (buildSessionIdentity{}) {
		t.Fatal("identity without client ownership should be empty")
	}
	if value := resolveBuildSessionIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", ""); value != (buildSessionIdentity{}) {
		t.Fatal("identity without an explicit key or session should be empty")
	}
}
