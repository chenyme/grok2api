package web

import (
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// Definition 集中声明 Grok Web 的稳定能力边界；Web 私有协议由本包内部维护。
func (a *Adapter) Definition() provider.Definition {
	return provider.Definition{
		Provider:       account.ProviderWeb,
		ModelNamespace: account.ProviderWeb.ModelNamespace(),
		ModelCatalog:   provider.ModelCatalogStatic,
		ModelCapabilities: []modeldomain.Capability{
			modeldomain.CapabilityChat,
			modeldomain.CapabilityImage,
			modeldomain.CapabilityImageEdit,
			modeldomain.CapabilityVideo,
		},
		Quota: provider.QuotaRemoteWindow,
		Credential: provider.CredentialSurface{
			AuthType: account.AuthTypeSSO, Import: true,
		},
		Conversation: provider.ConversationSurface{
			Responses: true, ChatCompletions: true, Messages: true, StoredResponses: true,
		},
		Media: provider.MediaSurface{
			ImageGeneration: true, ImageEdit: true, VideoGeneration: true,
		},
		Inference: provider.InferencePolicy{Usage: provider.UsageEstimated, RetryForbiddenAsEgress: true},
	}
}
