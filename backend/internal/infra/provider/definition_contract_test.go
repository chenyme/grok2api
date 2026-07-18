package provider_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	cliprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	consoleprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/console"
	webprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/web"
)

func TestReadDiagnosticBodyAppliesHardLimit(t *testing.T) {
	body := strings.Repeat("x", provider.MaxDiagnosticBodyBytes+4096)
	data, truncated, err := provider.ReadDiagnosticBody(strings.NewReader(body))
	if err != nil || !truncated || len(data) != provider.MaxDiagnosticBodyBytes {
		t.Fatalf("diagnostic body length = %d, truncated = %v, err = %v", len(data), truncated, err)
	}
}

func TestProductionProviderDefinitionsMatchImplementedCapabilities(t *testing.T) {
	registry := provider.NewRegistry(
		cliprovider.NewAdapter(cliprovider.Config{}, nil),
		webprovider.NewAdapter(webprovider.Config{}, nil, nil, nil, nil),
		consoleprovider.NewAdapter(consoleprovider.Config{}, nil, nil),
	)
	if err := registry.Validate(); err != nil {
		t.Fatalf("production registry validation failed: %v", err)
	}
	if values := registry.Providers(); !reflect.DeepEqual(values, account.Providers()) {
		t.Fatalf("providers = %#v, want %#v", values, account.Providers())
	}

	tests := []struct {
		provider     account.Provider
		catalog      provider.ModelCatalogKind
		quota        provider.QuotaKind
		capabilities []modeldomain.Capability
		credential   provider.CredentialSurface
		conversation provider.ConversationSurface
		media        provider.MediaSurface
		inference    provider.InferencePolicy
	}{
		{
			provider: account.ProviderBuild, catalog: provider.ModelCatalogRemote, quota: provider.QuotaBilling,
			capabilities: []modeldomain.Capability{modeldomain.CapabilityResponses, modeldomain.CapabilityVideo},
			credential:   provider.CredentialSurface{AuthType: account.AuthTypeOAuth, Import: true, Refresh: true, DeviceOAuth: true},
			conversation: provider.ConversationSurface{Responses: true, ChatCompletions: true, Messages: true, Compact: true, StoredResponses: true},
			media:        provider.MediaSurface{VideoGeneration: true},
			inference:    provider.InferencePolicy{Usage: provider.UsageUpstream},
		},
		{
			provider: account.ProviderWeb, catalog: provider.ModelCatalogStatic, quota: provider.QuotaRemoteWindow,
			capabilities: []modeldomain.Capability{modeldomain.CapabilityChat, modeldomain.CapabilityImage, modeldomain.CapabilityImageEdit, modeldomain.CapabilityVideo},
			credential:   provider.CredentialSurface{AuthType: account.AuthTypeSSO, Import: true},
			conversation: provider.ConversationSurface{Responses: true, ChatCompletions: true, Messages: true, StoredResponses: true},
			media:        provider.MediaSurface{ImageGeneration: true, ImageEdit: true, VideoGeneration: true},
			inference:    provider.InferencePolicy{Usage: provider.UsageEstimated, RetryForbiddenAsEgress: true},
		},
		{
			provider: account.ProviderConsole, catalog: provider.ModelCatalogStatic, quota: provider.QuotaLocalWindow,
			capabilities: []modeldomain.Capability{modeldomain.CapabilityResponses},
			credential:   provider.CredentialSurface{AuthType: account.AuthTypeSSO, Import: true},
			conversation: provider.ConversationSurface{Responses: true, ChatCompletions: true, Messages: true},
			inference:    provider.InferencePolicy{Usage: provider.UsageUpstream, RetryForbiddenAsEgress: true},
		},
	}
	for _, test := range tests {
		t.Run(string(test.provider), func(t *testing.T) {
			definition, ok := registry.Definition(test.provider)
			if !ok {
				t.Fatal("definition not registered")
			}
			if definition.ModelNamespace != test.provider.ModelNamespace() || definition.ModelCatalog != test.catalog || definition.Quota != test.quota {
				t.Fatalf("definition identity = %#v", definition)
			}
			if !reflect.DeepEqual(definition.ModelCapabilities, test.capabilities) || definition.Credential != test.credential || definition.Conversation != test.conversation || definition.Media != test.media || definition.Inference != test.inference {
				t.Fatalf("definition capabilities = %#v", definition)
			}
			for _, operation := range []string{"responses", "chat", "messages"} {
				if !registry.SupportsConversation(test.provider, operation) {
					t.Fatalf("%s does not expose declared %s compatibility", test.provider, operation)
				}
			}
		})
	}
	if !registry.SupportsResponseCompaction(account.ProviderBuild) || registry.SupportsResponseCompaction(account.ProviderWeb) || registry.SupportsResponseCompaction(account.ProviderConsole) {
		t.Fatal("response compaction capability boundary is inconsistent")
	}
	if !registry.SupportsConversation(account.ProviderBuild, "compaction") || registry.SupportsConversation(account.ProviderWeb, "compaction") || registry.SupportsConversation(account.ProviderConsole, "compaction") {
		t.Fatal("compaction operation capability boundary is inconsistent")
	}
	definition, _ := registry.Definition(account.ProviderWeb)
	definition.ModelCapabilities[0] = modeldomain.CapabilityResponses
	stored, _ := registry.Definition(account.ProviderWeb)
	if stored.ModelCapabilities[0] != modeldomain.CapabilityChat {
		t.Fatal("registry definition was mutated through a returned slice")
	}
}

func TestProviderDefinitionRejectsInconsistentMediaCapability(t *testing.T) {
	definition := provider.Definition{
		Provider:          account.ProviderWeb,
		ModelNamespace:    account.ProviderWeb.ModelNamespace(),
		ModelCatalog:      provider.ModelCatalogStatic,
		ModelCapabilities: []modeldomain.Capability{modeldomain.CapabilityImage},
		Quota:             provider.QuotaRemoteWindow,
		Credential:        provider.CredentialSurface{AuthType: account.AuthTypeSSO},
	}
	if err := definition.Validate(); err == nil {
		t.Fatal("inconsistent media declaration was accepted")
	}
}
