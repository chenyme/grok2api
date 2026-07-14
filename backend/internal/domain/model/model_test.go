package model

import (
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestNormalizePublicIDUsesStableProviderNamespace(t *testing.T) {
	tests := []struct {
		provider account.Provider
		input    string
		want     string
	}{
		{provider: account.ProviderBuild, input: "grok-4.3", want: "Build/grok-4.3"},
		{provider: account.ProviderWeb, input: "web/custom", want: "Web/custom"},
		{provider: account.ProviderConsole, input: " Console/grok-4.3 ", want: "Console/grok-4.3"},
	}
	for _, test := range tests {
		got, ok := NormalizePublicID(test.provider, test.input)
		if !ok || got != test.want {
			t.Fatalf("NormalizePublicID(%q, %q) = %q, %v; want %q", test.provider, test.input, got, ok, test.want)
		}
	}
	if _, ok := NormalizePublicID(account.ProviderBuild, "Web/grok-4.3"); ok {
		t.Fatal("cross-provider namespace was accepted")
	}
	if _, ok := NormalizePublicID(account.ProviderBuild, strings.Repeat("x", MaxPublicIDLength)); ok {
		t.Fatal("overlong canonical public ID was accepted")
	}
}

func TestIsCanonicalPublicIDRequiresExactNamespace(t *testing.T) {
	if !IsCanonicalPublicID(account.ProviderBuild, "Build/grok-4.3") {
		t.Fatal("canonical Build model was rejected")
	}
	for _, value := range []string{"grok-4.3", "build/grok-4.3", " Build/grok-4.3 "} {
		if IsCanonicalPublicID(account.ProviderBuild, value) {
			t.Fatalf("non-canonical model %q was accepted", value)
		}
	}
}

func TestExternalPublicIDAndCandidatesSeparateClientAndRouteNames(t *testing.T) {
	if got := ExternalPublicID(account.ProviderBuild, "Build/grok-4.5"); got != "grok-4.5" {
		t.Fatalf("external public ID = %q", got)
	}
	want := []string{"Build/grok-4.5", "Web/grok-4.5", "Console/grok-4.5"}
	got := PublicIDCandidates("grok-4.5")
	if len(got) != len(want) {
		t.Fatalf("candidates = %#v", got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("candidate %d = %q; want %q", index, got[index], want[index])
		}
	}
	if got := PublicIDCandidates("Console/grok-4.5"); len(got) != 1 || got[0] != "Console/grok-4.5" {
		t.Fatalf("qualified candidates = %#v", got)
	}
}

func TestNormalizeAndDisplayUpstreamModel(t *testing.T) {
	for _, input := range []string{"grok-4.5", "Build/grok-4.5", " build/grok-4.5 "} {
		got, ok := NormalizeUpstreamModel(account.ProviderBuild, input)
		if !ok || got != "grok-4.5" {
			t.Fatalf("NormalizeUpstreamModel(%q) = %q, %v", input, got, ok)
		}
	}
	if _, ok := NormalizeUpstreamModel(account.ProviderBuild, "Console/grok-4.5"); ok {
		t.Fatal("cross-provider upstream prefix was accepted")
	}
	if got := DisplayUpstreamModel(account.ProviderConsole, "grok-4.3"); got != "Console/grok-4.3" {
		t.Fatalf("display upstream = %q", got)
	}
}
