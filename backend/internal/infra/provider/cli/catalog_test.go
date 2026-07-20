package cli

import (
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestBuildModelAliasMapsCursorFriendlyNameToGrok45(t *testing.T) {
	registry := provider.NewRegistry(NewAdapter(Config{}, nil))
	alias, ok := registry.ResolveModelAlias("grok-4-5")
	if !ok {
		t.Fatal("grok-4-5 alias missing")
	}
	if alias.Provider != account.ProviderBuild {
		t.Fatalf("provider = %s, want Build", alias.Provider)
	}
	if alias.UpstreamModel != "grok-4.5" {
		t.Fatalf("upstream = %q, want grok-4.5", alias.UpstreamModel)
	}
	if !strings.HasPrefix(alias.PublicModel, "Build/") {
		t.Fatalf("public model = %q, want Build/ prefix", alias.PublicModel)
	}
}
