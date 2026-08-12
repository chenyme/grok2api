package console

import (
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

const (
	QuotaMode      = "console"
	QuotaModeImage = "console_image"
	QuotaModeVideo = "console_video"
)

type ModelSpec struct {
	PublicID                string
	UpstreamModel           string
	SupportsReasoning       bool
	SupportsReasoningEffort bool
	DefaultReasoningEffort  string
	MaxOutputTokens         int
}

var catalog = []ModelSpec{
	{PublicID: "grok-4.3", UpstreamModel: "grok-4.3", SupportsReasoning: true, SupportsReasoningEffort: true, DefaultReasoningEffort: "medium", MaxOutputTokens: 1_000_000},
	{PublicID: "grok-4.20-0309-reasoning", UpstreamModel: "grok-4.20-0309-reasoning", SupportsReasoning: true, MaxOutputTokens: 1_000_000},
	{PublicID: "grok-4.20-0309-non-reasoning", UpstreamModel: "grok-4.20-0309-non-reasoning", MaxOutputTokens: 1_000_000},
	{PublicID: "grok-4.20-multi-agent-0309", UpstreamModel: "grok-4.20-multi-agent-0309", SupportsReasoning: true, SupportsReasoningEffort: true, MaxOutputTokens: 1_000_000},
	{PublicID: "grok-4.5", UpstreamModel: "grok-4.5", SupportsReasoning: true, SupportsReasoningEffort: true, DefaultReasoningEffort: "medium", MaxOutputTokens: 1_000_000},
	{PublicID: "grok-build-0.1", UpstreamModel: "grok-build-0.1", MaxOutputTokens: 256_000},
}

var mediaCatalog = []struct {
	PublicID      string
	UpstreamModel string
	Capabilities  []modeldomain.Capability
}{
	{PublicID: "grok-imagine-image-quality", UpstreamModel: "grok-imagine-image-quality", Capabilities: []modeldomain.Capability{modeldomain.CapabilityImage, modeldomain.CapabilityImageEdit}},
	{PublicID: "grok-imagine-image", UpstreamModel: "grok-imagine-image", Capabilities: []modeldomain.Capability{modeldomain.CapabilityImage, modeldomain.CapabilityImageEdit}},
	{PublicID: "grok-imagine-video", UpstreamModel: "grok-imagine-video", Capabilities: []modeldomain.Capability{modeldomain.CapabilityVideo}},
	{PublicID: "grok-imagine-video-1.5", UpstreamModel: "grok-imagine-video-1.5", Capabilities: []modeldomain.Capability{modeldomain.CapabilityVideo}},
}

// Effort-suffixed aliases only include levels each Provider/model combination
// actually supports (see domain/model.SupportedReasoningEffortsForProvider).
// No blanket none/low/medium/high/xhigh/max template.
var aliases = []provider.ModelAlias{
	consoleAlias("grok-4.3-console", "grok-4.3", "grok-4.3", ""),
	consoleAlias("grok-4.20-0309-reasoning-console", "grok-4.20-0309-reasoning", "grok-4.20-0309-reasoning", ""),
	consoleAlias("grok-4.20-0309-non-reasoning-console", "grok-4.20-0309-non-reasoning", "grok-4.20-0309-non-reasoning", ""),
	consoleAlias("grok-4.20-multi-agent-console", "grok-4.20-multi-agent-0309", "grok-4.20-multi-agent-0309", ""),
	consoleAlias("grok-4.5-console", "grok-4.5", "grok-4.5", ""),
	consoleAlias("grok-build-console", "grok-build-0.1", "grok-build-0.1", ""),
	consoleAlias("grok-4.3-low", "grok-4.3", "grok-4.3", "low"),
	consoleAlias("grok-4.3-medium", "grok-4.3", "grok-4.3", "medium"),
	consoleAlias("grok-4.3-high", "grok-4.3", "grok-4.3", "high"),
	consoleAlias("grok-4.20-multi-agent-low", "grok-4.20-multi-agent-0309", "grok-4.20-multi-agent-0309", "low"),
	consoleAlias("grok-4.20-multi-agent-medium", "grok-4.20-multi-agent-0309", "grok-4.20-multi-agent-0309", "medium"),
	consoleAlias("grok-4.20-multi-agent-high", "grok-4.20-multi-agent-0309", "grok-4.20-multi-agent-0309", "high"),
	consoleAlias("grok-4.20-multi-agent-xhigh", "grok-4.20-multi-agent-0309", "grok-4.20-multi-agent-0309", "xhigh"),
}

func consoleAlias(alias, publicModel, upstreamModel, effort string) provider.ModelAlias {
	canonical, _ := modeldomain.NormalizePublicID(account.ProviderConsole, publicModel)
	return provider.ModelAlias{
		Alias: alias, PublicModel: canonical, Provider: account.ProviderConsole,
		UpstreamModel: upstreamModel, ReasoningEffort: effort,
	}
}

func Catalog() []ModelSpec { return append([]ModelSpec(nil), catalog...) }

func Routes() []modeldomain.Route {
	capacity := len(catalog)
	for _, spec := range mediaCatalog {
		capacity += len(spec.Capabilities)
	}
	values := make([]modeldomain.Route, 0, capacity)
	for _, spec := range catalog {
		publicID, _ := modeldomain.NormalizePublicID(account.ProviderConsole, spec.PublicID)
		values = append(values, modeldomain.Route{
			PublicID: publicID, Provider: account.ProviderConsole, UpstreamModel: spec.UpstreamModel,
			Capability: modeldomain.CapabilityResponses, Enabled: true,
		})
	}
	for _, spec := range mediaCatalog {
		publicID, _ := modeldomain.NormalizePublicID(account.ProviderConsole, spec.PublicID)
		for _, capability := range spec.Capabilities {
			values = append(values, modeldomain.Route{
				PublicID: publicID, Provider: account.ProviderConsole, UpstreamModel: spec.UpstreamModel,
				Capability: capability, Enabled: true,
			})
		}
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

func ResolveMedia(upstreamModel string, capability modeldomain.Capability) bool {
	for _, spec := range mediaCatalog {
		if spec.UpstreamModel != upstreamModel {
			continue
		}
		for _, supported := range spec.Capabilities {
			if supported == capability {
				return true
			}
		}
	}
	return false
}

func allModels() []string {
	values := make([]string, 0, len(catalog)+len(mediaCatalog))
	for _, spec := range catalog {
		values = append(values, spec.UpstreamModel)
	}
	for _, spec := range mediaCatalog {
		values = append(values, spec.UpstreamModel)
	}
	return values
}

func Aliases() []provider.ModelAlias { return append([]provider.ModelAlias(nil), aliases...) }
