package gateway

import (
	"context"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestApplyBuildBotFlaggedFilterOnlyAffectsBuild(t *testing.T) {
	selector := NewSelector(nil, nil, nil, nil, 0, 0, 0)
	selector.UpdateExcludeBuildBotFlaggedFromScheduling(true)

	values := []account.RoutingCandidate{
		{Credential: account.Credential{ID: 1, Provider: account.ProviderBuild}},
		{Credential: account.Credential{ID: 2, Provider: account.ProviderBuild, BuildBotFlagSource: 1}},
		{Credential: account.Credential{ID: 3, Provider: account.ProviderBuild}},
		{Credential: account.Credential{ID: 4, Provider: account.ProviderBuild, BuildBotFlagSource: 2}},
	}

	filtered, err := selector.applyBuildBotFlaggedFilter(context.Background(), account.ProviderBuild, values)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 2 || filtered[0].Credential.ID != 1 || filtered[1].Credential.ID != 3 {
		t.Fatalf("unexpected build filter result: %+v", filtered)
	}

	webValues := []account.RoutingCandidate{
		{Credential: account.Credential{ID: 2, Provider: account.ProviderWeb, BuildBotFlagSource: 1}},
		{Credential: account.Credential{ID: 4, Provider: account.ProviderWeb, BuildBotFlagSource: 2}},
	}
	webFiltered, err := selector.applyBuildBotFlaggedFilter(context.Background(), account.ProviderWeb, webValues)
	if err != nil {
		t.Fatal(err)
	}
	if len(webFiltered) != 2 {
		t.Fatalf("web candidates should not be filtered, got %d", len(webFiltered))
	}

	selector.UpdateExcludeBuildBotFlaggedFromScheduling(false)
	unfiltered, err := selector.applyBuildBotFlaggedFilter(context.Background(), account.ProviderBuild, values)
	if err != nil {
		t.Fatal(err)
	}
	if len(unfiltered) != 4 {
		t.Fatalf("disabled filter should keep all candidates, got %d", len(unfiltered))
	}
}
