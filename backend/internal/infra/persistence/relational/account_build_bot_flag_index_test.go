package relational

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestBuildBotFlagIndexDrivesRoutingFilteringAndAvailableCount(t *testing.T) {
	ctx := context.Background()
	repo := NewAccountRepository(openTestDatabase(t))
	now := time.Now().UTC().Truncate(time.Second)
	cooldownUntil := now.Add(time.Minute)

	flaggedOne, _, err := repo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "flagged-1", SourceKey: "flagged-1",
		EncryptedAccessToken: "secret-1", AuthStatus: account.AuthStatusActive,
		BuildBotFlagSource: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	flaggedTwo, _, err := repo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "flagged-2", SourceKey: "flagged-2",
		EncryptedAccessToken: "secret-2", AuthStatus: account.AuthStatusActive,
		BuildBotFlagSource: 2, CooldownUntil: &cooldownUntil,
	})
	if err != nil {
		t.Fatal(err)
	}
	normal, _, err := repo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "normal", SourceKey: "normal",
		EncryptedAccessToken: "secret-3", AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	web, _, err := repo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, Name: "web", SourceKey: "web",
		EncryptedAccessToken: "secret-4", AuthStatus: account.AuthStatusActive,
		BuildBotFlagSource: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if web.BuildBotFlagSource != 0 {
		t.Fatalf("non-Build source = %d, want 0", web.BuildBotFlagSource)
	}

	ids, err := repo.ListBuildBotFlaggedAccountIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []uint64{flaggedOne.ID, flaggedTwo.ID}) {
		t.Fatalf("flagged IDs = %v", ids)
	}
	flaggedCount, err := repo.CountBuildBotFlagged(ctx)
	if err != nil || flaggedCount != 2 {
		t.Fatalf("flagged count = %d, err=%v", flaggedCount, err)
	}
	available, err := repo.CountAvailableBuildBotFlagged(ctx, now)
	if err != nil || available != 1 {
		t.Fatalf("available flagged = %d, err=%v", available, err)
	}

	bases, err := repo.ListRoutingAccountBases(ctx, account.ProviderBuild, "")
	if err != nil {
		t.Fatal(err)
	}
	sources := make(map[uint64]int, len(bases))
	for _, base := range bases {
		if base.Credential.EncryptedAccessToken != "" {
			t.Fatalf("routing projection leaked a token: %#v", base.Credential)
		}
		sources[base.Credential.ID] = base.Credential.BuildBotFlagSource
	}
	if sources[flaggedOne.ID] != 1 || sources[flaggedTwo.ID] != 2 || sources[normal.ID] != 0 {
		t.Fatalf("routing sources = %v", sources)
	}

	_, total, err := repo.List(ctx, repository.AccountListQuery{
		Page: repository.PageQuery{Limit: 20},
		Filter: repository.AccountListFilter{
			Provider: string(account.ProviderBuild), Risk: "flagged", Now: now,
		},
	})
	if err != nil || total != 2 {
		t.Fatalf("flagged list total = %d, err=%v", total, err)
	}
	_, total, err = repo.List(ctx, repository.AccountListQuery{
		Page: repository.PageQuery{Limit: 20},
		Filter: repository.AccountListFilter{
			Provider: string(account.ProviderBuild), Risk: "normal", Now: now,
		},
	})
	if err != nil || total != 1 {
		t.Fatalf("normal list total = %d, err=%v", total, err)
	}
}

func TestCountAvailableAmongBatchesLargeIDLists(t *testing.T) {
	repo := NewAccountRepository(openTestDatabase(t))
	ids := make([]uint64, 33_000)
	for index := range ids {
		ids[index] = uint64(index + 1)
	}
	count, err := repo.CountAvailableAmong(context.Background(), account.ProviderBuild, ids, time.Now().UTC())
	if err != nil || count != 0 {
		t.Fatalf("large ID count = %d, err=%v", count, err)
	}
}

