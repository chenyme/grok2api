package model

import (
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestSupportedReasoningEffortsPerModel(t *testing.T) {
	tests := []struct {
		model string
		want  []string
	}{
		{model: "grok-4.5", want: []string{"low", "medium", "high"}},
		{model: "Build/grok-4.5", want: []string{"low", "medium", "high"}},
		{model: "grok-4.3", want: []string{"none", "low", "medium", "high"}},
		{model: "grok-4.20-0309-reasoning", want: []string{"low", "medium", "high"}},
		{model: "Console/grok-4.20-0309-reasoning", want: []string{}},
		{model: "grok-4.20-multi-agent-0309", want: []string{"low", "medium", "high", "xhigh"}},
		{model: "grok-4.6", want: []string{"low", "medium", "high", "xhigh"}},
		{model: "Build/grok-4.6", want: []string{"low", "medium", "high", "xhigh"}},
		{model: "Console/grok-4.6", want: []string{"low", "medium", "high", "xhigh"}},
		{model: "grok-build-0.1", want: []string{"none"}},
		{model: GrokComposer25Fast, want: []string{"none"}},
		{model: "unknown-model", want: []string{"none"}},
	}
	for _, test := range tests {
		got := SupportedReasoningEfforts(test.model)
		if len(got) != len(test.want) {
			t.Fatalf("%s levels = %#v, want %#v", test.model, got, test.want)
		}
		for i := range got {
			if got[i] != test.want[i] {
				t.Fatalf("%s levels = %#v, want %#v", test.model, got, test.want)
			}
		}
	}
	if SupportsReasoningEffort("grok-4.5", "none") || SupportsReasoningEffort("grok-4.5", "xhigh") || SupportsReasoningEffort("grok-4.5", "max") {
		t.Fatal("grok-4.5 must not advertise none/xhigh/max")
	}
	if !SupportsReasoningEffort("grok-4.3", "none") || SupportsReasoningEffort("grok-4.3", "xhigh") {
		t.Fatal("grok-4.3 effort support mismatch")
	}
	if !SupportsReasoningEffort("grok-4.6", "xhigh") || !SupportsReasoningEffort("Build/grok-4.6-xhigh", "xhigh") {
		t.Fatal("grok-4.6 must advertise xhigh")
	}
	if !SupportsReasoningEffortForProvider(account.ProviderBuild, "grok-4.6", "xhigh") ||
		!SupportsReasoningEffortForProvider(account.ProviderConsole, "grok-4.6", "xhigh") {
		t.Fatal("grok-4.6 xhigh must remain available through Build and Console")
	}
	if SupportsReasoningEffort("grok-4.6", "none") || SupportsReasoningEffort("grok-4.6", "max") {
		t.Fatal("grok-4.6 must not advertise none/max")
	}
	if SupportsReasoningEffortForProvider(account.ProviderConsole, "grok-4.20-0309-reasoning", "low") ||
		!SupportsReasoningEffortForProvider(account.ProviderBuild, "grok-4.20-0309-reasoning", "low") {
		t.Fatal("grok-4.20 reasoning effort restriction must remain Console-specific")
	}
	if !SupportsReasoningForProvider(account.ProviderConsole, "grok-4.20-0309-reasoning") {
		t.Fatal("fixed Console reasoning model must still advertise reasoning summaries")
	}
}

func TestIsGrokComposerModel(t *testing.T) {
	for _, value := range []string{GrokComposer25Fast, "Build/" + GrokComposer25Fast, "GROK-COMPOSER-FUTURE"} {
		if !IsGrokComposerModel(value) {
			t.Fatalf("Composer model not recognized: %q", value)
		}
	}
	for _, value := range []string{"grok-4.5", "Console/grok-composerish", ""} {
		if IsGrokComposerModel(value) {
			t.Fatalf("non-Composer model recognized: %q", value)
		}
	}
}

func TestParseReasoningModelAlias(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantBase  string
		wantLevel string
		wantOK    bool
	}{
		{name: "grok-4.5 low", input: "grok-4.5-low", wantBase: "grok-4.5", wantLevel: "low", wantOK: true},
		{name: "grok-4.5 high", input: "grok-4.5-high", wantBase: "grok-4.5", wantLevel: "high", wantOK: true},
		{name: "grok-4.5 rejects none", input: "grok-4.5-none", wantOK: false},
		{name: "grok-4.5 rejects xhigh", input: "grok-4.5-xhigh", wantOK: false},
		{name: "grok-4.3 none", input: "grok-4.3-none", wantBase: "grok-4.3", wantLevel: "none", wantOK: true},
		{name: "xhigh before high", input: "grok-4.20-multi-agent-0309-xhigh", wantBase: "grok-4.20-multi-agent-0309", wantLevel: "xhigh", wantOK: true},
		{name: "grok-4.6 xhigh", input: "grok-4.6-xhigh", wantBase: "grok-4.6", wantLevel: "xhigh", wantOK: true},
		{name: "grok-4.6 rejects max", input: "grok-4.6-max", wantOK: false},
		{name: "prefixed base", input: "Build/grok-4.5-medium", wantBase: "Build/grok-4.5", wantLevel: "medium", wantOK: true},
		{name: "base model not alias", input: "grok-4.5", wantOK: false},
		{name: "single-level model no alias", input: "grok-build-0.1-none", wantOK: false},
		{name: "fixed Console reasoning compatibility alias", input: "Console/grok-4.20-0309-reasoning-low", wantBase: "Console/grok-4.20-0309-reasoning", wantLevel: "low", wantOK: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base, level, ok := ParseReasoningModelAlias(test.input)
			if ok != test.wantOK || base != test.wantBase || level != test.wantLevel {
				t.Fatalf("ParseReasoningModelAlias(%q) = %q, %q, %v; want %q, %q, %v",
					test.input, base, level, ok, test.wantBase, test.wantLevel, test.wantOK)
			}
		})
	}
}

func TestReasoningAliasPublicIDs(t *testing.T) {
	if got := ReasoningAliasPublicIDs("grok-4.5"); len(got) != 3 || got[0] != "grok-4.5-low" || got[2] != "grok-4.5-high" {
		t.Fatalf("grok-4.5 aliases = %#v", got)
	}
	if got := ReasoningAliasPublicIDs("grok-4.6"); len(got) != 4 || got[0] != "grok-4.6-low" || got[3] != "grok-4.6-xhigh" {
		t.Fatalf("grok-4.6 aliases = %#v", got)
	}
	if got := ReasoningAliasPublicIDs("Build/grok-4.3"); len(got) != 4 || got[0] != "grok-4.3-none" {
		t.Fatalf("grok-4.3 aliases = %#v", got)
	}
	if got := ReasoningAliasPublicIDs("grok-build-0.1"); len(got) != 0 {
		t.Fatalf("single-level model should not expand aliases: %#v", got)
	}
	if got := ReasoningAliasPublicIDsForProvider(account.ProviderConsole, "grok-4.20-0309-reasoning"); len(got) != 0 {
		t.Fatalf("fixed Console reasoning model must not expand aliases: %#v", got)
	}
	if got := ReasoningAliasPublicIDsForProvider(account.ProviderBuild, "grok-4.20-0309-reasoning"); len(got) != 3 {
		t.Fatalf("Console restriction leaked into Build aliases: %#v", got)
	}
	if got := ReasoningAliasPublicIDsForProvider(account.ProviderConsole, "grok-4.6"); len(got) != 4 || got[3] != "grok-4.6-xhigh" {
		t.Fatalf("Console grok-4.6 aliases = %#v", got)
	}
}
