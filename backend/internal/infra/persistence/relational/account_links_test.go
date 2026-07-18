package relational

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestReconcileProviderLinksUsesOnlyHighConfidenceIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "account-links.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewAccountRepository(database)
	digest := strings.Repeat("a", 64)
	identity := "sso_" + digest[:32]
	web := createLinkedAccountTestCredential(t, ctx, repo, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: "web", SourceKey: "sso:" + digest,
		UserID: "user-1", EgressIdentity: identity,
	})
	nsfwEnabledAt := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	if err := repo.MarkWebNSFWEnabled(ctx, web.ID, nsfwEnabledAt); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkWebTermsAccepted(ctx, web.ID, account.CurrentWebTermsVersion, nsfwEnabledAt); err != nil {
		t.Fatal(err)
	}
	console := createLinkedAccountTestCredential(t, ctx, repo, account.Credential{
		Provider: account.ProviderConsole, AuthType: account.AuthTypeSSO, Name: "console", SourceKey: "console-sso:" + digest,
	})
	build := createLinkedAccountTestCredential(t, ctx, repo, account.Credential{
		Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: "build", SourceKey: "build-1", UserID: "user-1",
	})
	for _, id := range []uint64{console.ID, build.ID, web.ID} {
		if err := repo.ReconcileProviderLinks(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	web, err = repo.Get(ctx, web.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(web.LinkedAccounts) != 2 || web.LinkedAccounts[0].Provider != account.ProviderBuild || web.LinkedAccounts[1].Provider != account.ProviderConsole {
		t.Fatalf("web links = %#v", web.LinkedAccounts)
	}
	build, err = repo.Get(ctx, build.ID)
	if err != nil {
		t.Fatal(err)
	}
	console, err = repo.Get(ctx, console.ID)
	if err != nil {
		t.Fatal(err)
	}
	if build.EgressIdentity != identity || console.EgressIdentity != identity || web.EgressIdentity != identity {
		t.Fatalf("egress identities web=%q build=%q console=%q", web.EgressIdentity, build.EgressIdentity, console.EgressIdentity)
	}
	if web.WebNSFWEnabledAt == nil || build.WebNSFWEnabledAt == nil || console.WebNSFWEnabledAt == nil || !web.WebNSFWEnabledAt.Equal(nsfwEnabledAt) || !build.WebNSFWEnabledAt.Equal(nsfwEnabledAt) || !console.WebNSFWEnabledAt.Equal(nsfwEnabledAt) {
		t.Fatalf("shared NSFW markers web=%v build=%v console=%v", web.WebNSFWEnabledAt, build.WebNSFWEnabledAt, console.WebNSFWEnabledAt)
	}
	if web.WebTermsAcceptedAt == nil || build.WebTermsAcceptedAt == nil || console.WebTermsAcceptedAt == nil || !web.WebTermsAcceptedAt.Equal(nsfwEnabledAt) || !build.WebTermsAcceptedAt.Equal(nsfwEnabledAt) || !console.WebTermsAcceptedAt.Equal(nsfwEnabledAt) {
		t.Fatalf("shared terms markers web=%v build=%v console=%v", web.WebTermsAcceptedAt, build.WebTermsAcceptedAt, console.WebTermsAcceptedAt)
	}
	if web.WebTermsAcceptedVersion != account.CurrentWebTermsVersion || build.WebTermsAcceptedVersion != account.CurrentWebTermsVersion || console.WebTermsAcceptedVersion != account.CurrentWebTermsVersion {
		t.Fatalf("shared terms versions web=%d build=%d console=%d", web.WebTermsAcceptedVersion, build.WebTermsAcceptedVersion, console.WebTermsAcceptedVersion)
	}
	if build.LinkedAccountID != web.ID || build.LinkedProvider != account.ProviderWeb || len(console.LinkedAccounts) != 1 || console.LinkedAccounts[0].ID != web.ID {
		t.Fatalf("reverse links build=%#v console=%#v", build.LinkedAccounts, console.LinkedAccounts)
	}
	for _, provider := range []account.Provider{account.ProviderWeb, account.ProviderBuild, account.ProviderConsole} {
		values, listErr := repo.ListEnabled(ctx, provider)
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(values) != 1 || values[0].EgressIdentity != identity {
			t.Fatalf("routing identities for %s = %#v", provider, values)
		}
	}
	if _, err := repo.UpdateTokens(ctx, web.ID, "rotated-encrypted-token", "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	web, err = repo.Get(ctx, web.ID)
	if err != nil || web.EgressIdentity != identity {
		t.Fatalf("token update changed egress identity: %q err=%v", web.EgressIdentity, err)
	}

	emailWeb := createLinkedAccountTestCredential(t, ctx, repo, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: "email-web", SourceKey: "sso:" + strings.Repeat("b", 64), Email: "same@example.com",
	})
	_ = createLinkedAccountTestCredential(t, ctx, repo, account.Credential{
		Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: "email-build", SourceKey: "email-build", Email: "same@example.com",
	})
	if err := repo.ReconcileProviderLinks(ctx, emailWeb.ID); err != nil {
		t.Fatal(err)
	}
	emailWeb, err = repo.Get(ctx, emailWeb.ID)
	if err != nil || len(emailWeb.LinkedAccounts) != 0 {
		t.Fatalf("email-only account was linked: %#v err=%v", emailWeb.LinkedAccounts, err)
	}

	multiWeb := createLinkedAccountTestCredential(t, ctx, repo, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: "multi-web", SourceKey: "sso:" + strings.Repeat("d", 64), UserID: "shared-user",
	})
	for _, teamID := range []string{"team-a", "team-b"} {
		_ = createLinkedAccountTestCredential(t, ctx, repo, account.Credential{
			Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: "multi-" + teamID, SourceKey: "multi-" + teamID, UserID: "shared-user", TeamID: teamID,
		})
	}
	if err := repo.ReconcileProviderLinks(ctx, multiWeb.ID); err != nil {
		t.Fatal(err)
	}
	multiWeb, err = repo.Get(ctx, multiWeb.ID)
	if err != nil || len(multiWeb.LinkedAccounts) != 0 {
		t.Fatalf("ambiguous user was linked: %#v err=%v", multiWeb.LinkedAccounts, err)
	}

	if err := repo.Delete(ctx, web.ID); err != nil {
		t.Fatal(err)
	}
	build, buildErr := repo.Get(ctx, build.ID)
	console, consoleErr := repo.Get(ctx, console.ID)
	if buildErr != nil || consoleErr != nil || len(build.LinkedAccounts) != 0 || len(console.LinkedAccounts) != 0 {
		t.Fatalf("deleting Web affected linked accounts: build=%#v/%v console=%#v/%v", build, buildErr, console, consoleErr)
	}
}

func TestInitializeSchemaBackfillsStableWebEgressIdentity(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "legacy-account-links.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewAccountRepository(database)
	digest := strings.Repeat("c", 64)
	web := createLinkedAccountTestCredential(t, ctx, repo, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: "legacy-web", SourceKey: "sso:" + digest,
		FailureCount: 3, LastError: "preserve-me",
	})
	build := createLinkedAccountTestCredential(t, ctx, repo, account.Credential{
		Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: "legacy-build", SourceKey: "legacy-build",
	})
	if err := repo.LinkWebToBuild(ctx, web.ID, build.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := repo.SaveQuotaWindows(ctx, web.ID, account.WebTierAuto, now, []account.QuotaWindow{{
		AccountID: web.ID, Mode: "weekly", Remaining: 7, Total: 10, WindowSeconds: 3600, SyncedAt: &now, Source: account.QuotaSourceUpstream,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := database.db.Migrator().DropConstraint(&webAccountProfileModel{}, "chk_web_account_profiles_egress_identity"); err != nil {
		t.Fatal(err)
	}
	if err := database.db.Migrator().DropColumn(&webAccountProfileModel{}, "EgressIdentity"); err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatalf("migration is not idempotent: %v", err)
	}
	web, err = repo.Get(ctx, web.ID)
	if err != nil {
		t.Fatal(err)
	}
	build, err = repo.Get(ctx, build.ID)
	if err != nil {
		t.Fatal(err)
	}
	windows, err := repo.GetQuotaWindows(ctx, []uint64{web.ID})
	if err != nil {
		t.Fatal(err)
	}
	wantIdentity := "sso_" + digest[:32]
	if web.EgressIdentity != wantIdentity || build.EgressIdentity != wantIdentity || web.FailureCount != 3 || web.LastError != "preserve-me" || len(windows[web.ID]) != 1 || windows[web.ID][0].Remaining != 7 {
		t.Fatalf("migration result web=%#v buildIdentity=%q windows=%#v", web, build.EgressIdentity, windows[web.ID])
	}
}

func TestReconcileWebConsoleUsesUniqueUserIDAcrossDifferentSSOTokens(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "web-console-user-link.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewAccountRepository(database)
	webDigest := strings.Repeat("e", 64)
	web := createLinkedAccountTestCredential(t, ctx, repo, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: "web", SourceKey: "sso:" + webDigest,
		UserID: "same-user", Email: "same@example.com", EgressIdentity: "sso_" + webDigest[:32],
	})
	console := createLinkedAccountTestCredential(t, ctx, repo, account.Credential{
		Provider: account.ProviderConsole, AuthType: account.AuthTypeSSO, Name: "console",
		SourceKey: "console-sso:" + strings.Repeat("f", 64), UserID: "same-user", Email: "same@example.com",
	})
	if err := repo.ReconcileProviderLinks(ctx, console.ID); err != nil {
		t.Fatal(err)
	}
	console, err = repo.Get(ctx, console.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(console.LinkedAccounts) != 1 || console.LinkedAccounts[0].ID != web.ID || console.LinkedAccounts[0].Email != "same@example.com" || console.LinkedAccounts[0].UserID != "same-user" || console.EgressIdentity != web.EgressIdentity {
		t.Fatalf("console link = %#v identity=%q, web identity=%q", console.LinkedAccounts, console.EgressIdentity, web.EgressIdentity)
	}
}

func createLinkedAccountTestCredential(t *testing.T, ctx context.Context, repo *AccountRepository, value account.Credential) account.Credential {
	t.Helper()
	value.EncryptedAccessToken = "encrypted"
	value.Enabled = true
	value.AuthStatus = account.AuthStatusActive
	value.Priority = account.DefaultPriority
	value.MaxConcurrent = account.DefaultMaxConcurrent
	stored, _, err := repo.UpsertByIdentity(ctx, value)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}