func TestUpdateTokensUpdatesAndNormalizesBuildBotFlagSourceAtomically(t *testing.T) {
	ctx := context.Background()
	repo := NewAccountRepository(openTestDatabase(t))
	build, _, err := repo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "build", SourceKey: "build-refresh",
		EncryptedAccessToken: "old", AuthStatus: account.AuthStatusActive,
		BuildBotFlagSource: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := repo.UpdateTokens(ctx, build.ID, "new", "", time.Now().UTC().Add(time.Hour), 2)
	if err != nil || updated.EncryptedAccessToken != "new" || updated.BuildBotFlagSource != 2 {
		t.Fatalf("updated Build credential = %#v, err=%v", updated, err)
	}
	updated, err = repo.UpdateTokens(ctx, build.ID, "newer", "", time.Now().UTC().Add(time.Hour), 3)
	if err != nil || updated.BuildBotFlagSource != 0 {
		t.Fatalf("normalized Build credential = %#v, err=%v", updated, err)
	}

	web, _, err := repo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, Name: "web", SourceKey: "web-refresh",
		EncryptedAccessToken: "old", AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	web, err = repo.UpdateTokens(ctx, web.ID, "new", "", time.Now().UTC().Add(time.Hour), 2)
	if err != nil || web.BuildBotFlagSource != 0 {
		t.Fatalf("web credential = %#v, err=%v", web, err)
	}
}

func TestBuildBotFlagBackfillUsesAccessTokenCompareAndSwap(t *testing.T) {
	ctx := context.Background()
	repo := NewAccountRepository(openTestDatabase(t))
	build, _, err := repo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "build", SourceKey: "build-cas",
		EncryptedAccessToken: "current-token", AuthStatus: account.AuthStatusActive,
		BuildBotFlagSource: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateBuildBotFlagSources(ctx, []repository.BuildBotFlagSourceUpdate{{
		AccountID: build.ID, ExpectedEncryptedAccessToken: "stale-token", Source: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.Get(ctx, build.ID)
	if err != nil || stored.BuildBotFlagSource != 2 {
		t.Fatalf("stale backfill changed credential = %#v, err=%v", stored, err)
	}
	if err := repo.UpdateBuildBotFlagSources(ctx, []repository.BuildBotFlagSourceUpdate{{
		AccountID: build.ID, ExpectedEncryptedAccessToken: "current-token", Source: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	stored, err = repo.Get(ctx, build.ID)
	if err != nil || stored.BuildBotFlagSource != 1 {
		t.Fatalf("matching backfill did not update credential = %#v, err=%v", stored, err)
	}
}

func TestInitializeSchemaAddsBuildBotFlagIndexWithoutLosingCredentials(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewAccountRepository(database)
	created, _, err := repo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "legacy", SourceKey: "legacy",
		EncryptedAccessToken: "preserved-secret", AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Exec("DROP INDEX idx_account_credentials_build_bot_flag").Error; err != nil {
		t.Fatal(err)
	}
	if err := database.withSQLiteForeignKeysDisabled(ctx, func() error {
		if err := database.db.WithContext(ctx).Migrator().DropConstraint(&accountCredentialModel{}, "chk_account_credentials_build_bot_flag_source"); err != nil {
			return err
		}
		return database.db.WithContext(ctx).Migrator().DropColumn(&accountCredentialModel{}, "BuildBotFlagSource")
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if !database.db.WithContext(ctx).Migrator().HasColumn(&accountCredentialModel{}, "BuildBotFlagSource") {
		t.Fatal("build_bot_flag_source column was not restored")
	}
	assertSQLiteIndexes(t, database, "account_credentials", "idx_account_credentials_build_bot_flag")
	stored, err := repo.Get(ctx, created.ID)
	if err != nil || stored.EncryptedAccessToken != "preserved-secret" || stored.BuildBotFlagSource != 0 {
		t.Fatalf("stored credential = %#v, err=%v", stored, err)
	}
}
