package account

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestSyncAccountIdentityLinksUniqueBuildWithoutSharingState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, repo, adapter := newWebAccountSettingsTestService(t)
	web, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: "web", SourceKey: "sso:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: accountdomain.AuthStatusActive, Priority: 7, MaxConcurrent: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	build, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, AuthType: accountdomain.AuthTypeOAuth, Name: "build", SourceKey: "build", UserID: "user-1",
		EncryptedAccessToken: "encrypted", Enabled: false, AuthStatus: accountdomain.AuthStatusReauthRequired, Priority: 1, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	build.Enabled = false
	build, err = repo.Update(ctx, build)
	if err != nil {
		t.Fatal(err)
	}
	adapter.identity = provider.AccountIdentity{UserID: "user-1", Email: "user@example.com"}
	if err := service.SyncAccountIdentity(ctx, web.ID); err != nil {
		t.Fatal(err)
	}
	web, err = repo.Get(ctx, web.ID)
	if err != nil {
		t.Fatal(err)
	}
	build, err = repo.Get(ctx, build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if web.UserID != "user-1" || web.Email != "user@example.com" || len(web.LinkedAccounts) != 1 || web.LinkedAccounts[0].ID != build.ID {
		t.Fatalf("web = %#v", web)
	}
	if !web.Enabled || web.AuthStatus != accountdomain.AuthStatusActive || web.Priority != 7 || web.MaxConcurrent != 3 {
		t.Fatalf("web operational state changed: %#v", web)
	}
	if build.Enabled || build.AuthStatus != accountdomain.AuthStatusReauthRequired {
		t.Fatalf("build operational state changed: %#v", build)
	}
	if err := service.SyncAccountIdentity(ctx, web.ID); err != nil {
		t.Fatal(err)
	}
	if adapter.identityCalls != 1 {
		t.Fatalf("identity calls = %d", adapter.identityCalls)
	}
}

func TestSyncAccountIdentityUnauthorizedInvalidatesCurrentProviderAccount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, repo, adapter := newWebAccountSettingsTestService(t)
	web, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: "web", SourceKey: "sso:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: accountdomain.AuthStatusActive, Priority: 1, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter.identityErr = provider.ErrUnauthorized
	if err := service.SyncAccountIdentity(ctx, web.ID); !errors.Is(err, provider.ErrUnauthorized) {
		t.Fatalf("err = %v", err)
	}
	web, err = repo.Get(ctx, web.ID)
	if err != nil {
		t.Fatal(err)
	}
	if web.AuthStatus != accountdomain.AuthStatusReauthRequired || !web.Enabled || web.FailureCount != 0 {
		t.Fatalf("identity unauthorized state = %#v", web)
	}
}

func TestSyncAccountIdentityDoesNotRepeatWhenEmailIsKnown(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, repo, adapter := newWebAccountSettingsTestService(t)
	web, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: "web", Email: "known@example.com",
		SourceKey:            "sso:" + "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter.identity = provider.AccountIdentity{UserID: "stable-user", Email: "known@example.com"}
	if err := service.SyncAccountIdentity(ctx, web.ID); err != nil {
		t.Fatal(err)
	}
	web, err = repo.Get(ctx, web.ID)
	if err != nil {
		t.Fatal(err)
	}
	if web.UserID != "" || web.Email != "known@example.com" || adapter.identityCalls != 0 {
		t.Fatalf("identity was fetched again: user_id=%q email=%q calls=%d", web.UserID, web.Email, adapter.identityCalls)
	}
}

func TestSyncConsoleAccountIdentityLinksUniqueWebAccountOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "console-identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(database)
	web, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: "web",
		SourceKey: "sso:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		UserID:    "same-user", EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	console, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderConsole, AuthType: accountdomain.AuthTypeSSO, Name: "console",
		SourceKey:            "console-sso:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &consoleIdentityAdapterStub{identity: provider.AccountIdentity{UserID: "same-user", Email: "same@example.com"}}
	service := NewService(repo, nil, nil, nil, provider.NewRegistry(adapter), nil, nil)
	if err := service.SyncAccountIdentity(ctx, console.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.SyncAccountIdentity(ctx, console.ID); err != nil {
		t.Fatal(err)
	}
	console, err = repo.Get(ctx, console.ID)
	if err != nil {
		t.Fatal(err)
	}
	if adapter.calls != 1 || console.UserID != "same-user" || len(console.LinkedAccounts) != 1 || console.LinkedAccounts[0].ID != web.ID {
		t.Fatalf("calls=%d console=%#v", adapter.calls, console)
	}
}

type consoleIdentityAdapterStub struct {
	identity provider.AccountIdentity
	calls    int
}

func (*consoleIdentityAdapterStub) Provider() accountdomain.Provider {
	return accountdomain.ProviderConsole
}

func (a *consoleIdentityAdapterStub) SyncAccountIdentity(context.Context, accountdomain.Credential) (provider.AccountIdentity, error) {
	a.calls++
	return a.identity, nil
}
