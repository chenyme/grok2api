package web

import (
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
)

type ModelSpec struct {
	PublicID      string
	UpstreamModel string
	ProtocolModel string
	Capability    modeldomain.Capability
	Mode          string
	MinimumTier   account.WebTier
}

var catalog = []ModelSpec{
	{PublicID: "grok-chat-fast", UpstreamModel: "grok-chat-fast", Capability: modeldomain.CapabilityChat, Mode: "fast", MinimumTier: account.WebTierBasic},
	{PublicID: "grok-chat-auto", UpstreamModel: "grok-chat-auto", Capability: modeldomain.CapabilityChat, Mode: "auto", MinimumTier: account.WebTierSuper},
	{PublicID: "grok-chat-expert", UpstreamModel: "grok-chat-expert", Capability: modeldomain.CapabilityChat, Mode: "expert", MinimumTier: account.WebTierSuper},
	{PublicID: "grok-chat-heavy", UpstreamModel: "grok-chat-heavy", Capability: modeldomain.CapabilityChat, Mode: "heavy", MinimumTier: account.WebTierHeavy},
	{PublicID: "grok-imagine-image", UpstreamModel: "grok-imagine-image", ProtocolModel: "imagine-lite", Capability: modeldomain.CapabilityImage, Mode: "fast", MinimumTier: account.WebTierBasic},
	{PublicID: "grok-imagine-image-quality", UpstreamModel: "grok-imagine-image-quality", ProtocolModel: "imagine", Capability: modeldomain.CapabilityImage, MinimumTier: account.WebTierSuper},
	{PublicID: "grok-imagine-image-edit", UpstreamModel: "imagine-image-edit", Capability: modeldomain.CapabilityImageEdit, MinimumTier: account.WebTierSuper},
	{PublicID: "grok-imagine-video", UpstreamModel: "grok-imagine-video", ProtocolModel: "imagine-video-gen", Capability: modeldomain.CapabilityVideo, MinimumTier: account.WebTierSuper},
}

func Catalog() []ModelSpec { return append([]ModelSpec(nil), catalog...) }

func Routes() []modeldomain.Route {
	values := make([]modeldomain.Route, 0, len(catalog))
	for _, spec := range catalog {
		publicID, _ := modeldomain.NormalizePublicID(account.ProviderWeb, spec.PublicID)
		values = append(values, modeldomain.Route{PublicID: publicID, Provider: account.ProviderWeb, UpstreamModel: spec.UpstreamModel, Capability: spec.Capability, Enabled: true})
	}
	return values
}

func Resolve(upstreamModel string) (ModelSpec, bool) {
	for _, spec := range catalog {
		if spec.UpstreamModel == upstreamModel {
			return spec, true
		}
	}
	return ModelSpec{}, false
}

func TierSupports(actual, minimum account.WebTier) bool {
	rank := map[account.WebTier]int{account.WebTierBasic: 1, account.WebTierSuper: 2, account.WebTierHeavy: 3}
	return rank[actual] >= rank[minimum]
}
