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
	for _, modelID := range []string{"Console/grok-imagine-image", "Console/grok-imagine-image-quality", "Console/grok-imagine-image-2.0"} {
		if !seen[modelID]["image"] || !seen[modelID]["image_edit"] {
			t.Fatalf("Console image route capabilities for %s = %#v", modelID, seen[modelID])
		}
	}
	for _, modelID := range []string{"Console/grok-voice-latest", "Console/grok-voice-think-fast-2.0", "Console/grok-voice-think-fast-1.0"} {
		if !seen[modelID]["realtime"] || !seen[modelID]["tts"] {
			t.Fatalf("Console voice route capabilities for %s = %#v", modelID, seen[modelID])
		}
	}
	if !seen["Console/grok-stt"]["stt"] {
		t.Fatal("Console STT route is missing")
	}
	if !seen["Console/grok-imagine-video"]["video"] {
		t.Fatal("Console video route is missing")
	}
	if !seen["Console/grok-imagine-video-1.5"]["video"] {
		t.Fatal("Console video 1.5 route is missing")
	}
}
