package console

import (
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// Definition declares the stable Grok Console capability boundary; Console retains stateless Responses semantics.
func (a *Adapter) Definition() provider.Definition {
	return provider.Definition{
		Provider:          account.ProviderConsole,
		ModelNamespace:    account.ProviderConsole.ModelNamespace(),
		ModelCatalog:      provider.ModelCatalogStatic,
		ModelCapabilities: []modeldomain.Capability{modeldomain.CapabilityResponses},
		Quota:             provider.QuotaLocalWindow,
		Credential: provider.CredentialSurface{
			AuthType: account.AuthTypeSSO, Import: true,
		},
		Conversation: provider.ConversationSurface{
			Responses: true, ChatCompletions: true, Messages: true,
		},
		// Console shares the browser/clearance surface with Web. A 403 is
		// therefore normally an egress challenge, not proof that the SSO
		// credential is invalid; the gateway must retry after rebuilding the
		// browser session instead of cooling the account.
		Inference: provider.InferencePolicy{Usage: provider.UsageUpstream, RetryForbiddenAsEgress: true},
	}
}
