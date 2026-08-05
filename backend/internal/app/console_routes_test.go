package app

import (
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	consoleprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/console"
)

func TestConsoleRoutesUseStableProviderNamespace(t *testing.T) {
	routes := consoleprovider.Routes()
	if len(routes) == 0 {
		t.Fatal("console catalog is empty")
	}
	seen := make(map[string]map[string]bool, len(routes))
	for _, route := range routes {
		if route.Provider != account.ProviderConsole || !strings.HasPrefix(route.PublicID, "Console/") {
			t.Fatalf("non-canonical console route = %#v", route)
		}
		if seen[route.PublicID] == nil {
			seen[route.PublicID] = make(map[string]bool)
		}
		if seen[route.PublicID][string(route.Capability)] {
			t.Fatalf("duplicate console public id/capability %q/%q", route.PublicID, route.Capability)
		}
		seen[route.PublicID][string(route.Capability)] = true
	}
	if seen["Console/grok-4.3-console"] != nil {
		t.Fatal("legacy conflict suffix leaked into canonical Console model IDs")
	}
	if seen["Console/grok-4.3"] == nil {
		t.Fatal("canonical Console/grok-4.3 route is missing")
	}
	for _, modelID := range []string{"Console/grok-imagine-image-quality", "Console/grok-imagine-image"} {
		if !seen[modelID]["image"] || !seen[modelID]["image_edit"] {
			t.Fatalf("Console image route capabilities for %s = %#v", modelID, seen[modelID])
		}
	}
	if !seen["Console/grok-imagine-video"]["video"] {
		t.Fatal("Console video route is missing")
	}
}
