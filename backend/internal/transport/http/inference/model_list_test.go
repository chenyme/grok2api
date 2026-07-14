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
