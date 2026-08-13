package inference

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/gin-gonic/gin"
)

func TestNewModelListItemsDeduplicatesSharedPublicName(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	items := newModelListItems([]modeldomain.Route{
		{PublicID: "Build/grok-shared", Provider: account.ProviderBuild, CreatedAt: now},
		{PublicID: "Console/grok-shared", Provider: account.ProviderConsole, CreatedAt: now.Add(time.Second)},
		{PublicID: "Web/grok-chat-fast", Provider: account.ProviderWeb, CreatedAt: now},
		{PublicID: "Console/grok-imagine-image", Provider: account.ProviderConsole, Capability: modeldomain.CapabilityImage, CreatedAt: now},
		{PublicID: "Console/grok-imagine-image", Provider: account.ProviderConsole, Capability: modeldomain.CapabilityImageEdit, CreatedAt: now.Add(time.Second)},
	})
	if len(items) != 3 || items[0].ID != "grok-shared" || items[1].ID != "grok-chat-fast" || items[2].ID != "grok-imagine-image" {
		t.Fatalf("model list = %#v", items)
	}
}

func TestFilterModelRoutesForClientKeyUsesProviderAndModelIntersection(t *testing.T) {
	routes := []modeldomain.Route{
		{ID: 1, Provider: account.ProviderBuild, PublicID: "Build/grok-shared"},
		{ID: 2, Provider: account.ProviderWeb, PublicID: "Web/grok-shared"},
		{ID: 3, Provider: account.ProviderConsole, PublicID: "Console/grok-shared"},
	}
	key := clientkeydomain.Key{ProviderScope: clientkeydomain.ProviderScopeWeb | clientkeydomain.ProviderScopeConsole, AllowedModels: []uint64{2, 3}}
	filtered := filterModelRoutesForClientKey(routes, key)
	if len(filtered) != 2 || filtered[0].ID != 2 || filtered[1].ID != 3 {
		t.Fatalf("filtered routes = %#v", filtered)
	}
}

func TestAppendReasoningModelAliasesUsesRealSupportedLevels(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	base := newModelListItems([]modeldomain.Route{
		{PublicID: "Build/grok-4.5", Provider: account.ProviderBuild, Capability: modeldomain.CapabilityResponses, CreatedAt: now},
		{PublicID: "Build/grok-4.6", Provider: account.ProviderBuild, Capability: modeldomain.CapabilityResponses, CreatedAt: now},
		{PublicID: "Console/grok-4.3", Provider: account.ProviderConsole, Capability: modeldomain.CapabilityResponses, CreatedAt: now},
		{PublicID: "Console/grok-4.20-0309-reasoning", Provider: account.ProviderConsole, Capability: modeldomain.CapabilityResponses, CreatedAt: now},
		{PublicID: "Build/grok-build-0.1", Provider: account.ProviderBuild, Capability: modeldomain.CapabilityResponses, CreatedAt: now},
	})
	expanded := appendReasoningModelAliases(base)
	ids := make(map[string]bool, len(expanded))
	for _, item := range expanded {
		ids[item.ID] = true
	}
	for _, want := range []string{"grok-4.5", "grok-4.5-low", "grok-4.5-medium", "grok-4.5-high", "grok-4.6", "grok-4.6-low", "grok-4.6-medium", "grok-4.6-high", "grok-4.6-xhigh", "grok-4.3-none", "grok-4.3-low", "grok-4.3-medium", "grok-4.3-high", "grok-build-0.1"} {
		if !ids[want] {
			t.Fatalf("missing model %q in %#v", want, expanded)
		}
	}
	for _, reject := range []string{
		"grok-4.5-none", "grok-4.5-xhigh", "grok-4.5-max", "grok-4.3-xhigh", "grok-build-0.1-none",
		"grok-4.20-0309-reasoning-low", "grok-4.20-0309-reasoning-medium", "grok-4.20-0309-reasoning-high",
	} {
		if ids[reject] {
			t.Fatalf("unexpected unsupported alias %q", reject)
		}
	}
}

