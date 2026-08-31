package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
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

func TestAcquirePinnedBypassesBuildBotFlagSchedulingExclusion(t *testing.T) {
	repo := newLayeredRepositoryFixture()
	repo.bases[0].Credential.BuildBotFlagSource = 1
	selector := NewSelector(repo, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	selector.UpdateExcludeBuildBotFlaggedFromScheduling(true)

	_, err := selector.Acquire(context.Background(), account.ProviderBuild, 0, "model-a", "", "", nil, false)
	var unavailable *SelectionUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Reason != SelectionNoAccounts {
		t.Fatalf("ordinary scheduling error = %v", err)
	}

	lease, err := selector.AcquirePinned(context.Background(), account.ProviderBuild, 1, 0, "model-a", "", true)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Credential.ID != 1 || lease.Credential.BuildBotFlagSource != 1 {
		t.Fatalf("pinned lease = %#v", lease.Credential)
	}
}
