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
	seen := make(map[string]bool, len(routes))
	for _, route := range routes {
		if route.Provider != account.ProviderConsole || !strings.HasPrefix(route.PublicID, "Console/") {
			t.Fatalf("non-canonical console route = %#v", route)
		}
		if seen[route.PublicID] {
			t.Fatalf("duplicate console public id %q", route.PublicID)
		}
		seen[route.PublicID] = true
	}
	if seen["Console/grok-4.3-console"] {
		t.Fatal("legacy conflict suffix leaked into canonical Console model IDs")
	}
	if !seen["Console/grok-4.3"] {
		t.Fatal("canonical Console/grok-4.3 route is missing")
	}
}
