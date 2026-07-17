package relational

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestBuildSuperEntitledDefaultsFalseAndSurvivesUpsert(t *testing.T) {
	ctx := context.Background()
	repo := NewAccountRepository(openTestDatabase(t))
	created, wasCreated, err := repo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "entitled", SourceKey: "entitled-upsert",
		EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive,
	})
	if err != nil || !wasCreated {
		t.Fatalf("create = %#v created=%v err=%v", created, wasCreated, err)
	}
	if created.BuildSuperEntitled {
		t.Fatal("new account must default BuildSuperEntitled=false")
	}
	if created.BuildRouteMode != account.BuildRouteAuto {
		t.Fatalf("new account route mode = %q", created.BuildRouteMode)
	}
	created.BuildSuperEntitled = true
	created.BuildRouteMode = account.BuildRouteXAI
	if _, err := repo.Update(ctx, created); err != nil {
		t.Fatal(err)
	}
	// 普通 upsert 不得清除 entitlement。
	updated, wasCreated, err := repo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "entitled-renamed", SourceKey: "entitled-upsert",
		EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive,
		BuildSuperEntitled: false,
	})
	if err != nil || wasCreated {
		t.Fatalf("upsert = %#v created=%v err=%v", updated, wasCreated, err)
	}
	if !updated.BuildSuperEntitled || updated.BuildRouteMode != account.BuildRouteXAI || updated.Name != "entitled-renamed" {
		t.Fatalf("entitlement must survive upsert: %#v", updated)
	}
	// token refresh 路径不改 account 表 entitlement 列。
	refreshed, err := repo.UpdateTokens(ctx, updated.ID, "encrypted-new", "encrypted-refresh", time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !refreshed.BuildSuperEntitled || refreshed.BuildRouteMode != account.BuildRouteXAI {
		t.Fatalf("token refresh must preserve Build settings: %#v", refreshed)
	}
}

func TestBuildSuperEntitledNonBuildForcedFalse(t *testing.T) {
	ctx := context.Background()
	repo := NewAccountRepository(openTestDatabase(t))
	created, _, err := repo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: "web", SourceKey: "web-entitled",
		EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive,
		BuildSuperEntitled: true, BuildRouteMode: account.BuildRouteXAI,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.BuildSuperEntitled {
		t.Fatal("non-Build must not persist BuildSuperEntitled")
	}
	if created.BuildRouteMode != account.BuildRouteAuto {
		t.Fatalf("non-Build route mode = %q", created.BuildRouteMode)
	}
}

func TestListRoutingCandidatesSharesEntitledBuildModels(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accounts := NewAccountRepository(database)
	models := NewModelRepository(database)
	observer, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "observer", SourceKey: "ent-observer",
		EncryptedAccessToken: testEncryptedToken, Enabled: true, AuthStatus: account.AuthStatusActive,
		BuildSuperEntitled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// re-set after upsert create (fromAccountDomain true only when ProviderBuild — create path uses input)
	observer.BuildSuperEntitled = true
	if _, err := accounts.Update(ctx, observer); err != nil {
		t.Fatal(err)
	}
	peer, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "peer", SourceKey: "ent-peer",
		EncryptedAccessToken: testEncryptedToken, Enabled: true, AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	peer.BuildSuperEntitled = true
	if _, err := accounts.Update(ctx, peer); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	const sharedModel = "grok-imagine-video-1.5"
	if err := models.UpsertDiscovered(ctx, account.ProviderBuild, []string{sharedModel}); err != nil {
		t.Fatal(err)
	}
	if err := models.ReplaceAccountCapabilities(ctx, observer.ID, []string{sharedModel}, now); err != nil {
		t.Fatal(err)
	}
	if err := models.ReplaceAccountCapabilities(ctx, peer.ID, []string{"grok-4.5"}, now); err != nil {
		t.Fatal(err)
	}
	candidates, err := accounts.ListRoutingCandidates(ctx, account.ProviderBuild, sharedModel, "")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[uint64]account.RoutingCandidate{}
	for _, c := range candidates {
		byID[c.Credential.ID] = c
	}
	if c := byID[observer.ID]; !c.SupportsModel {
		t.Fatalf("entitled observer should support model: %#v", c)
	}
	if c := byID[peer.ID]; !c.SupportsModel || !c.ModelCapabilityKnown {
		t.Fatalf("entitled peer should share Super model support: %#v", c)
	}
	// paid filter includes entitlement
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{QuotaType: "paid", Now: now}, 2)
	assertAccountFilterCount(t, ctx, accounts, repository.AccountListFilter{QuotaType: "free", Now: now}, 0)
}
