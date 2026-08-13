package console

import (
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// Definition declares the stable Grok Console capability boundary. Conversation
// remains stateless while media uses the standard asynchronous xAI resources.
func (a *Adapter) Definition() provider.Definition {
	return provider.Definition{
		Provider:       account.ProviderConsole,
		ModelNamespace: account.ProviderConsole.ModelNamespace(),
		ModelCatalog:   provider.ModelCatalogStatic,
		ModelCapabilities: []modeldomain.Capability{
			modeldomain.CapabilityResponses,
			modeldomain.CapabilityImage,
			modeldomain.CapabilityImageEdit,
			modeldomain.CapabilityVideo,
			modeldomain.CapabilityTTS,
			modeldomain.CapabilitySTT,
			modeldomain.CapabilityRealtime,
		},
		Quota: provider.QuotaRemoteWindow,
		Credential: provider.CredentialSurface{
			AuthType: account.AuthTypeSSO, Import: true,
		},
		Conversation: provider.ConversationSurface{
			Responses: true, ChatCompletions: true, Messages: true,
		},
		Media: provider.MediaSurface{
			ImageGeneration: true, ImageEdit: true, VideoGeneration: true,
			TTS: true, STT: true, Realtime: true,
		},
		// Console shares the browser/clearance surface with Web. A 403 is
		// therefore normally an egress challenge, not proof that the SSO
		// credential is invalid; the gateway must retry after rebuilding the
		// browser session instead of cooling the account.
		Inference: provider.InferencePolicy{Usage: provider.UsageUpstream, RetryForbiddenAsEgress: true},
	}
}