func TestAppendReasoningModelAliasesIncludesConsoleGrok46XHigh(t *testing.T) {
	expanded := appendReasoningModelAliases([]modelListItem{{
		ID: "grok-4.6", Provider: account.ProviderConsole, Capability: modeldomain.CapabilityResponses,
	}})
	ids := make(map[string]bool, len(expanded))
	for _, item := range expanded {
		ids[item.ID] = true
	}
	for _, want := range []string{"grok-4.6-low", "grok-4.6-medium", "grok-4.6-high", "grok-4.6-xhigh"} {
		if !ids[want] {
			t.Fatalf("missing Console alias %q in %#v", want, expanded)
		}
	}
	if ids["grok-4.6-max"] {
		t.Fatalf("client compatibility value max must not be advertised as a model alias: %#v", expanded)
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

func TestCodexCatalogUsesGrok46MetadataForBaseAndXHighAlias(t *testing.T) {
	items := []modelListItem{
		{ID: "grok-4.6", Provider: account.ProviderBuild, Capability: modeldomain.CapabilityResponses},
		{ID: "grok-4.6-xhigh", Provider: account.ProviderBuild, Capability: modeldomain.CapabilityResponses},
	}
	models := newCodexModelCatalog(items).Models
	if len(models) != 2 {
		t.Fatalf("model count = %d, want 2", len(models))
	}

	base := models[0]
	if base.ContextWindow != 500000 || base.MaxContextWindow != 500000 {
		t.Fatalf("grok-4.6 context window = %d/%d, want 500000/500000", base.ContextWindow, base.MaxContextWindow)
	}
	if base.Description != "xAI Grok 4.6 frontier model with reasoning and vision." {
		t.Fatalf("grok-4.6 description = %q", base.Description)
	}
	if len(base.InputModalities) != 2 || base.InputModalities[0] != "text" || base.InputModalities[1] != "image" {
		t.Fatalf("grok-4.6 input modalities = %#v, want text/image", base.InputModalities)
	}
	if base.DefaultReasoningLevel != "medium" || len(base.SupportedReasoningLevels) != 4 || base.SupportedReasoningLevels[3].Effort != "xhigh" {
		t.Fatalf("grok-4.6 reasoning metadata = default %q, levels %#v", base.DefaultReasoningLevel, base.SupportedReasoningLevels)
	}
	if !base.SupportsReasoningSummaryParameter || !base.SupportsReasoningSummaries {
		t.Fatalf("grok-4.6 reasoning support missing: %#v", base)
	}

	alias := models[1]
	if alias.ContextWindow != base.ContextWindow || alias.MaxContextWindow != base.MaxContextWindow || alias.Description != base.Description {
		t.Fatalf("grok-4.6-xhigh did not inherit base metadata: %#v", alias)
	}
	if len(alias.InputModalities) != 2 || alias.DefaultReasoningLevel != "xhigh" || len(alias.SupportedReasoningLevels) != 1 || alias.SupportedReasoningLevels[0].Effort != "xhigh" {
		t.Fatalf("grok-4.6-xhigh metadata = %#v", alias)
	}
}

func TestCodexCatalogMarksConsoleGrok420AsFixedReasoning(t *testing.T) {
	entry := newCodexModelCatalog([]modelListItem{{
		ID: "grok-4.20-0309-reasoning", Provider: account.ProviderConsole, Capability: modeldomain.CapabilityResponses,
	}}).Models[0]
	if entry.DefaultReasoningLevel != "none" || len(entry.SupportedReasoningLevels) != 0 {
		t.Fatalf("fixed reasoning efforts = default %q, levels %#v", entry.DefaultReasoningLevel, entry.SupportedReasoningLevels)
	}
	if !entry.SupportsReasoningSummaryParameter || !entry.SupportsReasoningSummaries {
		t.Fatalf("fixed reasoning model lost summary support: %#v", entry)
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
		{PublicID: "Web/grok-imagine-image-lite", Provider: account.ProviderWeb, Capability: modeldomain.CapabilityImage, CreatedAt: now},
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
		case "grok-imagine-image-lite", "grok-imagine-video":
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
