package cli

import (
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// Build 侧兼容别名：将 Cursor 可用名解析到真实上游模型。
var aliases = []provider.ModelAlias{
	buildAlias("grok-4-5", "grok-4.5", "grok-4.5", ""),
}

func buildAlias(alias, publicModel, upstreamModel, effort string) provider.ModelAlias {
	canonical, _ := modeldomain.NormalizePublicID(account.ProviderBuild, publicModel)
	return provider.ModelAlias{
		Alias: alias, PublicModel: canonical, Provider: account.ProviderBuild,
		UpstreamModel: upstreamModel, ReasoningEffort: effort,
	}
}

func Aliases() []provider.ModelAlias {
	return append([]provider.ModelAlias(nil), aliases...)
}
