package inference

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/gin-gonic/gin"
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

func TestNewCodexModelCatalogIncludesRequiredProtocolFields(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	items := newModelListItems([]modeldomain.Route{
		{PublicID: "Build/grok-4.5", Provider: account.ProviderBuild, Capability: modeldomain.CapabilityResponses, CreatedAt: now},
	})
	catalog := newCodexModelCatalog(items)
	if len(catalog.Models) != 1 || catalog.Models[0].Slug != "grok-4.5" {
		t.Fatalf("models = %#v", catalog.Models)
	}
	entry := catalog.Models[0]
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
	if entry.DefaultReasoningLevel != "medium" {
		t.Fatalf("default reasoning = %q, want medium", entry.DefaultReasoningLevel)
	}
	if entry.BaseInstructions == "" || entry.ExperimentalSupportedTools == nil {
		t.Fatalf("required fields missing: %#v", entry)
	}
	if entry.ApplyPatchToolType == nil || !entry.SupportsParallelToolCalls {
		t.Fatalf("Build tool metadata = %#v", entry)
	}
	body, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]json.RawMessage
	if err = json.Unmarshal(body, &envelope); err != nil || len(envelope) != 1 || envelope["models"] == nil {
		t.Fatalf("catalog envelope = %s, err = %v", body, err)
	}
}

func TestNewCodexModelCacheRespectsNonReasoningModel(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	items := newModelListItems([]modeldomain.Route{
		{PublicID: "Build/grok-build-0.1", Provider: account.ProviderBuild, Capability: modeldomain.CapabilityResponses, CreatedAt: now},
	})
	entry := newCodexModelCatalog(items).Models[0]
	if entry.DefaultReasoningLevel != "none" {
		t.Fatalf("default reasoning = %q", entry.DefaultReasoningLevel)
	}
	if len(entry.SupportedReasoningLevels) != 1 || entry.SupportedReasoningLevels[0].Effort != "none" {
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
	catalog := newCodexModelCatalog(newModelListItems(routes))
	if len(catalog.Models) != 3 {
		t.Fatalf("model count = %d, want 3; slugs = %#v", len(catalog.Models), codexSlugs(catalog.Models))
	}
	for _, entry := range catalog.Models {
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

func TestCodexCatalogUsesConservativeUnknownModelDefaults(t *testing.T) {
	entry := newCodexModelCatalog([]modelListItem{{
		ID: "custom-model", Provider: account.ProviderWeb, Capability: modeldomain.CapabilityChat,
	}}).Models[0]
	if entry.ContextWindow != 128000 || entry.DefaultReasoningLevel != "none" {
		t.Fatalf("unknown model metadata = %#v", entry)
	}
	if len(entry.InputModalities) != 1 || entry.InputModalities[0] != "text" || entry.ApplyPatchToolType != nil || entry.SupportsParallelToolCalls || entry.SupportsSearchTool || entry.SupportVerbosity {
		t.Fatalf("unknown model over-advertises capabilities: %#v", entry)
	}
}

func TestWriteCodexModelCatalogSetsETagAndHandlesNotModified(t *testing.T) {
	gin.SetMode(gin.TestMode)
	catalog := newCodexModelCatalog([]modelListItem{{
		ID: "grok-4.5", Provider: account.ProviderBuild, Capability: modeldomain.CapabilityResponses,
	}})
	router := gin.New()
	router.GET("/v1/models", func(c *gin.Context) { writeCodexModelCatalog(c, catalog) })

	first := httptest.NewRecorder()
	firstRequest := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.145.0", nil)
	router.ServeHTTP(first, firstRequest)
	etag := first.Header().Get("ETag")
	if first.Code != http.StatusOK || etag == "" {
		t.Fatalf("first response = %d, etag = %q", first.Code, etag)
	}

	second := httptest.NewRecorder()
	secondRequest := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.145.0", nil)
	secondRequest.Header.Set("If-None-Match", etag)
	router.ServeHTTP(second, secondRequest)
	if second.Code != http.StatusNotModified || second.Body.Len() != 0 || second.Header().Get("ETag") != etag {
		t.Fatalf("conditional response = %d, body = %q, etag = %q", second.Code, second.Body.String(), second.Header().Get("ETag"))
	}
}

func codexSlugs(models []codexModelEntry) []string {
	slugs := make([]string, 0, len(models))
	for _, m := range models {
		slugs = append(slugs, m.Slug)
	}
	return slugs
}
