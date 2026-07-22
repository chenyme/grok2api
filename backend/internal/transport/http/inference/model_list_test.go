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

func TestNewCodexModelCacheShapesForClientVersion(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	items := newModelListItems([]modeldomain.Route{
		{PublicID: "Build/grok-4.5", Provider: account.ProviderBuild, CreatedAt: now},
	})
	cache := newCodexModelCache("0.114.5", items)
	if cache.ClientVersion != "0.114.5" {
		t.Fatalf("client_version = %q", cache.ClientVersion)
	}
	if len(cache.Models) != 1 || cache.Models[0].Slug != "grok-4.5" {
		t.Fatalf("models = %#v", cache.Models)
	}
	entry := cache.Models[0]
	if entry.DisplayName != "Grok 4.5" {
		t.Fatalf("display_name = %q", entry.DisplayName)
	}
	if len(entry.SupportedReasoningLevels) != 3 || entry.SupportedReasoningLevels[0].Effort != "low" {
		t.Fatalf("reasoning levels = %#v", entry.SupportedReasoningLevels)
	}
	if entry.ContextWindow != 500000 || entry.MaxContextWindow != 500000 {
		t.Fatalf("context window = %d/%d", entry.ContextWindow, entry.MaxContextWindow)
	}
	if entry.EffectiveContextWindowPercent != 95 {
		t.Fatalf("effective context window percent = %d, want 95", entry.EffectiveContextWindowPercent)
	}
	if entry.DefaultReasoningLevel != "high" {
		t.Fatalf("default reasoning = %q, want high", entry.DefaultReasoningLevel)
	}
	if entry.Description == "" {
		t.Fatalf("description is empty")
	}
	if entry.AutoCompactTokenLimit != nil {
		t.Fatalf("auto_compact_token_limit = %v, want nil", entry.AutoCompactTokenLimit)
	}
}

func TestNewCodexModelCacheRespectsNonReasoningModel(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	items := newModelListItems([]modeldomain.Route{
		{PublicID: "Build/grok-build-0.1", Provider: account.ProviderBuild, CreatedAt: now},
	})
	cache := newCodexModelCache("0.114.5", items)
	entry := cache.Models[0]
	if entry.DefaultReasoningLevel != "none" {
		t.Fatalf("default reasoning = %q", entry.DefaultReasoningLevel)
	}
	if len(entry.SupportedReasoningLevels) != 0 {
		t.Fatalf("reasoning levels = %#v", entry.SupportedReasoningLevels)
	}
	if entry.ContextWindow != 256000 {
		t.Fatalf("context window = %d", entry.ContextWindow)
	}
}

func TestCodexCatalogHidesMediaModels(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	routes := []modeldomain.Route{
		{PublicID: "Build/grok-4.5", Provider: account.ProviderBuild, Capability: modeldomain.CapabilityResponses, CreatedAt: now},
		{PublicID: "Web/grok-imagine-image", Provider: account.ProviderWeb, Capability: modeldomain.CapabilityImage, CreatedAt: now},
		{PublicID: "Web/grok-imagine-video", Provider: account.ProviderWeb, Capability: modeldomain.CapabilityVideo, CreatedAt: now},
	}
	cache := newCodexModelCache("0.114.5", newModelListItems(routes))
	if len(cache.Models) != 3 {
		t.Fatalf("model count = %d, want 3; slugs = %#v", len(cache.Models), codexSlugs(cache.Models))
	}
	for _, entry := range cache.Models {
		switch entry.Slug {
		case "grok-4.5":
			if entry.Visibility != "list" {
				t.Fatalf("visibility for %s = %q, want list", entry.Slug, entry.Visibility)
			}
		case "grok-imagine-image", "grok-imagine-video":
			if entry.Visibility != "hide" {
				t.Fatalf("visibility for %s = %q, want hide", entry.Slug, entry.Visibility)
			}
		}
	}
}

func codexSlugs(models []codexModelEntry) []string {
	slugs := make([]string, 0, len(models))
	for _, m := range models {
		slugs = append(slugs, m.Slug)
	}
	return slugs
}
