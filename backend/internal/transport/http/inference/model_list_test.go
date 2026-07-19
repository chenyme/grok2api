package inference

import (
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
)

func TestNewModelListItemsDeduplicatesSharedPublicName(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	items := newModelListItems([]modeldomain.Route{
		{PublicID: "Build/grok-shared", Provider: account.ProviderBuild, CreatedAt: now},
		{PublicID: "Console/grok-shared", Provider: account.ProviderConsole, CreatedAt: now.Add(time.Second)},
		{PublicID: "Web/grok-chat-fast", Provider: account.ProviderWeb, CreatedAt: now},
	})
	if len(items) != 2 || items[0].ID != "grok-shared" || items[1].ID != "grok-chat-fast" {
		t.Fatalf("model list = %#v", items)
	}
}

func TestNewModelListItemsPublishesMultiAgentEffortsFromConsoleBaseRoute(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	buildRoute := modeldomain.Route{PublicID: "Build/renamed-multi-agent", Provider: account.ProviderBuild, UpstreamModel: "grok-4.20-multi-agent-0309", CreatedAt: now}
	consoleRoute := modeldomain.Route{PublicID: "Console/renamed-multi-agent", Provider: account.ProviderConsole, UpstreamModel: "grok-4.20-multi-agent-0309", CreatedAt: now}
	if items := newModelListItems([]modeldomain.Route{buildRoute}); len(items) != 1 || items[0].ID != "renamed-multi-agent" {
		t.Fatalf("build-only model list = %#v", items)
	}
	items := newModelListItems([]modeldomain.Route{buildRoute, consoleRoute})
	want := []string{
		"grok-4.20-multi-agent-low", "grok-4.20-multi-agent-medium",
		"grok-4.20-multi-agent-high", "grok-4.20-multi-agent-xhigh",
	}
	if len(items) != len(want)+1 || items[0].ID != "renamed-multi-agent" {
		t.Fatalf("model list = %#v", items)
	}
	for index, publicID := range want {
		if items[index+1].ID != publicID || items[index+1].Created != now.Unix() {
			t.Fatalf("model list = %#v", items)
		}
	}
}
