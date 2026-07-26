package model

import (
	"strings"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// ReasoningEffort is a client-facing reasoning depth level accepted by Grok models.
// Only levels a model actually supports should be advertised or accepted as aliases.
type ReasoningEffort = string

const (
	ReasoningEffortNone   ReasoningEffort = "none"
	ReasoningEffortLow    ReasoningEffort = "low"
	ReasoningEffortMedium ReasoningEffort = "medium"
	ReasoningEffortHigh   ReasoningEffort = "high"
	ReasoningEffortXHigh  ReasoningEffort = "xhigh"
	ReasoningEffortMax    ReasoningEffort = "max"
)

// reasoningEffortSuffixes is ordered longest-first so "xhigh" wins over "high".
var reasoningEffortSuffixes = []string{
	ReasoningEffortXHigh,
	ReasoningEffortMedium,
	ReasoningEffortHigh,
	ReasoningEffortLow,
	ReasoningEffortNone,
	ReasoningEffortMax,
}

// grokReasoningCapabilities maps external public model IDs to the reasoning levels
// each model actually accepts. Values follow xAI docs and observed Build/Console behavior:
//   - grok-4.5: low/medium/high (reasoning cannot be disabled; no xhigh/max)
//   - grok-4.3: none/low/medium/high
//   - grok-4.20-multi-agent: low/medium/high/xhigh (effort controls agent count)
// Unknown models default to none-only and never expand into effort aliases.
var grokReasoningCapabilities = map[string][]string{
	"grok-4.5":                     {ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh},
	"grok-4.3":                     {ReasoningEffortNone, ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh},
	"grok-build-0.1":               {ReasoningEffortNone},
	"grok-4.20-0309-reasoning":     {ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh},
	"grok-4.20-0309-non-reasoning": {ReasoningEffortNone},
	"grok-4.20-multi-agent-0309":   {ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh, ReasoningEffortXHigh},
	"grok-3-mini":                  {ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh},
	"grok-3-mini-fast":             {ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh},
	"grok-composer-2.5-fast":       {ReasoningEffortNone},
}

// SupportedReasoningEfforts returns the reasoning levels a public model ID actually supports.
// Provider prefixes are stripped; unknown models only advertise "none".
// Effort-suffixed aliases (e.g. grok-4.5-low) inherit the base model's levels.
func SupportedReasoningEfforts(publicModel string) []string {
	slug := externalModelSlug(publicModel)
	if base, _, ok := parseReasoningModelAliasSlug(slug); ok {
		slug = base
	}
	return reasoningEffortsForSlug(slug)
}

// SupportsReasoningEffort reports whether publicModel accepts the given effort level.
func SupportsReasoningEffort(publicModel, effort string) bool {
	effort = strings.ToLower(strings.TrimSpace(effort))
	for _, level := range SupportedReasoningEfforts(publicModel) {
		if level == effort {
			return true
		}
	}
	return false
}

// DefaultReasoningEffort picks a stable default from supported levels (prefer medium).
// For effort-suffixed aliases the pinned effort is the default.
func DefaultReasoningEffort(publicModel string) string {
	if _, effort, ok := ParseReasoningModelAlias(publicModel); ok {
		return effort
	}
	levels := SupportedReasoningEfforts(publicModel)
	for _, level := range levels {
		if level == ReasoningEffortMedium {
			return level
		}
	}
	if len(levels) > 0 {
		return levels[0]
	}
	return ReasoningEffortNone
}

func reasoningEffortsForSlug(slug string) []string {
	if levels, ok := grokReasoningCapabilities[slug]; ok {
		return append([]string(nil), levels...)
	}
	return []string{ReasoningEffortNone}
}

// ReasoningAliasPublicIDs returns effort-suffixed aliases for a base public model ID.
// Models with fewer than two controllable levels produce no aliases (base name is enough).
// Only levels the model truly supports are included — never a blanket none/low/medium/high/xhigh/max template.
func ReasoningAliasPublicIDs(publicModel string) []string {
	base := strings.TrimSpace(publicModel)
	if base == "" {
		return nil
	}
	levels := SupportedReasoningEfforts(base)
	if len(levels) < 2 {
		return nil
	}
	// Prefer the external (unprefixed) form so clients see "grok-4.5-low", not "Build/grok-4.5-low".
	external := externalModelSlug(base)
	if external == "" {
		return nil
	}
	aliases := make([]string, 0, len(levels))
	for _, level := range levels {
		aliases = append(aliases, external+"-"+level)
	}
	return aliases
}

// ParseReasoningModelAlias splits names like "grok-4.5-low" into base model + effort
// only when the base model actually supports that effort.
// Base retains any provider prefix present on the input (e.g. Build/grok-4.5).
func ParseReasoningModelAlias(publicModel string) (baseModel, effort string, ok bool) {
	name := strings.TrimSpace(publicModel)
	if name == "" {
		return "", "", false
	}
	providerPrefix := ""
	local := name
	for _, providerValue := range account.Providers() {
		prefix := providerValue.ModelNamespace() + "/"
		if len(name) >= len(prefix) && strings.EqualFold(name[:len(prefix)], prefix) {
			providerPrefix = name[:len(prefix)]
			local = strings.TrimSpace(name[len(prefix):])
			break
		}
	}
	baseLocal, effort, ok := parseReasoningModelAliasSlug(local)
	if !ok {
		return "", "", false
	}
	return providerPrefix + baseLocal, effort, true
}

// parseReasoningModelAliasSlug parses an unprefixed model slug only.
// It looks up capabilities directly to avoid recursion with SupportedReasoningEfforts.
func parseReasoningModelAliasSlug(slug string) (base, effort string, ok bool) {
	name := strings.TrimSpace(slug)
	if name == "" {
		return "", "", false
	}
	for _, level := range reasoningEffortSuffixes {
		suffix := "-" + level
		if len(name) <= len(suffix) {
			continue
		}
		if !strings.EqualFold(name[len(name)-len(suffix):], suffix) {
			continue
		}
		base = strings.TrimSpace(name[:len(name)-len(suffix)])
		if base == "" {
			continue
		}
		// Aliases only exist when the base model has multiple controllable levels;
		// single-level models (e.g. grok-build-0.1 → none only) keep the base name.
		levels := reasoningEffortsForSlug(base)
		if len(levels) < 2 || !levelsContain(levels, level) {
			continue
		}
		return base, level, true
	}
	return "", "", false
}

func levelsContain(levels []string, effort string) bool {
	effort = strings.ToLower(strings.TrimSpace(effort))
	for _, level := range levels {
		if level == effort {
			return true
		}
	}
	return false
}

// IsReasoningModelAlias reports whether value is a valid effort-suffixed model alias.
func IsReasoningModelAlias(publicModel string) bool {
	_, _, ok := ParseReasoningModelAlias(publicModel)
	return ok
}

func externalModelSlug(publicModel string) string {
	value := strings.TrimSpace(publicModel)
	if value == "" {
		return ""
	}
	for _, providerValue := range account.Providers() {
		prefix := providerValue.ModelNamespace() + "/"
		if len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix) {
			return strings.TrimSpace(value[len(prefix):])
		}
	}
	return value
}
